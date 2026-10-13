// Package db 的 MySqlStore 方法：任务流程与 TAPD（需求/缺陷/计划/文档/Wiki/仪表盘）。
package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"mochi-oa-server-go/internal/types"
)

// ==================== 项目 ====================

// CreateProject 创建项目。
func (s *MySqlStore) CreateProject(ctx context.Context, in CreateProjectInput) (*types.Project, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO projects (company_id, name, pm_id, created_at) VALUES (?,?,?,NOW(3))`, in.CompanyID, in.Name, in.PmID)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO project_members (project_id, user_id, role, joined_at) VALUES (?,?,'pm',NOW(3))`, id, in.PmID); err != nil {
		return nil, err
	}
	return s.getProjectByID(ctx, id)
}

func (s *MySqlStore) getProjectByID(ctx context.Context, id int64) (*types.Project, error) {
	var r types.ProjectRow
	err := s.db.QueryRowContext(ctx, `SELECT id, company_id, name, pm_id, created_at FROM projects WHERE id=?`, id).
		Scan(&r.ID, &r.CompanyID, &r.Name, &r.PmID, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
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
	rows, err := s.db.QueryContext(ctx, `SELECT id, company_id, name, pm_id, created_at FROM projects WHERE company_id=? ORDER BY id ASC`, companyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.Project{}
	for rows.Next() {
		var r types.ProjectRow
		if err := rows.Scan(&r.ID, &r.CompanyID, &r.Name, &r.PmID, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, types.Project{ID: r.ID, CompanyID: r.CompanyID, Name: r.Name, PmID: r.PmID, CreatedAt: toIso(r.CreatedAt)})
	}
	return out, rows.Err()
}

// DeleteProject 删除项目（级联成员）。
func (s *MySqlStore) DeleteProject(ctx context.Context, projectID int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM project_members WHERE project_id=?`, projectID); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM projects WHERE id=?`, projectID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{Msg: "project not found"}
	}
	return nil
}

// SetProjectRole 设置项目成员角色。
func (s *MySqlStore) SetProjectRole(ctx context.Context, projectID, userID int64, role string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO project_members (project_id, user_id, role, joined_at) VALUES (?,?,?,NOW(3)) ON DUPLICATE KEY UPDATE role=VALUES(role)`, projectID, userID, role)
	return err
}

// ListProjectMembers 列项目成员。
func (s *MySqlStore) ListProjectMembers(ctx context.Context, projectID int64) ([]types.ProjectMember, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT project_id, user_id, role, joined_at FROM project_members WHERE project_id=? ORDER BY joined_at ASC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.ProjectMember{}
	for rows.Next() {
		var r types.ProjectMemberRow
		if err := rows.Scan(&r.ProjectID, &r.UserID, &r.Role, &r.JoinedAt); err != nil {
			return nil, err
		}
		out = append(out, types.ProjectMember{ProjectID: r.ProjectID, UserID: r.UserID, Role: r.Role, JoinedAt: toIso(r.JoinedAt)})
	}
	return out, rows.Err()
}

// ==================== 任务 ====================

// CreateTask 创建任务。
func (s *MySqlStore) CreateTask(ctx context.Context, in CreateTaskInput) (*types.TaskItem, error) {
	start, _ := parseTimeFlex(in.StartTime)
	due, _ := parseTimeFlex(in.DueTime)
	images := in.Images
	if images == nil {
		images = []string{}
	}
	imgJSON, _ := json.Marshal(images)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO tasks (company_id, project_id, title, description, start_time, due_time, status, images, created_by, created_at, updated_at) VALUES (?,?,?,?,?,?,'created',?,?,?,NOW(3),NOW(3))`,
		in.CompanyID, in.ProjectID, in.Title, in.Description, start, due, string(imgJSON), in.CreatedBy)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.GetTask(ctx, id)
}

