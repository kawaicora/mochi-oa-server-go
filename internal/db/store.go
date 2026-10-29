// Package db 是存储层抽象与实现。
//
//   - Store：存储层接口（方法集原样移植自 db/store.ts，后续所有 handler 按它编码）；
//   - MySqlStore：唯一实现（GORM + gorm.io/driver/mysql；默认启动时自动建库建表，
//     见 setup.go，可用 AUTO_CREATE_DATABASE / AUTO_MIGRATE 关闭）；
//   - Open/GetStore：包级单例（sync.Once），全局只连接一次。
//
// 所有方法首个参数为 context.Context（Go 惯例，用于取消/超时），不改变方法语义。
package db

import (
	"context"
	"errors"
	"time"

	"mochi-oa-server-go/internal/types"
)

// 存储层业务错误（对应 store.ts 的四个 Error 子类）。
// handler 用 errors.As 判断具体类型。
var (
	ErrAlreadyExists = errors.New("already exists")
	ErrNotFound      = errors.New("not found")
	ErrNotMember     = errors.New("not a member")
	ErrPermission    = errors.New("permission denied")
)

// AlreadyExistsError 冲突（唯一约束/重复加入）。
type AlreadyExistsError struct{ Msg string }

func (e *AlreadyExistsError) Error() string { return e.Msg }

// NotFoundError 记录不存在。
type NotFoundError struct{ Msg string }

func (e *NotFoundError) Error() string { return e.Msg }

// NotMemberError 非成员。
type NotMemberError struct{ Msg string }

func (e *NotMemberError) Error() string { return e.Msg }

// PermissionError 无权限。
type PermissionError struct{ Msg string }

func (e *PermissionError) Error() string { return e.Msg }

// ---- 输入结构（对应 store.ts 各方法的内联 input 对象；可选字段用指针区分「未传」与「传空」） ----

// CreateUserInput 创建用户。
type CreateUserInput struct {
	Username     string
	PasswordHash string
	Nick         string
	Email        string
	Avatar       string
	Phone        string
}

// UserProfilePatch 更新资料（nil 字段不更新）。
type UserProfilePatch struct {
	Nick   *string
	Avatar *string
	Phone  *string
	Email  *string
	Extra  *string
}

// CreateCompanyInput 创建公司。
type CreateCompanyInput struct {
	Name    string
	Code    string
	OwnerID int64
}

// CreateDepartmentInput 创建部门。
type CreateDepartmentInput struct {
	CompanyID int64
	Name      string
	ParentID  *int64
}

// CreateGroupInput 创建群。
type CreateGroupInput struct {
	CompanyID    int64
	DepartmentID *int64
	Name         string
	Code         string
	OwnerID      int64
}

// CreateInvitationInput 创建公司邀请。
type CreateInvitationInput struct {
	CompanyID    int64
	UserID       int64
	DepartmentID *int64
	InvitedBy    int64
	Code         string
	ExpiresAt    *string
}

// CreateSessionInput 创建登录会话。
type CreateSessionInput struct {
	UserID    int64
	TokenHash string
	Device    string
	IP        string
	Location  string
	ExpiresAt string
}

// UpsertAfdianOrderInput 落库爱发电订单。
type UpsertAfdianOrderInput struct {
	OutTradeNo    string
	CustomOrderID string
	PlanID        string
	PlanTitle     string
	Title         string
	Month         int64
	Amount        string
	ShowAmount    string
	Status        int64
	Remark        string
	RedeemID      string
	ProductType   int64
	Discount      string
	BuyerUserID   string
	BuyerName     string
	BuyerAvatar   string
	CreateTime    int64
	SkuDetail     string
	Raw           string
}

// MailConfigScope 邮件配置查询作用域：CompanyID nil=主配置。
type MailConfigScope struct {
	CompanyID *int64
}

// SaveMailConfigInput 新增/更新邮件配置。
type SaveMailConfigInput struct {
	ID          *int64
	CompanyID   *int64
	Email       string
	DisplayName string
	Host        string
	Port        *int64
	Secure      *bool
	User        string
	Password    string
	IsDefault   *bool
	Enabled     *bool
	Priority    *int64
}

// SaveHolidayInput 新增/更新假期。
type SaveHolidayInput struct {
	Date string
	Name string
	Type string
}

// CreateProjectInput 创建项目。
type CreateProjectInput struct {
	CompanyID int64
	Name      string
	PmID      int64
}

// CreateTaskInput 创建任务。
type CreateTaskInput struct {
	CompanyID   int64
	ProjectID   *int64
	Title       string
	Description string
	StartTime   string
	DueTime     string
	Images      []string
	CreatedBy   int64
}

// UpdateTaskInput 更新任务。
type UpdateTaskInput struct {
	ID          int64
	Title       *string
	Description *string
	StartTime   *string
	DueTime     *string
	Images      []string
}

// SetTaskStatusInput 变更任务状态。
type SetTaskStatusInput struct {
	TaskID int64
	Status string
	UserID int64
	Note   string
}

// TaskReminderInput 更新提醒阈值。
type TaskReminderInput struct {
	TaskID         int64
	ReminderYellow int64
	ReminderRed    int64
}

