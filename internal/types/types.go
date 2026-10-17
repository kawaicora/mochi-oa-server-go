// Package types 定义跨模块共享的数据结构。
//
// 包含三类：
//  1. Row struct（*Row 后缀）：对应 32 张数据库表，列名严格 snake_case（sequelize underscored）。
//     同时带 db 标签（历史扫描用途）与 gorm 标签（列类型/主键/索引/默认值，供 GORM 读写
//     与 AutoMigrate 建表）；DATE(3) 列用 time.Time，可空列用指针。
//  2. 负载 struct（无后缀）：对应 types.ts 的接口，序列化给客户端用，时间统一为 ISO 字符串。
//  3. Auth：socket 认证中间件写入连接的身份结构（对标 index.ts 的 socket.data.auth）。
//
// DataTypes 映射：INTEGER→int、BIGINT→int64、TINYINT(1)/BOOLEAN→bool、STRING/TEXT→string、
// DATE(3)→time.Time、ENUM/VARCHAR 中的状态字段如实保留为 string。
package types

import "time"

// ==================== Row struct（数据库表映射，列名 snake_case） ====================

// UserRow 对应 users 表。
type UserRow struct {
	ID           int64     `db:"id" gorm:"primaryKey;autoIncrement;type:bigint unsigned;not null"`
	Username     string    `db:"username" gorm:"uniqueIndex;type:varchar(64);not null"`
	PasswordHash string    `db:"password_hash" gorm:"type:varchar(255);not null"` // 敏感，绝不进日志
	Nick         string    `db:"nick" gorm:"type:varchar(64);not null"`
	Avatar       string    `db:"avatar" gorm:"type:varchar(255);not null"`
	Email        string    `db:"email" gorm:"type:varchar(128);not null"`
	Phone        string    `db:"phone" gorm:"type:varchar(32);not null"`
	Extra        *string   `db:"extra" gorm:"type:text"`                // TEXT，可空
	SessionDays  *int64    `db:"session_days" gorm:"type:int unsigned"` // 可空：nil=用服务端默认
	CreatedAt    time.Time `db:"created_at" gorm:"type:datetime;not null"`
}

// CompanyRow 对应 companies 表。
type CompanyRow struct {
	ID        int64     `db:"id" gorm:"primaryKey;autoIncrement;type:bigint unsigned;not null"`
	Name      string    `db:"name" gorm:"type:varchar(128);not null"`
	Code      string    `db:"code" gorm:"uniqueIndex;type:varchar(32);not null"`
	OwnerID   int64     `db:"owner_id" gorm:"index;type:bigint unsigned;not null"`
	CreatedAt time.Time `db:"created_at" gorm:"type:datetime;not null"`
}

// CompanyMemberRow 对应 company_members 表（联合主键 company_id+user_id）。
type CompanyMemberRow struct {
	CompanyID int64     `db:"company_id" gorm:"primaryKey;type:bigint unsigned;not null"`
	UserID    int64     `db:"user_id" gorm:"primaryKey;index;type:bigint unsigned;not null"`
	Role      string    `db:"role" gorm:"type:enum('owner','admin','member');not null;default:member"` // owner|admin|member
	JoinedAt  time.Time `db:"joined_at" gorm:"type:datetime;not null"`
}

// DepartmentRow 对应 departments 表。
type DepartmentRow struct {
	ID        int64     `db:"id" gorm:"primaryKey;autoIncrement;type:bigint unsigned;not null"`
	CompanyID int64     `db:"company_id" gorm:"index;type:bigint unsigned;not null"`
	Name      string    `db:"name" gorm:"type:varchar(64);not null"`
	ParentID  *int64    `db:"parent_id" gorm:"index;type:bigint unsigned"` // 可空：顶级部门
	CreatedAt time.Time `db:"created_at" gorm:"type:datetime;not null"`
}

// DepartmentMemberRow 对应 department_members 表。
type DepartmentMemberRow struct {
	DepartmentID int64     `db:"department_id" gorm:"primaryKey;type:bigint unsigned;not null"`
	UserID       int64     `db:"user_id" gorm:"primaryKey;index;type:bigint unsigned;not null"`
	JoinedAt     time.Time `db:"joined_at" gorm:"type:datetime;not null"`
}

// GroupRow 对应 groups 表。company_id 可空（NULL 表示无公司，对齐 sequelize allowNull）。
type GroupRow struct {
	ID           int64     `db:"id" gorm:"primaryKey;autoIncrement;type:bigint unsigned;not null"`
	CompanyID    *int64    `db:"company_id" gorm:"index;type:bigint unsigned"` // nil=无公司
	DepartmentID *int64    `db:"department_id" gorm:"index;type:bigint unsigned"`
	Name         string    `db:"name" gorm:"type:varchar(64);not null"`
	Code         string    `db:"code" gorm:"uniqueIndex;type:varchar(32);not null"`
	OwnerID      int64     `db:"owner_id" gorm:"index;type:bigint unsigned;not null"`
	CreatedAt    time.Time `db:"created_at" gorm:"type:datetime;not null"`
}

// GroupMemberRow 对应 group_members 表。
type GroupMemberRow struct {
	GroupID    int64      `db:"group_id" gorm:"primaryKey;type:bigint unsigned;not null"`
	UserID     int64      `db:"user_id" gorm:"primaryKey;index;type:bigint unsigned;not null"`
	Role       string     `db:"role" gorm:"type:enum('owner','admin','member');not null;default:member"` // owner|admin|member
	JoinedAt   time.Time  `db:"joined_at" gorm:"type:datetime;not null"`
	MutedUntil *time.Time `db:"muted_until" gorm:"type:datetime"` // 可空：禁言截止
}

// ConversationRow 对应 conversations 表。
type ConversationRow struct {
	ID        int64     `db:"id" gorm:"primaryKey;autoIncrement;type:bigint unsigned;not null"`
	Type      string    `db:"type" gorm:"type:enum('group','dm');not null"` // group|dm
	GroupID   *int64    `db:"group_id" gorm:"type:bigint unsigned"`
	DmUserA   *int64    `db:"dm_user_a" gorm:"type:bigint unsigned"`
	DmUserB   *int64    `db:"dm_user_b" gorm:"type:bigint unsigned"`
	CreatedAt time.Time `db:"created_at" gorm:"type:datetime;not null"`
}

// ConversationMemberRow 对应 conversation_members 表（timestamps:false）。
type ConversationMemberRow struct {
	ConversationID    int64      `db:"conversation_id" gorm:"primaryKey;type:bigint unsigned;not null"`
	UserID            int64      `db:"user_id" gorm:"primaryKey;type:bigint unsigned;not null"`
	Pinned            bool       `db:"pinned" gorm:"type:tinyint(1);not null;default:0"`
	LastMessageAt     *time.Time `db:"last_message_at" gorm:"type:datetime(3)"`
	LastPreview       *string    `db:"last_preview" gorm:"type:varchar(255)"`
	Unread            int64      `db:"unread" gorm:"type:int;not null;default:0"`
	LastReadMessageID *int64     `db:"last_read_message_id" gorm:"type:bigint unsigned"`
}

// MessageRow 对应 messages 表。
type MessageRow struct {
	ID             int64      `db:"id" gorm:"primaryKey;autoIncrement;type:bigint unsigned;not null"`
	ConversationID int64      `db:"conversation_id" gorm:"index;type:bigint unsigned;not null"`
	FromID         int64      `db:"from_id" gorm:"type:bigint unsigned;not null"`
	Kind           string     `db:"kind" gorm:"type:enum('text','image','file','video','audio','folder');not null;default:text"` // text|image|video|audio|file|folder
	Content        string     `db:"content" gorm:"type:text;not null"`
	DeletedAt      *time.Time `db:"deleted_at" gorm:"type:datetime(3)"` // 软删除标记
	CreatedAt      time.Time  `db:"created_at" gorm:"type:datetime;not null"`
}

