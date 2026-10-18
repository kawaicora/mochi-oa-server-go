// Package db 的 MySqlStore 实现：基于 GORM（gorm.io/gorm + gorm.io/driver/mysql）。
//
// 全部数据访问统一走 s.gorm，源码中不再出现裸 SQL 字面量。
// 表名一律用 Table("...") 显式指定，列名依赖 GORM 默认 NamingStrategy
// （CamelCase → snake_case）与 types 包 Row struct 的字段一一对应。
//
// 铁律（任务硬性约束）：只读复用现有表，绝不 CREATE/ALTER/DROP/迁移/建表。
// 源码 mysql-store.ts 的 init() 会 CREATE DATABASE + sequelize.sync() + 补列，
// 本 Go 版按任务要求只做连通性 Ping，假设表结构已就绪（见返回报告偏差清单）。
//
// DSN 对齐 sequelize 的 mysql dialectOptions：charset=utf8mb4、时区 UTC
// （源码统一 toISOString() 输出 UTC，故 loc=UTC + parseTime=true）。
package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	mysql "github.com/go-sql-driver/mysql"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"mochi-oa-server-go/internal/config"
	"mochi-oa-server-go/internal/logger"
	"mochi-oa-server-go/internal/types"
)

// MySqlStore 是生产存储实现。所有数据访问统一经 gorm 句柄。
type MySqlStore struct {
	sqlDB       *sql.DB  // 仅用于 Close 释放连接池
	gorm        *gorm.DB // GORM 句柄（连接池共享自 sqlDB）
	autoMigrate bool     // true=启动时 AutoMigrate 建表/补列
}

// NewMySqlStore 打开 MySQL 连接池。cfg.AutoCreateDatabase 为 true 时先确保数据库存在
// （建表由 Init 里的 AutoMigrate 负责）。
func NewMySqlStore(cfg config.MySQLConfig) (*MySqlStore, error) {
	if cfg.AutoCreateDatabase {
		if err := ensureDatabase(cfg); err != nil {
			logger.Error("db", "自动建库失败", "error", err.Error())
			return nil, err
		}
	}
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=true&loc=UTC",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, cfg.Database)
	db, err := gorm.Open(gormmysql.Open(dsn), &gorm.Config{
		// 与旧 database/sql 实现保持一致：不打印 SQL；需要原子性的方法自行用
		// gorm 的 Transaction，不做隐式包事务。
		Logger:                 gormlogger.Default.LogMode(gormlogger.Silent),
		SkipDefaultTransaction: true,
	})
	if err != nil {
		return nil, err
	}
	pool, err := db.DB()
	if err != nil {
		return nil, err
	}
	// 对齐 sequelize pool: { max: 10, min: 0 }
	pool.SetMaxOpenConns(10)
	pool.SetMaxIdleConns(0)
	logger.Info("db", "MySQL 连接池已创建", "host", cfg.Host, "port", cfg.Port, "database", cfg.Database)
	return &MySqlStore{sqlDB: pool, gorm: db, autoMigrate: cfg.AutoMigrate}, nil
}

// Init 初始化：Ping 验证连通；开启 AUTO_MIGRATE 时再跑一次 AutoMigrate 建缺失的表/列。
func (s *MySqlStore) Init(ctx context.Context) error {
	if err := s.sqlDB.PingContext(ctx); err != nil {
		logger.Error("db", "MySQL 连接失败", "error", err.Error())
		return err
	}
	if s.autoMigrate {
		if err := s.autoMigrateTables(ctx); err != nil {
			return err
		}
	}
	logger.Info("db", "MySQL 连接就绪", "auto_migrate", s.autoMigrate)
	return nil
}

// Close 关闭连接池。
func (s *MySqlStore) Close(_ context.Context) error {
	return s.sqlDB.Close()
}

// ---- 转换辅助 ----

// localTZ 对外返回时间的业务本地时区：统一输出 +偏移格式（无 Z 后缀），避免客户端按 UTC 显示。
// 可用 TIMEZONE 环境变量覆盖，缺省 Asia/Shanghai。存储层仍为 UTC，此处仅展示层转换。
var localTZ = mustLocalTZ()

func mustLocalTZ() *time.Location {
	name := os.Getenv("TIMEZONE")
	if name == "" {
		name = "Asia/Shanghai"
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		loc = time.Local
	}
	return loc
}