// AddAssignmentInput 新增分配。
type AddAssignmentInput struct {
	TaskID  int64
	UserID  int64
	Content string
}

// SetAssignmentStatusInput 执行人提交分配状态。
type SetAssignmentStatusInput struct {
	ID     int64
	Status string
	UserID int64
}

// AddTaskCommentInput 新增任务留言。
type AddTaskCommentInput struct {
	TaskID      int64
	UserID      int64
	Content     string
	Attachments []types.TaskAttachment
}

// AddTaskIssueInput 新增任务问题。
type AddTaskIssueInput struct {
	TaskID  int64
	UserID  int64
	Title   string
	Content string
}

// RequestExtensionInput 申请延期。
type RequestExtensionInput struct {
	TaskID           int64
	UserID           int64
	RequestedDueTime string
	Reason           string
}

// DecideExtensionInput 审批延期。
type DecideExtensionInput struct {
	ExtensionID int64
	Approved    bool
	DecidedBy   int64
}

// CreateRequirementInput 创建需求。
type CreateRequirementInput struct {
	CompanyID   int64
	ProjectID   *int64
	Title       string
	Description string
	Category    string
	Priority    string
	HandlerID   *int64
	StartTime   string
	DueTime     string
	CreatorID   int64
}

// UpdateRequirementInput 更新需求。
type UpdateRequirementInput struct {
	ID          int64
	Title       *string
	Description *string
	Category    *string
	Priority    *string
	HandlerID   *int64
	StartTime   *string
	DueTime     *string
}

// SetRequirementStatusInput 变更需求状态。
type SetRequirementStatusInput struct {
	RequirementID int64
	Status        string
	UserID        int64
}

// LinkRequirementTasksInput 关联任务（先清后写）。
type LinkRequirementTasksInput struct {
	RequirementID int64
	TaskIDs       []int64
	UserID        int64
	CompanyID     int64
}

// CreateBugInput 创建缺陷。
type CreateBugInput struct {
	CompanyID     int64
	ProjectID     *int64
	RequirementID *int64
	Title         string
	Description   string
	Severity      string
	Priority      string
	HandlerID     *int64
	FoundVersion  string
	CreatorID     int64
}

// UpdateBugInput 更新缺陷。
type UpdateBugInput struct {
	ID           int64
	Title        *string
	Description  *string
	Severity     *string
	Priority     *string
	HandlerID    *int64
	FoundVersion *string
}

// SetBugStatusInput 变更缺陷状态。
type SetBugStatusInput struct {
	BugID  int64
	Status string
	UserID int64
}

// CreatePlanInput 创建计划。
type CreatePlanInput struct {
	CompanyID   int64
	ProjectID   *int64
	Name        string
	Description string
	StartTime   string
	DueTime     string
	CreatorID   int64
}

// UpdatePlanInput 更新计划。
type UpdatePlanInput struct {
	ID          int64
	Name        *string
	Description *string
	StartTime   *string
	DueTime     *string
	Status      *string
}

// CreateDocumentInput 创建文档。
type CreateDocumentInput struct {
	CompanyID int64
	ProjectID *int64
	Title     string
	Content   string
	CreatorID int64
}

// UpdateDocumentInput 更新文档。
type UpdateDocumentInput struct {
	ID      int64
	Title   *string
	Content *string
}

// CreateWikiInput 创建 Wiki 页。
type CreateWikiInput struct {
	CompanyID int64
	ProjectID *int64
	Title     string
	Content   string
	CreatorID int64
}

// UpdateWikiInput 更新 Wiki 页。
type UpdateWikiInput struct {
	ID      int64
	Title   *string
	Content *string
}

// SaveFileInput 上传文件记录。
type SaveFileInput struct {
	UUID     string
	Filename string
	Mime     string
	Size     int64
	Storage  string
	URL      string
}

