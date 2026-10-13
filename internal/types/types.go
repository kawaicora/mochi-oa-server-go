// Package types 定义跨模块共享的数据结构。
//
// 包含三类：
//  1. Row struct（*Row 后缀）：对应 32 张数据库表，列名严格 snake_case（sequelize underscored），
//     带 db 标签供 database/sql 扫描；DATE(3) 列用 time.Time，可空列用指针。
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
	ID           int64      `db:"id"`
	Username     string     `db:"username"`
	PasswordHash string     `db:"password_hash"` // 敏感，绝不进日志
	Nick         string     `db:"nick"`
	Avatar       string     `db:"avatar"`
	Email        string     `db:"email"`
	Phone        string     `db:"phone"`
	Extra        *string    `db:"extra"`        // TEXT，可空
	SessionDays  *int64     `db:"session_days"` // 可空：nil=用服务端默认
	CreatedAt    time.Time  `db:"created_at"`
}

// CompanyRow 对应 companies 表。
type CompanyRow struct {
	ID        int64     `db:"id"`
	Name      string    `db:"name"`
	Code      string    `db:"code"`
	OwnerID   int64     `db:"owner_id"`
	CreatedAt time.Time `db:"created_at"`
}

// CompanyMemberRow 对应 company_members 表（联合主键 company_id+user_id）。
type CompanyMemberRow struct {
	CompanyID int64     `db:"company_id"`
	UserID    int64     `db:"user_id"`
	Role      string    `db:"role"` // owner|admin|member
	JoinedAt  time.Time `db:"joined_at"`
}

// DepartmentRow 对应 departments 表。
type DepartmentRow struct {
	ID        int64     `db:"id"`
	CompanyID int64     `db:"company_id"`
	Name      string    `db:"name"`
	ParentID  *int64    `db:"parent_id"` // 可空：顶级部门
	CreatedAt time.Time `db:"created_at"`
}

// DepartmentMemberRow 对应 department_members 表。
type DepartmentMemberRow struct {
	DepartmentID int64     `db:"department_id"`
	UserID       int64     `db:"user_id"`
	JoinedAt     time.Time `db:"joined_at"`
}

// GroupRow 对应 groups 表。company_id 可空（NULL 表示无公司，对齐 sequelize allowNull）。
type GroupRow struct {
	ID           int64     `db:"id"`
	CompanyID    *int64    `db:"company_id"` // nil=无公司
	DepartmentID *int64    `db:"department_id"`
	Name         string    `db:"name"`
	Code         string    `db:"code"`
	OwnerID      int64     `db:"owner_id"`
	CreatedAt    time.Time `db:"created_at"`
}

// GroupMemberRow 对应 group_members 表。
type GroupMemberRow struct {
	GroupID   int64      `db:"group_id"`
	UserID    int64      `db:"user_id"`
	Role      string     `db:"role"` // owner|admin|member
	JoinedAt  time.Time  `db:"joined_at"`
	MutedUntil *time.Time `db:"muted_until"` // 可空：禁言截止
}

// ConversationRow 对应 conversations 表。
type ConversationRow struct {
	ID        int64     `db:"id"`
	Type      string    `db:"type"` // group|dm
	GroupID   *int64   `db:"group_id"`
	DmUserA   *int64   `db:"dm_user_a"`
	DmUserB   *int64   `db:"dm_user_b"`
	CreatedAt time.Time `db:"created_at"`
}

// ConversationMemberRow 对应 conversation_members 表（timestamps:false）。
type ConversationMemberRow struct {
	ConversationID     int64      `db:"conversation_id"`
	UserID             int64      `db:"user_id"`
	Pinned             bool       `db:"pinned"`
	LastMessageAt      *time.Time `db:"last_message_at"`
	LastPreview        *string    `db:"last_preview"`
	Unread             int64      `db:"unread"`
	LastReadMessageID  *int64     `db:"last_read_message_id"`
}