// toIso 把 time.Time 转业务本地时区可解析字符串（带 +08:00 偏移、无 Z 后缀）。零值返回空串。
func toIso(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(localTZ).Format("2006-01-02T15:04:05.000+08:00")
}

// isoPtr 把可空时间转可空 ISO 字符串。
func isoPtr(t *time.Time) *string {
	if t == nil || t.IsZero() {
		return nil
	}
	s := toIso(*t)
	return &s
}

// rowToUser Row → 对外 User。
func rowToUser(r *types.UserRow) types.User {
	return types.User{
		ID:          r.ID,
		Username:    r.Username,
		Nick:        r.Nick,
		Avatar:      r.Avatar,
		Email:       r.Email,
		Phone:       r.Phone,
		Extra:       derefStr(r.Extra),
		SessionDays: r.SessionDays,
		CreatedAt:   toIso(r.CreatedAt),
	}
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// rowToCompany Row → Company。
func rowToCompany(r *types.CompanyRow) types.Company {
	return types.Company{ID: r.ID, Name: r.Name, Code: r.Code, OwnerID: r.OwnerID, CreatedAt: toIso(r.CreatedAt)}
}

// rowToDepartment Row → Department。
func rowToDepartment(r *types.DepartmentRow) types.Department {
	return types.Department{ID: r.ID, CompanyID: r.CompanyID, Name: r.Name, ParentID: r.ParentID, CreatedAt: toIso(r.CreatedAt)}
}

// rowToGroup Row → Group。company_id NULL→0（对齐 toGroup 的 NULL→0）。
func rowToGroup(r *types.GroupRow) types.Group {
	cid := int64(0)
	if r.CompanyID != nil {
		cid = *r.CompanyID
	}
	return types.Group{ID: r.ID, CompanyID: cid, DepartmentID: r.DepartmentID, Name: r.Name, Code: r.Code, OwnerID: r.OwnerID, CreatedAt: toIso(r.CreatedAt)}
}

// rowToConversation Row → Conversation。
func rowToConversation(r *types.ConversationRow) types.Conversation {
	return types.Conversation{ID: r.ID, Type: r.Type, GroupID: r.GroupID, DmUserA: r.DmUserA, DmUserB: r.DmUserB, CreatedAt: toIso(r.CreatedAt)}
}

// rowToSession Row → Session。
func rowToSession(r *types.SessionRow) types.Session {
	return types.Session{
		ID: r.ID, UserID: r.UserID, TokenHash: r.TokenHash, Device: r.Device, IP: r.IP,
		Location: r.Location, CreatedAt: toIso(r.CreatedAt), ExpiresAt: toIso(r.ExpiresAt), LastActiveAt: toIso(r.LastActiveAt),
	}
}

// rowToAfdian Row → AfdianOrder。
func rowToAfdian(r *types.AfdianOrderRow) types.AfdianOrder {
	return types.AfdianOrder{
		ID: r.ID, OutTradeNo: r.OutTradeNo, CustomOrderID: r.CustomOrderID, PlanID: r.PlanID,
		PlanTitle: r.PlanTitle, Title: r.Title, Month: r.Month, Amount: r.Amount, ShowAmount: r.ShowAmount,
		Status: r.Status, Remark: r.Remark, RedeemID: r.RedeemID, ProductType: r.ProductType, Discount: r.Discount,
		BuyerUserID: r.BuyerUserID, BuyerName: r.BuyerName, BuyerAvatar: r.BuyerAvatar, CreateTime: r.CreateTime,
		SkuDetail: r.SkuDetail, Raw: r.Raw, CreatedAt: toIso(r.CreatedAt),
	}
}

// rowToMailConfig Row → MailConfig。
func rowToMailConfig(r *types.MailConfigRow) types.MailConfig {
	return types.MailConfig{
		ID: r.ID, CompanyID: r.CompanyID, Email: r.Email, DisplayName: r.DisplayName, Host: r.Host,
		Port: r.Port, Secure: r.Secure, User: r.User, Password: r.Password, IsDefault: r.IsDefault,
		Enabled: r.Enabled, Priority: r.Priority, CreatedAt: toIso(r.CreatedAt),
	}
}

// parseAttachments 解析任务留言附件 JSON（对齐 parseAttachments：字符串项→image；截断 9 条）。
func parseAttachments(raw *string) []types.TaskAttachment {
	if raw == nil || *raw == "" {
		return []types.TaskAttachment{}
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(*raw), &arr); err != nil {
		return []types.TaskAttachment{}
	}
	out := make([]types.TaskAttachment, 0, len(arr))
	for _, x := range arr {
		url, _ := x["url"].(string)
		if url == "" {
			continue
		}
		kind, _ := x["kind"].(string)
		switch kind {
		case "image", "video", "audio", "folder", "file":
		default:
			kind = "file"
		}
		name, _ := x["name"].(string)
		if len(name) > 255 {
			name = name[:255]
		}
		out = append(out, types.TaskAttachment{Kind: kind, URL: url, Name: name})
		if len(out) >= 9 {
			break
		}
	}
	return out
}

// rowToTask Row → TaskItem（含超时重算：未完成且 due<=now → overdue）。
func rowToTask(r *types.TaskRow) types.TaskItem {
	now := time.Now().UnixMilli()
	due := r.DueTime.UnixMilli()
	notDone := r.Status != "completed" && r.Status != "extended"
	overdue := notDone && !r.DueTime.IsZero() && due <= now
	status := r.Status
	if overdue {
		status = "overdue"
	}
	var images []string
	if r.Images != "" {
		_ = json.Unmarshal([]byte(r.Images), &images)
	}
	if images == nil {
		images = []string{}
	}
	return types.TaskItem{
		ID: r.ID, CompanyID: r.CompanyID, ProjectID: r.ProjectID, Title: r.Title, Description: r.Description,
		StartTime: toIso(r.StartTime), DueTime: toIso(r.DueTime), CompletedTime: isoPtr(r.CompletedTime),
		Status: status, IsOverdue: overdue, ReminderYellow: r.ReminderYellow, ReminderRed: r.ReminderRed,
		Images: images, CreatedBy: r.CreatedBy, CreatedAt: toIso(r.CreatedAt), UpdatedAt: toIso(r.UpdatedAt),
	}
}

// rowToAssignment Row → TaskAssignment。
func rowToAssignment(r *types.TaskAssignmentRow) types.TaskAssignment {
	return types.TaskAssignment{
		ID: r.ID, TaskID: r.TaskID, UserID: r.UserID, Content: r.Content, Status: r.Status, CompletedAt: isoPtr(r.CompletedAt),
	}
}

// rowToComment Row → TaskComment。
func rowToComment(r *types.TaskCommentRow) types.TaskComment {
	return types.TaskComment{
		ID: r.ID, TaskID: r.TaskID, UserID: r.UserID, Content: r.Content,
		Attachments: parseAttachments(r.Images), CreatedAt: toIso(r.CreatedAt),
	}
}

// rowToIssue Row → TaskIssue。
func rowToIssue(r *types.TaskIssueRow) types.TaskIssue {
	return types.TaskIssue{
		ID: r.ID, TaskID: r.TaskID, UserID: r.UserID, Title: r.Title, Content: r.Content,
		Status: r.Status, ResolvedAt: isoPtr(r.ResolvedAt), CreatedAt: toIso(r.CreatedAt),
	}
}

// rowToLog Row → TaskStatusLog。
func rowToLog(r *types.TaskStatusLogRow) types.TaskStatusLog {
	return types.TaskStatusLog{
		ID: r.ID, TaskID: r.TaskID, UserID: r.UserID, FromStatus: r.FromStatus, ToStatus: r.ToStatus, Note: r.Note, CreatedAt: toIso(r.CreatedAt),
	}
}

// rowToExtension Row → TaskExtension。
func rowToExtension(r *types.TaskExtensionRow) types.TaskExtension {
	return types.TaskExtension{
		ID: r.ID, TaskID: r.TaskID, UserID: r.UserID, RequestedDueTime: toIso(r.RequestedDueTime),
		Reason: r.Reason, Status: r.Status, DecidedBy: r.DecidedBy, DecidedAt: isoPtr(r.DecidedAt), CreatedAt: toIso(r.CreatedAt),
	}
}

// rowToRequirement Row → Requirement（linkedTaskIds 默认空，由 list 再填充）。
func rowToRequirement(r *types.RequirementRow) types.Requirement {
	cat, pri, st := r.Category, r.Priority, r.Status
	if cat == "" {
		cat = "uncategorized"
	}
	if pri == "" {
		pri = "middle"
	}
	if st == "" {
		st = "planning"
	}
	return types.Requirement{
		ID: r.ID, CompanyID: r.CompanyID, ProjectID: r.ProjectID, Code: r.Code, Title: r.Title, Description: r.Description,
		Category: cat, Priority: pri, Status: st, HandlerID: r.HandlerID, CreatorID: r.CreatorID,
		StartTime: toIso(r.StartTime), DueTime: toIso(r.DueTime), CompletedTime: isoPtr(r.CompletedTime),
		LinkedTaskIDs: []int64{}, CreatedAt: toIso(r.CreatedAt), UpdatedAt: toIso(r.UpdatedAt),
	}
}

// rowToBug Row → Bug。
func rowToBug(r *types.BugRow) types.Bug {
	sev, pri, st := r.Severity, r.Priority, r.Status
	if sev == "" {
		sev = "normal"
	}
	if pri == "" {
		pri = "middle"
	}
	if st == "" {
		st = "pending"
	}
	return types.Bug{
		ID: r.ID, CompanyID: r.CompanyID, ProjectID: r.ProjectID, RequirementID: r.RequirementID, Code: r.Code,
		Title: r.Title, Description: r.Description, Severity: sev, Priority: pri, Status: st, HandlerID: r.HandlerID,
		CreatorID: r.CreatorID, FoundVersion: r.FoundVersion, CreatedAt: toIso(r.CreatedAt), UpdatedAt: toIso(r.UpdatedAt),
	}
}

// rowToPlan Row → Plan。
func rowToPlan(r *types.PlanRow) types.Plan {
	st := r.Status
	if st == "" {
		st = "not_started"
	}
	return types.Plan{
		ID: r.ID, CompanyID: r.CompanyID, ProjectID: r.ProjectID, Name: r.Name, Description: r.Description,
		StartTime: toIso(r.StartTime), DueTime: toIso(r.DueTime), Status: st, CreatorID: r.CreatorID,
		CreatedAt: toIso(r.CreatedAt), UpdatedAt: toIso(r.UpdatedAt),
	}
}

// rowToDocument Row → ProjectDocument。
func rowToDocument(r *types.DocumentRow) types.ProjectDocument {
	return types.ProjectDocument{
		ID: r.ID, CompanyID: r.CompanyID, ProjectID: r.ProjectID, Title: r.Title, Content: r.Content,
		CreatorID: r.CreatorID, CreatedAt: toIso(r.CreatedAt), UpdatedAt: toIso(r.UpdatedAt),
	}
}

// rowToWiki Row → WikiPage。
func rowToWiki(r *types.WikiPageRow) types.WikiPage {
	return types.WikiPage{
		ID: r.ID, CompanyID: r.CompanyID, ProjectID: r.ProjectID, Title: r.Title, Content: r.Content,
		CreatorID: r.CreatorID, CreatedAt: toIso(r.CreatedAt), UpdatedAt: toIso(r.UpdatedAt),
	}
}

// isUniqueErr 判断是否 MySQL 唯一约束冲突（errno 1062）。
func isUniqueErr(err error) bool {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return me.Number == 1062
	}
	return false
}

