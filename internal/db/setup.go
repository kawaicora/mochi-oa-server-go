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

	"gorm.io/gorm"

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
	// ── 旧库升级迁移 ──
	// 设计原则：全新库 AutoMigrate 已按 types 建出最终表，以下迁移全部静默跳过，启动零报错；
	// 仅当检测到「旧列结构」才执行，用于从旧库（varchar 存时刻、work_start 快照）无损升级。
	g := s.gorm.WithContext(ctx)

	// 1) work_start/work_end 旧列 → std_start/std_end 快照拷贝（目标列为 TIME，空串用 NULLIF 归一为 NULL）。
	if columnExists(g, "checkins", "work_start") {
		if err := g.Exec(
			"UPDATE checkins SET std_start = NULLIF(work_start, ''), std_end = NULLIF(work_end, '') WHERE std_start IS NULL AND work_start IS NOT NULL",
		).Error; err != nil {
			logger.Error("db", "checkins 快照列迁移 std_start/std_end 失败(不阻断)", "error", err.Error())
		}
	}

	// 2) checkins.std_start/std_end：仅旧库 varchar→TIME（新库已是 time 跳过）。先清空串再 MODIFY。
	if columnType(g, "checkins", "std_start") == "varchar" {
		for _, sql := range []string{
			"UPDATE checkins SET std_start = NULL WHERE std_start = ''",
			"UPDATE checkins SET std_end = NULL WHERE std_end = ''",
			"ALTER TABLE checkins MODIFY `std_start` TIME NULL",
			"ALTER TABLE checkins MODIFY `std_end` TIME NULL",
		} {
			if err := g.Exec(sql).Error; err != nil {
				logger.Error("db", "时间字段类型迁移失败", "sql", sql, "error", err.Error())
			}
		}
	}
	// 3) company_checkin_rules.offwork_deadline：仅旧库 varchar→TIME（注意列在 rules 表，不在 settings）。
	if columnType(g, "company_checkin_rules", "offwork_deadline") == "varchar" {
		if err := g.Exec("ALTER TABLE company_checkin_rules MODIFY `offwork_deadline` TIME NOT NULL").Error; err != nil {
			logger.Error("db", "offwork_deadline 改 TIME 失败(不阻断)", "error", err.Error())
		}
	}
	// 4) checkins.check_date：仅旧库 varchar→DATE。
	if columnType(g, "checkins", "check_date") == "varchar" {
		if err := g.Exec("ALTER TABLE checkins MODIFY `check_date` DATE NOT NULL").Error; err != nil {
			logger.Error("db", "check_date 改 DATE 失败(不阻断)", "error", err.Error())
		}
	}
	// 5) attendance_requests.punch_time：仅旧库 varchar→DATETIME。
	if columnType(g, "attendance_requests", "punch_time") == "varchar" {
		for _, sql := range []string{
			"UPDATE attendance_requests SET punch_time = REPLACE(punch_time, 'T', ' ') WHERE punch_time LIKE '%T%'",
			"UPDATE attendance_requests SET punch_time = NULL WHERE punch_time = ''",
			"ALTER TABLE attendance_requests MODIFY `punch_time` DATETIME NULL",
		} {
			if err := g.Exec(sql).Error; err != nil {
				logger.Error("db", "时间字段类型迁移失败", "sql", sql, "error", err.Error())
			}
		}
	}
	// 6) afdian_orders.create_time：仅旧库 bigint(unix秒)→DATETIME；新库已是 datetime 直接跳过，避免误删正确列。
	if columnType(g, "afdian_orders", "create_time") == "bigint" {
		for _, sql := range []string{
			"ALTER TABLE afdian_orders ADD COLUMN _ct_dt DATETIME NULL",
			"UPDATE afdian_orders SET _ct_dt = FROM_UNIXTIME(create_time) WHERE create_time > 0 AND _ct_dt IS NULL",
			"ALTER TABLE afdian_orders DROP COLUMN create_time",
			"ALTER TABLE afdian_orders CHANGE COLUMN _ct_dt create_time DATETIME NOT NULL",
		} {
			if err := g.Exec(sql).Error; err != nil {
				logger.Error("db", "时间字段类型迁移失败", "sql", sql, "error", err.Error())
			}
		}
	}
	return nil
}

// columnExists 判断某表某列是否存在（information_schema，兼容旧库升级检测）。
func columnExists(g *gorm.DB, table, column string) bool {
	var n int64
	g.Raw("SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=? AND COLUMN_NAME=?", table, column).Scan(&n)
	return n > 0
}

// columnType 取某列数据类型（小写，如 time/datetime/bigint/varchar）；列不存在返回空串。
func columnType(g *gorm.DB, table, column string) string {
	var t string
	g.Raw("SELECT DATA_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=? AND COLUMN_NAME=?", table, column).Scan(&t)
	return t
}