// FileRow 对应 files 表（UUID 主键）。
type FileRow struct {
	UUID      string    `db:"uuid" gorm:"primaryKey;type:char(36);not null"`
	Filename  string    `db:"filename" gorm:"type:varchar(255);not null"`
	Mime      string    `db:"mime" gorm:"type:varchar(128);not null"`
	Size      int64     `db:"size" gorm:"type:bigint unsigned;not null;default:0"`
	Storage   string    `db:"storage" gorm:"type:enum('local','ftp');not null;default:local"` // local|ftp
	URL       string    `db:"url" gorm:"type:varchar(512);not null"`
	CreatedAt time.Time `db:"created_at" gorm:"type:datetime;not null"`
}

// FriendRow 对应 friends 表（user_a<user_b 约定，联合主键）。
type FriendRow struct {
	UserA     int64     `db:"user_a" gorm:"primaryKey;type:bigint unsigned;not null"`
	UserB     int64     `db:"user_b" gorm:"primaryKey;type:bigint unsigned;not null"`
	CreatedAt time.Time `db:"created_at" gorm:"type:datetime;not null"`
}

// CompanyInvitationRow 对应 company_invitations 表。
type CompanyInvitationRow struct {
	ID           int64      `db:"id" gorm:"primaryKey;autoIncrement;type:bigint unsigned;not null"`
	CompanyID    int64      `db:"company_id" gorm:"index;type:bigint unsigned;not null"`
	UserID       int64      `db:"user_id" gorm:"index;type:bigint unsigned;not null"`
	DepartmentID *int64     `db:"department_id" gorm:"index;type:bigint unsigned"`
	InvitedBy    int64      `db:"invited_by" gorm:"index;type:bigint unsigned;not null"`
	Code         string     `db:"code" gorm:"uniqueIndex;type:varchar(32);not null"`
	ExpiresAt    *time.Time `db:"expires_at" gorm:"type:datetime(3)"`
	Status       string     `db:"status" gorm:"type:enum('pending','accepted','declined','expired');not null;default:pending"` // pending|accepted|declined|expired
	CreatedAt    time.Time  `db:"created_at" gorm:"type:datetime;not null"`
}

// MemberProfileRow 对应 member_profiles 表（仅 updatedAt，无 createdAt）。
type MemberProfileRow struct {
	CompanyID    int64     `db:"company_id" gorm:"primaryKey;type:bigint unsigned;not null"`
	UserID       int64     `db:"user_id" gorm:"primaryKey;type:bigint unsigned;not null"`
	RealName     string    `db:"real_name" gorm:"type:varchar(64);not null"`
	IDCard       string    `db:"id_card" gorm:"type:varchar(64);not null"`
	BankCard     string    `db:"bank_card" gorm:"type:varchar(64);not null"`
	ResumeURL    string    `db:"resume_url" gorm:"type:varchar(512);not null"`
	PortfolioURL string    `db:"portfolio_url" gorm:"type:varchar(512);not null"`
	UpdatedAt    time.Time `db:"updated_at" gorm:"type:datetime;not null"`
}

// SessionRow 对应 sessions 表（多端登录会话）。
type SessionRow struct {
	ID           int64     `db:"id" gorm:"primaryKey;autoIncrement;type:bigint unsigned;not null"`
	UserID       int64     `db:"user_id" gorm:"type:bigint unsigned;not null"`
	TokenHash    string    `db:"token_hash" gorm:"uniqueIndex;type:char(64);not null"`
	Device       string    `db:"device" gorm:"type:varchar(128);not null"`
	IP           string    `db:"ip" gorm:"type:varchar(64);not null"`
	Location     string    `db:"location" gorm:"type:varchar(128);not null"`
	ExpiresAt    time.Time `db:"expires_at" gorm:"type:datetime(3);not null"`
	LastActiveAt time.Time `db:"last_active_at" gorm:"type:datetime(3);not null"`
	CreatedAt    time.Time `db:"created_at" gorm:"type:datetime;not null"`
}

// MailConfigRow 对应 mail_configs 表。
type MailConfigRow struct {
	ID          int64     `db:"id" gorm:"primaryKey;autoIncrement;type:bigint unsigned;not null"`
	CompanyID   *int64    `db:"company_id" gorm:"index;type:bigint unsigned"` // nil=主配置（全局）
	Email       string    `db:"email" gorm:"type:varchar(128);not null"`
	DisplayName string    `db:"display_name" gorm:"type:varchar(128);not null"`
	Host        string    `db:"host" gorm:"type:varchar(255);not null"`
	Port        int64     `db:"port" gorm:"type:int unsigned;not null;default:465"`
	Secure      bool      `db:"secure" gorm:"type:tinyint(1);not null;default:1"`
	User        string    `db:"user" gorm:"type:varchar(128);not null"`
	Password    string    `db:"password" gorm:"type:varchar(255);not null"` // 敏感
	IsDefault   bool      `db:"is_default" gorm:"type:tinyint(1);not null;default:0"`
	Enabled     bool      `db:"enabled" gorm:"type:tinyint(1);not null;default:1"`
	Priority    int64     `db:"priority" gorm:"type:int;not null;default:100"`
	CreatedAt   time.Time `db:"created_at" gorm:"type:datetime;not null"`
}

// HolidayRow 对应 holidays 表。
type HolidayRow struct {
	ID        int64     `db:"id" gorm:"primaryKey;autoIncrement;type:bigint unsigned;not null"`
	Date      string    `db:"date" gorm:"uniqueIndex;type:varchar(10);not null"` // YYYY-MM-DD 唯一
	Name      string    `db:"name" gorm:"type:varchar(64);not null"`
	Type      string    `db:"type" gorm:"type:enum('legal','workday','custom');not null;default:legal"` // legal|workday|custom
	CreatedAt time.Time `db:"created_at" gorm:"type:datetime;not null"`
}

// ProjectRow 对应 projects 表。
type ProjectRow struct {
	ID        int64     `db:"id" gorm:"primaryKey;autoIncrement;type:bigint unsigned;not null"`
	CompanyID int64     `db:"company_id" gorm:"index;type:bigint unsigned;not null"`
	Name      string    `db:"name" gorm:"type:varchar(64);not null"`
	PmID      int64     `db:"pm_id" gorm:"index;type:bigint unsigned;not null"`
	CreatedAt time.Time `db:"created_at" gorm:"type:datetime;not null"`
}

// ProjectMemberRow 对应 project_members 表。
type ProjectMemberRow struct {
	ProjectID int64     `db:"project_id" gorm:"primaryKey;type:bigint unsigned;not null"`
	UserID    int64     `db:"user_id" gorm:"primaryKey;index;type:bigint unsigned;not null"`
	Role      string    `db:"role" gorm:"type:enum('pm','leader','member');not null;default:member"` // pm|leader|member
	JoinedAt  time.Time `db:"joined_at" gorm:"type:datetime;not null"`
}

// TaskRow 对应 tasks 表。
type TaskRow struct {
	ID             int64      `db:"id" gorm:"primaryKey;autoIncrement;type:bigint unsigned;not null"`
	CompanyID      int64      `db:"company_id" gorm:"index;type:bigint unsigned;not null"`
	ProjectID      *int64     `db:"project_id" gorm:"index;type:bigint unsigned"`
	Title          string     `db:"title" gorm:"type:varchar(128);not null"`
	Description    string     `db:"description" gorm:"type:text;not null"`
	StartTime      time.Time  `db:"start_time" gorm:"type:datetime(3);not null"`
	DueTime        time.Time  `db:"due_time" gorm:"type:datetime(3);not null"`
	CompletedTime  *time.Time `db:"completed_time" gorm:"type:datetime(3)"`
	Status         string     `db:"status" gorm:"type:enum('created','in_progress','completed','pending_extension','extended','overdue');not null;default:created"`
	IsOverdue      bool       `db:"is_overdue" gorm:"type:tinyint(1);not null;default:0"`
	ReminderYellow int64      `db:"reminder_yellow" gorm:"type:int unsigned;not null;default:2"`
	ReminderRed    int64      `db:"reminder_red" gorm:"type:int unsigned;not null;default:1"`
	Images         string     `db:"images" gorm:"type:text;not null"` // JSON 字符串数组
	CreatedBy      int64      `db:"created_by" gorm:"index;type:bigint unsigned;not null"`
	CreatedAt      time.Time  `db:"created_at" gorm:"type:datetime;not null"`
	UpdatedAt      time.Time  `db:"updated_at" gorm:"type:datetime;not null"`
}

