// Package db 的 MySqlStore 方法：任务流程（项目/任务/分配/留言/问题/延期）。
//
// 本文件全部走 GORM（s.gorm），不再出现裸 SQL 字面量；表名一律用 Table("...") 显式指定。
//
// 关于「插入后要拿自增主键」：GORM 只有结构体 Create 会回填主键，而 map Create 的列清单
// 才与原 SQL 完全一致。凡两者都需要的场景，这里定义只含目标列的局部插入结构体
// （见 taskInsertRow 等），既能精确控制列清单，又能回填 ID。
package db

import (
	"context"
	"encoding/json"
	"time"

	"gorm.io/gorm/clause"

	"mochi-oa-server-go/internal/types"
)

// ==================== 项目 ====================

// CreateProject 创建项目。
func (s *MySqlStore) CreateProject(ctx context.Context, in CreateProjectInput) (*types.Project, error) {
	row := types.ProjectRow{CompanyID: in.CompanyID, Name: in.Name, PmID: in.PmID, CreatedAt: time.Now().UTC()}
	if err := s.gorm.WithContext(ctx).Table("projects").Create(&row).Error; err != nil {
		return nil, err
	}
	if err := s.gorm.WithContext(ctx).Table("project_members").Create(map[string]any{
		"project_id": row.ID, "user_id": in.PmID, "role": "pm", "joined_at": time.Now().UTC(),
	}).Error; err != nil {
		return nil, err
	}
	return s.getProjectByID(ctx, row.ID)
}

func (s *MySqlStore) getProjectByID(ctx context.Context, id int64) (*types.Project, error) {
	var r types.ProjectRow
	err := s.gorm.WithContext(ctx).Table("projects").Where("id = ?", id).Take(&r).Error
	if gormNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p := types.Project{ID: r.ID, CompanyID: r.CompanyID, Name: r.Name, PmID: r.PmID, CreatedAt: toIso(r.CreatedAt)}
	return &p, nil
}

// ListProjects 列公司项目。
func (s *MySqlStore) ListProjects(ctx context.Context, companyID int64) ([]types.Project, error) {
	rows := []types.ProjectRow{}
	if err := s.gorm.WithContext(ctx).Table("projects").
		Where("company_id = ?", companyID).Order("id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := []types.Project{}
	for i := range rows {
		r := &rows[i]
		out = append(out, types.Project{ID: r.ID, CompanyID: r.CompanyID, Name: r.Name, PmID: r.PmID, CreatedAt: toIso(r.CreatedAt)})
	}
	return out, nil
}

// DeleteProject 删除项目（级联成员）。
func (s *MySqlStore) DeleteProject(ctx context.Context, projectID int64) error {
	if err := s.gorm.WithContext(ctx).Table("project_members").
		Where("project_id = ?", projectID).Delete(nil).Error; err != nil {
		return err
	}
	res := s.gorm.WithContext(ctx).Table("projects").Where("id = ?", projectID).Delete(nil)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return &NotFoundError{Msg: "project not found"}
	}
	return nil
}

// SetProjectRole 设置项目成员角色。
func (s *MySqlStore) SetProjectRole(ctx context.Context, projectID, userID int64, role string) error {
	// 对应原 SQL 的 ON DUPLICATE KEY UPDATE role=VALUES(role)。
	return s.gorm.WithContext(ctx).Table("project_members").
		Clauses(clause.OnConflict{DoUpdates: clause.AssignmentColumns([]string{"role"})}).
		Create(map[string]any{
			"project_id": projectID, "user_id": userID, "role": role, "joined_at": time.Now().UTC(),
		}).Error
}