// ListTasks 列任务（可按项目过滤，due_time 升序）。
func (s *MySqlStore) ListTasks(ctx context.Context, companyID int64, projectID *int64) ([]types.TaskItem, error) {
	q := `SELECT id, company_id, project_id, title, description, start_time, due_time, completed_time, status, is_overdue, reminder_yellow, reminder_red, images, created_by, created_at, updated_at FROM tasks WHERE company_id=?`
	args := []any{companyID}
	if projectID != nil {
		q += ` AND project_id=?`
		args = append(args, *projectID)
	}
	q += ` ORDER BY due_time ASC, id ASC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.TaskItem{}
	for rows.Next() {
		var r types.TaskRow
		if err := rows.Scan(&r.ID, &r.CompanyID, &r.ProjectID, &r.Title, &r.Description, &r.StartTime, &r.DueTime, &r.CompletedTime, &r.Status, &r.IsOverdue, &r.ReminderYellow, &r.ReminderRed, &r.Images, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, rowToTask(&r))
	}
	return out, rows.Err()
}

// GetTask 按主键取任务。
func (s *MySqlStore) GetTask(ctx context.Context, taskID int64) (*types.TaskItem, error) {
	var r types.TaskRow
	err := s.db.QueryRowContext(ctx, `SELECT id, company_id, project_id, title, description, start_time, due_time, completed_time, status, is_overdue, reminder_yellow, reminder_red, images, created_by, created_at, updated_at FROM tasks WHERE id=?`, taskID).
		Scan(&r.ID, &r.CompanyID, &r.ProjectID, &r.Title, &r.Description, &r.StartTime, &r.DueTime, &r.CompletedTime, &r.Status, &r.IsOverdue, &r.ReminderYellow, &r.ReminderRed, &r.Images, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
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
	aRows, err := s.db.QueryContext(ctx, `SELECT id, task_id, user_id, content, status, completed_at FROM task_assignments WHERE task_id=? ORDER BY id ASC`, taskID)
	if err != nil {
		return nil, err
	}
	for aRows.Next() {
		var r types.TaskAssignmentRow
		if err := aRows.Scan(&r.ID, &r.TaskID, &r.UserID, &r.Content, &r.Status, &r.CompletedAt); err != nil {
			aRows.Close()
			return nil, err
		}
		a := rowToAssignment(&r)
		if u, err := s.GetUserByID(ctx, r.UserID); err == nil && u != nil {
			a.Username = &u.Username
			a.Nick = &u.Nick
			a.Avatar = &u.Avatar
		}
		assigns = append(assigns, a)
	}
	aRows.Close()

	comments := []types.TaskComment{}
	cRows, err := s.db.QueryContext(ctx, `SELECT id, task_id, user_id, content, images, created_at FROM task_comments WHERE task_id=? ORDER BY created_at ASC`, taskID)
	if err != nil {
		return nil, err
	}
	for cRows.Next() {
		var r types.TaskCommentRow
		if err := cRows.Scan(&r.ID, &r.TaskID, &r.UserID, &r.Content, &r.Images, &r.CreatedAt); err != nil {
			cRows.Close()
			return nil, err
		}
		c := rowToComment(&r)
		if u, err := s.GetUserByID(ctx, r.UserID); err == nil && u != nil {
			c.Username = &u.Username
			c.Nick = &u.Nick
			c.Avatar = &u.Avatar
		}
		comments = append(comments, c)
	}
	cRows.Close()

	issues := []types.TaskIssue{}
	iRows, err := s.db.QueryContext(ctx, `SELECT id, task_id, user_id, title, content, status, resolved_at, created_at FROM task_issues WHERE task_id=? ORDER BY created_at ASC`, taskID)
	if err != nil {
		return nil, err
	}
	for iRows.Next() {
		var r types.TaskIssueRow
		if err := iRows.Scan(&r.ID, &r.TaskID, &r.UserID, &r.Title, &r.Content, &r.Status, &r.ResolvedAt, &r.CreatedAt); err != nil {
			iRows.Close()
			return nil, err
		}
		is := rowToIssue(&r)
		if u, err := s.GetUserByID(ctx, r.UserID); err == nil && u != nil {
			is.Username = &u.Username
			is.Nick = &u.Nick
			is.Avatar = &u.Avatar
		}
		issues = append(issues, is)
	}
	iRows.Close()

	extensions := []types.TaskExtension{}
	eRows, err := s.db.QueryContext(ctx, `SELECT id, task_id, user_id, requested_due_time, reason, status, decided_by, decided_at, created_at FROM task_extensions WHERE task_id=? ORDER BY created_at ASC`, taskID)
	if err != nil {
		return nil, err
	}
	for eRows.Next() {
		var r types.TaskExtensionRow
		if err := eRows.Scan(&r.ID, &r.TaskID, &r.UserID, &r.RequestedDueTime, &r.Reason, &r.Status, &r.DecidedBy, &r.DecidedAt, &r.CreatedAt); err != nil {
			eRows.Close()
			return nil, err
		}
		ex := rowToExtension(&r)
		if u, err := s.GetUserByID(ctx, r.UserID); err == nil && u != nil {
			un := u.Username
			ex.Username = &un
			nk := u.Nick
			ex.Nick = &nk
		}
		extensions = append(extensions, ex)
	}
	eRows.Close()

	logs := []types.TaskStatusLog{}
	lRows, err := s.db.QueryContext(ctx, `SELECT id, task_id, user_id, from_status, to_status, note, created_at FROM task_status_logs WHERE task_id=? ORDER BY created_at ASC`, taskID)
	if err != nil {
		return nil, err
	}
	for lRows.Next() {
		var r types.TaskStatusLogRow
		if err := lRows.Scan(&r.ID, &r.TaskID, &r.UserID, &r.FromStatus, &r.ToStatus, &r.Note, &r.CreatedAt); err != nil {
			lRows.Close()
			return nil, err
		}
		lg := rowToLog(&r)
		if u, err := s.GetUserByID(ctx, r.UserID); err == nil && u != nil {
			un := u.Username
			lg.Username = &un
			nk := u.Nick
			lg.Nick = &nk
		}
		logs = append(logs, lg)
	}
	lRows.Close()

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
	_, err = s.db.ExecContext(ctx, `UPDATE tasks SET title=?, description=?, start_time=?, due_time=?, images=?, updated_at=NOW(3) WHERE id=?`, title, desc, start, due, string(imgJSON), in.ID)
	if err != nil {
		return nil, err
	}
	return s.GetTask(ctx, in.ID)
}

// DeleteTask 删除任务。
func (s *MySqlStore) DeleteTask(ctx context.Context, taskID int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM tasks WHERE id=?`, taskID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
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
	if in.Status == "completed" {
		_, err = s.db.ExecContext(ctx, `UPDATE tasks SET status=?, completed_time=NOW(3), is_overdue=false, updated_at=NOW(3) WHERE id=?`, in.Status, in.TaskID)
	} else {
		_, err = s.db.ExecContext(ctx, `UPDATE tasks SET status=?, is_overdue=false, updated_at=NOW(3) WHERE id=?`, in.Status, in.TaskID)
	}
	if err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO task_status_logs (task_id, user_id, from_status, to_status, note, created_at) VALUES (?,?,?,?,?,NOW(3))`, in.TaskID, in.UserID, base.Status, in.Status, in.Note); err != nil {
		return nil, err
	}
	return s.GetTask(ctx, in.TaskID)
}

// UpdateTaskReminder 更新提醒阈值。
func (s *MySqlStore) UpdateTaskReminder(ctx context.Context, in TaskReminderInput) (*types.TaskItem, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET reminder_yellow=?, reminder_red=?, updated_at=NOW(3) WHERE id=?`, in.ReminderYellow, in.ReminderRed, in.TaskID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, &NotFoundError{Msg: "task not found"}
	}
	return s.GetTask(ctx, in.TaskID)
}

