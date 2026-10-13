// Package db 的 MySqlStore 方法：TAPD（需求/缺陷/计划/文档/Wiki/仪表盘/成员跟踪）。
//
// 本文件全部走 GORM（s.gorm），不再出现裸 SQL 字面量；表名一律用 Table("...") 显式指定。
package db

import (
	"context"
	"time"

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
	now := time.Now().UTC()
	row := types.RequirementRow{
		CompanyID: in.CompanyID, ProjectID: in.ProjectID, Code: "", Title: in.Title,
		Description: in.Description, Category: category, Priority: priority, Status: "planning",
		HandlerID: in.HandlerID, CreatorID: in.CreatorID, StartTime: start, DueTime: due,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.gorm.WithContext(ctx).Table("requirements").Create(&row).Error; err != nil {
		return nil, err
	}
	return s.GetRequirement(ctx, row.ID)
}

// ListRequirements 列需求（可按项目，updated_at 倒序）。
func (s *MySqlStore) ListRequirements(ctx context.Context, companyID int64, projectID *int64) ([]types.Requirement, error) {
	q := s.gorm.WithContext(ctx).Table("requirements").Where("company_id = ?", companyID)
	if projectID != nil {
		q = q.Where("project_id = ?", *projectID)
	}
	rows := []types.RequirementRow{}
	if err := q.Order("updated_at DESC, id DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := []types.Requirement{}
	for i := range rows {
		r := &rows[i]
		req := rowToRequirement(r)
		linked, _ := s.linkedTaskIDs(ctx, r.ID)
		req.LinkedTaskIDs = linked
		s.fillUserNames(ctx, &req.HandlerName, &req.CreatorName, r.HandlerID, &r.CreatorID)
		out = append(out, req)
	}
	return out, nil
}

// GetRequirement 按主键取。
func (s *MySqlStore) GetRequirement(ctx context.Context, requirementID int64) (*types.Requirement, error) {
	var r types.RequirementRow
	err := s.gorm.WithContext(ctx).Table("requirements").Where("id = ?", requirementID).Take(&r).Error
	if gormNotFound(err) {
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
	err = s.gorm.WithContext(ctx).Table("requirements").Where("id = ?", in.ID).Updates(map[string]any{
		"title":       title,
		"description": desc,
		"category":    category,
		"priority":    priority,
		"handler_id":  handlerID,
		"start_time":  start,
		"due_time":    due,
		"updated_at":  time.Now().UTC(),
	}).Error
	if err != nil {
		return nil, err
	}
	return s.GetRequirement(ctx, in.ID)
}

// SetRequirementStatus 变更需求状态（completed 写 completed_time）。
func (s *MySqlStore) SetRequirementStatus(ctx context.Context, in SetRequirementStatusInput) (*types.Requirement, error) {
	now := time.Now().UTC()
	updates := map[string]any{"status": in.Status, "updated_at": now}
	if in.Status == "completed" {
		updates["completed_time"] = now
	}
	if err := s.gorm.WithContext(ctx).Table("requirements").
		Where("id = ?", in.RequirementID).Updates(updates).Error; err != nil {
		return nil, err
	}
	return s.GetRequirement(ctx, in.RequirementID)
}

// DeleteRequirement 删除需求（含关联）。
func (s *MySqlStore) DeleteRequirement(ctx context.Context, requirementID int64) error {
	if err := s.gorm.WithContext(ctx).Table("requirement_links").
		Where("requirement_id = ?", requirementID).Delete(nil).Error; err != nil {
		return err
	}
	res := s.gorm.WithContext(ctx).Table("requirements").Where("id = ?", requirementID).Delete(nil)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return &NotFoundError{Msg: "requirement not found"}
	}
	return nil
}

// linkedTaskIDs 取需求关联的任务 id 列表。
func (s *MySqlStore) linkedTaskIDs(ctx context.Context, requirementID int64) ([]int64, error) {
	out := []int64{}
	if err := s.gorm.WithContext(ctx).Table("requirement_links").
		Where("requirement_id = ?", requirementID).
		Order("id ASC").Pluck("task_id", &out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// LinkRequirementTasks 覆盖式写需求-任务关联。
func (s *MySqlStore) LinkRequirementTasks(ctx context.Context, in LinkRequirementTasksInput) ([]int64, error) {
	if err := s.gorm.WithContext(ctx).Table("requirement_links").
		Where("requirement_id = ?", in.RequirementID).Delete(nil).Error; err != nil {
		return nil, err
	}
	linked := []int64{}
	for _, tid := range in.TaskIDs {
		if err := s.gorm.WithContext(ctx).Table("requirement_links").Create(map[string]any{
			"company_id": in.CompanyID, "requirement_id": in.RequirementID,
			"task_id": tid, "user_id": in.UserID, "created_at": time.Now().UTC(),
		}).Error; err != nil {
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
	now := time.Now().UTC()
	row := types.BugRow{
		CompanyID: in.CompanyID, ProjectID: in.ProjectID, RequirementID: in.RequirementID,
		Code: "", Title: in.Title, Description: in.Description, Severity: severity,
		Priority: priority, Status: "pending", HandlerID: in.HandlerID, CreatorID: in.CreatorID,
		FoundVersion: in.FoundVersion, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.gorm.WithContext(ctx).Table("bugs").Create(&row).Error; err != nil {
		return nil, err
	}
	return s.GetBug(ctx, row.ID)
}

// ListBugs 列缺陷。
func (s *MySqlStore) ListBugs(ctx context.Context, companyID int64, projectID *int64) ([]types.Bug, error) {
	q := s.gorm.WithContext(ctx).Table("bugs").Where("company_id = ?", companyID)
	if projectID != nil {
		q = q.Where("project_id = ?", *projectID)
	}
	rows := []types.BugRow{}
	if err := q.Order("updated_at DESC, id DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := []types.Bug{}
	for i := range rows {
		r := &rows[i]
		b := rowToBug(r)
		s.fillUserNames(ctx, &b.HandlerName, &b.CreatorName, r.HandlerID, &r.CreatorID)
		out = append(out, b)
	}
	return out, nil
}

// GetBug 按主键取缺陷。
func (s *MySqlStore) GetBug(ctx context.Context, bugID int64) (*types.Bug, error) {
	var r types.BugRow
	err := s.gorm.WithContext(ctx).Table("bugs").Where("id = ?", bugID).Take(&r).Error
	if gormNotFound(err) {
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
	err = s.gorm.WithContext(ctx).Table("bugs").Where("id = ?", in.ID).Updates(map[string]any{
		"title":         title,
		"description":   desc,
		"severity":      severity,
		"priority":      priority,
		"handler_id":    handlerID,
		"found_version": version,
		"updated_at":    time.Now().UTC(),
	}).Error
	if err != nil {
		return nil, err
	}
	return s.GetBug(ctx, in.ID)
}

// SetBugStatus 变更缺陷状态。
func (s *MySqlStore) SetBugStatus(ctx context.Context, in SetBugStatusInput) (*types.Bug, error) {
	res := s.gorm.WithContext(ctx).Table("bugs").Where("id = ?", in.BugID).Updates(map[string]any{
		"status": in.Status, "updated_at": time.Now().UTC(),
	})
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, &NotFoundError{Msg: "bug not found"}
	}
	return s.GetBug(ctx, in.BugID)
}

// DeleteBug 删除缺陷。
func (s *MySqlStore) DeleteBug(ctx context.Context, bugID int64) error {
	res := s.gorm.WithContext(ctx).Table("bugs").Where("id = ?", bugID).Delete(nil)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return &NotFoundError{Msg: "bug not found"}
	}
	return nil
}

// ==================== 计划 ====================

// CreatePlan 创建计划。
func (s *MySqlStore) CreatePlan(ctx context.Context, in CreatePlanInput) (*types.Plan, error) {
	start, _ := parseTimeFlex(in.StartTime)
	due, _ := parseTimeFlex(in.DueTime)
	now := time.Now().UTC()
	row := types.PlanRow{
		CompanyID: in.CompanyID, ProjectID: in.ProjectID, Name: in.Name, Description: in.Description,
		StartTime: start, DueTime: due, Status: "not_started", CreatorID: in.CreatorID,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.gorm.WithContext(ctx).Table("plans").Create(&row).Error; err != nil {
		return nil, err
	}
	return s.GetPlanByID(ctx, row.ID)
}

func (s *MySqlStore) GetPlanByID(ctx context.Context, id int64) (*types.Plan, error) {
	var r types.PlanRow
	err := s.gorm.WithContext(ctx).Table("plans").Where("id = ?", id).Take(&r).Error
	if gormNotFound(err) {
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
	q := s.gorm.WithContext(ctx).Table("plans").Where("company_id = ?", companyID)
	if projectID != nil {
		q = q.Where("project_id = ?", *projectID)
	}
	rows := []types.PlanRow{}
	if err := q.Order("start_time ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := []types.Plan{}
	for i := range rows {
		out = append(out, rowToPlan(&rows[i]))
	}
	return out, nil
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
	err = s.gorm.WithContext(ctx).Table("plans").Where("id = ?", in.ID).Updates(map[string]any{
		"name":        name,
		"description": desc,
		"start_time":  start,
		"due_time":    due,
		"status":      status,
		"updated_at":  time.Now().UTC(),
	}).Error
	if err != nil {
		return nil, err
	}
	return s.GetPlanByID(ctx, in.ID)
}

// DeletePlan 删除计划。
func (s *MySqlStore) DeletePlan(ctx context.Context, planID int64) error {
	res := s.gorm.WithContext(ctx).Table("plans").Where("id = ?", planID).Delete(nil)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return &NotFoundError{Msg: "plan not found"}
	}
	return nil
}

// ==================== 文档 ====================

// CreateDocument 创建文档。
func (s *MySqlStore) CreateDocument(ctx context.Context, in CreateDocumentInput) (*types.ProjectDocument, error) {
	now := time.Now().UTC()
	row := types.DocumentRow{
		CompanyID: in.CompanyID, ProjectID: in.ProjectID, Title: in.Title,
		Content: in.Content, CreatorID: in.CreatorID, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.gorm.WithContext(ctx).Table("documents").Create(&row).Error; err != nil {
		return nil, err
	}
	return s.GetDocumentByID(ctx, row.ID)
}

func (s *MySqlStore) GetDocumentByID(ctx context.Context, id int64) (*types.ProjectDocument, error) {
	var r types.DocumentRow
	err := s.gorm.WithContext(ctx).Table("documents").Where("id = ?", id).Take(&r).Error
	if gormNotFound(err) {
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
	q := s.gorm.WithContext(ctx).Table("documents").Where("company_id = ?", companyID)
	if projectID != nil {
		q = q.Where("project_id = ?", *projectID)
	}
	rows := []types.DocumentRow{}
	if err := q.Order("updated_at DESC, id DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := []types.ProjectDocument{}
	for i := range rows {
		out = append(out, rowToDocument(&rows[i]))
	}
	return out, nil
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
	err = s.gorm.WithContext(ctx).Table("documents").Where("id = ?", in.ID).Updates(map[string]any{
		"title": title, "content": content, "updated_at": time.Now().UTC(),
	}).Error
	if err != nil {
		return nil, err
	}
	return s.GetDocumentByID(ctx, in.ID)
}

// DeleteDocument 删除文档。
func (s *MySqlStore) DeleteDocument(ctx context.Context, documentID int64) error {
	res := s.gorm.WithContext(ctx).Table("documents").Where("id = ?", documentID).Delete(nil)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return &NotFoundError{Msg: "document not found"}
	}
	return nil
}

// ==================== Wiki ====================

// CreateWikiPage 创建 Wiki 页。
func (s *MySqlStore) CreateWikiPage(ctx context.Context, in CreateWikiInput) (*types.WikiPage, error) {
	now := time.Now().UTC()
	row := types.WikiPageRow{
		CompanyID: in.CompanyID, ProjectID: in.ProjectID, Title: in.Title,
		Content: in.Content, CreatorID: in.CreatorID, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.gorm.WithContext(ctx).Table("wiki_pages").Create(&row).Error; err != nil {
		return nil, err
	}
	return s.GetWikiByID(ctx, row.ID)
}

func (s *MySqlStore) GetWikiByID(ctx context.Context, id int64) (*types.WikiPage, error) {
	var r types.WikiPageRow
	err := s.gorm.WithContext(ctx).Table("wiki_pages").Where("id = ?", id).Take(&r).Error
	if gormNotFound(err) {
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
	q := s.gorm.WithContext(ctx).Table("wiki_pages").Where("company_id = ?", companyID)
	if projectID != nil {
		q = q.Where("project_id = ?", *projectID)
	}
	rows := []types.WikiPageRow{}
	if err := q.Order("updated_at DESC, id DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := []types.WikiPage{}
	for i := range rows {
		out = append(out, rowToWiki(&rows[i]))
	}
	return out, nil
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
	err = s.gorm.WithContext(ctx).Table("wiki_pages").Where("id = ?", in.ID).Updates(map[string]any{
		"title": title, "content": content, "updated_at": time.Now().UTC(),
	}).Error
	if err != nil {
		return nil, err
	}
	return s.GetWikiByID(ctx, in.ID)
}

// DeleteWikiPage 删除 Wiki。
func (s *MySqlStore) DeleteWikiPage(ctx context.Context, wikiID int64) error {
	res := s.gorm.WithContext(ctx).Table("wiki_pages").Where("id = ?", wikiID).Delete(nil)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return &NotFoundError{Msg: "wiki page not found"}
	}
	return nil
}

// ==================== 仪表盘 ====================

// statusCountRow 分组统计的临时行。
type statusCountRow struct {
	Key string `gorm:"column:key"`
	N   int64  `gorm:"column:n"`
}

// DashboardStats 项目仪表盘聚合统计。
func (s *MySqlStore) DashboardStats(ctx context.Context, companyID int64, projectID *int64) (*types.DashboardStats, error) {
	// 原实现忽略这些计数的错误，这里保持一致。
	var newReq30d int64
	_ = s.gorm.WithContext(ctx).Table("requirements").
		Where("company_id = ? AND created_at >= (NOW(3) - INTERVAL 30 DAY)", companyID).
		Count(&newReq30d).Error

	var overdueReq int64
	_ = s.gorm.WithContext(ctx).Table("requirements").
		Where("company_id = ? AND status NOT IN ('completed','rejected') AND due_time < NOW(3)", companyID).
		Count(&overdueReq).Error

	var totalBugs int64
	_ = s.gorm.WithContext(ctx).Table("bugs").
		Where("company_id = ?", companyID).Count(&totalBugs).Error

	var resolvedBugs int64
	_ = s.gorm.WithContext(ctx).Table("bugs").
		Where("company_id = ? AND status = 'resolved'", companyID).Count(&resolvedBugs).Error

	var unresolvedBugs int64
	_ = s.gorm.WithContext(ctx).Table("bugs").
		Where("company_id = ? AND status <> 'resolved'", companyID).Count(&unresolvedBugs).Error

	resolveRate := int64(0)
	if totalBugs > 0 {
		resolveRate = resolvedBugs * 100 / totalBugs
	}

	// 需求状态分布
	statusMap := map[string]int64{}
	sRows := []statusCountRow{}
	if err := s.gorm.WithContext(ctx).Table("requirements").
		Select("status AS `key`, COUNT(*) AS n").
		Where("company_id = ?", companyID).
		Group("status").
		Scan(&sRows).Error; err != nil {
		return nil, err
	}
	for _, r := range sRows {
		statusMap[r.Key] = r.N
	}

	// 缺陷严重度分布
	sevMap := map[string]int64{}
	sevRows := []statusCountRow{}
	if err := s.gorm.WithContext(ctx).Table("bugs").
		Select("severity AS `key`, COUNT(*) AS n").
		Where("company_id = ?", companyID).
		Group("severity").
		Scan(&sevRows).Error; err != nil {
		return nil, err
	}
	for _, r := range sevRows {
		sevMap[r.Key] = r.N
	}

	return &types.DashboardStats{
		NewRequirement30d:   newReq30d,
		OverdueRequirement:  overdueReq,
		BugResolveRate:      resolveRate,
		UnresolvedBug:       unresolvedBugs,
		RequirementByStatus: statusMap,
		BugBySeverity:       sevMap,
	}, nil
}

// memberTrackRow MemberTracking 的聚合临时行。
type memberTrackRow struct {
	UserID     int64   `gorm:"column:user_id"`
	Username   *string `gorm:"column:username"`
	Nick       *string `gorm:"column:nick"`
	NotDone    *int64  `gorm:"column:not_done"`
	Completed  *int64  `gorm:"column:completed"`
	InProgress *int64  `gorm:"column:in_progress"`
	Overdue    *int64  `gorm:"column:overdue"`
	Total      *int64  `gorm:"column:total"`
}

// MemberTracking 成员任务跟踪（按分配聚合）。
func (s *MySqlStore) MemberTracking(ctx context.Context, companyID int64, projectID *int64) ([]types.MemberTrackItem, error) {
	q := s.gorm.WithContext(ctx).Table("task_assignments a").
		Select(`a.user_id, u.username, u.nick,
			SUM(CASE WHEN a.status IN ('created','in_progress') THEN 1 ELSE 0 END) AS not_done,
			SUM(CASE WHEN a.status='completed' THEN 1 ELSE 0 END) AS completed,
			SUM(CASE WHEN t.status IN ('created','in_progress') THEN 1 ELSE 0 END) AS in_progress,
			SUM(CASE WHEN t.is_overdue=1 THEN 1 ELSE 0 END) AS overdue,
			COUNT(*) AS total`).
		Joins("JOIN tasks t ON t.id=a.task_id").
		Joins("LEFT JOIN users u ON u.id=a.user_id").
		Where("t.company_id = ?", companyID)
	if projectID != nil {
		q = q.Where("t.project_id = ?", *projectID)
	}
	rows := []memberTrackRow{}
	if err := q.Group("a.user_id, u.username, u.nick").
		Order("total DESC").
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := []types.MemberTrackItem{}
	for _, r := range rows {
		it := types.MemberTrackItem{UserID: r.UserID}
		if r.Username != nil {
			it.Username = *r.Username
		}
		if r.Nick != nil {
			it.Nick = *r.Nick
		}
		if it.Nick == "" {
			it.Nick = it.Username
		}
		if r.Completed != nil {
			it.Completed = *r.Completed
		}
		if r.InProgress != nil {
			it.InProgress = *r.InProgress
		}
		if r.Overdue != nil {
			it.Overdue = *r.Overdue
		}
		if r.Total != nil {
			it.Total = *r.Total
		}
		out = append(out, it)
	}
	return out, nil
}
