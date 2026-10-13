// Package db 的 MySqlStore 方法：TAPD（需求/缺陷/计划/文档/Wiki/仪表盘/成员跟踪）。
package db

import (
	"context"
	"database/sql"
	"errors"

	"mochi-oa-server-go/internal/types"
)

// ==================== 需求 ====================

// CreateRequirement 创建需求。
func (s *MySqlStore) CreateRequirement(ctx context.Context, in CreateRequirementInput) (*types.Requirement, error) {
	start, _ := parseTimeFlex(in.StartTime)
	due, _ := parseTimeFlex(in.DueTime)
	category := in.Category
	if category == "" {
		category = "uncategorized"
	}
	priority := in.Priority
	if priority == "" {
		priority = "middle"
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO requirements (company_id, project_id, code, title, description, category, priority, status, handler_id, creator_id, start_time, due_time, created_at, updated_at) VALUES (?,?,?,?,?,?,?,'planning',?,?,?,?,NOW(3),NOW(3))`,
		in.CompanyID, in.ProjectID, "", in.Title, in.Description, category, priority, in.HandlerID, in.CreatorID, start, due)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.GetRequirement(ctx, id)
}

// ListRequirements 列需求（可按项目，updated_at 倒序）。
func (s *MySqlStore) ListRequirements(ctx context.Context, companyID int64, projectID *int64) ([]types.Requirement, error) {
	q := `SELECT id, company_id, project_id, code, title, description, category, priority, status, handler_id, creator_id, start_time, due_time, completed_time, created_at, updated_at FROM requirements WHERE company_id=?`
	args := []any{companyID}
	if projectID != nil {
		q += ` AND project_id=?`
		args = append(args, *projectID)
	}
	q += ` ORDER BY updated_at DESC, id DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.Requirement{}
	for rows.Next() {
		var r types.RequirementRow
		if err := rows.Scan(&r.ID, &r.CompanyID, &r.ProjectID, &r.Code, &r.Title, &r.Description, &r.Category, &r.Priority, &r.Status, &r.HandlerID, &r.CreatorID, &r.StartTime, &r.DueTime, &r.CompletedTime, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		req := rowToRequirement(&r)
		linked, _ := s.linkedTaskIDs(ctx, r.ID)
		req.LinkedTaskIDs = linked
		s.fillUserNames(ctx, &req.HandlerName, &req.CreatorName, r.HandlerID, &r.CreatorID)
		out = append(out, req)
	}
	return out, rows.Err()
}

// GetRequirement 按主键取。
func (s *MySqlStore) GetRequirement(ctx context.Context, requirementID int64) (*types.Requirement, error) {
	var r types.RequirementRow
	err := s.db.QueryRowContext(ctx, `SELECT id, company_id, project_id, code, title, description, category, priority, status, handler_id, creator_id, start_time, due_time, completed_time, created_at, updated_at FROM requirements WHERE id=?`, requirementID).
		Scan(&r.ID, &r.CompanyID, &r.ProjectID, &r.Code, &r.Title, &r.Description, &r.Category, &r.Priority, &r.Status, &r.HandlerID, &r.CreatorID, &r.StartTime, &r.DueTime, &r.CompletedTime, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	req := rowToRequirement(&r)
	linked, _ := s.linkedTaskIDs(ctx, r.ID)
	req.LinkedTaskIDs = linked
	s.fillUserNames(ctx, &req.HandlerName, &req.CreatorName, r.HandlerID, &r.CreatorID)
	return &req, nil
}

// UpdateRequirement 更新需求。
func (s *MySqlStore) UpdateRequirement(ctx context.Context, in UpdateRequirementInput) (*types.Requirement, error) {
	base, err := s.GetRequirement(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	if base == nil {
		return nil, &NotFoundError{Msg: "requirement not found"}
	}
	title := base.Title
	if in.Title != nil {
		title = *in.Title
	}
	desc := base.Description
	if in.Description != nil {
		desc = *in.Description
	}
	category := base.Category
	if in.Category != nil {
		category = *in.Category
	}
	priority := base.Priority
	if in.Priority != nil {
		priority = *in.Priority
	}
	handlerID := base.HandlerID
	if in.HandlerID != nil {
		handlerID = in.HandlerID
	}
	startStr := base.StartTime
	if in.StartTime != nil {
		startStr = *in.StartTime
	}
	dueStr := base.DueTime
	if in.DueTime != nil {
		dueStr = *in.DueTime
	}
	start, _ := parseTimeFlex(startStr)
	due, _ := parseTimeFlex(dueStr)
	_, err = s.db.ExecContext(ctx, `UPDATE requirements SET title=?, description=?, category=?, priority=?, handler_id=?, start_time=?, due_time=?, updated_at=NOW(3) WHERE id=?`,
		title, desc, category, priority, handlerID, start, due, in.ID)
	if err != nil {
		return nil, err
	}
	return s.GetRequirement(ctx, in.ID)
}

// SetRequirementStatus 变更需求状态（completed 写 completed_time）。
func (s *MySqlStore) SetRequirementStatus(ctx context.Context, in SetRequirementStatusInput) (*types.Requirement, error) {
	if in.Status == "completed" {
		_, err := s.db.ExecContext(ctx, `UPDATE requirements SET status=?, completed_time=NOW(3), updated_at=NOW(3) WHERE id=?`, in.Status, in.RequirementID)
		if err != nil {
			return nil, err
		}
	} else {
		_, err := s.db.ExecContext(ctx, `UPDATE requirements SET status=?, updated_at=NOW(3) WHERE id=?`, in.Status, in.RequirementID)
		if err != nil {
			return nil, err
		}
	}
	return s.GetRequirement(ctx, in.RequirementID)
}

// DeleteRequirement 删除需求（含关联）。
func (s *MySqlStore) DeleteRequirement(ctx context.Context, requirementID int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM requirement_links WHERE requirement_id=?`, requirementID); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM requirements WHERE id=?`, requirementID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{Msg: "requirement not found"}
	}
	return nil
}

// linkedTaskIDs 取需求关联的任务 id 列表。
func (s *MySqlStore) linkedTaskIDs(ctx context.Context, requirementID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT task_id FROM requirement_links WHERE requirement_id=? ORDER BY id ASC`, requirementID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// LinkRequirementTasks 覆盖式写需求-任务关联。
func (s *MySqlStore) LinkRequirementTasks(ctx context.Context, in LinkRequirementTasksInput) ([]int64, error) {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM requirement_links WHERE requirement_id=?`, in.RequirementID); err != nil {
		return nil, err
	}
	linked := []int64{}
	for _, tid := range in.TaskIDs {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO requirement_links (company_id, requirement_id, task_id, user_id, created_at) VALUES (?,?,?,?,NOW(3))`, in.CompanyID, in.RequirementID, tid, in.UserID); err != nil {
			return nil, err
		}
		linked = append(linked, tid)
	}
	return linked, nil
}

// fillUserNames 填 handler/creator 的昵称（找不到留空）。
func (s *MySqlStore) fillUserNames(ctx context.Context, handlerName, creatorName **string, handlerID, creatorID *int64) {
	if handlerID != nil {
		if u, err := s.GetUserByID(ctx, *handlerID); err == nil && u != nil {
			n := u.Nick
			if n == "" {
				n = u.Username
			}
			*handlerName = &n
		}
	}
	if creatorID != nil {
		if u, err := s.GetUserByID(ctx, *creatorID); err == nil && u != nil {
			n := u.Nick
			if n == "" {
				n = u.Username
			}
			*creatorName = &n
		}
	}
}

// ==================== 缺陷 ====================

// CreateBug 创建缺陷。
func (s *MySqlStore) CreateBug(ctx context.Context, in CreateBugInput) (*types.Bug, error) {
	severity := in.Severity
	if severity == "" {
		severity = "normal"
	}
	priority := in.Priority
	if priority == "" {
		priority = "middle"
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO bugs (company_id, project_id, requirement_id, code, title, description, severity, priority, status, handler_id, creator_id, found_version, created_at, updated_at) VALUES (?,?,?,?,?,?,?,'pending',?,?,?,?,NOW(3),NOW(3))`,
		in.CompanyID, in.ProjectID, in.RequirementID, "", in.Title, in.Description, severity, priority, in.HandlerID, in.CreatorID, in.FoundVersion)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.GetBug(ctx, id)
}

// ListBugs 列缺陷。
func (s *MySqlStore) ListBugs(ctx context.Context, companyID int64, projectID *int64) ([]types.Bug, error) {
	q := `SELECT id, company_id, project_id, requirement_id, code, title, description, severity, priority, status, handler_id, creator_id, found_version, created_at, updated_at FROM bugs WHERE company_id=?`
	args := []any{companyID}
	if projectID != nil {
		q += ` AND project_id=?`
		args = append(args, *projectID)
	}
	q += ` ORDER BY updated_at DESC, id DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.Bug{}
	for rows.Next() {
		var r types.BugRow
		if err := rows.Scan(&r.ID, &r.CompanyID, &r.ProjectID, &r.RequirementID, &r.Code, &r.Title, &r.Description, &r.Severity, &r.Priority, &r.Status, &r.HandlerID, &r.CreatorID, &r.FoundVersion, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		b := rowToBug(&r)
		s.fillUserNames(ctx, &b.HandlerName, &b.CreatorName, r.HandlerID, &r.CreatorID)
		out = append(out, b)
	}
	return out, rows.Err()
}

// GetBug 按主键取缺陷。
func (s *MySqlStore) GetBug(ctx context.Context, bugID int64) (*types.Bug, error) {
	var r types.BugRow
	err := s.db.QueryRowContext(ctx, `SELECT id, company_id, project_id, requirement_id, code, title, description, severity, priority, status, handler_id, creator_id, found_version, created_at, updated_at FROM bugs WHERE id=?`, bugID).
		Scan(&r.ID, &r.CompanyID, &r.ProjectID, &r.RequirementID, &r.Code, &r.Title, &r.Description, &r.Severity, &r.Priority, &r.Status, &r.HandlerID, &r.CreatorID, &r.FoundVersion, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b := rowToBug(&r)
	s.fillUserNames(ctx, &b.HandlerName, &b.CreatorName, r.HandlerID, &r.CreatorID)
	return &b, nil
}

// UpdateBug 更新缺陷。
func (s *MySqlStore) UpdateBug(ctx context.Context, in UpdateBugInput) (*types.Bug, error) {
	base, err := s.GetBug(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	if base == nil {
		return nil, &NotFoundError{Msg: "bug not found"}
	}
	title := base.Title
	if in.Title != nil {
		title = *in.Title
	}
	desc := base.Description
	if in.Description != nil {
		desc = *in.Description
	}
	severity := base.Severity
	if in.Severity != nil {
		severity = *in.Severity
	}
	priority := base.Priority
	if in.Priority != nil {
		priority = *in.Priority
	}
	handlerID := base.HandlerID
	if in.HandlerID != nil {
		handlerID = in.HandlerID
	}
	version := base.FoundVersion
	if in.FoundVersion != nil {
		version = *in.FoundVersion
	}
	_, err = s.db.ExecContext(ctx, `UPDATE bugs SET title=?, description=?, severity=?, priority=?, handler_id=?, found_version=?, updated_at=NOW(3) WHERE id=?`, title, desc, severity, priority, handlerID, version, in.ID)
	if err != nil {
		return nil, err
	}
	return s.GetBug(ctx, in.ID)
}

// SetBugStatus 变更缺陷状态。
func (s *MySqlStore) SetBugStatus(ctx context.Context, in SetBugStatusInput) (*types.Bug, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE bugs SET status=?, updated_at=NOW(3) WHERE id=?`, in.Status, in.BugID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, &NotFoundError{Msg: "bug not found"}
	}
	return s.GetBug(ctx, in.BugID)
}

// DeleteBug 删除缺陷。
func (s *MySqlStore) DeleteBug(ctx context.Context, bugID int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM bugs WHERE id=?`, bugID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{Msg: "bug not found"}
	}
	return nil
}

// ==================== 计划 ====================

// CreatePlan 创建计划。
func (s *MySqlStore) CreatePlan(ctx context.Context, in CreatePlanInput) (*types.Plan, error) {
	start, _ := parseTimeFlex(in.StartTime)
	due, _ := parseTimeFlex(in.DueTime)
	res, err := s.db.ExecContext(ctx, `INSERT INTO plans (company_id, project_id, name, description, start_time, due_time, status, creator_id, created_at, updated_at) VALUES (?,?,?,?,?,?,'not_started',?,NOW(3),NOW(3))`,
		in.CompanyID, in.ProjectID, in.Name, in.Description, start, due, in.CreatorID)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.GetPlanByID(ctx, id)
}

func (s *MySqlStore) GetPlanByID(ctx context.Context, id int64) (*types.Plan, error) {
	var r types.PlanRow
	err := s.db.QueryRowContext(ctx, `SELECT id, company_id, project_id, name, description, start_time, due_time, status, creator_id, created_at, updated_at FROM plans WHERE id=?`, id).
		Scan(&r.ID, &r.CompanyID, &r.ProjectID, &r.Name, &r.Description, &r.StartTime, &r.DueTime, &r.Status, &r.CreatorID, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p := rowToPlan(&r)
	return &p, nil
}

// ListPlans 列计划。
func (s *MySqlStore) ListPlans(ctx context.Context, companyID int64, projectID *int64) ([]types.Plan, error) {
	q := `SELECT id, company_id, project_id, name, description, start_time, due_time, status, creator_id, created_at, updated_at FROM plans WHERE company_id=?`
	args := []any{companyID}
	if projectID != nil {
		q += ` AND project_id=?`
		args = append(args, *projectID)
	}
	q += ` ORDER BY start_time ASC, id ASC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.Plan{}
	for rows.Next() {
		var r types.PlanRow
		if err := rows.Scan(&r.ID, &r.CompanyID, &r.ProjectID, &r.Name, &r.Description, &r.StartTime, &r.DueTime, &r.Status, &r.CreatorID, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, rowToPlan(&r))
	}
	return out, rows.Err()
}

// UpdatePlan 更新计划。
func (s *MySqlStore) UpdatePlan(ctx context.Context, in UpdatePlanInput) (*types.Plan, error) {
	base, err := s.GetPlanByID(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	if base == nil {
		return nil, &NotFoundError{Msg: "plan not found"}
	}
	name := base.Name
	if in.Name != nil {
		name = *in.Name
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
	status := base.Status
	if in.Status != nil {
		status = *in.Status
	}
	start, _ := parseTimeFlex(startStr)
	due, _ := parseTimeFlex(dueStr)
	_, err = s.db.ExecContext(ctx, `UPDATE plans SET name=?, description=?, start_time=?, due_time=?, status=?, updated_at=NOW(3) WHERE id=?`, name, desc, start, due, status, in.ID)
	if err != nil {
		return nil, err
	}
	return s.GetPlanByID(ctx, in.ID)
}

// DeletePlan 删除计划。
func (s *MySqlStore) DeletePlan(ctx context.Context, planID int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM plans WHERE id=?`, planID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{Msg: "plan not found"}
	}
	return nil
}

// ==================== 文档 ====================

// CreateDocument 创建文档。
func (s *MySqlStore) CreateDocument(ctx context.Context, in CreateDocumentInput) (*types.ProjectDocument, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO documents (company_id, project_id, title, content, creator_id, created_at, updated_at) VALUES (?,?,?,?,?,NOW(3),NOW(3))`,
		in.CompanyID, in.ProjectID, in.Title, in.Content, in.CreatorID)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.GetDocumentByID(ctx, id)
}

func (s *MySqlStore) GetDocumentByID(ctx context.Context, id int64) (*types.ProjectDocument, error) {
	var r types.DocumentRow
	err := s.db.QueryRowContext(ctx, `SELECT id, company_id, project_id, title, content, creator_id, created_at, updated_at FROM documents WHERE id=?`, id).
		Scan(&r.ID, &r.CompanyID, &r.ProjectID, &r.Title, &r.Content, &r.CreatorID, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d := rowToDocument(&r)
	return &d, nil
}

// ListDocuments 列文档。
func (s *MySqlStore) ListDocuments(ctx context.Context, companyID int64, projectID *int64) ([]types.ProjectDocument, error) {
	q := `SELECT id, company_id, project_id, title, content, creator_id, created_at, updated_at FROM documents WHERE company_id=?`
	args := []any{companyID}
	if projectID != nil {
		q += ` AND project_id=?`
		args = append(args, *projectID)
	}
	q += ` ORDER BY updated_at DESC, id DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.ProjectDocument{}
	for rows.Next() {
		var r types.DocumentRow
		if err := rows.Scan(&r.ID, &r.CompanyID, &r.ProjectID, &r.Title, &r.Content, &r.CreatorID, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, rowToDocument(&r))
	}
	return out, rows.Err()
}

// UpdateDocument 更新文档。
func (s *MySqlStore) UpdateDocument(ctx context.Context, in UpdateDocumentInput) (*types.ProjectDocument, error) {
	base, err := s.GetDocumentByID(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	if base == nil {
		return nil, &NotFoundError{Msg: "document not found"}
	}
	title := base.Title
	if in.Title != nil {
		title = *in.Title
	}
	content := base.Content
	if in.Content != nil {
		content = *in.Content
	}
	_, err = s.db.ExecContext(ctx, `UPDATE documents SET title=?, content=?, updated_at=NOW(3) WHERE id=?`, title, content, in.ID)
	if err != nil {
		return nil, err
	}
	return s.GetDocumentByID(ctx, in.ID)
}

// DeleteDocument 删除文档。
func (s *MySqlStore) DeleteDocument(ctx context.Context, documentID int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM documents WHERE id=?`, documentID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{Msg: "document not found"}
	}
	return nil
}

// ==================== Wiki ====================

// CreateWikiPage 创建 Wiki 页。
func (s *MySqlStore) CreateWikiPage(ctx context.Context, in CreateWikiInput) (*types.WikiPage, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO wiki_pages (company_id, project_id, title, content, creator_id, created_at, updated_at) VALUES (?,?,?,?,?,NOW(3),NOW(3))`,
		in.CompanyID, in.ProjectID, in.Title, in.Content, in.CreatorID)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.GetWikiByID(ctx, id)
}

func (s *MySqlStore) GetWikiByID(ctx context.Context, id int64) (*types.WikiPage, error) {
	var r types.WikiPageRow
	err := s.db.QueryRowContext(ctx, `SELECT id, company_id, project_id, title, content, creator_id, created_at, updated_at FROM wiki_pages WHERE id=?`, id).
		Scan(&r.ID, &r.CompanyID, &r.ProjectID, &r.Title, &r.Content, &r.CreatorID, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	w := rowToWiki(&r)
	return &w, nil
}

// ListWikiPages 列 Wiki。
func (s *MySqlStore) ListWikiPages(ctx context.Context, companyID int64, projectID *int64) ([]types.WikiPage, error) {
	q := `SELECT id, company_id, project_id, title, content, creator_id, created_at, updated_at FROM wiki_pages WHERE company_id=?`
	args := []any{companyID}
	if projectID != nil {
		q += ` AND project_id=?`
		args = append(args, *projectID)
	}
	q += ` ORDER BY updated_at DESC, id DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.WikiPage{}
	for rows.Next() {
		var r types.WikiPageRow
		if err := rows.Scan(&r.ID, &r.CompanyID, &r.ProjectID, &r.Title, &r.Content, &r.CreatorID, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, rowToWiki(&r))
	}
	return out, rows.Err()
}

// UpdateWikiPage 更新 Wiki。
func (s *MySqlStore) UpdateWikiPage(ctx context.Context, in UpdateWikiInput) (*types.WikiPage, error) {
	base, err := s.GetWikiByID(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	if base == nil {
		return nil, &NotFoundError{Msg: "wiki page not found"}
	}
	title := base.Title
	if in.Title != nil {
		title = *in.Title
	}
	content := base.Content
	if in.Content != nil {
		content = *in.Content
	}
	_, err = s.db.ExecContext(ctx, `UPDATE wiki_pages SET title=?, content=?, updated_at=NOW(3) WHERE id=?`, title, content, in.ID)
	if err != nil {
		return nil, err
	}
	return s.GetWikiByID(ctx, in.ID)
}

// DeleteWikiPage 删除 Wiki。
func (s *MySqlStore) DeleteWikiPage(ctx context.Context, wikiID int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM wiki_pages WHERE id=?`, wikiID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{Msg: "wiki page not found"}
	}
	return nil
}

// ==================== 仪表盘 ====================

// scalarCount 执行 count 查询，返回单行数字。
func (s *MySqlStore) scalarCount(ctx context.Context, q string, args ...any) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, q, args...).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

// DashboardStats 项目仪表盘聚合统计。
func (s *MySqlStore) DashboardStats(ctx context.Context, companyID int64, projectID *int64) (*types.DashboardStats, error) {
	newReq30d, _ := s.scalarCount(ctx, `SELECT COUNT(*) FROM requirements WHERE company_id=? AND created_at >= (NOW(3) - INTERVAL 30 DAY)`, companyID)
	overdueReq, _ := s.scalarCount(ctx, `SELECT COUNT(*) FROM requirements WHERE company_id=? AND status NOT IN ('completed','rejected') AND due_time < NOW(3)`, companyID)
	totalBugs, _ := s.scalarCount(ctx, `SELECT COUNT(*) FROM bugs WHERE company_id=?`, companyID)
	resolvedBugs, _ := s.scalarCount(ctx, `SELECT COUNT(*) FROM bugs WHERE company_id=? AND status='resolved'`, companyID)
	unresolvedBugs, _ := s.scalarCount(ctx, `SELECT COUNT(*) FROM bugs WHERE company_id=? AND status<>'resolved'`, companyID)
	resolveRate := int64(0)
	if totalBugs > 0 {
		resolveRate = resolvedBugs * 100 / totalBugs
	}
	// 需求状态分布
	statusMap := map[string]int64{}
	sRows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM requirements WHERE company_id=? GROUP BY status`, companyID)
	if err != nil {
		return nil, err
	}
	for sRows.Next() {
		var st string
		var n int64
		_ = sRows.Scan(&st, &n)
		statusMap[st] = n
	}
	sRows.Close()
	// 缺陷严重度分布
	sevMap := map[string]int64{}
	sevRows, err := s.db.QueryContext(ctx, `SELECT severity, COUNT(*) FROM bugs WHERE company_id=? GROUP BY severity`, companyID)
	if err != nil {
		return nil, err
	}
	for sevRows.Next() {
		var sv string
		var n int64
		_ = sevRows.Scan(&sv, &n)
		sevMap[sv] = n
	}
	sevRows.Close()
	return &types.DashboardStats{
		NewRequirement30d:   newReq30d,
		OverdueRequirement:  overdueReq,
		BugResolveRate:      resolveRate,
		UnresolvedBug:       unresolvedBugs,
		RequirementByStatus: statusMap,
		BugBySeverity:       sevMap,
	}, nil
}

// MemberTracking 成员任务跟踪（按分配聚合）。
func (s *MySqlStore) MemberTracking(ctx context.Context, companyID int64, projectID *int64) ([]types.MemberTrackItem, error) {
	q := `SELECT a.user_id, u.username, u.nick,
		SUM(CASE WHEN a.status IN ('created','in_progress') THEN 1 ELSE 0 END) AS notDone,
		SUM(CASE WHEN a.status='completed' THEN 1 ELSE 0 END) AS completed,
		SUM(CASE WHEN t.status IN ('created','in_progress') THEN 1 ELSE 0 END) AS inProgress,
		SUM(CASE WHEN t.is_overdue=1 THEN 1 ELSE 0 END) AS overdue,
		COUNT(*) AS total
	 FROM task_assignments a JOIN tasks t ON t.id=a.task_id LEFT JOIN users u ON u.id=a.user_id
	 WHERE t.company_id=?`
	args := []any{companyID}
	if projectID != nil {
		q += ` AND t.project_id=?`
		args = append(args, *projectID)
	}
	q += ` GROUP BY a.user_id, u.username, u.nick ORDER BY total DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.MemberTrackItem{}
	for rows.Next() {
		var it types.MemberTrackItem
		var username, nick sql.NullString
		var notDone, completed, inProgress, overdue, total sql.NullInt64
		if err := rows.Scan(&it.UserID, &username, &nick, &notDone, &completed, &inProgress, &overdue, &total); err != nil {
			return nil, err
		}
		it.Username = username.String
		it.Nick = nick.String
		if it.Nick == "" {
			it.Nick = it.Username
		}
		it.Completed = completed.Int64
		it.InProgress = inProgress.Int64
		it.Overdue = overdue.Int64
		it.Total = total.Int64
		out = append(out, it)
	}
	return out, rows.Err()
}