// MessageRow 对应 messages 表。
type MessageRow struct {
	ID             int64      `db:"id"`
	ConversationID int64      `db:"conversation_id"`
	FromID         int64      `db:"from_id"`
	Kind           string     `db:"kind"` // text|image|video|audio|file|folder
	Content        string     `db:"content"`
	DeletedAt      *time.Time `db:"deleted_at"` // 软删除标记
	CreatedAt      time.Time  `db:"created_at"`
}

// FileRow 对应 files 表（UUID 主键）。
type FileRow struct {
	UUID      string    `db:"uuid"`
	Filename  string    `db:"filename"`
	Mime      string    `db:"mime"`
	Size      int64     `db:"size"`
	Storage   string    `db:"storage"` // local|ftp
	URL       string    `db:"url"`
	CreatedAt time.Time `db:"created_at"`
}

// FriendRow 对应 friends 表（user_a<user_b 约定，联合主键）。
type FriendRow struct {
	UserA    int64     `db:"user_a"`
	UserB    int64     `db:"user_b"`
	CreatedAt time.Time `db:"created_at"`
}

// CompanyInvitationRow 对应 company_invitations 表。
type CompanyInvitationRow struct {
	ID          int64      `db:"id"`
	CompanyID   int64      `db:"company_id"`
	UserID      int64      `db:"user_id"`
	DepartmentID *int64    `db:"department_id"`
	InvitedBy   int64      `db:"invited_by"`
	Code        string     `db:"code"`
	ExpiresAt   *time.Time `db:"expires_at"`
	Status      string     `db:"status"` // pending|accepted|declined|expired
	CreatedAt   time.Time  `db:"created_at"`
}

// MemberProfileRow 对应 member_profiles 表（仅 updatedAt，无 createdAt）。
type MemberProfileRow struct {
	CompanyID    int64     `db:"company_id"`
	UserID       int64     `db:"user_id"`
	RealName     string    `db:"real_name"`
	IDCard       string    `db:"id_card"`
	BankCard     string    `db:"bank_card"`
	ResumeURL    string    `db:"resume_url"`
	PortfolioURL string    `db:"portfolio_url"`
	UpdatedAt    time.Time `db:"updated_at"`
}

// SessionRow 对应 sessions 表（多端登录会话）。
type SessionRow struct {
	ID           int64     `db:"id"`
	UserID       int64     `db:"user_id"`
	TokenHash    string    `db:"token_hash"`
	Device       string    `db:"device"`
	IP           string    `db:"ip"`
	Location     string    `db:"location"`
	ExpiresAt    time.Time `db:"expires_at"`
	LastActiveAt time.Time `db:"last_active_at"`
	CreatedAt    time.Time `db:"created_at"`
}

// MailConfigRow 对应 mail_configs 表。
type MailConfigRow struct {
	ID          int64     `db:"id"`
	CompanyID   *int64    `db:"company_id"` // nil=主配置（全局）
	Email       string    `db:"email"`
	DisplayName string    `db:"display_name"`
	Host        string    `db:"host"`
	Port        int64     `db:"port"`
	Secure      bool      `db:"secure"`
	User        string    `db:"user"`
	Password    string    `db:"password"` // 敏感
	IsDefault   bool      `db:"is_default"`
	Enabled     bool      `db:"enabled"`
	Priority    int64     `db:"priority"`
	CreatedAt   time.Time `db:"created_at"`
}

// HolidayRow 对应 holidays 表。
type HolidayRow struct {
	ID        int64     `db:"id"`
	Date      string    `db:"date"` // YYYY-MM-DD 唯一
	Name      string    `db:"name"`
	Type      string    `db:"type"` // legal|workday|custom
	CreatedAt time.Time `db:"created_at"`
}

// ProjectRow 对应 projects 表。
type ProjectRow struct {
	ID        int64     `db:"id"`
	CompanyID int64     `db:"company_id"`
	Name      string    `db:"name"`
	PmID      int64     `db:"pm_id"`
	CreatedAt time.Time `db:"created_at"`
}