// AddAssignment 新增分配。
func (s *MySqlStore) AddAssignment(ctx context.Context, in AddAssignmentInput) (*types.TaskAssignment, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO task_assignments (task_id, user_id, content, status, created_at) VALUES (?,?,?,'created',NOW(3))`, in.TaskID, in.UserID, in.Content)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.getAssignmentByID(ctx, id)
}

func (s *MySqlStore) getAssignmentByID(ctx context.Context, id int64) (*types.TaskAssignment, error) {
	var r types.TaskAssignmentRow
	err := s.db.QueryRowContext(ctx, `SELECT id, task_id, user_id, content, status, completed_at FROM task_assignments WHERE id=?`, id).
		Scan(&r.ID, &r.TaskID, &r.UserID, &r.Content, &r.Status, &r.CompletedAt)
	if errors.Is(err, sql.ErrNoRows) {
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
	res, err := s.db.ExecContext(ctx, `DELETE FROM task_assignments WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{Msg: "assignment not found"}
	}
	return nil
}

// SetAssignmentStatus 执行人提交状态。
// SetAssignmentStatus 执行人提交分配状态。
// 对齐 TS setAssignmentStatus（mysql-store.ts:1461）：先 findByPk 取行；
// 记录不存在 或 执行人非本人 → 返回 (nil, nil)（不抛错），handler 据此回 ack fail「仅本人可提交」。
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
	var res sql.Result
	if in.Status == "completed" {
		res, err = s.db.ExecContext(ctx, `UPDATE task_assignments SET status=?, completed_time=NOW(3) WHERE id=?`, in.Status, in.ID)
	} else {
		res, err = s.db.ExecContext(ctx, `UPDATE task_assignments SET status=?, completed_time=NULL WHERE id=?`, in.Status, in.ID)
	}
	if err != nil {
		return nil, err
	}
	_ = res
	return s.getAssignmentByID(ctx, in.ID)
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
	res, err := s.db.ExecContext(ctx, `INSERT INTO task_comments (task_id, user_id, content, images, created_at) VALUES (?,?,?,?,NOW(3))`, in.TaskID, in.UserID, in.Content, string(imgJSON))
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.getTaskCommentByID(ctx, id)
}