// Store 是存储层抽象：生产用 MySqlStore，开发/测试用 MemoryStore。
// 方法集与 db/store.ts 一一对应，后续 handler 只面向本接口编程。
type Store interface {
	Init(ctx context.Context) error
	Close(ctx context.Context) error

	// ---- 用户 ----
	CreateUser(ctx context.Context, in CreateUserInput) (*types.User, error)
	GetUserByUsername(ctx context.Context, username string) (*types.UserWithPassword, error)
	GetUserByEmail(ctx context.Context, email string) (*types.UserWithPassword, error)
	GetUserByID(ctx context.Context, id int64) (*types.User, error)
	ListUsers(ctx context.Context) ([]types.User, error)
	UpdateUserProfile(ctx context.Context, userID int64, patch UserProfilePatch) error
	UpdateUserPassword(ctx context.Context, userID int64, passwordHash string) error

	// ---- 公司 ----
	CreateCompany(ctx context.Context, in CreateCompanyInput) (*types.Company, error)
	GetCompanyByID(ctx context.Context, id int64) (*types.Company, error)
	GetCompanyByCode(ctx context.Context, code string) (*types.Company, error)
	GetCompanyByName(ctx context.Context, name string) (*types.Company, error)
	ListCompanies(ctx context.Context, keyword string) ([]types.Company, error)
	AddMember(ctx context.Context, companyID, userID int64, role string) error
	RemoveMember(ctx context.Context, companyID, userID int64) error
	SetRole(ctx context.Context, companyID, userID int64, role string) error
	GetMemberRole(ctx context.Context, companyID, userID int64) (string, error)
	GetApprovalCc(ctx context.Context, companyID int64) ([]int64, error)
	SetApprovalCc(ctx context.Context, companyID int64, userIDs []int64) error
	GetMembers(ctx context.Context, companyID int64) ([]types.Member, error)
	GetUserCompanies(ctx context.Context, userID int64) ([]types.Membership, error)
	UpdateCompanyLogo(ctx context.Context, companyID int64, avatar string) error
	RenameCompany(ctx context.Context, companyID int64, name string) error
	SetCompanyOwner(ctx context.Context, companyID, ownerID int64) error
	DeleteCompany(ctx context.Context, companyID int64) error

	// ---- 部门 ----
	CreateDepartment(ctx context.Context, in CreateDepartmentInput) (*types.Department, error)
	GetDepartmentByID(ctx context.Context, id int64) (*types.Department, error)
	ListDepartments(ctx context.Context, companyID int64) ([]types.Department, error)
	AssignDepartment(ctx context.Context, departmentID, userID int64) error
	RemoveDepartmentMember(ctx context.Context, departmentID, userID int64) error
	GetDepartmentMembers(ctx context.Context, departmentID int64) ([]types.DepartmentMember, error)
	DeleteDepartment(ctx context.Context, departmentID int64) error

	// ---- 群 ----
	CreateGroup(ctx context.Context, in CreateGroupInput) (*types.Group, error)
	GetGroupByID(ctx context.Context, id int64) (*types.Group, error)
	GetGroupByCode(ctx context.Context, code string) (*types.Group, error)
	GetGroupByName(ctx context.Context, companyID int64, name string) (*types.Group, error)
	ListGroups(ctx context.Context, companyID int64, keyword string) ([]types.Group, error)
	AddGroupMember(ctx context.Context, groupID, userID int64, role string) error
	RemoveGroupMember(ctx context.Context, groupID, userID int64) error
	GetGroupMemberRole(ctx context.Context, groupID, userID int64) (string, error)
	GetGroupMember(ctx context.Context, groupID, userID int64) (role string, mutedUntil *string, err error)
	SetGroupMemberRole(ctx context.Context, groupID, userID int64, role string) error
	SetGroupMemberMuted(ctx context.Context, groupID, userID int64, mutedUntil *string) error
	SetGroupOwner(ctx context.Context, groupID, ownerID int64) error
	DeleteGroup(ctx context.Context, groupID int64) error
	GetGroupMembers(ctx context.Context, groupID int64) ([]types.GroupMember, error)
	GetUserGroups(ctx context.Context, userID int64) ([]types.Group, error)

	// ---- 对话 ----
	GetOrCreateGroupConversation(ctx context.Context, groupID, ownerID int64) (*types.Conversation, error)
	GetOrCreateDmConversation(ctx context.Context, a, b int64) (*types.Conversation, error)
	GetConversationByID(ctx context.Context, id int64) (*types.Conversation, error)
	AddConversationMember(ctx context.Context, conversationID, userID int64) error
	RemoveConversationMember(ctx context.Context, conversationID, userID int64) error
	SetConversationPinned(ctx context.Context, conversationID, userID int64, pinned bool) error
	GetConversationMember(ctx context.Context, conversationID, userID int64) (*types.ConversationMember, error)
	ListConversations(ctx context.Context, userID int64, typ string) ([]types.ConversationItem, error)

	// ---- 消息 ----
	SaveMessage(ctx context.Context, conversationID, fromID int64, kind, content string) (*types.ChatMessage, error)
	ListMessages(ctx context.Context, conversationID, beforeTs int64, limit int64) ([]types.ChatMessage, error)
	GetMessage(ctx context.Context, conversationID int64, messageID string) (*types.ChatMessage, error)
	FindOwnMessageByContent(ctx context.Context, conversationID, userID int64, content string) (*types.ChatMessage, error)
	FindOwnMessageByContentGlobal(ctx context.Context, userID int64, content string) (*types.ChatMessage, error)
	SoftDeleteMessage(ctx context.Context, conversationID int64, messageID string) (bool, error)
	HardDeleteMessage(ctx context.Context, conversationID int64, messageID string) (bool, error)
	TouchConversation(ctx context.Context, conversationID, userID int64, preview string, fromID int64) error
	// MarkRead 记录已读位置（只前进）并重算未读；返回 (未读数, 实际生效的已读位置)。
	// lastReadMessageID 为 nil 时表示「读到该会话最新一条」（兼容旧客户端）。
	MarkRead(ctx context.Context, conversationID, userID int64, lastReadMessageID *int64) (int64, *int64, error)
	GetReadReceipts(ctx context.Context, conversationID int64) ([]types.ReadReceipt, error)

	// ---- 文件 ----
	SaveFile(ctx context.Context, in SaveFileInput) (*types.FileRecord, error)
	GetFileByUUID(ctx context.Context, uuid string) (*types.FileRecord, error)

	// ---- 好友 ----
	AddFriend(ctx context.Context, a, b int64) error
	RemoveFriend(ctx context.Context, a, b int64) error
	ListFriends(ctx context.Context, userID int64) ([]types.Friend, error)

	// ---- 公司邀请 ----
	CreateCompanyInvitation(ctx context.Context, in CreateInvitationInput) (*types.CompanyInvitation, error)
	GetCompanyInvitationByID(ctx context.Context, id int64) (*types.CompanyInvitation, error)
	GetCompanyInvitationByCode(ctx context.Context, code string) (*types.CompanyInvitation, error)
	ListCompanyInvitations(ctx context.Context, companyID int64) ([]types.CompanyInvitation, error)
	ListPendingInvitationsForUser(ctx context.Context, userID int64) ([]types.CompanyInvitation, error)
	SetCompanyInvitationStatus(ctx context.Context, id int64, status string) error
	GetCompanyMainGroup(ctx context.Context, companyID int64) (*types.Group, error)

	// ---- 成员人事信息 ----
	GetMemberProfile(ctx context.Context, companyID, userID int64) (*types.MemberProfile, error)
	UpsertMemberProfile(ctx context.Context, companyID, userID int64, p MemberProfileInput) (*types.MemberProfile, error)

	// ---- 登录会话 ----
	CreateSession(ctx context.Context, in CreateSessionInput) (*types.Session, error)
	GetSessionByTokenHash(ctx context.Context, tokenHash string) (*types.Session, error)
	ListSessionsByUser(ctx context.Context, userID int64) ([]types.Session, error)
	GetUserActiveLocation(ctx context.Context, userID int64) (string, error)
	TouchSession(ctx context.Context, id int64) error
	DeleteSession(ctx context.Context, id int64) error
	DeleteExpiredSessions(ctx context.Context, now int64) (int64, error)
	GetSessionDays(ctx context.Context, userID int64) (*int64, error)
	SetSessionDays(ctx context.Context, userID int64, days *int64) error

	// ---- 爱发电 ----
	UpsertAfdianOrder(ctx context.Context, in UpsertAfdianOrderInput) (*types.AfdianOrder, error)
	GetAfdianOrderByOutTradeNo(ctx context.Context, outTradeNo string) (*types.AfdianOrder, error)
	ListAfdianOrders(ctx context.Context, limit, offset int64) ([]types.AfdianOrder, error)

	// ---- 邮件配置 ----
	ListMailConfigs(ctx context.Context, scope MailConfigScope) ([]types.MailConfig, error)
	GetMailConfig(ctx context.Context, id int64) (*types.MailConfig, error)
	GetDefaultMailConfig(ctx context.Context, scope MailConfigScope) (*types.MailConfig, error)
	SaveMailConfig(ctx context.Context, in SaveMailConfigInput) (*types.MailConfig, error)
	DeleteMailConfig(ctx context.Context, id int64) error

	// ---- SERVER_ADMIN（server_admins 表，持久化，重启保留） ----
	ListServerAdmins(ctx context.Context) ([]types.ServerAdminRow, error)
	// UpsertServerAdmin 设某用户为 SERVER_ADMIN（已存在则更新时间）；createdBy 记录创建人用户 id。
	UpsertServerAdmin(ctx context.Context, username string, createdBy int64) error
	DeleteServerAdmin(ctx context.Context, username string) error

	// ---- 假期 ----
	ListHolidays(ctx context.Context, year int64) ([]types.Holiday, error)
	SaveHoliday(ctx context.Context, in SaveHolidayInput) (*types.Holiday, error)
	DeleteHoliday(ctx context.Context, date string) error

	// ---- 任务流程 ----
	CreateProject(ctx context.Context, in CreateProjectInput) (*types.Project, error)
	ListProjects(ctx context.Context, companyID int64) ([]types.Project, error)
	DeleteProject(ctx context.Context, projectID int64) error
	SetProjectRole(ctx context.Context, projectID, userID int64, role string) error
	ListProjectMembers(ctx context.Context, projectID int64) ([]types.ProjectMember, error)
	CreateTask(ctx context.Context, in CreateTaskInput) (*types.TaskItem, error)
	ListTasks(ctx context.Context, companyID int64, projectID *int64) ([]types.TaskItem, error)
	GetTask(ctx context.Context, taskID int64) (*types.TaskItem, error)
	GetTaskDetail(ctx context.Context, taskID int64) (*types.TaskDetail, error)
	UpdateTask(ctx context.Context, in UpdateTaskInput) (*types.TaskItem, error)
	DeleteTask(ctx context.Context, taskID int64) error
	SetTaskStatus(ctx context.Context, in SetTaskStatusInput) (*types.TaskItem, error)
	UpdateTaskReminder(ctx context.Context, in TaskReminderInput) (*types.TaskItem, error)
	AddAssignment(ctx context.Context, in AddAssignmentInput) (*types.TaskAssignment, error)
	RemoveAssignment(ctx context.Context, id int64) error
	SetAssignmentStatus(ctx context.Context, in SetAssignmentStatusInput) (*types.TaskAssignment, error)
	AddTaskComment(ctx context.Context, in AddTaskCommentInput) (*types.TaskComment, error)
	AddTaskIssue(ctx context.Context, in AddTaskIssueInput) (*types.TaskIssue, error)
	ResolveTaskIssue(ctx context.Context, issueID int64) (*types.TaskIssue, error)
	RequestTaskExtension(ctx context.Context, in RequestExtensionInput) (*types.TaskExtension, error)
	DecideTaskExtension(ctx context.Context, in DecideExtensionInput) (*types.TaskExtension, error)

	// ---- TAPD ----
	CreateRequirement(ctx context.Context, in CreateRequirementInput) (*types.Requirement, error)
	ListRequirements(ctx context.Context, companyID int64, projectID *int64) ([]types.Requirement, error)
	GetRequirement(ctx context.Context, requirementID int64) (*types.Requirement, error)
	UpdateRequirement(ctx context.Context, in UpdateRequirementInput) (*types.Requirement, error)
	SetRequirementStatus(ctx context.Context, in SetRequirementStatusInput) (*types.Requirement, error)
	DeleteRequirement(ctx context.Context, requirementID int64) error
	LinkRequirementTasks(ctx context.Context, in LinkRequirementTasksInput) ([]int64, error)
	CreateBug(ctx context.Context, in CreateBugInput) (*types.Bug, error)
	ListBugs(ctx context.Context, companyID int64, projectID *int64) ([]types.Bug, error)
	GetBug(ctx context.Context, bugID int64) (*types.Bug, error)
	UpdateBug(ctx context.Context, in UpdateBugInput) (*types.Bug, error)
	SetBugStatus(ctx context.Context, in SetBugStatusInput) (*types.Bug, error)
	DeleteBug(ctx context.Context, bugID int64) error
	CreatePlan(ctx context.Context, in CreatePlanInput) (*types.Plan, error)
	ListPlans(ctx context.Context, companyID int64, projectID *int64) ([]types.Plan, error)
	UpdatePlan(ctx context.Context, in UpdatePlanInput) (*types.Plan, error)
	DeletePlan(ctx context.Context, planID int64) error
	CreateDocument(ctx context.Context, in CreateDocumentInput) (*types.ProjectDocument, error)
	ListDocuments(ctx context.Context, companyID int64, projectID *int64) ([]types.ProjectDocument, error)
	UpdateDocument(ctx context.Context, in UpdateDocumentInput) (*types.ProjectDocument, error)
	DeleteDocument(ctx context.Context, documentID int64) error
	CreateWikiPage(ctx context.Context, in CreateWikiInput) (*types.WikiPage, error)
	ListWikiPages(ctx context.Context, companyID int64, projectID *int64) ([]types.WikiPage, error)
	UpdateWikiPage(ctx context.Context, in UpdateWikiInput) (*types.WikiPage, error)
	DeleteWikiPage(ctx context.Context, wikiID int64) error
	DashboardStats(ctx context.Context, companyID int64, projectID *int64) (*types.DashboardStats, error)
	MemberTracking(ctx context.Context, companyID int64, projectID *int64) ([]types.MemberTrackItem, error)

	// ---- 日报/周报（公司隔离；仅 company_members 成员可用） ----
	CreateReportTemplate(ctx context.Context, in CreateReportTemplateInput) (*types.ReportTemplate, error)
	UpdateReportTemplate(ctx context.Context, id int64, in UpdateReportTemplateInput) (*types.ReportTemplate, error)
	DeleteReportTemplate(ctx context.Context, id int64) error
	ListReportTemplates(ctx context.Context, companyID int64) ([]types.ReportTemplate, error)
	GetReportTemplateByID(ctx context.Context, id int64) (*types.ReportTemplate, error)
	CreateReport(ctx context.Context, in CreateReportInput) (*types.ReportItem, error)
	GetReportByID(ctx context.Context, id int64) (*types.ReportItem, error)
	GetReportByPeriod(ctx context.Context, companyID, userID int64, typ, periodDate string) (*types.ReportItem, error)
	UpdateReport(ctx context.Context, id int64, in UpdateReportInput) (*types.ReportItem, error)
	SubmitReport(ctx context.Context, id int64) (*types.ReportItem, error)
	DeleteReport(ctx context.Context, id int64) error
	// ListReports 公司+筛选+分页，返回（当前页行, 满足条件总数）。
	ListReports(ctx context.Context, in ListReportsInput) ([]types.ReportItem, int64, error)
	// ListReportsByPeriod 统计用：一次取 [periodStart,periodEnd] 内全部报告；userIDs 为空=全公司。
	ListReportsByPeriod(ctx context.Context, companyID int64, typ, periodStart, periodEnd string, userIDs []int64) ([]types.ReportItem, error)
	// ListInboxReports 收件箱：查「别人发给我的」（JSON_CONTAINS recipient_ids 含本人），
	// createdAt 倒序分页，返回（当前页行, 总数）。
	ListInboxReports(ctx context.Context, companyID, userID int64, page, pageSize int64) ([]types.ReportItem, int64, error)

	// ─── 考勤申请 ───
	// CreateAttendanceRequest 发起申请（approver/cc 为创建时流程快照）。
	CreateAttendanceRequest(ctx context.Context, in CreateAttendanceRequestInput) (*types.AttendanceRequest, error)
	// GetAttendanceRequestByID 按 id 取（联表补 nick/username）。
	GetAttendanceRequestByID(ctx context.Context, id int64) (*types.AttendanceRequest, error)
	// ListAttendanceRequestsByUser 只看本人历史（createdAt 倒序分页），type 可筛选。
	// dateStart/dateEnd（YYYY-MM-DD 含两端）为空串=不按区间过滤；
	// 语义：start_time 落在 [dateStart,dateEnd] 内即命中（用于日历紫色点）。
	ListAttendanceRequestsByUser(ctx context.Context, companyID, userID int64, typ, dateStart, dateEnd string, page, pageSize int64) ([]types.AttendanceRequest, int64, error)
	// ApproveAttendanceRequest 置 approved：approverID=审批人、approvedAt=now。
	ApproveAttendanceRequest(ctx context.Context, id, approverID int64) (*types.AttendanceRequest, error)
	// RejectAttendanceRequest 置 rejected：approvalNote=驳回理由。
	RejectAttendanceRequest(ctx context.Context, id int64, note string) (*types.AttendanceRequest, error)
	// ListReceivedAttendance 收到审批：本人提交 OR 本人为审批人（含自审批）。
	ListReceivedAttendance(ctx context.Context, companyID, userID int64) ([]types.AttendanceRequest, error)
	// ListReceivedCC 收到抄送：本人提交 OR 本人为抄送人（含自抄送）。
	ListReceivedCC(ctx context.Context, companyID, userID int64) ([]types.AttendanceRequest, error)
	// MarkCCViewed 把 userID 加入 cc_viewed_ids（幂等）。
	MarkCCViewed(ctx context.Context, requestID, userID int64) error
	// StopAttendanceNotification 置 notify_stopped=1（循环提醒停止，幂等）。
	StopAttendanceNotification(ctx context.Context, requestID, companyID int64) error

	// ─── 报销申请 ───
	CreateExpenseRequest(ctx context.Context, in CreateExpenseRequestInput) (*types.ExpenseRequest, error)
	GetExpenseByID(ctx context.Context, id int64) (*types.ExpenseRequest, error)
	ListExpenseRequestsByUser(ctx context.Context, companyID, userID int64, category string, page, pageSize int64) ([]types.ExpenseRequest, int64, error)
	ApproveExpenseRequest(ctx context.Context, id, approverID int64) (*types.ExpenseRequest, error)
	RejectExpenseRequest(ctx context.Context, id int64, note string) (*types.ExpenseRequest, error)
	ListReceivedExpense(ctx context.Context, companyID, userID int64) ([]types.ExpenseRequest, error)
	ListReceivedExpenseCC(ctx context.Context, companyID, userID int64) ([]types.ExpenseRequest, error)
	MarkExpenseCCViewed(ctx context.Context, requestID, userID int64) error
	StopExpenseNotification(ctx context.Context, requestID int64, companyID int64) error

	// ─── 备用金申请 ───
	CreateAdvanceRequest(ctx context.Context, in CreateAdvanceRequestInput) (*types.AdvanceRequest, error)
	GetAdvanceByID(ctx context.Context, id int64) (*types.AdvanceRequest, error)
	ListAdvanceRequestsByUser(ctx context.Context, companyID, userID int64, category string, page, pageSize int64) ([]types.AdvanceRequest, int64, error)
	ApproveAdvanceRequest(ctx context.Context, id, approverID int64) (*types.AdvanceRequest, error)
	RejectAdvanceRequest(ctx context.Context, id int64, note string) (*types.AdvanceRequest, error)
	ListReceivedAdvance(ctx context.Context, companyID, userID int64) ([]types.AdvanceRequest, error)
	ListReceivedAdvanceCC(ctx context.Context, companyID, userID int64) ([]types.AdvanceRequest, error)
	MarkAdvanceCCViewed(ctx context.Context, requestID, userID int64) error
	StopAdvanceNotification(ctx context.Context, requestID int64, companyID int64) error

	// ─── 打卡 ───
	// CreateCheckin 写入/更新当天打卡：0条→INSERT(first)；1条→INSERT(second)；≥2条→UPDATE 当天最后一条 check_time(override，不新增行)。返回 item 与 kind(first/second/override)。
	// workStart/workEnd/graceLate/graceEarly 为打卡时的排班规则快照，随记录落库供历史判定。
	// offWindow(variadic)：命中「前一天下班+8h 窗口」时传 true，此时当天 0 条也按下班卡写入。
	CreateCheckin(ctx context.Context, companyID, userID int64, checkDate, ip string, mac *string, workStart, workEnd string, graceLate, graceEarly int64, offWindow ...bool) (*types.CheckinItem, string, error)
	// ListCheckinsByMonth 本人某月全部记录（check_date=月份前缀），联表补昵称。
	ListCheckinsByMonth(ctx context.Context, companyID, userID int64, month string) ([]types.CheckinItem, error)
	// InsertMakeupCheckin 补卡批准后把补卡时间写入 checkins（补卡算打卡）。
	// checkDate 为目标日期（被补那天，可跨天补下班）；punchTime 为实际打卡时刻（本地时间字符串）。
	// 解析失败或同时间点已存在时幂等跳过。workStart/workEnd/graceLate/graceEarly 为补卡时的排班规则快照；
	// slot=补的卡类型(1上班/2下班)。
	InsertMakeupCheckin(ctx context.Context, companyID, userID int64, checkDate, punchTime, workStart, workEnd string, graceLate, graceEarly, slot int64) error
	// GetCheckinSettings 公司打卡白名单；未配置返回 (nil, nil)。
	GetCheckinSettings(ctx context.Context, companyID int64) (*types.CheckinSettings, error)
	// SaveCheckinSettings upsert 公司打卡白名单（一公司一条）。macEnabled 控制是否启用 MAC 限制。
	SaveCheckinSettings(ctx context.Context, companyID int64, macEnabled bool, macWhitelist, ipWhitelist, noPunchUserIds []string) (*types.CheckinSettings, error)

	// GetCheckinRules 公司打卡排班规则；未配置返回 (nil, nil)。
	GetCheckinRules(ctx context.Context, companyID int64) (*types.CompanyCheckinRulesRow, error)
	// SaveCheckinRules upsert 排班规则（weekRulesJSON/altRuleJSON 为已校验后的 JSON 字符串，可空；offworkDeadline 下班卡次日截止 HH:MM）。
	SaveCheckinRules(ctx context.Context, companyID int64, mode string, graceLate, graceEarly int64, weekRulesJSON string, altRuleJSON *string, offworkDeadline string) error

	// ─── 出网代理 ───
	// CreateProxyAccount 新建账号（username 唯一、冲突由实现重试；返回完整行含 secret，仅一次性）。
	CreateProxyAccount(ctx context.Context, companyID, createdBy int64, expiresAt time.Time, note *string) (*types.ProxyAccountRow, error)
	// ProxyAuthLookup 按 username+secret 鉴权；返回行供调用方校验 status/expires。
	ProxyAuthLookup(ctx context.Context, username, secret string) (*types.ProxyAccountRow, error)
	// ListProxyAccounts 公司账号分页（视图不含 secret）。
	ListProxyAccounts(ctx context.Context, companyID int64, page, pageSize int64) ([]types.ProxyAccountItem, int64, error)
	// DisableProxyAccount 置 disabled（立即失效）；带 company_id 双保险，跨公司不可写。
	DisableProxyAccount(ctx context.Context, companyID, id int64) error
	// DeleteProxyAccount 物理删除；带 company_id 双保险，跨公司不可删。
	DeleteProxyAccount(ctx context.Context, companyID, id int64) error
	// SweepExpiredProxyAccounts 把已过期的 active 账号批量置 disabled，返回处置条数。
	SweepExpiredProxyAccounts(ctx context.Context) (int64, error)
	// ReactivateProxyAccount 续期：status=active 且 expires_at=新时间。
	ReactivateProxyAccount(ctx context.Context, id int64, expiresAt time.Time) error
	// GetProxyAccountByID 取账号行（含 secret，仅服务端内部用）。
	GetProxyAccountByID(ctx context.Context, id int64) (*types.ProxyAccountRow, error)

	// ─── 上网申请 ───
	// CreateProxyAccessRequest 新建上网申请。
	CreateProxyAccessRequest(ctx context.Context, in CreateProxyAccessRequestInput) (*types.ProxyRequestItem, error)
	// GetProxyAccessRequestByID 按 id 取（联表补昵称）。
	GetProxyAccessRequestByID(ctx context.Context, id int64) (*types.ProxyRequestItem, error)
	// ListProxyAccessRequests 公司申请列表；all=true 时忽略 userID 看全部（admin），否则只看本人。
	ListProxyAccessRequests(ctx context.Context, companyID, userID int64, all bool, status string, page, pageSize int64) ([]types.ProxyRequestItem, int64, error)
	// UpdateProxyAccessRequest 通用状态/字段更新（approve/reject/renew 复用）。
	UpdateProxyAccessRequest(ctx context.Context, id int64, in UpdateProxyAccessRequestInput) (*types.ProxyRequestItem, error)
	// StopProxyNotification 置 notify_stopped=1（循环提醒停止，幂等）。
	StopProxyNotification(ctx context.Context, id, companyID int64) error

	// ─── 资产管理 ───
	CreateAsset(ctx context.Context, in CreateAssetInput) (*types.AssetItem, error)
	GetAssetByID(ctx context.Context, id int64) (*types.AssetItem, error)
	GetAssetRowByCode(ctx context.Context, code string) (*types.AssetRow, error) // 二维码页用
	ListAssets(ctx context.Context, companyID int64, keyword string, page, pageSize int64) ([]types.AssetItem, int64, error)
	UpdateAsset(ctx context.Context, id int64, in UpdateAssetInput) (*types.AssetItem, error)
	DeleteAsset(ctx context.Context, id int64) error // 连同流转记录一起删
	CreateAssetFlow(ctx context.Context, in CreateAssetFlowInput) (*types.FlowItem, *types.AssetItem, error)
	ListAssetFlows(ctx context.Context, companyID, assetID int64, page, pageSize int64) ([]types.FlowItem, int64, error)

	// ─── 公司职位 ───
	EnsureDefaultJobs(ctx context.Context, companyID int64) (int64, error) // 该公司 jobs 为空时播种默认清单，返回插入条数
	CreateJob(ctx context.Context, in CreateJobInput) (*types.JobItem, error)
	UpdateJob(ctx context.Context, id int64, in UpdateJobInput) (*types.JobItem, error)
	DeleteJob(ctx context.Context, id int64) error // 同时把绑定该职位的成员 position_id 置空
	ListJobs(ctx context.Context, companyID int64, keyword string, enabledOnly bool, page, pageSize int64) ([]types.JobItem, int64, error)
	BindPosition(ctx context.Context, companyID, userID int64, jobID *int64, extra *string) error
	ListBindings(ctx context.Context, companyID int64, page, pageSize int64) ([]types.BindingItem, int64, error)
}