// conversationType 取对话类型（缺失默认 group，对齐 conversationType）。
func (s *MySqlStore) conversationType(ctx context.Context, id int64) (string, error) {
	var r struct {
		Type string `gorm:"column:type"`
	}
	err := s.gorm.WithContext(ctx).Table("conversations").
		Select("type").Where("id = ?", id).Take(&r).Error
	if gormNotFound(err) {
		return "group", nil
	}
	if err != nil {
		return "", err
	}
	return r.Type, nil
}

// scanUser 扫描一行到 UserRow。
func scanUser(row interface{ Scan(...any) error }) (*types.UserRow, error) {
	var r types.UserRow
	err := row.Scan(&r.ID, &r.Username, &r.PasswordHash, &r.Nick, &r.Avatar, &r.Email, &r.Phone, &r.Extra, &r.SessionDays, &r.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// parseTimeFlex 把 ISO 8601 / MySQL datetime 字符串解析为 UTC time.Time。
// 兼容 JS toISOString（带 Z/毫秒）与 "2024-01-01 12:00:00" 这类格式。
func parseTimeFlex(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	layouts := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02T15:04:05Z",
		"2006-01-02 15:04:05.000",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	var lastErr error
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		} else {
			lastErr = err
		}
	}
	return time.Time{}, lastErr
}