func (s *MySqlStore) getTaskCommentByID(ctx context.Context, id int64) (*types.TaskComment, error) {
	var r types.TaskCommentRow
	err := s.db.QueryRowContext(ctx, `SELECT id, task_id, user_id, content, images, created_at FROM task_comments WHERE id=?`, id).
		Scan(&r.ID, &r.TaskID, &r.UserID, &r.Content, &r.Images, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c := rowToComment(&r)
	return &c, nil
}

// AddTaskIssue 新增问题。
func (s *MySqlStore) AddTaskIssue(ctx context.Context, in AddTaskIssueInput) (*types.TaskIssue, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO task_issues (task_id, user_id, title, content, status, created_at) VALUES (?,?,?,?,'open',NOW(3))`, in.TaskID, in.UserID, in.Title, in.Content)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.getTaskIssueByID(ctx, id)
}

func (s *MySqlStore) getTaskIssueByID(ctx context.Context, id int64) (*types.TaskIssue, error) {
	var r types.TaskIssueRow
	err := s.db.QueryRowContext(ctx, `SELECT id, task_id, user_id, title, content, status, resolved_at, created_at FROM task_issues WHERE id=?`, id).
		Scan(&r.ID, &r.TaskID, &r.UserID, &r.Title, &r.Content, &r.Status, &r.ResolvedAt, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
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
	res, err := s.db.ExecContext(ctx, `UPDATE task_issues SET status='resolved', resolved_at=NOW(3) WHERE id=?`, issueID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, &NotFoundError{Msg: "issue not found"}
	}
	return s.getTaskIssueByID(ctx, issueID)
}

// RequestTaskExtension 申请延期。
func (s *MySqlStore) RequestTaskExtension(ctx context.Context, in RequestExtensionInput) (*types.TaskExtension, error) {
	reqDue, _ := parseTimeFlex(in.RequestedDueTime)
	res, err := s.db.ExecContext(ctx, `INSERT INTO task_extensions (task_id, user_id, requested_due_time, reason, status, created_at) VALUES (?,?,?,?, 'pending', NOW(3))`, in.TaskID, in.UserID, reqDue, in.Reason)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.getTaskExtensionByID(ctx, id)
}

func (s *MySqlStore) getTaskExtensionByID(ctx context.Context, id int64) (*types.TaskExtension, error) {
	var r types.TaskExtensionRow
	err := s.db.QueryRowContext(ctx, `SELECT id, task_id, user_id, requested_due_time, reason, status, decided_by, decided_at, created_at FROM task_extensions WHERE id=?`, id).
		Scan(&r.ID, &r.TaskID, &r.UserID, &r.RequestedDueTime, &r.Reason, &r.Status, &r.DecidedBy, &r.DecidedAt, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
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
	res, err := s.db.ExecContext(ctx, `UPDATE task_extensions SET status=?, decided_by=?, decided_at=NOW(3) WHERE id=?`, status, in.DecidedBy, in.ExtensionID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, &NotFoundError{Msg: "extension not found"}
	}
	ext, err := s.getTaskExtensionByID(ctx, in.ExtensionID)
	if err != nil {
		return nil, err
	}
	if in.Approved && ext != nil {
		newDue, _ := parseTimeFlex(ext.RequestedDueTime)
		if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET due_time=?, updated_at=NOW(3) WHERE id=?`, newDue, ext.TaskID); err != nil {
			return nil, err
		}
	}
	return s.getTaskExtensionByID(ctx, in.ExtensionID)
}