// TaskAssignmentRow 对应 task_assignments 表。
type TaskAssignmentRow struct {
	ID          int64      `db:"id" gorm:"primaryKey;autoIncrement;type:bigint unsigned;not null"`
	TaskID      int64      `db:"task_id" gorm:"index;type:bigint unsigned;not null"`
	UserID      int64      `db:"user_id" gorm:"index;type:bigint unsigned;not null"`
	Content     string     `db:"content" gorm:"type:text;not null"`
	Status      string     `db:"status" gorm:"type:enum('created','in_progress','completed');not null;default:created"` // created|in_progress|completed
	CompletedAt *time.Time `db:"completed_at" gorm:"type:datetime(3)"`
	CreatedAt   time.Time  `db:"created_at" gorm:"type:datetime;not null"`
}

// TaskCommentRow 对应 task_comments 表。
type TaskCommentRow struct {
	ID        int64     `db:"id" gorm:"primaryKey;autoIncrement;type:bigint unsigned;not null"`
	TaskID    int64     `db:"task_id" gorm:"index;type:bigint unsigned;not null"`
	UserID    int64     `db:"user_id" gorm:"index;type:bigint unsigned;not null"`
	Content   string    `db:"content" gorm:"type:text;not null"`
	Images    *string   `db:"images" gorm:"type:text"` // JSON 附件数组，可空
	CreatedAt time.Time `db:"created_at" gorm:"type:datetime;not null"`
}

// TaskIssueRow 对应 task_issues 表。
type TaskIssueRow struct {
	ID         int64      `db:"id" gorm:"primaryKey;autoIncrement;type:bigint unsigned;not null"`
	TaskID     int64      `db:"task_id" gorm:"index;type:bigint unsigned;not null"`
	UserID     int64      `db:"user_id" gorm:"index;type:bigint unsigned;not null"`
	Title      string     `db:"title" gorm:"type:varchar(128);not null"`
	Content    string     `db:"content" gorm:"type:text;not null"`
	Status     string     `db:"status" gorm:"type:enum('open','resolved');not null;default:open"` // open|resolved
	ResolvedAt *time.Time `db:"resolved_at" gorm:"type:datetime(3)"`
	CreatedAt  time.Time  `db:"created_at" gorm:"type:datetime;not null"`
}

// TaskStatusLogRow 对应 task_status_logs 表。
type TaskStatusLogRow struct {
	ID         int64     `db:"id" gorm:"primaryKey;autoIncrement;type:bigint unsigned;not null"`
	TaskID     int64     `db:"task_id" gorm:"index;type:bigint unsigned;not null"`
	UserID     int64     `db:"user_id" gorm:"index;type:bigint unsigned;not null"`
	FromStatus string    `db:"from_status" gorm:"type:varchar(32);not null"`
	ToStatus   string    `db:"to_status" gorm:"type:enum('created','in_progress','completed','pending_extension','extended','overdue');not null"`
	Note       string    `db:"note" gorm:"type:text;not null"`
	CreatedAt  time.Time `db:"created_at" gorm:"type:datetime;not null"`
}

// TaskExtensionRow 对应 task_extensions 表。
type TaskExtensionRow struct {
	ID               int64      `db:"id" gorm:"primaryKey;autoIncrement;type:bigint unsigned;not null"`
	TaskID           int64      `db:"task_id" gorm:"index;type:bigint unsigned;not null"`
	UserID           int64      `db:"user_id" gorm:"index;type:bigint unsigned;not null"`
	RequestedDueTime time.Time  `db:"requested_due_time" gorm:"type:datetime(3);not null"`
	Reason           string     `db:"reason" gorm:"type:text;not null"`
	Status           string     `db:"status" gorm:"type:enum('pending','approved','rejected');not null;default:pending"` // pending|approved|rejected
	DecidedBy        *int64     `db:"decided_by" gorm:"type:bigint unsigned"`
	DecidedAt        *time.Time `db:"decided_at" gorm:"type:datetime(3)"`
	CreatedAt        time.Time  `db:"created_at" gorm:"type:datetime;not null"`
}

// RequirementRow 对应 requirements 表。
type RequirementRow struct {
	ID            int64      `db:"id" gorm:"primaryKey;autoIncrement;type:bigint unsigned;not null"`
	CompanyID     int64      `db:"company_id" gorm:"index;type:bigint unsigned;not null"`
	ProjectID     *int64     `db:"project_id" gorm:"index;type:bigint unsigned"`
	Code          string     `db:"code" gorm:"type:varchar(32);not null"`
	Title         string     `db:"title" gorm:"type:varchar(128);not null"`
	Description   string     `db:"description" gorm:"type:text;not null"`
	Category      string     `db:"category" gorm:"type:varchar(32);not null;default:uncategorized"`
	Priority      string     `db:"priority" gorm:"type:varchar(32);not null;default:middle"`
	Status        string     `db:"status" gorm:"type:varchar(32);not null;default:planning"`
	HandlerID     *int64     `db:"handler_id" gorm:"index;type:bigint unsigned"`
	CreatorID     int64      `db:"creator_id" gorm:"index;type:bigint unsigned;not null"`
	StartTime     time.Time  `db:"start_time" gorm:"type:datetime(3);not null"`
	DueTime       time.Time  `db:"due_time" gorm:"type:datetime(3);not null"`
	CompletedTime *time.Time `db:"completed_time" gorm:"type:datetime(3)"`
	CreatedAt     time.Time  `db:"created_at" gorm:"type:datetime;not null"`
	UpdatedAt     time.Time  `db:"updated_at" gorm:"type:datetime;not null"`
}

// BugRow 对应 bugs 表。
type BugRow struct {
	ID            int64     `db:"id" gorm:"primaryKey;autoIncrement;type:bigint unsigned;not null"`
	CompanyID     int64     `db:"company_id" gorm:"index;type:bigint unsigned;not null"`
	ProjectID     *int64    `db:"project_id" gorm:"index;type:bigint unsigned"`
	RequirementID *int64    `db:"requirement_id" gorm:"index;type:bigint unsigned"`
	Code          string    `db:"code" gorm:"type:varchar(32);not null"`
	Title         string    `db:"title" gorm:"type:varchar(128);not null"`
	Description   string    `db:"description" gorm:"type:text;not null"`
	Severity      string    `db:"severity" gorm:"type:varchar(32);not null;default:normal"`
	Priority      string    `db:"priority" gorm:"type:varchar(32);not null;default:middle"`
	Status        string    `db:"status" gorm:"type:varchar(32);not null;default:pending"`
	HandlerID     *int64    `db:"handler_id" gorm:"index;type:bigint unsigned"`
	CreatorID     int64     `db:"creator_id" gorm:"index;type:bigint unsigned;not null"`
	FoundVersion  string    `db:"found_version" gorm:"type:varchar(64);not null"`
	CreatedAt     time.Time `db:"created_at" gorm:"type:datetime;not null"`
	UpdatedAt     time.Time `db:"updated_at" gorm:"type:datetime;not null"`
}

// PlanRow 对应 plans 表。
type PlanRow struct {
	ID          int64     `db:"id" gorm:"primaryKey;autoIncrement;type:bigint unsigned;not null"`
	CompanyID   int64     `db:"company_id" gorm:"index;type:bigint unsigned;not null"`
	ProjectID   *int64    `db:"project_id" gorm:"index;type:bigint unsigned"`
	Name        string    `db:"name" gorm:"type:varchar(128);not null"`
	Description string    `db:"description" gorm:"type:text;not null"`
	StartTime   time.Time `db:"start_time" gorm:"type:datetime(3);not null"`
	DueTime     time.Time `db:"due_time" gorm:"type:datetime(3);not null"`
	Status      string    `db:"status" gorm:"type:varchar(32);not null;default:not_started"`
	CreatorID   int64     `db:"creator_id" gorm:"index;type:bigint unsigned;not null"`
	CreatedAt   time.Time `db:"created_at" gorm:"type:datetime;not null"`
	UpdatedAt   time.Time `db:"updated_at" gorm:"type:datetime;not null"`
}