// ListProjectMembers 列项目成员。
func (s *MySqlStore) ListProjectMembers(ctx context.Context, projectID int64) ([]types.ProjectMember, error) {
	rows := []types.ProjectMemberRow{}
	if err := s.gorm.WithContext(ctx).Table("project_members").
		Where("project_id = ?", projectID).Order("joined_at ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := []types.ProjectMember{}
	for i := range rows {
		r := &rows[i]
		out = append(out, types.ProjectMember{ProjectID: r.ProjectID, UserID: r.UserID, Role: r.Role, JoinedAt: toIso(r.JoinedAt)})
	}
	return out, nil
}

// ==================== 任务 ====================

// taskInsertRow 只含原 INSERT 列清单的插入行（用于精确控制列 + 回填自增主键）。
// 原 SQL 刻意不写 completed_time / is_overdue / reminder_yellow / reminder_red，
// 让库侧默认值生效；此结构体同样不包含这些列，语义保持一致。
type taskInsertRow struct {
	ID          int64     `gorm:"column:id;primaryKey;autoIncrement"`
	CompanyID   int64     `gorm:"column:company_id"`
	ProjectID   *int64    `gorm:"column:project_id"`
	Title       string    `gorm:"column:title"`
	Description string    `gorm:"column:description"`
	StartTime   time.Time `gorm:"column:start_time"`
	DueTime     time.Time `gorm:"column:due_time"`
	Status      string    `gorm:"column:status"`
	Images      string    `gorm:"column:images"`
	CreatedBy   int64     `gorm:"column:created_by"`
	CreatedAt   time.Time `gorm:"column:created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
}

// CreateTask 创建任务。
func (s *MySqlStore) CreateTask(ctx context.Context, in CreateTaskInput) (*types.TaskItem, error) {
	start, _ := parseTimeFlex(in.StartTime)
	due, _ := parseTimeFlex(in.DueTime)
	images := in.Images
	if images == nil {
		images = []string{}
	}
	imgJSON, _ := json.Marshal(images)
	now := time.Now().UTC()
	row := taskInsertRow{
		CompanyID: in.CompanyID, ProjectID: in.ProjectID, Title: in.Title, Description: in.Description,
		StartTime: start, DueTime: due, Status: "created", Images: string(imgJSON),
		CreatedBy: in.CreatedBy, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.gorm.WithContext(ctx).Table("tasks").Create(&row).Error; err != nil {
		return nil, err
	}
	return s.GetTask(ctx, row.ID)
}

// ListTasks 列任务（可按项目过滤，due_time 升序）。
func (s *MySqlStore) ListTasks(ctx context.Context, companyID int64, projectID *int64) ([]types.TaskItem, error) {
	q := s.gorm.WithContext(ctx).Table("tasks").Where("company_id = ?", companyID)
	if projectID != nil {
		q = q.Where("project_id = ?", *projectID)
	}
	rows := []types.TaskRow{}
	if err := q.Order("due_time ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := []types.TaskItem{}
	for i := range rows {
		out = append(out, rowToTask(&rows[i]))
	}
	return out, nil
}

// GetTask 按主键取任务。
func (s *MySqlStore) GetTask(ctx context.Context, taskID int64) (*types.TaskItem, error) {
	var r types.TaskRow
	err := s.gorm.WithContext(ctx).Table("tasks").Where("id = ?", taskID).Take(&r).Error
	if gormNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t := rowToTask(&r)
	return &t, nil
}

// GetTaskDetail 任务详情（任务+分配+留言+问题+延期+日志）。
func (s *MySqlStore) GetTaskDetail(ctx context.Context, taskID int64) (*types.TaskDetail, error) {
	task, err := s.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, &NotFoundError{Msg: "task not found"}
	}

	assigns := []types.TaskAssignment{}
	aRows := []types.TaskAssignmentRow{}
	if err := s.gorm.WithContext(ctx).Table("task_assignments").
		Where("task_id = ?", taskID).Order("id ASC").Find(&aRows).Error; err != nil {
		return nil, err
	}
	for i := range aRows {
		r := &aRows[i]
		a := rowToAssignment(r)
		if u, err := s.GetUserByID(ctx, r.UserID); err == nil && u != nil {
			a.Username = &u.Username
			a.Nick = &u.Nick
			a.Avatar = &u.Avatar
		}
		assigns = append(assigns, a)
	}

	comments := []types.TaskComment{}
	cRows := []types.TaskCommentRow{}
	if err := s.gorm.WithContext(ctx).Table("task_comments").
		Where("task_id = ?", taskID).Order("created_at ASC").Find(&cRows).Error; err != nil {
		return nil, err
	}
	for i := range cRows {
		r := &cRows[i]
		c := rowToComment(r)
		if u, err := s.GetUserByID(ctx, r.UserID); err == nil && u != nil {
			c.Username = &u.Username
			c.Nick = &u.Nick
			c.Avatar = &u.Avatar
		}
		comments = append(comments, c)
	}

	issues := []types.TaskIssue{}
	iRows := []types.TaskIssueRow{}
	if err := s.gorm.WithContext(ctx).Table("task_issues").
		Where("task_id = ?", taskID).Order("created_at ASC").Find(&iRows).Error; err != nil {
		return nil, err
	}
	for i := range iRows {
		r := &iRows[i]
		is := rowToIssue(r)
		if u, err := s.GetUserByID(ctx, r.UserID); err == nil && u != nil {
			is.Username = &u.Username
			is.Nick = &u.Nick
			is.Avatar = &u.Avatar
		}
		issues = append(issues, is)
	}

	extensions := []types.TaskExtension{}
	eRows := []types.TaskExtensionRow{}
	if err := s.gorm.WithContext(ctx).Table("task_extensions").
		Where("task_id = ?", taskID).Order("created_at ASC").Find(&eRows).Error; err != nil {
		return nil, err
	}
	for i := range eRows {
		r := &eRows[i]
		ex := rowToExtension(r)
		if u, err := s.GetUserByID(ctx, r.UserID); err == nil && u != nil {
			un := u.Username
			ex.Username = &un
			nk := u.Nick
			ex.Nick = &nk
		}
		extensions = append(extensions, ex)
	}

	logs := []types.TaskStatusLog{}
	lRows := []types.TaskStatusLogRow{}
	if err := s.gorm.WithContext(ctx).Table("task_status_logs").
		Where("task_id = ?", taskID).Order("created_at ASC").Find(&lRows).Error; err != nil {
		return nil, err
	}
	for i := range lRows {
		r := &lRows[i]
		lg := rowToLog(r)
		if u, err := s.GetUserByID(ctx, r.UserID); err == nil && u != nil {
			un := u.Username
			lg.Username = &un
			nk := u.Nick
			lg.Nick = &nk
		}
		logs = append(logs, lg)
	}

	return &types.TaskDetail{Task: *task, Assignments: assigns, Comments: comments, Issues: issues, Extensions: extensions, Logs: logs}, nil
}

// UpdateTask 更新任务。
func (s *MySqlStore) UpdateTask(ctx context.Context, in UpdateTaskInput) (*types.TaskItem, error) {
	base, err := s.GetTask(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	if base == nil {
		return nil, &NotFoundError{Msg: "task not found"}
	}
	title := base.Title
	if in.Title != nil {
		title = *in.Title
	}
	desc := base.Description
	if in.Description != nil {
		desc = *in.Description
	}
	startStr := base.StartTime
	if in.StartTime != nil {
		startStr = *in.StartTime
	}
	dueStr := base.DueTime
	if in.DueTime != nil {
		dueStr = *in.DueTime
	}
	images := in.Images
	if images == nil {
		images = []string{}
	}
	imgJSON, _ := json.Marshal(images)
	start, _ := parseTimeFlex(startStr)
	due, _ := parseTimeFlex(dueStr)
	err = s.gorm.WithContext(ctx).Table("tasks").Where("id = ?", in.ID).Updates(map[string]any{
		"title":       title,
		"description": desc,
		"start_time":  start,
		"due_time":    due,
		"images":      string(imgJSON),
		"updated_at":  time.Now().UTC(),
	}).Error
	if err != nil {
		return nil, err
	}
	return s.GetTask(ctx, in.ID)
}

// DeleteTask 删除任务。
func (s *MySqlStore) DeleteTask(ctx context.Context, taskID int64) error {
	res := s.gorm.WithContext(ctx).Table("tasks").Where("id = ?", taskID).Delete(nil)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return &NotFoundError{Msg: "task not found"}
	}
	return nil
}

// SetTaskStatus 变更状态并写日志。
func (s *MySqlStore) SetTaskStatus(ctx context.Context, in SetTaskStatusInput) (*types.TaskItem, error) {
	base, err := s.GetTask(ctx, in.TaskID)
	if err != nil {
		return nil, err
	}
	if base == nil {
		return nil, &NotFoundError{Msg: "task not found"}
	}
	now := time.Now().UTC()
	updates := map[string]any{"status": in.Status, "is_overdue": false, "updated_at": now}
	if in.Status == "completed" {
		updates["completed_time"] = now
	}
	if err := s.gorm.WithContext(ctx).Table("tasks").
		Where("id = ?", in.TaskID).Updates(updates).Error; err != nil {
		return nil, err
	}
	if err := s.gorm.WithContext(ctx).Table("task_status_logs").Create(map[string]any{
		"task_id": in.TaskID, "user_id": in.UserID, "from_status": base.Status,
		"to_status": in.Status, "note": in.Note, "created_at": time.Now().UTC(),
	}).Error; err != nil {
		return nil, err
	}
	return s.GetTask(ctx, in.TaskID)
}

// UpdateTaskReminder 更新提醒阈值。
func (s *MySqlStore) UpdateTaskReminder(ctx context.Context, in TaskReminderInput) (*types.TaskItem, error) {
	res := s.gorm.WithContext(ctx).Table("tasks").Where("id = ?", in.TaskID).Updates(map[string]any{
		"reminder_yellow": in.ReminderYellow,
		"reminder_red":    in.ReminderRed,
		"updated_at":      time.Now().UTC(),
	})
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, &NotFoundError{Msg: "task not found"}
	}
	return s.GetTask(ctx, in.TaskID)
}

// assignmentInsertRow 只含原 INSERT 列清单的插入行。
type assignmentInsertRow struct {
	ID        int64     `gorm:"column:id;primaryKey;autoIncrement"`
	TaskID    int64     `gorm:"column:task_id"`
	UserID    int64     `gorm:"column:user_id"`
	Content   string    `gorm:"column:content"`
	Status    string    `gorm:"column:status"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

// AddAssignment 新增分配。
func (s *MySqlStore) AddAssignment(ctx context.Context, in AddAssignmentInput) (*types.TaskAssignment, error) {
	row := assignmentInsertRow{
		TaskID: in.TaskID, UserID: in.UserID, Content: in.Content,
		Status: "created", CreatedAt: time.Now().UTC(),
	}
	if err := s.gorm.WithContext(ctx).Table("task_assignments").Create(&row).Error; err != nil {
		return nil, err
	}
	return s.getAssignmentByID(ctx, row.ID)
}

func (s *MySqlStore) getAssignmentByID(ctx context.Context, id int64) (*types.TaskAssignment, error) {
	var r types.TaskAssignmentRow
	err := s.gorm.WithContext(ctx).Table("task_assignments").Where("id = ?", id).Take(&r).Error
	if gormNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a := rowToAssignment(&r)
	return &a, nil
}

// RemoveAssignment 删除分配。
func (s *MySqlStore) RemoveAssignment(ctx context.Context, id int64) error {
	res := s.gorm.WithContext(ctx).Table("task_assignments").Where("id = ?", id).Delete(nil)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return &NotFoundError{Msg: "assignment not found"}
	}
	return nil
}

// SetAssignmentStatus 执行人提交状态。
// SetAssignmentStatus 执行人提交分配状态。
// 对齐 TS setAssignmentStatus（mysql-store.ts:1461）：先 findByPk 取行；
// 记录不存在 或 执行人非本人 → 返回 (nil, nil)（不抛错），handler 据此回 ack fail「仅本人可提交」。
//
// 注意：原实现写的是 completed_time 列，而 task_assignments 表里实际叫 completed_at
// （见 mochioa.sql）。这是改造前就存在的缺陷，本次迁移按「行为不变」原则原样保留。
func (s *MySqlStore) SetAssignmentStatus(ctx context.Context, in SetAssignmentStatusInput) (*types.TaskAssignment, error) {
	// 先取行以区分「不存在」与「非本人」——盲 UPDATE 无法识别这两种情况。
	a, err := s.getAssignmentByID(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, nil // 记录不存在 → null
	}
	if a.UserID != in.UserID {
		return nil, nil // 非本人 → null（仅本人可提交）
	}
	var updates map[string]any
	if in.Status == "completed" {
		updates = map[string]any{"status": in.Status, "completed_time": time.Now().UTC()}
	} else {
		updates = map[string]any{"status": in.Status, "completed_time": nil}
	}
	if err := s.gorm.WithContext(ctx).Table("task_assignments").
		Where("id = ?", in.ID).Updates(updates).Error; err != nil {
		return nil, err
	}
	return s.getAssignmentByID(ctx, in.ID)
}

// commentInsertRow 只含原 INSERT 列清单的插入行。
type commentInsertRow struct {
	ID        int64     `gorm:"column:id;primaryKey;autoIncrement"`
	TaskID    int64     `gorm:"column:task_id"`
	UserID    int64     `gorm:"column:user_id"`
	Content   string    `gorm:"column:content"`
	Images    *string   `gorm:"column:images"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

// AddTaskComment 新增留言。
func (s *MySqlStore) AddTaskComment(ctx context.Context, in AddTaskCommentInput) (*types.TaskComment, error) {
	atts := in.Attachments
	if atts == nil {
		atts = []types.TaskAttachment{}
	}
	type attOut struct {
		Kind string `json:"kind"`
		URL  string `json:"url"`
		Name string `json:"name"`
	}
	out := make([]attOut, 0, len(atts))
	for _, a := range atts {
		name := a.Name
		if len(name) > 255 {
			name = name[:255]
		}
		out = append(out, attOut{Kind: a.Kind, URL: a.URL, Name: name})
	}
	imgJSON, _ := json.Marshal(out)
	imgStr := string(imgJSON)
	row := commentInsertRow{
		TaskID: in.TaskID, UserID: in.UserID, Content: in.Content,
		Images: &imgStr, CreatedAt: time.Now().UTC(),
	}
	if err := s.gorm.WithContext(ctx).Table("task_comments").Create(&row).Error; err != nil {
		return nil, err
	}
	return s.getTaskCommentByID(ctx, row.ID)
}

func (s *MySqlStore) getTaskCommentByID(ctx context.Context, id int64) (*types.TaskComment, error) {
	var r types.TaskCommentRow
	err := s.gorm.WithContext(ctx).Table("task_comments").Where("id = ?", id).Take(&r).Error
	if gormNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c := rowToComment(&r)
	return &c, nil
}

// issueInsertRow 只含原 INSERT 列清单的插入行。
type issueInsertRow struct {
	ID        int64     `gorm:"column:id;primaryKey;autoIncrement"`
	TaskID    int64     `gorm:"column:task_id"`
	UserID    int64     `gorm:"column:user_id"`
	Title     string    `gorm:"column:title"`
	Content   string    `gorm:"column:content"`
	Status    string    `gorm:"column:status"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

// AddTaskIssue 新增问题。
func (s *MySqlStore) AddTaskIssue(ctx context.Context, in AddTaskIssueInput) (*types.TaskIssue, error) {
	row := issueInsertRow{
		TaskID: in.TaskID, UserID: in.UserID, Title: in.Title, Content: in.Content,
		Status: "open", CreatedAt: time.Now().UTC(),
	}
	if err := s.gorm.WithContext(ctx).Table("task_issues").Create(&row).Error; err != nil {
		return nil, err
	}
	return s.getTaskIssueByID(ctx, row.ID)
}

func (s *MySqlStore) getTaskIssueByID(ctx context.Context, id int64) (*types.TaskIssue, error) {
	var r types.TaskIssueRow
	err := s.gorm.WithContext(ctx).Table("task_issues").Where("id = ?", id).Take(&r).Error
	if gormNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	is := rowToIssue(&r)
	return &is, nil
}

// ResolveTaskIssue 标记问题为已解决。
func (s *MySqlStore) ResolveTaskIssue(ctx context.Context, issueID int64) (*types.TaskIssue, error) {
	res := s.gorm.WithContext(ctx).Table("task_issues").Where("id = ?", issueID).Updates(map[string]any{
		"status": "resolved", "resolved_at": time.Now().UTC(),
	})
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, &NotFoundError{Msg: "issue not found"}
	}
	return s.getTaskIssueByID(ctx, issueID)
}

// extensionInsertRow 只含原 INSERT 列清单的插入行。
type extensionInsertRow struct {
	ID               int64     `gorm:"column:id;primaryKey;autoIncrement"`
	TaskID           int64     `gorm:"column:task_id"`
	UserID           int64     `gorm:"column:user_id"`
	RequestedDueTime time.Time `gorm:"column:requested_due_time"`
	Reason           string    `gorm:"column:reason"`
	Status           string    `gorm:"column:status"`
	CreatedAt        time.Time `gorm:"column:created_at"`
}

// RequestTaskExtension 申请延期。
func (s *MySqlStore) RequestTaskExtension(ctx context.Context, in RequestExtensionInput) (*types.TaskExtension, error) {
	reqDue, _ := parseTimeFlex(in.RequestedDueTime)
	row := extensionInsertRow{
		TaskID: in.TaskID, UserID: in.UserID, RequestedDueTime: reqDue,
		Reason: in.Reason, Status: "pending", CreatedAt: time.Now().UTC(),
	}
	if err := s.gorm.WithContext(ctx).Table("task_extensions").Create(&row).Error; err != nil {
		return nil, err
	}
	return s.getTaskExtensionByID(ctx, row.ID)
}

func (s *MySqlStore) getTaskExtensionByID(ctx context.Context, id int64) (*types.TaskExtension, error) {
	var r types.TaskExtensionRow
	err := s.gorm.WithContext(ctx).Table("task_extensions").Where("id = ?", id).Take(&r).Error
	if gormNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ex := rowToExtension(&r)
	return &ex, nil
}

// DecideTaskExtension 审批延期。
func (s *MySqlStore) DecideTaskExtension(ctx context.Context, in DecideExtensionInput) (*types.TaskExtension, error) {
	status := "rejected"
	if in.Approved {
		status = "approved"
	}
	res := s.gorm.WithContext(ctx).Table("task_extensions").Where("id = ?", in.ExtensionID).Updates(map[string]any{
		"status": status, "decided_by": in.DecidedBy, "decided_at": time.Now().UTC(),
	})
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, &NotFoundError{Msg: "extension not found"}
	}
	ext, err := s.getTaskExtensionByID(ctx, in.ExtensionID)
	if err != nil {
		return nil, err
	}
	if in.Approved && ext != nil {
		newDue, _ := parseTimeFlex(ext.RequestedDueTime)
		if err := s.gorm.WithContext(ctx).Table("tasks").Where("id = ?", ext.TaskID).Updates(map[string]any{
			"due_time": newDue, "updated_at": time.Now().UTC(),
		}).Error; err != nil {
			return nil, err
		}
	}
	return s.getTaskExtensionByID(ctx, in.ExtensionID)
}