// ProjectMemberRow 对应 project_members 表。
type ProjectMemberRow struct {
	ProjectID int64     `db:"project_id"`
	UserID    int64     `db:"user_id"`
	Role      string    `db:"role"` // pm|leader|member
	JoinedAt  time.Time `db:"joined_at"`
}

// TaskRow 对应 tasks 表。
type TaskRow struct {
	ID            int64      `db:"id"`
	CompanyID     int64      `db:"company_id"`
	ProjectID     *int64     `db:"project_id"`
	Title         string     `db:"title"`
	Description   string     `db:"description"`
	StartTime     time.Time  `db:"start_time"`
	DueTime       time.Time  `db:"due_time"`
	CompletedTime *time.Time `db:"completed_time"`
	Status        string     `db:"status"`
	IsOverdue     bool       `db:"is_overdue"`
	ReminderYellow int64     `db:"reminder_yellow"`
	ReminderRed   int64      `db:"reminder_red"`
	Images        string     `db:"images"` // JSON 字符串数组
	CreatedBy     int64      `db:"created_by"`
	CreatedAt     time.Time  `db:"created_at"`
	UpdatedAt     time.Time  `db:"updated_at"`
}

// TaskAssignmentRow 对应 task_assignments 表。
type TaskAssignmentRow struct {
	ID          int64      `db:"id"`
	TaskID      int64      `db:"task_id"`
	UserID      int64      `db:"user_id"`
	Content     string     `db:"content"`
	Status      string     `db:"status"` // created|in_progress|completed
	CompletedAt *time.Time `db:"completed_at"`
	CreatedAt   time.Time  `db:"created_at"`
}

// TaskCommentRow 对应 task_comments 表。
type TaskCommentRow struct {
	ID        int64     `db:"id"`
	TaskID    int64     `db:"task_id"`
	UserID    int64     `db:"user_id"`
	Content   string    `db:"content"`
	Images    *string   `db:"images"` // JSON 附件数组，可空
	CreatedAt time.Time `db:"created_at"`
}

// TaskIssueRow 对应 task_issues 表。
type TaskIssueRow struct {
	ID         int64      `db:"id"`
	TaskID     int64      `db:"task_id"`
	UserID     int64      `db:"user_id"`
	Title      string     `db:"title"`
	Content    string     `db:"content"`
	Status     string     `db:"status"` // open|resolved
	ResolvedAt *time.Time `db:"resolved_at"`
	CreatedAt  time.Time  `db:"created_at"`
}

// TaskStatusLogRow 对应 task_status_logs 表。
type TaskStatusLogRow struct {
	ID         int64     `db:"id"`
	TaskID     int64     `db:"task_id"`
	UserID     int64     `db:"user_id"`
	FromStatus string    `db:"from_status"`
	ToStatus   string    `db:"to_status"`
	Note       string    `db:"note"`
	CreatedAt  time.Time `db:"created_at"`
}

// TaskExtensionRow 对应 task_extensions 表。
type TaskExtensionRow struct {
	ID              int64      `db:"id"`
	TaskID          int64      `db:"task_id"`
	UserID          int64      `db:"user_id"`
	RequestedDueTime time.Time `db:"requested_due_time"`
	Reason          string     `db:"reason"`
	Status          string     `db:"status"` // pending|approved|rejected
	DecidedBy       *int64     `db:"decided_by"`
	DecidedAt       *time.Time `db:"decided_at"`
	CreatedAt       time.Time  `db:"created_at"`
}

// RequirementRow 对应 requirements 表。
type RequirementRow struct {
	ID            int64      `db:"id"`
	CompanyID     int64      `db:"company_id"`
	ProjectID     *int64     `db:"project_id"`
	Code          string     `db:"code"`
	Title         string     `db:"title"`
	Description   string     `db:"description"`
	Category      string     `db:"category"`
	Priority      string     `db:"priority"`
	Status        string     `db:"status"`
	HandlerID     *int64     `db:"handler_id"`
	CreatorID     int64      `db:"creator_id"`
	StartTime     time.Time  `db:"start_time"`
	DueTime       time.Time  `db:"due_time"`
	CompletedTime *time.Time `db:"completed_time"`
	CreatedAt     time.Time  `db:"created_at"`
	UpdatedAt     time.Time  `db:"updated_at"`
}