// DocumentRow 对应 documents 表（TAPD 项目文档）。
type DocumentRow struct {
	ID        int64     `db:"id" gorm:"primaryKey;autoIncrement;type:bigint unsigned;not null"`
	CompanyID int64     `db:"company_id" gorm:"index;type:bigint unsigned;not null"`
	ProjectID *int64    `db:"project_id" gorm:"index;type:bigint unsigned"`
	Title     string    `db:"title" gorm:"type:varchar(128);not null"`
	Content   string    `db:"content" gorm:"type:text;not null"`
	CreatorID int64     `db:"creator_id" gorm:"index;type:bigint unsigned;not null"`
	CreatedAt time.Time `db:"created_at" gorm:"type:datetime;not null"`
	UpdatedAt time.Time `db:"updated_at" gorm:"type:datetime;not null"`
}

// WikiPageRow 对应 wiki_pages 表。
type WikiPageRow struct {
	ID        int64     `db:"id" gorm:"primaryKey;autoIncrement;type:bigint unsigned;not null"`
	CompanyID int64     `db:"company_id" gorm:"index;type:bigint unsigned;not null"`
	ProjectID *int64    `db:"project_id" gorm:"index;type:bigint unsigned"`
	Title     string    `db:"title" gorm:"type:varchar(128);not null"`
	Content   string    `db:"content" gorm:"type:text;not null"`
	CreatorID int64     `db:"creator_id" gorm:"index;type:bigint unsigned;not null"`
	CreatedAt time.Time `db:"created_at" gorm:"type:datetime;not null"`
	UpdatedAt time.Time `db:"updated_at" gorm:"type:datetime;not null"`
}

// RequirementLinkRow 对应 requirement_links 表（需求-任务关联）。
type RequirementLinkRow struct {
	ID            int64     `db:"id" gorm:"primaryKey;autoIncrement;type:bigint unsigned;not null"`
	CompanyID     int64     `db:"company_id" gorm:"type:bigint unsigned;not null"`
	RequirementID int64     `db:"requirement_id" gorm:"index;type:bigint unsigned;not null"`
	TaskID        int64     `db:"task_id" gorm:"index;type:bigint unsigned;not null"`
	UserID        int64     `db:"user_id" gorm:"type:bigint unsigned;not null"`
	CreatedAt     time.Time `db:"created_at" gorm:"type:datetime;not null"`
}

// AfdianOrderRow 对应 afdian_orders 表。
// 注意：这张表不在项目自带的 mochioa.sql 里（那里只有另一张 pay_orders），
// 但代码一直在读写它；AutoMigrate 会按本结构体把缺失的表建出来。
type AfdianOrderRow struct {
	ID            int64     `db:"id" gorm:"primaryKey;autoIncrement;type:bigint unsigned;not null"`
	OutTradeNo    string    `db:"out_trade_no" gorm:"uniqueIndex;type:varchar(64);not null"`
	CustomOrderID string    `db:"custom_order_id" gorm:"type:varchar(64);not null"`
	PlanID        string    `db:"plan_id" gorm:"type:varchar(64);not null"`
	PlanTitle     string    `db:"plan_title" gorm:"type:varchar(255);not null"`
	Title         string    `db:"title" gorm:"type:varchar(255);not null"`
	Month         int64     `db:"month" gorm:"type:int;not null;default:0"`
	Amount        string    `db:"amount" gorm:"type:varchar(32);not null"`
	ShowAmount    string    `db:"show_amount" gorm:"type:varchar(32);not null"`
	Status        int64     `db:"status" gorm:"type:int;not null;default:0"`
	Remark        string    `db:"remark" gorm:"type:text;not null"`
	RedeemID      string    `db:"redeem_id" gorm:"type:varchar(64);not null"`
	ProductType   int64     `db:"product_type" gorm:"type:int;not null;default:0"`
	Discount      string    `db:"discount" gorm:"type:varchar(32);not null"`
	BuyerUserID   string    `db:"buyer_user_id" gorm:"type:varchar(64);not null"`
	BuyerName     string    `db:"buyer_name" gorm:"type:varchar(128);not null"`
	BuyerAvatar   string    `db:"buyer_avatar" gorm:"type:varchar(512);not null"`
	CreateTime    int64     `db:"create_time" gorm:"type:bigint;not null;default:0"`
	SkuDetail     string    `db:"sku_detail" gorm:"type:text;not null"`
	Raw           string    `db:"raw" gorm:"type:text;not null"`
	CreatedAt     time.Time `db:"created_at" gorm:"type:datetime;not null"`
}

// ReportTemplateRow 对应 report_templates 表（日报/周报模板，公司隔离；admin/owner 维护）。
type ReportTemplateRow struct {
	ID          int64     `db:"id" gorm:"primaryKey;autoIncrement;type:bigint unsigned;not null"`
	CompanyID   int64     `db:"company_id" gorm:"index;type:bigint unsigned;not null"`
	Name        string    `db:"name" gorm:"type:varchar(100);not null"`
	Description string    `db:"description" gorm:"type:varchar(500);not null;default:''"`
	Sections    string    `db:"sections" gorm:"type:text;not null"` // JSON 字符串数组（如 "[\"今日完成\",\"明日计划\"]"）
	CreatedBy   int64     `db:"created_by" gorm:"index;type:bigint unsigned;not null"`
	CreatedAt   time.Time `db:"created_at" gorm:"type:datetime;not null"`
	UpdatedAt   time.Time `db:"updated_at" gorm:"type:datetime;not null"`
}

// ReportRow 对应 reports 表（日报/周报，公司+成员隔离）。
// 联合唯一索引 idx_report_period(company_id,user_id,type,period_date) 保证一人一周期一条。
type ReportRow struct {
	ID         int64  `db:"id" gorm:"primaryKey;autoIncrement;type:bigint unsigned;not null"`
	CompanyID  int64  `db:"company_id" gorm:"uniqueIndex:idx_report_period;index;type:bigint unsigned;not null"`
	UserID     int64  `db:"user_id" gorm:"uniqueIndex:idx_report_period;index;type:bigint unsigned;not null"`
	Type       string `db:"type" gorm:"uniqueIndex:idx_report_period;index;type:varchar(16);not null"`        // daily|weekly
	PeriodDate string `db:"period_date" gorm:"uniqueIndex:idx_report_period;index;type:varchar(16);not null"` // YYYY-MM-DD：日报=当天，周报=本周周一
	Title      string `db:"title" gorm:"type:varchar(200);not null;default:''"`
	Content    string `db:"content" gorm:"type:text;not null"`
	TemplateID int64  `db:"template_id" gorm:"type:bigint unsigned;not null;default:0"`
	// RecipientIDs 接收人 user_id 数组，落库为 JSON 字符串（如 "[1,2]"）；"[]"=未发送给任何人。
	// 注意：MySQL 8 的 JSON 列不允许带字面 DEFAULT（Error 1101），也不能 not null 无 default
	// （老表 ALTER 补列会失败）；故只声明 type:json 可空。写入侧永远落 encodeRecipients 的结果
	// （空数组写 "[]" 而非 NULL），读取侧 decodeRecipients 对 NULL/空串/损坏兜底返回 []。
	RecipientIDs string     `db:"recipient_ids" gorm:"type:json"`
	Submitted    bool       `db:"submitted" gorm:"type:tinyint(1);not null;default:0"`
	SubmittedAt  *time.Time `db:"submitted_at" gorm:"type:datetime(3);index"` // 可空：未提交
	CreatedAt    time.Time  `db:"created_at" gorm:"type:datetime;not null"`
	UpdatedAt    time.Time  `db:"updated_at" gorm:"type:datetime;not null"`
}