// CreateJobInput 新建职位。
type CreateJobInput struct {
	CompanyID   int64
	Name        string
	Description *string
	SortOrder   int64
}

// UpdateJobInput 更新职位（nil 不动）。
type UpdateJobInput struct {
	Name        *string
	Description *string
	Enabled     *bool
	SortOrder   *int64
}

// CreateAssetInput 新建资产。
type CreateAssetInput struct {
	CompanyID int64
	Name      string
	Price     float64
	AssetType string
}

// UpdateAssetInput 更新资产（nil 不动）。
type UpdateAssetInput struct {
	Name      *string
	Price     *float64
	AssetType *string
	Status    *string
}

// CreateAssetFlowInput 新建流转记录。
type CreateAssetFlowInput struct {
	CompanyID  int64
	AssetID    int64
	FlowType   string
	Direction  string // in/out
	OperatorID int64
	RelatedTo  *string
	Note       *string
}

// CreateProxyAccessRequestInput 新建上网申请。
type CreateProxyAccessRequestInput struct {
	CompanyID   int64
	UserID      int64
	Note        *string
	ApproverIDs []int64 // 审批流快照
	CcIDs       []int64
}

// UpdateProxyAccessRequestInput 上网申请字段更新（nil 字段不动）。
type UpdateProxyAccessRequestInput struct {
	Status       *string
	AccountID    *int64
	ApproverID   *int64
	ApprovalNote *string
	ApprovedAt   *time.Time
	ExpiresAt    *time.Time
	ApproverIDs  []int64 // 非 nil 时覆盖快照（renew 用）
	CcIDs        []int64
}