// BugRow 对应 bugs 表。
type BugRow struct {
	ID            int64     `db:"id"`
	CompanyID     int64     `db:"company_id"`
	ProjectID     *int64    `db:"project_id"`
	RequirementID *int64    `db:"requirement_id"`
	Code          string    `db:"code"`
	Title         string    `db:"title"`
	Description   string    `db:"description"`
	Severity      string    `db:"severity"`
	Priority      string    `db:"priority"`
	Status        string    `db:"status"`
	HandlerID     *int64    `db:"handler_id"`
	CreatorID     int64     `db:"creator_id"`
	FoundVersion  string    `db:"found_version"`
	CreatedAt     time.Time `db:"created_at"`
	UpdatedAt     time.Time `db:"updated_at"`
}

// PlanRow 对应 plans 表。
type PlanRow struct {
	ID        int64     `db:"id"`
	CompanyID int64     `db:"company_id"`
	ProjectID *int64    `db:"project_id"`
	Name      string    `db:"name"`
	Description string  `db:"description"`
	StartTime time.Time `db:"start_time"`
	DueTime   time.Time `db:"due_time"`
	Status    string    `db:"status"`
	CreatorID int64     `db:"creator_id"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

// DocumentRow 对应 documents 表（TAPD 项目文档）。
type DocumentRow struct {
	ID        int64     `db:"id"`
	CompanyID int64     `db:"company_id"`
	ProjectID *int64    `db:"project_id"`
	Title     string    `db:"title"`
	Content   string    `db:"content"`
	CreatorID int64     `db:"creator_id"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

// WikiPageRow 对应 wiki_pages 表。
type WikiPageRow struct {
	ID        int64     `db:"id"`
	CompanyID int64     `db:"company_id"`
	ProjectID *int64    `db:"project_id"`
	Title     string    `db:"title"`
	Content   string    `db:"content"`
	CreatorID int64     `db:"creator_id"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

// RequirementLinkRow 对应 requirement_links 表（需求-任务关联）。
type RequirementLinkRow struct {
	ID            int64     `db:"id"`
	CompanyID     int64     `db:"company_id"`
	RequirementID int64     `db:"requirement_id"`
	TaskID        int64     `db:"task_id"`
	UserID        int64     `db:"user_id"`
	CreatedAt     time.Time `db:"created_at"`
}

// AfdianOrderRow 对应 afdian_orders 表。
type AfdianOrderRow struct {
	ID             int64     `db:"id"`
	OutTradeNo     string    `db:"out_trade_no"`
	CustomOrderID  string    `db:"custom_order_id"`
	PlanID         string    `db:"plan_id"`
	PlanTitle      string    `db:"plan_title"`
	Title          string    `db:"title"`
	Month          int64     `db:"month"`
	Amount         string    `db:"amount"`
	ShowAmount     string    `db:"show_amount"`
	Status         int64     `db:"status"`
	Remark         string    `db:"remark"`
	RedeemID       string    `db:"redeem_id"`
	ProductType    int64     `db:"product_type"`
	Discount       string    `db:"discount"`
	BuyerUserID    string    `db:"buyer_user_id"`
	BuyerName      string    `db:"buyer_name"`
	BuyerAvatar    string    `db:"buyer_avatar"`
	CreateTime     int64     `db:"create_time"`
	SkuDetail      string    `db:"sku_detail"`
	Raw            string    `db:"raw"`
	CreatedAt      time.Time `db:"created_at"`
}

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
	Company Company    `json:"company"`
	Role    string     `json:"role"`
}

// Department 部门。
type Department struct {
	ID        int64   `json:"id"`
	CompanyID int64   `json:"companyId"`
	Name      string  `json:"name"`
	ParentID  *int64  `json:"parentId"`
	CreatedAt string  `json:"createdAt"`
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
	ID          int64   `json:"id"`
	CompanyID   int64   `json:"companyId"`
	DepartmentID *int64 `json:"departmentId"`
	Name        string  `json:"name"`
	Code        string  `json:"code"`
	OwnerID     int64   `json:"ownerId"`
	CreatedAt   string  `json:"createdAt"`
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
	ID        int64   `json:"id"`
	Type      string  `json:"type"`
	GroupID   *int64  `json:"groupId"`
	DmUserA   *int64  `json:"dmUserA"`
	DmUserB   *int64  `json:"dmUserB"`
	CreatedAt string  `json:"createdAt"`
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
	UserID          int64   `json:"userId"`
	LastReadMessageID *int64 `json:"lastReadMessageId"`
}

// ConversationItem 对话列表条目。
type ConversationItem struct {
	ConversationID int64          `json:"conversationId"`
	Type           string         `json:"type"`
	GroupID        *int64         `json:"groupId"`
	DmUserID       *int64         `json:"dmUserId"`
	Pinned         bool           `json:"pinned"`
	LastMessageAt  *string        `json:"lastMessageAt"`
	LastPreview    *string        `json:"lastPreview"`
	Unread         int64          `json:"unread"`
	ReadReceipts   []ReadReceipt  `json:"readReceipts"`
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
	ID           int64    `json:"id"`
	CompanyID    int64    `json:"companyId"`
	CompanyName  string   `json:"companyName"`
	CompanyCode  string   `json:"companyCode"`
	UserID       int64    `json:"userId"`
	Username     string   `json:"username"`
	UserNick     string   `json:"userNick"`
	DepartmentID *int64   `json:"departmentId"`
	DepartmentName *string `json:"departmentName"`
	InvitedBy    int64    `json:"invitedBy"`
	InviterNick  string   `json:"inviterNick"`
	Code         string   `json:"code"`
	ExpiresAt    *string  `json:"expiresAt"`
	Status       string   `json:"status"`
	CreatedAt    string   `json:"createdAt"`
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
	ID          int64   `json:"id"`
	CompanyID   *int64  `json:"companyId"` // nil=主配置（全局）
	Email       string  `json:"email"`
	DisplayName string  `json:"displayName"`
	Host        string  `json:"host"`
	Port        int64   `json:"port"`
	Secure      bool    `json:"secure"`
	User        string  `json:"user"`
	Password    string  `json:"password"` // 敏感
	IsDefault   bool    `json:"isDefault"`
	Enabled     bool    `json:"enabled"`
	Priority    int64   `json:"priority"`
	CreatedAt   string  `json:"createdAt"`
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
	ID            int64    `json:"id"`
	CompanyID     int64    `json:"companyId"`
	ProjectID     *int64   `json:"projectId"`
	Title         string   `json:"title"`
	Description   string   `json:"description"`
	StartTime     string   `json:"startTime"`
	DueTime       string   `json:"dueTime"`
	CompletedTime *string  `json:"completedTime"`
	Status        string   `json:"status"`
	IsOverdue     bool     `json:"isOverdue"`
	ReminderYellow int64   `json:"reminderYellow"`
	ReminderRed   int64    `json:"reminderRed"`
	Images        []string `json:"images"`
	CreatedBy     int64    `json:"createdBy"`
	CreatedAt     string   `json:"createdAt"`
	UpdatedAt     string   `json:"updatedAt"`
}

// TaskAssignment 任务分配。
type TaskAssignment struct {
	ID          int64    `json:"id"`
	TaskID      int64    `json:"taskId"`
	UserID      int64    `json:"userId"`
	Content     string   `json:"content"`
	Status      string   `json:"status"`
	CompletedAt *string  `json:"completedAt"`
	Username    *string  `json:"username,omitempty"`
	Nick        *string  `json:"nick,omitempty"`
	Avatar      *string  `json:"avatar,omitempty"`
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
	ID          int64             `json:"id"`
	TaskID      int64             `json:"taskId"`
	UserID      int64             `json:"userId"`
	Content     string            `json:"content"`
	Attachments []TaskAttachment  `json:"attachments"`
	CreatedAt   string            `json:"createdAt"`
	Username    *string           `json:"username,omitempty"`
	Nick        *string           `json:"nick,omitempty"`
	Avatar      *string           `json:"avatar,omitempty"`
}

// TaskIssue 任务问题。
type TaskIssue struct {
	ID         int64    `json:"id"`
	TaskID     int64    `json:"taskId"`
	UserID     int64    `json:"userId"`
	Title      string   `json:"title"`
	Content    string   `json:"content"`
	Status     string   `json:"status"`
	ResolvedAt *string  `json:"resolvedAt"`
	CreatedAt  string   `json:"createdAt"`
	Username   *string  `json:"username,omitempty"`
	Nick       *string  `json:"nick,omitempty"`
	Avatar     *string  `json:"avatar,omitempty"`
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
	ID              int64    `json:"id"`
	TaskID          int64    `json:"taskId"`
	UserID          int64    `json:"userId"`
	RequestedDueTime string  `json:"requestedDueTime"`
	Reason          string   `json:"reason"`
	Status          string   `json:"status"`
	DecidedBy       *int64   `json:"decidedBy"`
	DecidedAt       *string  `json:"decidedAt"`
	CreatedAt       string   `json:"createdAt"`
	Username        *string  `json:"username,omitempty"`
	Nick            *string  `json:"nick,omitempty"`
}

// TaskDetail 任务详情（任务+分配+留言+问题+延期+状态日志）。
type TaskDetail struct {
	Task        TaskItem          `json:"task"`
	Assignments []TaskAssignment  `json:"assignments"`
	Comments    []TaskComment     `json:"comments"`
	Issues      []TaskIssue       `json:"issues"`
	Extensions  []TaskExtension   `json:"extensions"`
	Logs        []TaskStatusLog    `json:"logs"`
}

// RequirementStatus 需求状态。
type RequirementStatus = string

// Requirement 需求。
type Requirement struct {
	ID            int64    `json:"id"`
	CompanyID     int64    `json:"companyId"`
	ProjectID     *int64   `json:"projectId"`
	Code          string   `json:"code"`
	Title         string   `json:"title"`
	Description   string   `json:"description"`
	Category      string   `json:"category"`
	Priority      string   `json:"priority"`
	Status        string   `json:"status"`
	HandlerID     *int64   `json:"handlerId"`
	CreatorID     int64    `json:"creatorId"`
	StartTime     string   `json:"startTime"`
	DueTime       string   `json:"dueTime"`
	CompletedTime *string  `json:"completedTime"`
	LinkedTaskIDs []int64  `json:"linkedTaskIds"`
	CreatedAt     string   `json:"createdAt"`
	UpdatedAt     string   `json:"updatedAt"`
	HandlerName   *string  `json:"handlerName,omitempty"`
	CreatorName   *string  `json:"creatorName,omitempty"`
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
	ID          int64   `json:"id"`
	CompanyID   int64   `json:"companyId"`
	ProjectID   *int64  `json:"projectId"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	StartTime   string  `json:"startTime"`
	DueTime     string  `json:"dueTime"`
	Status      string  `json:"status"`
	CreatorID   int64   `json:"creatorId"`
	CreatedAt   string  `json:"createdAt"`
	UpdatedAt   string  `json:"updatedAt"`
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
	NewRequirement30d int64            `json:"newRequirement30d"`
	OverdueRequirement int64            `json:"overdueRequirement"`
	BugResolveRate    int64            `json:"bugResolveRate"`
	UnresolvedBug      int64            `json:"unresolvedBug"`
	RequirementByStatus map[string]int64 `json:"requirementByStatus"`
	BugBySeverity      map[string]int64 `json:"bugBySeverity"`
}

// MemberTrackItem 成员任务跟踪。
type MemberTrackItem struct {
	UserID    int64  `json:"userId"`
	Nick      string `json:"nick"`
	Username  string `json:"username"`
	Total     int64  `json:"total"`
	Completed int64  `json:"completed"`
	InProgress int64 `json:"inProgress"`
	Overdue   int64  `json:"overdue"`
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