// AttendanceRequestRow 对应 attendance_requests 表（考勤申请：补卡/请假/调休，公司隔离）。
// 无唯一约束：同一人可多次发起。审批流 pending → approved/rejected（admin/owner 审批）。
type AttendanceRequestRow struct {
	ID            int64      `db:"id" gorm:"primaryKey;autoIncrement;type:bigint unsigned;not null"`
	CompanyID     int64      `db:"company_id" gorm:"index;type:bigint unsigned;not null"`
	UserID        int64      `db:"user_id" gorm:"index;type:bigint unsigned;not null"`
	Type          string     `db:"type" gorm:"type:varchar(16);not null"`                   // make_up|leave|comp_time
	Status        string     `db:"status" gorm:"type:varchar(16);not null;default:pending"` // pending|approved|rejected
	PunchTime     *string    `db:"punch_time" gorm:"type:varchar(32)"`                      // 补卡时间，如 "2026-10-17 09:00"
	LeaveType     *string    `db:"leave_type" gorm:"type:varchar(32)"`                      // 请假类型
	CompType      *string    `db:"comp_type" gorm:"type:varchar(32)"`                       // 调休类型
	StartTime     *time.Time `db:"start_time" gorm:"type:datetime(3)"`                      // 可空：补卡无区间
	EndTime       *time.Time `db:"end_time" gorm:"type:datetime(3)"`
	DurationHours *float64   `db:"duration_hours" gorm:"type:float"` // 可空：时长（小时）
	Reason        string     `db:"reason" gorm:"type:text;not null"` // HTML 富文本
	ApproverID    *int64     `db:"approver_id" gorm:"type:bigint unsigned"`
	ApprovalNote  *string    `db:"approval_note" gorm:"type:text"` // 驳回理由
	ApprovedAt    *time.Time `db:"approved_at" gorm:"type:datetime(3)"`
	// ApproverIDs/CcIDs 是创建时按公司审批流配置的快照（JSON 数组字符串）。
	// JSON 列一律可空、无 default（MySQL 8 禁止 JSON 列字面 DEFAULT，Error 1101）；
	// 写入侧空数组落 "[]"，读取侧 NULL/空兜底 []。
	ApproverIDs string    `db:"approver_ids" gorm:"type:json"`
	CcIDs       string    `db:"cc_ids" gorm:"type:json"`
	CreatedAt   time.Time `db:"created_at" gorm:"type:datetime;not null"`
	UpdatedAt   time.Time `db:"updated_at" gorm:"type:datetime;not null"`
}

// ApprovalFlowRow 对应 approval_flows 表（公司级审批流配置，按申请类型配置审批人/抄送人）。
// 联合唯一 (company_id, type)：每公司每类型一条配置。
type ApprovalFlowRow struct {
	ID          int64     `db:"id" gorm:"primaryKey;autoIncrement;type:bigint unsigned;not null"`
	CompanyID   int64     `db:"company_id" gorm:"uniqueIndex:idx_approval_flow;index;type:bigint unsigned;not null"`
	Type        string    `db:"type" gorm:"uniqueIndex:idx_approval_flow;type:varchar(32);not null"` // leave|make_up|comp_time|overtime|expense
	ApproverIDs string    `db:"approver_ids" gorm:"type:json"`                                       // JSON 数组字符串，可空无 default
	CcIDs       string    `db:"cc_ids" gorm:"type:json"`
	CreatedBy   int64     `db:"created_by" gorm:"index;type:bigint unsigned;not null"`
	CreatedAt   time.Time `db:"created_at" gorm:"type:datetime;not null"`
	UpdatedAt   time.Time `db:"updated_at" gorm:"type:datetime;not null"`
}

// CheckinRow 对应 checkins 表（打卡记录，公司隔离）。无唯一约束：一天可多次打卡，
// 前端用当天 count>=2 判断「上下班正常」。
type CheckinRow struct {
	ID        int64     `db:"id" gorm:"primaryKey;autoIncrement;type:bigint unsigned;not null"`
	CompanyID int64     `db:"company_id" gorm:"index;type:bigint unsigned;not null"`
	UserID    int64     `db:"user_id" gorm:"index;type:bigint unsigned;not null"`
	CheckDate string    `db:"check_date" gorm:"type:varchar(16);not null;index"` // YYYY-MM-DD（服务端本地今天）
	CheckTime time.Time `db:"check_time" gorm:"type:datetime(3);not null"`       // 打卡时刻
	IP        *string   `db:"ip" gorm:"type:varchar(64)"`                        // 可空
	MAC       *string   `db:"mac" gorm:"type:varchar(64)"`                       // 可空
	CreatedAt time.Time `db:"created_at" gorm:"type:datetime;not null"`
}

// CompanyCheckinSettingsRow 对应 company_checkin_settings 表（公司级打卡白名单，一公司一条）。
// mac/ip_whitelist 为 JSON 字符串数组（如 ["AA:BB:CC:DD:EE:FF"]）；空数组=该类别不限制。
// JSON 列一律可空、无 default（MySQL 8 禁止 JSON 列字面 DEFAULT，Error 1101）；写入侧空数组落 "[]"。
type CompanyCheckinSettingsRow struct {
	ID           int64     `db:"id" gorm:"primaryKey;autoIncrement;type:bigint unsigned;not null"`
	CompanyID    int64     `db:"company_id" gorm:"uniqueIndex;type:bigint unsigned;not null"`
	MacWhitelist string    `db:"mac_whitelist" gorm:"type:json"`
	IpWhitelist  string    `db:"ip_whitelist" gorm:"type:json"`
	CreatedAt    time.Time `db:"created_at" gorm:"type:datetime;not null"`
	UpdatedAt    time.Time `db:"updated_at" gorm:"type:datetime;not null"`
}

// ---- 表名映射：GORM 的 AutoMigrate 依赖它，查询侧仍统一用 Table("...") 显式指定。 ----

func (UserRow) TableName() string                   { return "users" }
func (CompanyRow) TableName() string                { return "companies" }
func (CompanyMemberRow) TableName() string          { return "company_members" }
func (DepartmentRow) TableName() string             { return "departments" }
func (DepartmentMemberRow) TableName() string       { return "department_members" }
func (GroupRow) TableName() string                  { return "groups" }
func (GroupMemberRow) TableName() string            { return "group_members" }
func (ConversationRow) TableName() string           { return "conversations" }
func (ConversationMemberRow) TableName() string     { return "conversation_members" }
func (MessageRow) TableName() string                { return "messages" }
func (FileRow) TableName() string                   { return "files" }
func (FriendRow) TableName() string                 { return "friends" }
func (CompanyInvitationRow) TableName() string      { return "company_invitations" }
func (MemberProfileRow) TableName() string          { return "member_profiles" }
func (SessionRow) TableName() string                { return "sessions" }
func (MailConfigRow) TableName() string             { return "mail_configs" }
func (HolidayRow) TableName() string                { return "holidays" }
func (ProjectRow) TableName() string                { return "projects" }
func (ProjectMemberRow) TableName() string          { return "project_members" }
func (TaskRow) TableName() string                   { return "tasks" }
func (TaskAssignmentRow) TableName() string         { return "task_assignments" }
func (TaskCommentRow) TableName() string            { return "task_comments" }
func (TaskIssueRow) TableName() string              { return "task_issues" }
func (TaskStatusLogRow) TableName() string          { return "task_status_logs" }
func (TaskExtensionRow) TableName() string          { return "task_extensions" }
func (RequirementRow) TableName() string            { return "requirements" }
func (BugRow) TableName() string                    { return "bugs" }
func (PlanRow) TableName() string                   { return "plans" }
func (DocumentRow) TableName() string               { return "documents" }
func (WikiPageRow) TableName() string               { return "wiki_pages" }
func (RequirementLinkRow) TableName() string        { return "requirement_links" }
func (AfdianOrderRow) TableName() string            { return "afdian_orders" }
func (ReportTemplateRow) TableName() string         { return "report_templates" }
func (ReportRow) TableName() string                 { return "reports" }
func (AttendanceRequestRow) TableName() string      { return "attendance_requests" }
func (ApprovalFlowRow) TableName() string           { return "approval_flows" }
func (CheckinRow) TableName() string                { return "checkins" }
func (CompanyCheckinSettingsRow) TableName() string { return "company_checkin_settings" }

