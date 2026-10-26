// 启动期自动建库 / 建表（GORM AutoMigrate）。
//
//   - ensureDatabase：用不带库名的连接执行 CREATE DATABASE IF NOT EXISTS，
//     解决「全新环境里库还不存在，连都连不上」的问题；
//   - autoMigrateTables：把 types 包里的 32 张表 Row 结构体交给 GORM AutoMigrate，
//     缺表则建表，已存在的表则补齐缺失的列/索引（不会删列、不会改数据）。
//
// 两者分别由 MySQL 配置项 AutoCreateDatabase / AutoMigrate 控制
// （环境变量 AUTO_CREATE_DATABASE、AUTO_MIGRATE，均默认 true）。
// 关掉后行为退回到「只读复用现有表」。
package db

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"

	// 注册 "mysql" 驱动，供下面的建库连接使用（GORM 的 mysql dialector 同样依赖它）。
	_ "github.com/go-sql-driver/mysql"

	"mochi-oa-server-go/internal/config"
	"mochi-oa-server-go/internal/logger"
	"mochi-oa-server-go/internal/types"
)

// validDBName 只允许常规库名，避免把配置值直接拼进 DDL 造成注入。
var validDBName = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// ensureDatabase 确保目标数据库存在（不存在则创建，字符集/排序规则与 mochioa.sql 一致）。
func ensureDatabase(cfg config.MySQLConfig) error {
	if !validDBName.MatchString(cfg.Database) {
		return fmt.Errorf("非法的数据库名 %q：只允许字母、数字与下划线", cfg.Database)
	}
	// 不带库名的 DSN：此时目标库可能还不存在，不能连进去。
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/?charset=utf8mb4&parseTime=true&loc=UTC",
		cfg.User, cfg.Password, cfg.Host, cfg.Port)
	server, err := sql.Open("mysql", dsn)
	if err != nil {
		return err
	}
	defer server.Close()

	// 库名已由 validDBName 约束，可安全拼入。
	stmt := fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci", cfg.Database)
	if _, err := server.Exec(stmt); err != nil {
		return err
	}
	logger.Info("db", "数据库已就绪", "database", cfg.Database)
	return nil
}

// autoMigrateTables 按 Row 结构体建表/补列。
func (s *MySqlStore) autoMigrateTables(ctx context.Context) error {
	models := []any{
		&types.UserRow{},
		&types.CompanyRow{},
		&types.CompanyMemberRow{},
		&types.DepartmentRow{},
		&types.DepartmentMemberRow{},
		&types.GroupRow{},
		&types.GroupMemberRow{},
		&types.ConversationRow{},
		&types.ConversationMemberRow{},
		&types.MessageRow{},
		&types.FileRow{},
		&types.FriendRow{},
		&types.CompanyInvitationRow{},
		&types.MemberProfileRow{},
		&types.SessionRow{},
		&types.MailConfigRow{},
		&types.HolidayRow{},
		&types.ProjectRow{},
		&types.ProjectMemberRow{},
		&types.TaskRow{},
		&types.TaskAssignmentRow{},
		&types.TaskCommentRow{},
		&types.TaskIssueRow{},
		&types.TaskStatusLogRow{},
		&types.TaskExtensionRow{},
		&types.RequirementRow{},
		&types.BugRow{},
		&types.PlanRow{},
		&types.DocumentRow{},
		&types.WikiPageRow{},
		&types.RequirementLinkRow{},
		&types.AfdianOrderRow{},
		&types.ReportTemplateRow{},
		&types.ReportRow{},
		&types.AttendanceRequestRow{},
		&types.ExpenseRequestRow{},
		&types.AdvanceRequestRow{},
		&types.CheckinRow{},
		&types.CompanyCheckinSettingsRow{},
		&types.CompanyCheckinRulesRow{},
		&types.ProxyAccountRow{},
		&types.ProxyAccessRequestRow{},
		&types.AssetRow{},
		&types.AssetFlowRow{},
		&types.JobRow{},
	}
	if err := s.gorm.WithContext(ctx).AutoMigrate(models...); err != nil {
		logger.Error("db", "自动建表失败", "error", err.Error())
		return err
	}
	// messages.kind 为 MySQL ENUM，AutoMigrate 不会扩展枚举值；
	// 网络申请卡片 proxy_card / 报销卡片 expense_card / 备用金卡片 advance_card 需加入枚举（重复执行幂等）。
	alterSQL := "ALTER TABLE messages MODIFY `kind` ENUM('text','image','file','video','audio','folder','proxy_card','expense_card','advance_card') NOT NULL DEFAULT 'text'"
	if err := s.gorm.WithContext(ctx).Exec(alterSQL).Error; err != nil {
		logger.Error("db", "扩展 messages.kind 枚举失败", "error", err.Error())
		// 不阻断启动（旧库可能结构差异），仅记录。
	}
	logger.Info("db", "自动建表完成", "tables", len(models))
	// attendance_requests.cc_viewed_ids：抄送人已查看标记（JSON 数组，可空无默认）。
	// MySQL 不支持 ADD COLUMN IF NOT EXISTS（MariaDB 语法），重复加列会报 1060，忽略即可。
	addColSQL := "ALTER TABLE attendance_requests ADD COLUMN cc_viewed_ids JSON NULL"
	if err := s.gorm.WithContext(ctx).Exec(addColSQL).Error; err != nil {
		logger.Info("db", "补 attendance_requests.cc_viewed_ids 跳过（可能已存在）", "error", err.Error())
	}
	// notify_stopped：业务通知循环提醒停止标记（审批/申请处理或用户主动停止后 true，不再每小时重弹）。
	for _, tbl := range []string{"attendance_requests", "proxy_access_requests"} {
		sql := "ALTER TABLE " + tbl + " ADD COLUMN notify_stopped TINYINT NOT NULL DEFAULT 0"
		if err := s.gorm.WithContext(ctx).Exec(sql).Error; err != nil {
			logger.Info("db", "补 "+tbl+".notify_stopped 跳过（可能已存在）", "error", err.Error())
		}
	}
	// 列改名 work_start/work_end → std_start/std_end：AutoMigrate 已建新列，把旧快照数据拷过去保住历史判定。
	if err := s.gorm.WithContext(ctx).Exec(
		"UPDATE checkins SET std_start = work_start, std_end = work_end WHERE (std_start IS NULL OR std_start = '') AND work_start IS NOT NULL",
	).Error; err != nil {
		logger.Error("db", "checkins 快照列迁移 std_start/std_end 失败", "error", err.Error())
		return err
	}
	// 时间字段类型规范化：varchar 存时刻 → TIME。先清理空串再 MODIFY，重复执行幂等，错误仅记录不阻断启动。
	timeTypeMigrations := []string{
		// checkins.std_start/std_end：空串置 NULL（TIME 列不接受 ''），再 varchar→TIME。
		"UPDATE checkins SET std_start = NULL WHERE std_start = ''",
		"UPDATE checkins SET std_end = NULL WHERE std_end = ''",
		"ALTER TABLE checkins MODIFY `std_start` TIME NULL",
		"ALTER TABLE checkins MODIFY `std_end` TIME NULL",
		// company_checkin_settings.offwork_deadline：varchar→TIME。
		"ALTER TABLE company_checkin_settings MODIFY `offwork_deadline` TIME NOT NULL DEFAULT '04:00'",
	}
	for _, sql := range timeTypeMigrations {
		if err := s.gorm.WithContext(ctx).Exec(sql).Error; err != nil {
			logger.Error("db", "时间字段类型迁移失败", "sql", sql, "error", err.Error())
		}
	}
	return nil
}
