// 启动期自动建库 / 建表（GORM AutoMigrate）。
//
//   - ensureDatabase：用不带库名的连接执行 CREATE DATABASE IF NOT EXISTS，
//     解决「全新环境里库还不存在，连都连不上」的问题；
//   - autoMigrateTables：把 types 包里的表 Row 结构体交给 GORM AutoMigrate，
//     缺表则建表，已存在的表则补齐缺失的列/索引（不会删列、不会改数据）。
//
// 显式 DDL 迁移（枚举扩展 / 补列 / 旧库列类型升级，如 checkins.std_start 等）
// 已抽离为独立工具 cmd/tools/database_update，见 build.ps1 / build.sh；
// 服务端启动只负责 AutoMigrate，不内嵌任何 ALTER/ADD 语句。
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
	"strings"

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
		&types.ServerAdminRow{},
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
		&types.CompanyApprovalCcRow{},
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
	// 显式 DDL 迁移（messages.kind 枚举扩展 / attendance_requests.cc_viewed_ids /
	// notify_stopped 补列 / checkins.std_start 等旧库列类型升级）已抽离为独立工具
	// cmd/tools/database_update；服务端启动只负责 GORM AutoMigrate 建表/补列，
	// 需要执行迁移时单独运行该工具（build.ps1 / build.sh 已编译，见 dist/mochi-oa-database_update_*）。
	// 建表完成后，首次部署播种 .env 业务配置（server_admins + mail_configs）到库。
	s.seedConfigFromEnv(ctx, config.Get())
	return nil
}

// seedConfigFromEnv 首次部署播种：把 .env 的业务配置作为第一条写入数据库（幂等）。
//
// 只播种两块（用户确认范围）：
//   - server_admins：cfg.ServerAdmins（SERVER_ADMIN_USERS / ADMIN_USER）→ server_admins 表
//   - mail_configs：cfg.Mail（MAIL_*）→ 主邮件配置
//
// 判定"首次"用表是否为空：目标表无记录才写入，有数据则跳过（库可为权威，重启保留，
// 不覆盖后台已修改的值）。MySQL/PORT/STORAGE/JWT 等连接期配置不入库。
func (s *MySqlStore) seedConfigFromEnv(ctx context.Context, cfg *config.ServerConfig) {
	// 1) SERVER_ADMIN 播种：server_admins 表为空才写 .env 种子。
	var admCount int64
	if err := s.gorm.WithContext(ctx).Model(&types.ServerAdminRow{}).Count(&admCount).Error; err != nil {
		logger.Warn("db", "查 server_admins 数量失败，跳过播种", "error", err.Error())
	} else if admCount == 0 {
		for _, u := range cfg.ServerAdmins {
			if err := s.UpsertServerAdmin(ctx, u, 0); err != nil {
				logger.Warn("db", "播种 SERVER_ADMIN 失败", "user", u, "error", err.Error())
			}
		}
		if len(cfg.ServerAdmins) > 0 {
			logger.Info("db", "首次播种 SERVER_ADMIN 到 server_admins", "users", strings.Join(cfg.ServerAdmins, ","))
		}
	}

	// 2) 主邮件播种：mail_configs 无任何记录且环境已配 MAIL_* 才写入一条主配置。
	if cfg.Mail != nil && cfg.Mail.Enabled {
		existing, err := s.ListMailConfigs(ctx, MailConfigScope{})
		if err != nil {
			logger.Warn("db", "查 mail_configs 失败，跳过邮件播种", "error", err.Error())
			return
		}
		if len(existing) == 0 {
			port := int64(cfg.Mail.Port)
			secure := cfg.Mail.Secure
			def := true
			en := true
			if _, err := s.SaveMailConfig(ctx, SaveMailConfigInput{
				Email:       cfg.Mail.FromEmail,
				DisplayName: cfg.Mail.FromName,
				Host:        cfg.Mail.Host,
				Port:        &port,
				Secure:      &secure,
				User:        cfg.Mail.User,
				Password:    cfg.Mail.Password,
				IsDefault:   &def,
				Enabled:     &en,
			}); err != nil {
				logger.Warn("db", "播种主邮件配置失败", "error", err.Error())
				return
			}
			logger.Info("db", "首次播种主邮件配置到 mail_configs", "host", cfg.Mail.Host, "from", cfg.Mail.FromEmail)
		}
	}
}

// columnExists / columnType 辅助函数已随显式迁移一并移入 cmd/tools/database_update 工具。