// ==================== 负载 struct（序列化给客户端，对应 types.ts） ====================

// User 是对外用户视图（不含密码散列）。
type User struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	Nick        string `json:"nick"`
	Avatar      string `json:"avatar"`
	Email       string `json:"email"`
	Phone       string `json:"phone"`
	Extra       string `json:"extra"`
	SessionDays *int64 `json:"sessionDays"` // null=用服务端默认
	CreatedAt   string `json:"createdAt"`
}

// UserWithPassword 是带密码散列的内部用户记录（仅服务端使用，绝不外泄）。
type UserWithPassword struct {
	User
	PasswordHash string `json:"-"`
}

// CompanyRole 公司内角色。
type CompanyRole = string

// GroupRole 群内角色。
type GroupRole = string

// Company 公司。
type Company struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Code      string `json:"code"`
	OwnerID   int64  `json:"ownerId"`
	CreatedAt string `json:"createdAt"`
}

// Member 公司成员（联表用户信息）。
type Member struct {
	CompanyID int64   `json:"companyId"`
	UserID    int64   `json:"userId"`
	Role      string  `json:"role"`
	JoinedAt  string  `json:"joinedAt"`
	Username  *string `json:"username,omitempty"`
	Nick      *string `json:"nick,omitempty"`
	Avatar    *string `json:"avatar,omitempty"`
	Phone     *string `json:"phone,omitempty"`
	Extra     *string `json:"extra,omitempty"`
	Location  *string `json:"location,omitempty"`
	Online    *bool   `json:"online,omitempty"` // presence 填充
}

// Membership 用户所持公司摘要。
type Membership struct {
	Company Company `json:"company"`
	Role    string  `json:"role"`
}

// Department 部门。
type Department struct {
	ID        int64  `json:"id"`
	CompanyID int64  `json:"companyId"`
	Name      string `json:"name"`
	ParentID  *int64 `json:"parentId"`
	CreatedAt string `json:"createdAt"`
}

// DepartmentMember 部门成员。
type DepartmentMember struct {
	DepartmentID int64   `json:"departmentId"`
	UserID       int64   `json:"userId"`
	JoinedAt     string  `json:"joinedAt"`
	Phone        *string `json:"phone,omitempty"`
	Extra        *string `json:"extra,omitempty"`
	Location     *string `json:"location,omitempty"`
	Username     *string `json:"username,omitempty"`
	Nick         *string `json:"nick,omitempty"`
	Avatar       *string `json:"avatar,omitempty"`
	Online       *bool   `json:"online,omitempty"`
}

// Group 群。
type Group struct {
	ID           int64  `json:"id"`
	CompanyID    int64  `json:"companyId"`
	DepartmentID *int64 `json:"departmentId"`
	Name         string `json:"name"`
	Code         string `json:"code"`
	OwnerID      int64  `json:"ownerId"`
	CreatedAt    string `json:"createdAt"`
}

// GroupMember 群成员。
type GroupMember struct {
	GroupID    int64   `json:"groupId"`
	UserID     int64   `json:"userId"`
	Role       string  `json:"role"`
	JoinedAt   string  `json:"joinedAt"`
	MutedUntil *string `json:"mutedUntil,omitempty"`
	Username   *string `json:"username,omitempty"`
	Nick       *string `json:"nick,omitempty"`
	Avatar     *string `json:"avatar,omitempty"`
	Online     *bool   `json:"online,omitempty"`
}

// ConversationType 对话类型。
type ConversationType = string

// ChatKind 消息种类。
type ChatKind = string

// Conversation 对话。
type Conversation struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	GroupID   *int64 `json:"groupId"`
	DmUserA   *int64 `json:"dmUserA"`
	DmUserB   *int64 `json:"dmUserB"`
	CreatedAt string `json:"createdAt"`
}

// ConversationMember 会话成员的个人状态。
type ConversationMember struct {
	ConversationID int64   `json:"conversationId"`
	UserID         int64   `json:"userId"`
	Pinned         bool    `json:"pinned"`
	LastMessageAt  *string `json:"lastMessageAt"`
	LastPreview    *string `json:"lastPreview"`
	Unread         int64   `json:"unread"`
}

// ReadReceipt 单条已读回执。
type ReadReceipt struct {
	UserID            int64  `json:"userId"`
	LastReadMessageID *int64 `json:"lastReadMessageId"`
}

// ConversationItem 对话列表条目。
type ConversationItem struct {
	ConversationID int64         `json:"conversationId"`
	Type           string        `json:"type"`
	GroupID        *int64        `json:"groupId"`
	DmUserID       *int64        `json:"dmUserId"`
	Pinned         bool          `json:"pinned"`
	LastMessageAt  *string       `json:"lastMessageAt"`
	LastPreview    *string       `json:"lastPreview"`
	Unread         int64         `json:"unread"`
	ReadReceipts   []ReadReceipt `json:"readReceipts"`
}

// ChatMessage 消息。
type ChatMessage struct {
	ID             string `json:"id"`
	ConversationID int64  `json:"conversationId"`
	Type           string `json:"type"`
	FromID         int64  `json:"fromId"`
	Nick           string `json:"nick"`
	Avatar         string `json:"avatar,omitempty"`
	Kind           string `json:"kind"`
	Content        string `json:"content"`
	Ts             int64  `json:"ts"`
}

// FileRecord 上传文件记录。
type FileRecord struct {
	UUID      string `json:"uuid"`
	Filename  string `json:"filename"`
	Mime      string `json:"mime"`
	Size      int64  `json:"size"`
	Storage   string `json:"storage"`
	URL       string `json:"url"`
	CreatedAt string `json:"createdAt"`
}

// Friend 好友。
type Friend struct {
	UserID   int64  `json:"userId"`
	Username string `json:"username"`
	Nick     string `json:"nick"`
	Avatar   string `json:"avatar"`
	AddedAt  string `json:"addedAt"`
}

// InvitationStatus 邀请状态。
type InvitationStatus = string

// CompanyInvitation 公司入司邀请。
type CompanyInvitation struct {
	ID             int64   `json:"id"`
	CompanyID      int64   `json:"companyId"`
	CompanyName    string  `json:"companyName"`
	CompanyCode    string  `json:"companyCode"`
	UserID         int64   `json:"userId"`
	Username       string  `json:"username"`
	UserNick       string  `json:"userNick"`
	DepartmentID   *int64  `json:"departmentId"`
	DepartmentName *string `json:"departmentName"`
	InvitedBy      int64   `json:"invitedBy"`
	InviterNick    string  `json:"inviterNick"`
	Code           string  `json:"code"`
	ExpiresAt      *string `json:"expiresAt"`
	Status         string  `json:"status"`
	CreatedAt      string  `json:"createdAt"`
}

// MemberProfile 成员人事信息。
type MemberProfile struct {
	CompanyID    int64  `json:"companyId"`
	UserID       int64  `json:"userId"`
	RealName     string `json:"realName"`
	IDCard       string `json:"idCard"`
	BankCard     string `json:"bankCard"`
	ResumeURL    string `json:"resumeUrl"`
	PortfolioURL string `json:"portfolioUrl"`
	UpdatedAt    string `json:"updatedAt"`
}

// Session 登录会话（多端）。
type Session struct {
	ID           int64  `json:"id"`
	UserID       int64  `json:"userId"`
	TokenHash    string `json:"tokenHash"`
	Device       string `json:"device"`
	IP           string `json:"ip"`
	Location     string `json:"location"`
	CreatedAt    string `json:"createdAt"`
	ExpiresAt    string `json:"expiresAt"`
	LastActiveAt string `json:"lastActiveAt"`
}