// MemberProfileInput 是 upsertMemberProfile 的可更新字段（nil 字段沿用旧值）。
type MemberProfileInput struct {
	RealName     *string
	IDCard       *string
	BankCard     *string
	ResumeURL    *string
	PortfolioURL *string
}

// CreateReportTemplateInput 创建日报/周报模板。
type CreateReportTemplateInput struct {
	CompanyID   int64
	Name        string
	Description string
	Sections    []string // 分节标题数组，落库为 JSON 字符串
	CreatedBy   int64
}

// UpdateReportTemplateInput 更新模板（nil 字段不更新；Sections 非 nil 时整体覆盖）。
type UpdateReportTemplateInput struct {
	Name        *string
	Description *string
	Sections    []string
}

// CreateReportInput 创建日报/周报。
type CreateReportInput struct {
	CompanyID  int64
	UserID     int64
	Type       string // daily|weekly
	PeriodDate string // YYYY-MM-DD：日报=当天，周报=本周周一
	Title      string
	Content    string
	TemplateID int64   // 0=不关联模板
	Recipients []int64 // 接收人 user_id 数组（已由 handler 校验为本公司成员）；nil/空=不发送
}

// UpdateReportInput 更新报告（nil 字段不更新；已提交也可改内容，submitted 状态不变）。
type UpdateReportInput struct {
	Title      *string
	Content    *string
	TemplateID *int64
	Recipients []int64 // 非 nil 时整体替换接收人（传空切片=清空接收人）
}

