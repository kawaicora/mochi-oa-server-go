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
		&types.CheckinRow{},
		&types.CompanyCheckinSettingsRow{},
		&types.ProxyAccountRow{},
		&types.ProxyAccessRequestRow{},
		&types.AssetRow{},
		&types.AssetFlowRow{},
	}
	if err := s.gorm.WithContext(ctx).AutoMigrate(models...); err != nil {
		logger.Error("db", "自动建表失败", "error", err.Error())
		return err
	}
	logger.Info("db", "自动建表完成", "tables", len(models))
	return nil
}