// AfdianOrder 爱发电订单。
type AfdianOrder struct {
	ID            int64  `json:"id"`
	OutTradeNo    string `json:"outTradeNo"`
	CustomOrderID string `json:"customOrderId"`
	PlanID        string `json:"planId"`
	PlanTitle     string `json:"planTitle"`
	Title         string `json:"title"`
	Month         int64  `json:"month"`
	Amount        string `json:"amount"`
	ShowAmount    string `json:"showAmount"`
	Status        int64  `json:"status"`
	Remark        string `json:"remark"`
	RedeemID      string `json:"redeemId"`
	ProductType   int64  `json:"productType"`
	Discount      string `json:"discount"`
	BuyerUserID   string `json:"buyerUserId"`
	BuyerName     string `json:"buyerName"`
	BuyerAvatar   string `json:"buyerAvatar"`
	CreateTime    int64  `json:"createTime"`
	SkuDetail     string `json:"skuDetail"`
	Raw           string `json:"raw"`
	CreatedAt     string `json:"createdAt"`
}

// MailConfig 邮件发送配置。
type MailConfig struct {
	ID          int64  `json:"id"`
	CompanyID   *int64 `json:"companyId"` // nil=主配置（全局）
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	Host        string `json:"host"`
	Port        int64  `json:"port"`
	Secure      bool   `json:"secure"`
	User        string `json:"user"`
	Password    string `json:"password"` // 敏感
	IsDefault   bool   `json:"isDefault"`
	Enabled     bool   `json:"enabled"`
	Priority    int64  `json:"priority"`
	CreatedAt   string `json:"createdAt"`
}

// HolidayType 假期类型。
type HolidayType = string

// Holiday 假期/工作日标记。
type Holiday struct {
	ID        int64  `json:"id"`
	Date      string `json:"date"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	CreatedAt string `json:"createdAt"`
}

// ProjectRole 项目内角色。
type ProjectRole = string

// Project 项目。
type Project struct {
	ID        int64  `json:"id"`
	CompanyID int64  `json:"companyId"`
	Name      string `json:"name"`
	PmID      int64  `json:"pmId"`
	CreatedAt string `json:"createdAt"`
}

// ProjectMember 项目成员。
type ProjectMember struct {
	ProjectID int64  `json:"projectId"`
	UserID    int64  `json:"userId"`
	Role      string `json:"role"`
	JoinedAt  string `json:"joinedAt"`
}

// TaskStatus 任务状态。
type TaskStatus = string

// AssignmentStatus 分配状态。
type AssignmentStatus = string

// IssueStatus 任务问题状态。
type IssueStatus = string

// ExtensionStatus 延期申请状态。
type ExtensionStatus = string

// TaskItem 任务。
type TaskItem struct {
	ID             int64    `json:"id"`
	CompanyID      int64    `json:"companyId"`
	ProjectID      *int64   `json:"projectId"`
	Title          string   `json:"title"`
	Description    string   `json:"description"`
	StartTime      string   `json:"startTime"`
	DueTime        string   `json:"dueTime"`
	CompletedTime  *string  `json:"completedTime"`
	Status         string   `json:"status"`
	IsOverdue      bool     `json:"isOverdue"`
	ReminderYellow int64    `json:"reminderYellow"`
	ReminderRed    int64    `json:"reminderRed"`
	Images         []string `json:"images"`
	CreatedBy      int64    `json:"createdBy"`
	CreatedAt      string   `json:"createdAt"`
	UpdatedAt      string   `json:"updatedAt"`
}

// TaskAssignment 任务分配。
type TaskAssignment struct {
	ID          int64   `json:"id"`
	TaskID      int64   `json:"taskId"`
	UserID      int64   `json:"userId"`
	Content     string  `json:"content"`
	Status      string  `json:"status"`
	CompletedAt *string `json:"completedAt"`
	Username    *string `json:"username,omitempty"`
	Nick        *string `json:"nick,omitempty"`
	Avatar      *string `json:"avatar,omitempty"`
}

// TaskAttachmentKind 任务附件种类。
type TaskAttachmentKind = string

// TaskAttachment 任务附件。
type TaskAttachment struct {
	Kind string `json:"kind"`
	URL  string `json:"url"`
	Name string `json:"name"`
}

// TaskComment 任务留言。
type TaskComment struct {
	ID          int64            `json:"id"`
	TaskID      int64            `json:"taskId"`
	UserID      int64            `json:"userId"`
	Content     string           `json:"content"`
	Attachments []TaskAttachment `json:"attachments"`
	CreatedAt   string           `json:"createdAt"`
	Username    *string          `json:"username,omitempty"`
	Nick        *string          `json:"nick,omitempty"`
	Avatar      *string          `json:"avatar,omitempty"`
}

// TaskIssue 任务问题。
type TaskIssue struct {
	ID         int64   `json:"id"`
	TaskID     int64   `json:"taskId"`
	UserID     int64   `json:"userId"`
	Title      string  `json:"title"`
	Content    string  `json:"content"`
	Status     string  `json:"status"`
	ResolvedAt *string `json:"resolvedAt"`
	CreatedAt  string  `json:"createdAt"`
	Username   *string `json:"username,omitempty"`
	Nick       *string `json:"nick,omitempty"`
	Avatar     *string `json:"avatar,omitempty"`
}

// TaskStatusLog 任务状态变更日志。
type TaskStatusLog struct {
	ID         int64   `json:"id"`
	TaskID     int64   `json:"taskId"`
	UserID     int64   `json:"userId"`
	FromStatus string  `json:"fromStatus"`
	ToStatus   string  `json:"toStatus"`
	Note       string  `json:"note"`
	CreatedAt  string  `json:"createdAt"`
	Username   *string `json:"username,omitempty"`
	Nick       *string `json:"nick,omitempty"`
}

// TaskExtension 任务延期申请。
type TaskExtension struct {
	ID               int64   `json:"id"`
	TaskID           int64   `json:"taskId"`
	UserID           int64   `json:"userId"`
	RequestedDueTime string  `json:"requestedDueTime"`
	Reason           string  `json:"reason"`
	Status           string  `json:"status"`
	DecidedBy        *int64  `json:"decidedBy"`
	DecidedAt        *string `json:"decidedAt"`
	CreatedAt        string  `json:"createdAt"`
	Username         *string `json:"username,omitempty"`
	Nick             *string `json:"nick,omitempty"`
}

// TaskDetail 任务详情（任务+分配+留言+问题+延期+状态日志）。
type TaskDetail struct {
	Task        TaskItem         `json:"task"`
	Assignments []TaskAssignment `json:"assignments"`
	Comments    []TaskComment    `json:"comments"`
	Issues      []TaskIssue      `json:"issues"`
	Extensions  []TaskExtension  `json:"extensions"`
	Logs        []TaskStatusLog  `json:"logs"`
}

// RequirementStatus 需求状态。
type RequirementStatus = string

// Requirement 需求。
type Requirement struct {
	ID            int64   `json:"id"`
	CompanyID     int64   `json:"companyId"`
	ProjectID     *int64  `json:"projectId"`
	Code          string  `json:"code"`
	Title         string  `json:"title"`
	Description   string  `json:"description"`
	Category      string  `json:"category"`
	Priority      string  `json:"priority"`
	Status        string  `json:"status"`
	HandlerID     *int64  `json:"handlerId"`
	CreatorID     int64   `json:"creatorId"`
	StartTime     string  `json:"startTime"`
	DueTime       string  `json:"dueTime"`
	CompletedTime *string `json:"completedTime"`
	LinkedTaskIDs []int64 `json:"linkedTaskIds"`
	CreatedAt     string  `json:"createdAt"`
	UpdatedAt     string  `json:"updatedAt"`
	HandlerName   *string `json:"handlerName,omitempty"`
	CreatorName   *string `json:"creatorName,omitempty"`
}

// BugStatus 缺陷状态。
type BugStatus = string

// Bug 缺陷。
type Bug struct {
	ID            int64   `json:"id"`
	CompanyID     int64   `json:"companyId"`
	ProjectID     *int64  `json:"projectId"`
	RequirementID *int64  `json:"requirementId"`
	Code          string  `json:"code"`
	Title         string  `json:"title"`
	Description   string  `json:"description"`
	Severity      string  `json:"severity"`
	Priority      string  `json:"priority"`
	Status        string  `json:"status"`
	HandlerID     *int64  `json:"handlerId"`
	CreatorID     int64   `json:"creatorId"`
	FoundVersion  string  `json:"foundVersion"`
	CreatedAt     string  `json:"createdAt"`
	UpdatedAt     string  `json:"updatedAt"`
	HandlerName   *string `json:"handlerName,omitempty"`
	CreatorName   *string `json:"creatorName,omitempty"`
}

// PlanStatus 计划状态。
type PlanStatus = string

// Plan 计划。
type Plan struct {
	ID          int64  `json:"id"`
	CompanyID   int64  `json:"companyId"`
	ProjectID   *int64 `json:"projectId"`
	Name        string `json:"name"`
	Description string `json:"description"`
	StartTime   string `json:"startTime"`
	DueTime     string `json:"dueTime"`
	Status      string `json:"status"`
	CreatorID   int64  `json:"creatorId"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
}