// ListReportsInput 报告列表筛选 + 分页（periodStart/periodEnd 含两端）。
type ListReportsInput struct {
	CompanyID   int64
	Type        string // 空串=不限
	UserID      int64  // 0=不限
	PeriodStart string // 空串=不限
	PeriodEnd   string // 空串=不限
	Page        int64  // 调用方兜底默认 1
	PageSize    int64  // 调用方兜底默认 20、上限 100
}

// CreateAttendanceRequestInput 发起考勤申请。
// ApproverIDs/CcIDs 是创建时按公司审批流配置的快照（未配则为空数组）。
type CreateAttendanceRequestInput struct {
	CompanyID     int64
	UserID        int64
	Type          string     // make_up|leave|comp_time
	PunchTime     *string    // 补卡实际时刻（可跨天到次日凌晨）
	MakeupDate    string     // 补卡目标日期（被补那天）；空=无，落库为 NULL（非 ''，DATE 列拒空串）
	MakeupSlot    string     // 补卡槽位 work|off
	LeaveType     *string    // 请假类型
	CompType      *string    // 调休类型
	StartTime     *time.Time // 可空
	EndTime       *time.Time // 可空
	DurationHours *float64   // 可空
	Reason        string
	ApproverIDs   []int64 // 快照，空数组=无
	CcIDs         []int64 // 快照，空数组=无
}