// ProjectDocument TAPD 文档。
type ProjectDocument struct {
	ID        int64  `json:"id"`
	CompanyID int64  `json:"companyId"`
	ProjectID *int64 `json:"projectId"`
	Title     string `json:"title"`
	Content   string `json:"content"`
	CreatorID int64  `json:"creatorId"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

// WikiPage TAPD Wiki 页面。
type WikiPage struct {
	ID        int64  `json:"id"`
	CompanyID int64  `json:"companyId"`
	ProjectID *int64 `json:"projectId"`
	Title     string `json:"title"`
	Content   string `json:"content"`
	CreatorID int64  `json:"creatorId"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

// DashboardStats 项目仪表盘聚合统计。
type DashboardStats struct {
	NewRequirement30d   int64            `json:"newRequirement30d"`
	OverdueRequirement  int64            `json:"overdueRequirement"`
	BugResolveRate      int64            `json:"bugResolveRate"`
	UnresolvedBug       int64            `json:"unresolvedBug"`
	RequirementByStatus map[string]int64 `json:"requirementByStatus"`
	BugBySeverity       map[string]int64 `json:"bugBySeverity"`
}

// MemberTrackItem 成员任务跟踪。
type MemberTrackItem struct {
	UserID     int64  `json:"userId"`
	Nick       string `json:"nick"`
	Username   string `json:"username"`
	Total      int64  `json:"total"`
	Completed  int64  `json:"completed"`
	InProgress int64  `json:"inProgress"`
	Overdue    int64  `json:"overdue"`
}

// Auth 是 socket 认证中间件写入连接的身份（对标 index.ts: socket.data.auth）。
// 认证放行后由中间件写入，handler 从 conn.Get("auth") 取出。
type Auth struct {
	ID            int64  `json:"id"`
	Username      string `json:"username"`
	Nick          string `json:"nick"`
	SessionID     int64  `json:"sessionId"`
	SessionDevice string `json:"sessionDevice"`
}

// ReportItem 日报/周报条目（查询时 LEFT JOIN users 补 nick/username）。
type ReportItem struct {
	ID           int64   `json:"id"`
	CompanyID    int64   `json:"companyId"`
	UserID       int64   `json:"userId"`
	Type         string  `json:"type"`       // daily|weekly
	PeriodDate   string  `json:"periodDate"` // YYYY-MM-DD
	Title        string  `json:"title"`
	Content      string  `json:"content"`
	TemplateID   int64   `json:"templateId"`
	RecipientIds []int64 `json:"recipientIds"` // 接收人 user_id 数组（空=未发送给任何人）
	Submitted    bool    `json:"submitted"`
	SubmittedAt  *string `json:"submittedAt"` // null=未提交
	CreatedAt    string  `json:"createdAt"`
	UpdatedAt    string  `json:"updatedAt"`
	Nick         string  `json:"nick"`
	Username     string  `json:"username"`
}

// ReportTemplate 报告模板（sections 为分节标题数组）。
type ReportTemplate struct {
	ID          int64    `json:"id"`
	CompanyID   int64    `json:"companyId"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Sections    []string `json:"sections"`
	CreatedBy   int64    `json:"createdBy"`
	CreatedAt   string   `json:"createdAt"`
	UpdatedAt   string   `json:"updatedAt"`
}

// ReportMemberStat 报告统计中的单成员行（admin/owner 看全员提交情况用）。
type ReportMemberStat struct {
	UserID          int64   `json:"userId"`
	Nick            string  `json:"nick"`
	Username        string  `json:"username"`
	SubmittedDays   int64   `json:"submittedDays"`
	MissingDays     int64   `json:"missingDays"`
	LastSubmittedAt *string `json:"lastSubmittedAt"` // null=从未提交
}

// AttendanceRequest 考勤申请（查询时 LEFT JOIN users 补 nick/username）。
type AttendanceRequest struct {
	ID            int64    `json:"id"`
	CompanyID     int64    `json:"companyId"`
	UserID        int64    `json:"userId"`
	Type          string   `json:"type"`   // make_up|leave|comp_time
	Status        string   `json:"status"` // pending|approved|rejected
	PunchTime     *string  `json:"punchTime"`
	LeaveType     *string  `json:"leaveType"`
	CompType      *string  `json:"compType"`
	StartTime     *string  `json:"startTime"`
	EndTime       *string  `json:"endTime"`
	DurationHours *float64 `json:"durationHours"`
	Reason        string   `json:"reason"`
	ApproverID    *int64   `json:"approverId"`
	ApprovalNote  *string  `json:"approvalNote"`
	ApprovedAt    *string  `json:"approvedAt"`
	ApproverIDs   []int64  `json:"approverIds"` // 创建时按审批流配置的快照
	CcIDs         []int64  `json:"ccIds"`
	CreatedAt     string   `json:"createdAt"`
	UpdatedAt     string   `json:"updatedAt"`
	Nick          string   `json:"nick"`
	Username      string   `json:"username"`
}

// ApprovalFlow 公司级审批流配置（按申请类型）。
type ApprovalFlow struct {
	ID          int64   `json:"id"`
	CompanyID   int64   `json:"companyId"`
	Type        string  `json:"type"`
	ApproverIDs []int64 `json:"approverIds"`
	CcIDs       []int64 `json:"ccIds"`
	CreatedBy   int64   `json:"createdBy"`
	CreatedAt   string  `json:"createdAt"`
	UpdatedAt   string  `json:"updatedAt"`
}

// CheckinItem 单条打卡记录（联表补发起人 nick/username）。
type CheckinItem struct {
	ID        int64   `json:"id"`
	CompanyID int64   `json:"companyId"`
	UserID    int64   `json:"userId"`
	CheckDate string  `json:"checkDate"`
	CheckTime string  `json:"checkTime"`
	IP        *string `json:"ip"`
	MAC       *string `json:"mac"`
	CreatedAt string  `json:"createdAt"`
	Nick      string  `json:"nick"`
	Username  string  `json:"username"`
}

// CheckinDay 某用户某一天的打卡聚合：date 倒序、组内按 check_time 升序。
type CheckinDay struct {
	Date      string        `json:"date"`
	Count     int64         `json:"count"`
	FirstTime string        `json:"firstTime"`
	LastTime  string        `json:"lastTime"`
	Records   []CheckinItem `json:"records"`
}

// CheckinSettings 公司打卡白名单配置。
type CheckinSettings struct {
	ID           int64    `json:"id"`
	CompanyID    int64    `json:"companyId"`
	MacWhitelist []string `json:"macWhitelist"`
	IpWhitelist  []string `json:"ipWhitelist"`
	CreatedAt    string   `json:"createdAt"`
	UpdatedAt    string   `json:"updatedAt"`
}
