// Package db 的 MemoryStore 方法（项目、任务流程、TAPD、仪表盘、成员跟踪）。
package db

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"mochi-oa-server-go/internal/types"
)

// ==================== 项目 ====================

func (m *MemoryStore) CreateProject(_ context.Context, in CreateProjectInput) (*types.Project, error) {
	id := m.nextId()
	m.projects[id] = &types.ProjectRow{ID: id, CompanyID: in.CompanyID, Name: in.Name, PmID: in.PmID, CreatedAt: time.Now().UTC()}
	m.projectMem[id] = map[int64]*types.ProjectMemberRow{in.PmID: {ProjectID: id, UserID: in.PmID, Role: "pm", JoinedAt: time.Now().UTC()}}
	p := types.Project{ID: id, CompanyID: in.CompanyID, Name: in.Name, PmID: in.PmID, CreatedAt: toIso(m.projects[id].CreatedAt)}
	return &p, nil
}

func (m *MemoryStore) ListProjects(_ context.Context, companyID int64) ([]types.Project, error) {
	out := []types.Project{}
	for _, r := range m.projects {
		if r.CompanyID == companyID {
			out = append(out, types.Project{ID: r.ID, CompanyID: r.CompanyID, Name: r.Name, PmID: r.PmID, CreatedAt: toIso(r.CreatedAt)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemoryStore) DeleteProject(_ context.Context, projectID int64) error {
	delete(m.projectMem, projectID)
	delete(m.projects, projectID)
	return nil
}

func (m *MemoryStore) SetProjectRole(_ context.Context, projectID, userID int64, role string) error {
	set := m.projectMem[projectID]
	if set == nil {
		set = map[int64]*types.ProjectMemberRow{}
		m.projectMem[projectID] = set
	}
	set[userID] = &types.ProjectMemberRow{ProjectID: projectID, UserID: userID, Role: role, JoinedAt: time.Now().UTC()}
	return nil
}

func (m *MemoryStore) ListProjectMembers(_ context.Context, projectID int64) ([]types.ProjectMember, error) {
	out := []types.ProjectMember{}
	if set := m.projectMem[projectID]; set != nil {
		for _, r := range set {
			out = append(out, types.ProjectMember{ProjectID: r.ProjectID, UserID: r.UserID, Role: r.Role, JoinedAt: toIso(r.JoinedAt)})
		}
	}
	return out, nil
}

// ==================== 任务 ====================

func (m *MemoryStore) CreateTask(_ context.Context, in CreateTaskInput) (*types.TaskItem, error) {
	start, _ := parseTimeFlex(in.StartTime)
	due, _ := parseTimeFlex(in.DueTime)
	images := in.Images
	if images == nil {
		images = []string{}
	}
	b, _ := json.Marshal(images)
	id := m.nextId()
	m.tasks[id] = &types.TaskRow{ID: id, CompanyID: in.CompanyID, ProjectID: in.ProjectID, Title: in.Title, Description: in.Description, StartTime: start, DueTime: due, Status: "created", Images: string(b), CreatedBy: in.CreatedBy, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	t := rowToTask(m.tasks[id])
	return &t, nil
}

func (m *MemoryStore) ListTasks(_ context.Context, companyID int64, projectID *int64) ([]types.TaskItem, error) {
	out := []types.TaskItem{}
	for _, r := range m.tasks {
		if r.CompanyID != companyID {
			continue
		}
		if projectID != nil && (r.ProjectID == nil || *r.ProjectID != *projectID) {
			continue
		}
		out = append(out, rowToTask(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DueTime < out[j].DueTime })
	return out, nil
}

func (m *MemoryStore) GetTask(_ context.Context, taskID int64) (*types.TaskItem, error) {
	if r, ok := m.tasks[taskID]; ok {
		t := rowToTask(r)
		return &t, nil
	}
	return nil, nil
}

func (m *MemoryStore) GetTaskDetail(_ context.Context, taskID int64) (*types.TaskDetail, error) {
	task, err := m.GetTask(context.Background(), taskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, &NotFoundError{Msg: "task not found"}
	}
	assigns := []types.TaskAssignment{}
	for _, r := range m.assignments {
		if r.TaskID == taskID {
			a := rowToAssignment(r)
			if u, _ := m.GetUserByID(context.Background(), r.UserID); u != nil {
				un, nk, av := u.Username, u.Nick, u.Avatar
				a.Username, a.Nick, a.Avatar = &un, &nk, &av
			}
			assigns = append(assigns, a)
		}
	}
	comments := []types.TaskComment{}
	for _, r := range m.taskComments {
		if r.TaskID == taskID {
			c := rowToComment(r)
			if u, _ := m.GetUserByID(context.Background(), r.UserID); u != nil {
				un, nk, av := u.Username, u.Nick, u.Avatar
				c.Username, c.Nick, c.Avatar = &un, &nk, &av
			}
			comments = append(comments, c)
		}
	}
	issues := []types.TaskIssue{}
	for _, r := range m.taskIssues {
		if r.TaskID == taskID {
			is := rowToIssue(r)
			if u, _ := m.GetUserByID(context.Background(), r.UserID); u != nil {
				un, nk, av := u.Username, u.Nick, u.Avatar
				is.Username, is.Nick, is.Avatar = &un, &nk, &av
			}
			issues = append(issues, is)
		}
	}
	exts := []types.TaskExtension{}
	for _, r := range m.taskExts {
		if r.TaskID == taskID {
			ex := rowToExtension(r)
			if u, _ := m.GetUserByID(context.Background(), r.UserID); u != nil {
				un, nk := u.Username, u.Nick
				ex.Username, ex.Nick = &un, &nk
			}
			exts = append(exts, ex)
		}
	}
	logs := []types.TaskStatusLog{}
	for _, r := range m.taskLogs {
		if r.TaskID == taskID {
			lg := rowToLog(r)
			if u, _ := m.GetUserByID(context.Background(), r.UserID); u != nil {
				un, nk := u.Username, u.Nick
				lg.Username, lg.Nick = &un, &nk
			}
			logs = append(logs, lg)
		}
	}
	return &types.TaskDetail{Task: *task, Assignments: assigns, Comments: comments, Issues: issues, Extensions: exts, Logs: logs}, nil
}

func (m *MemoryStore) UpdateTask(_ context.Context, in UpdateTaskInput) (*types.TaskItem, error) {
	r, ok := m.tasks[in.ID]
	if !ok {
		return nil, &NotFoundError{Msg: "task not found"}
	}
	if in.Title != nil {
		r.Title = *in.Title
	}
	if in.Description != nil {
		r.Description = *in.Description
	}
	if in.StartTime != nil {
		if t, err := parseTimeFlex(*in.StartTime); err == nil {
			r.StartTime = t
		}
	}
	if in.DueTime != nil {
		if t, err := parseTimeFlex(*in.DueTime); err == nil {
			r.DueTime = t
		}
	}
	if in.Images != nil {
		b, _ := json.Marshal(in.Images)
		r.Images = string(b)
	}
	r.UpdatedAt = time.Now().UTC()
	t := rowToTask(r)
	return &t, nil
}

func (m *MemoryStore) DeleteTask(_ context.Context, taskID int64) error {
	delete(m.tasks, taskID)
	return nil
}

func (m *MemoryStore) SetTaskStatus(ctx context.Context, in SetTaskStatusInput) (*types.TaskItem, error) {
	r, ok := m.tasks[in.TaskID]
	if !ok {
		return nil, &NotFoundError{Msg: "task not found"}
	}
	from := r.Status
	r.Status = in.Status
	r.IsOverdue = false
	if in.Status == "completed" {
		now := time.Now().UTC()
		r.CompletedTime = &now
	}
	r.UpdatedAt = time.Now().UTC()
	id := m.nextId()
	m.taskLogs[id] = &types.TaskStatusLogRow{ID: id, TaskID: in.TaskID, UserID: in.UserID, FromStatus: from, ToStatus: in.Status, Note: in.Note, CreatedAt: time.Now().UTC()}
	return m.GetTask(ctx, in.TaskID)
}

func (m *MemoryStore) UpdateTaskReminder(_ context.Context, in TaskReminderInput) (*types.TaskItem, error) {
	r, ok := m.tasks[in.TaskID]
	if !ok {
		return nil, &NotFoundError{Msg: "task not found"}
	}
	r.ReminderYellow = in.ReminderYellow
	r.ReminderRed = in.ReminderRed
	t := rowToTask(r)
	return &t, nil
}

func (m *MemoryStore) AddAssignment(_ context.Context, in AddAssignmentInput) (*types.TaskAssignment, error) {
	id := m.nextId()
	m.assignments[id] = &types.TaskAssignmentRow{ID: id, TaskID: in.TaskID, UserID: in.UserID, Content: in.Content, Status: "created", CreatedAt: time.Now().UTC()}
	a := rowToAssignment(m.assignments[id])
	return &a, nil
}

func (m *MemoryStore) RemoveAssignment(_ context.Context, id int64) error {
	if _, ok := m.assignments[id]; ok {
		delete(m.assignments, id)
		return nil
	}
	return &NotFoundError{Msg: "assignment not found"}
}

func (m *MemoryStore) SetAssignmentStatus(_ context.Context, in SetAssignmentStatusInput) (*types.TaskAssignment, error) {
	r, ok := m.assignments[in.ID]
	if !ok {
		return nil, &NotFoundError{Msg: "assignment not found"}
	}
	r.Status = in.Status
	if in.Status == "completed" {
		now := time.Now().UTC()
		r.CompletedAt = &now
	} else {
		r.CompletedAt = nil
	}
	a := rowToAssignment(r)
	return &a, nil
}

func (m *MemoryStore) AddTaskComment(_ context.Context, in AddTaskCommentInput) (*types.TaskComment, error) {
	atts := in.Attachments
	if atts == nil {
		atts = []types.TaskAttachment{}
	}
	b, _ := json.Marshal(atts)
	id := m.nextId()
	s := string(b)
	m.taskComments[id] = &types.TaskCommentRow{ID: id, TaskID: in.TaskID, UserID: in.UserID, Content: in.Content, Images: &s, CreatedAt: time.Now().UTC()}
	c := rowToComment(m.taskComments[id])
	return &c, nil
}

func (m *MemoryStore) AddTaskIssue(_ context.Context, in AddTaskIssueInput) (*types.TaskIssue, error) {
	id := m.nextId()
	m.taskIssues[id] = &types.TaskIssueRow{ID: id, TaskID: in.TaskID, UserID: in.UserID, Title: in.Title, Content: in.Content, Status: "open", CreatedAt: time.Now().UTC()}
	is := rowToIssue(m.taskIssues[id])
	return &is, nil
}

func (m *MemoryStore) ResolveTaskIssue(_ context.Context, issueID int64) (*types.TaskIssue, error) {
	r, ok := m.taskIssues[issueID]
	if !ok {
		return nil, &NotFoundError{Msg: "issue not found"}
	}
	r.Status = "resolved"
	now := time.Now().UTC()
	r.ResolvedAt = &now
	is := rowToIssue(r)
	return &is, nil
}

func (m *MemoryStore) RequestTaskExtension(_ context.Context, in RequestExtensionInput) (*types.TaskExtension, error) {
	reqDue, _ := parseTimeFlex(in.RequestedDueTime)
	id := m.nextId()
	m.taskExts[id] = &types.TaskExtensionRow{ID: id, TaskID: in.TaskID, UserID: in.UserID, RequestedDueTime: reqDue, Reason: in.Reason, Status: "pending", CreatedAt: time.Now().UTC()}
	ex := rowToExtension(m.taskExts[id])
	return &ex, nil
}

func (m *MemoryStore) DecideTaskExtension(_ context.Context, in DecideExtensionInput) (*types.TaskExtension, error) {
	r, ok := m.taskExts[in.ExtensionID]
	if !ok {
		return nil, &NotFoundError{Msg: "extension not found"}
	}
	st := "rejected"
	if in.Approved {
		st = "approved"
	}
	r.Status = st
	db := in.DecidedBy
	r.DecidedBy = &db
	now := time.Now().UTC()
	r.DecidedAt = &now
	if in.Approved {
		if t, ok := m.tasks[r.TaskID]; ok {
			t.DueTime = r.RequestedDueTime
			t.UpdatedAt = now
		}
	}
	ex := rowToExtension(r)
	return &ex, nil
}

// ==================== TAPD ====================

func (m *MemoryStore) linkedTaskIDsFor(reqID int64) []int64 {
	out := []int64{}
	for _, r := range m.reqLinks {
		if r.RequirementID == reqID {
			out = append(out, r.TaskID)
		}
	}
	return out
}

func (m *MemoryStore) fillNames(ctx context.Context, handlerName, creatorName **string, handlerID, creatorID *int64) {
	if handlerID != nil {
		if u, _ := m.GetUserByID(ctx, *handlerID); u != nil {
			n := u.Nick
			if n == "" {
				n = u.Username
			}
			*handlerName = &n
		}
	}
	if creatorID != nil {
		if u, _ := m.GetUserByID(ctx, *creatorID); u != nil {
			n := u.Nick
			if n == "" {
				n = u.Username
			}
			*creatorName = &n
		}
	}
}

func (m *MemoryStore) CreateRequirement(_ context.Context, in CreateRequirementInput) (*types.Requirement, error) {
	start, _ := parseTimeFlex(in.StartTime)
	due, _ := parseTimeFlex(in.DueTime)
	cat, pri := in.Category, in.Priority
	if cat == "" {
		cat = "uncategorized"
	}
	if pri == "" {
		pri = "middle"
	}
	id := m.nextId()
	m.requirements[id] = &types.RequirementRow{ID: id, CompanyID: in.CompanyID, ProjectID: in.ProjectID, Title: in.Title, Description: in.Description, Category: cat, Priority: pri, Status: "planning", HandlerID: in.HandlerID, CreatorID: in.CreatorID, StartTime: start, DueTime: due, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	r := m.requirements[id]
	req := rowToRequirement(r)
	req.LinkedTaskIDs = []int64{}
	m.fillNames(context.Background(), &req.HandlerName, &req.CreatorName, r.HandlerID, &r.CreatorID)
	return &req, nil
}

func (m *MemoryStore) ListRequirements(ctx context.Context, companyID int64, projectID *int64) ([]types.Requirement, error) {
	out := []types.Requirement{}
	for _, r := range m.requirements {
		if r.CompanyID != companyID {
			continue
		}
		if projectID != nil && (r.ProjectID == nil || *r.ProjectID != *projectID) {
			continue
		}
		req := rowToRequirement(r)
		req.LinkedTaskIDs = m.linkedTaskIDsFor(r.ID)
		m.fillNames(ctx, &req.HandlerName, &req.CreatorName, r.HandlerID, &r.CreatorID)
		out = append(out, req)
	}
	return out, nil
}

func (m *MemoryStore) GetRequirement(ctx context.Context, requirementID int64) (*types.Requirement, error) {
	if r, ok := m.requirements[requirementID]; ok {
		req := rowToRequirement(r)
		req.LinkedTaskIDs = m.linkedTaskIDsFor(r.ID)
		m.fillNames(ctx, &req.HandlerName, &req.CreatorName, r.HandlerID, &r.CreatorID)
		return &req, nil
	}
	return nil, nil
}

func (m *MemoryStore) UpdateRequirement(_ context.Context, in UpdateRequirementInput) (*types.Requirement, error) {
	r, ok := m.requirements[in.ID]
	if !ok {
		return nil, &NotFoundError{Msg: "requirement not found"}
	}
	if in.Title != nil {
		r.Title = *in.Title
	}
	if in.Description != nil {
		r.Description = *in.Description
	}
	if in.Category != nil {
		r.Category = *in.Category
	}
	if in.Priority != nil {
		r.Priority = *in.Priority
	}
	if in.HandlerID != nil {
		r.HandlerID = in.HandlerID
	}
	if in.StartTime != nil {
		if t, err := parseTimeFlex(*in.StartTime); err == nil {
			r.StartTime = t
		}
	}
	if in.DueTime != nil {
		if t, err := parseTimeFlex(*in.DueTime); err == nil {
			r.DueTime = t
		}
	}
	r.UpdatedAt = time.Now().UTC()
	return m.GetRequirement(context.Background(), in.ID)
}

func (m *MemoryStore) SetRequirementStatus(_ context.Context, in SetRequirementStatusInput) (*types.Requirement, error) {
	r, ok := m.requirements[in.RequirementID]
	if !ok {
		return nil, &NotFoundError{Msg: "requirement not found"}
	}
	r.Status = in.Status
	if in.Status == "completed" {
		now := time.Now().UTC()
		r.CompletedTime = &now
	}
	r.UpdatedAt = time.Now().UTC()
	return m.GetRequirement(context.Background(), in.RequirementID)
}

func (m *MemoryStore) DeleteRequirement(_ context.Context, requirementID int64) error {
	for id, r := range m.reqLinks {
		if r.RequirementID == requirementID {
			delete(m.reqLinks, id)
		}
	}
	delete(m.requirements, requirementID)
	return nil
}

func (m *MemoryStore) LinkRequirementTasks(_ context.Context, in LinkRequirementTasksInput) ([]int64, error) {
	for id, r := range m.reqLinks {
		if r.RequirementID == in.RequirementID {
			delete(m.reqLinks, id)
		}
	}
	linked := []int64{}
	for _, tid := range in.TaskIDs {
		id := m.nextId()
		m.reqLinks[id] = &types.RequirementLinkRow{ID: id, CompanyID: in.CompanyID, RequirementID: in.RequirementID, TaskID: tid, UserID: in.UserID, CreatedAt: time.Now().UTC()}
		linked = append(linked, tid)
	}
	return linked, nil
}

func (m *MemoryStore) CreateBug(_ context.Context, in CreateBugInput) (*types.Bug, error) {
	sev, pri := in.Severity, in.Priority
	if sev == "" {
		sev = "normal"
	}
	if pri == "" {
		pri = "middle"
	}
	id := m.nextId()
	m.bugs[id] = &types.BugRow{ID: id, CompanyID: in.CompanyID, ProjectID: in.ProjectID, RequirementID: in.RequirementID, Title: in.Title, Description: in.Description, Severity: sev, Priority: pri, Status: "pending", HandlerID: in.HandlerID, CreatorID: in.CreatorID, FoundVersion: in.FoundVersion, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	b := rowToBug(m.bugs[id])
	m.fillNames(context.Background(), &b.HandlerName, &b.CreatorName, m.bugs[id].HandlerID, &m.bugs[id].CreatorID)
	return &b, nil
}

func (m *MemoryStore) ListBugs(ctx context.Context, companyID int64, projectID *int64) ([]types.Bug, error) {
	out := []types.Bug{}
	for _, r := range m.bugs {
		if r.CompanyID != companyID {
			continue
		}
		if projectID != nil && (r.ProjectID == nil || *r.ProjectID != *projectID) {
			continue
		}
		b := rowToBug(r)
		m.fillNames(ctx, &b.HandlerName, &b.CreatorName, r.HandlerID, &r.CreatorID)
		out = append(out, b)
	}
	return out, nil
}

func (m *MemoryStore) GetBug(ctx context.Context, bugID int64) (*types.Bug, error) {
	if r, ok := m.bugs[bugID]; ok {
		b := rowToBug(r)
		m.fillNames(ctx, &b.HandlerName, &b.CreatorName, r.HandlerID, &r.CreatorID)
		return &b, nil
	}
	return nil, nil
}

func (m *MemoryStore) UpdateBug(_ context.Context, in UpdateBugInput) (*types.Bug, error) {
	r, ok := m.bugs[in.ID]
	if !ok {
		return nil, &NotFoundError{Msg: "bug not found"}
	}
	if in.Title != nil {
		r.Title = *in.Title
	}
	if in.Description != nil {
		r.Description = *in.Description
	}
	if in.Severity != nil {
		r.Severity = *in.Severity
	}
	if in.Priority != nil {
		r.Priority = *in.Priority
	}
	if in.HandlerID != nil {
		r.HandlerID = in.HandlerID
	}
	if in.FoundVersion != nil {
		r.FoundVersion = *in.FoundVersion
	}
	r.UpdatedAt = time.Now().UTC()
	return m.GetBug(context.Background(), in.ID)
}

func (m *MemoryStore) SetBugStatus(_ context.Context, in SetBugStatusInput) (*types.Bug, error) {
	r, ok := m.bugs[in.BugID]
	if !ok {
		return nil, &NotFoundError{Msg: "bug not found"}
	}
	r.Status = in.Status
	r.UpdatedAt = time.Now().UTC()
	return m.GetBug(context.Background(), in.BugID)
}

func (m *MemoryStore) DeleteBug(_ context.Context, bugID int64) error {
	delete(m.bugs, bugID)
	return nil
}

func (m *MemoryStore) CreatePlan(_ context.Context, in CreatePlanInput) (*types.Plan, error) {
	start, _ := parseTimeFlex(in.StartTime)
	due, _ := parseTimeFlex(in.DueTime)
	id := m.nextId()
	m.plans[id] = &types.PlanRow{ID: id, CompanyID: in.CompanyID, ProjectID: in.ProjectID, Name: in.Name, Description: in.Description, StartTime: start, DueTime: due, Status: "not_started", CreatorID: in.CreatorID, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	p := rowToPlan(m.plans[id])
	return &p, nil
}

func (m *MemoryStore) ListPlans(_ context.Context, companyID int64, projectID *int64) ([]types.Plan, error) {
	out := []types.Plan{}
	for _, r := range m.plans {
		if r.CompanyID != companyID {
			continue
		}
		if projectID != nil && (r.ProjectID == nil || *r.ProjectID != *projectID) {
			continue
		}
		out = append(out, rowToPlan(r))
	}
	return out, nil
}

func (m *MemoryStore) UpdatePlan(_ context.Context, in UpdatePlanInput) (*types.Plan, error) {
	r, ok := m.plans[in.ID]
	if !ok {
		return nil, &NotFoundError{Msg: "plan not found"}
	}
	if in.Name != nil {
		r.Name = *in.Name
	}
	if in.Description != nil {
		r.Description = *in.Description
	}
	if in.StartTime != nil {
		if t, err := parseTimeFlex(*in.StartTime); err == nil {
			r.StartTime = t
		}
	}
	if in.DueTime != nil {
		if t, err := parseTimeFlex(*in.DueTime); err == nil {
			r.DueTime = t
		}
	}
	if in.Status != nil {
		r.Status = *in.Status
	}
	r.UpdatedAt = time.Now().UTC()
	p := rowToPlan(r)
	return &p, nil
}

func (m *MemoryStore) DeletePlan(_ context.Context, planID int64) error {
	delete(m.plans, planID)
	return nil
}

func (m *MemoryStore) CreateDocument(_ context.Context, in CreateDocumentInput) (*types.ProjectDocument, error) {
	id := m.nextId()
	m.documents[id] = &types.DocumentRow{ID: id, CompanyID: in.CompanyID, ProjectID: in.ProjectID, Title: in.Title, Content: in.Content, CreatorID: in.CreatorID, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	d := rowToDocument(m.documents[id])
	return &d, nil
}

func (m *MemoryStore) ListDocuments(_ context.Context, companyID int64, projectID *int64) ([]types.ProjectDocument, error) {
	out := []types.ProjectDocument{}
	for _, r := range m.documents {
		if r.CompanyID != companyID {
			continue
		}
		if projectID != nil && (r.ProjectID == nil || *r.ProjectID != *projectID) {
			continue
		}
		out = append(out, rowToDocument(r))
	}
	return out, nil
}

func (m *MemoryStore) UpdateDocument(_ context.Context, in UpdateDocumentInput) (*types.ProjectDocument, error) {
	r, ok := m.documents[in.ID]
	if !ok {
		return nil, &NotFoundError{Msg: "document not found"}
	}
	if in.Title != nil {
		r.Title = *in.Title
	}
	if in.Content != nil {
		r.Content = *in.Content
	}
	r.UpdatedAt = time.Now().UTC()
	d := rowToDocument(r)
	return &d, nil
}

func (m *MemoryStore) DeleteDocument(_ context.Context, documentID int64) error {
	delete(m.documents, documentID)
	return nil
}

func (m *MemoryStore) CreateWikiPage(_ context.Context, in CreateWikiInput) (*types.WikiPage, error) {
	id := m.nextId()
	m.wikiPages[id] = &types.WikiPageRow{ID: id, CompanyID: in.CompanyID, ProjectID: in.ProjectID, Title: in.Title, Content: in.Content, CreatorID: in.CreatorID, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	w := rowToWiki(m.wikiPages[id])
	return &w, nil
}

func (m *MemoryStore) ListWikiPages(_ context.Context, companyID int64, projectID *int64) ([]types.WikiPage, error) {
	out := []types.WikiPage{}
	for _, r := range m.wikiPages {
		if r.CompanyID != companyID {
			continue
		}
		if projectID != nil && (r.ProjectID == nil || *r.ProjectID != *projectID) {
			continue
		}
		out = append(out, rowToWiki(r))
	}
	return out, nil
}

func (m *MemoryStore) UpdateWikiPage(_ context.Context, in UpdateWikiInput) (*types.WikiPage, error) {
	r, ok := m.wikiPages[in.ID]
	if !ok {
		return nil, &NotFoundError{Msg: "wiki page not found"}
	}
	if in.Title != nil {
		r.Title = *in.Title
	}
	if in.Content != nil {
		r.Content = *in.Content
	}
	r.UpdatedAt = time.Now().UTC()
	w := rowToWiki(r)
	return &w, nil
}

func (m *MemoryStore) DeleteWikiPage(_ context.Context, wikiID int64) error {
	delete(m.wikiPages, wikiID)
	return nil
}

func (m *MemoryStore) DashboardStats(_ context.Context, companyID int64, projectID *int64) (*types.DashboardStats, error) {
	newReq, overdueReq, resolved, total, unresolved := int64(0), int64(0), int64(0), int64(0), int64(0)
	statusMap := map[string]int64{}
	for _, r := range m.requirements {
		if r.CompanyID != companyID {
			continue
		}
		statusMap[r.Status]++
		if r.CreatedAt.After(time.Now().AddDate(0, 0, -30)) {
			newReq++
		}
		if r.Status != "completed" && r.Status != "rejected" && r.DueTime.Before(time.Now()) {
			overdueReq++
		}
	}
	for _, r := range m.bugs {
		if r.CompanyID != companyID {
			continue
		}
		total++
		if r.Status == "resolved" {
			resolved++
		} else {
			unresolved++
		}
	}
	rate := int64(0)
	if total > 0 {
		rate = resolved * 100 / total
	}
	sevMap := map[string]int64{}
	for _, r := range m.bugs {
		if r.CompanyID == companyID {
			sevMap[r.Severity]++
		}
	}
	return &types.DashboardStats{NewRequirement30d: newReq, OverdueRequirement: overdueReq, BugResolveRate: rate, UnresolvedBug: unresolved, RequirementByStatus: statusMap, BugBySeverity: sevMap}, nil
}

func (m *MemoryStore) MemberTracking(_ context.Context, companyID int64, projectID *int64) ([]types.MemberTrackItem, error) {
	type agg struct {
		total, completed, inProgress, overdue int64
	}
	aggs := map[int64]*agg{}
	for _, a := range m.assignments {
		t, ok := m.tasks[a.TaskID]
		if !ok || t.CompanyID != companyID {
			continue
		}
		if projectID != nil && (t.ProjectID == nil || *t.ProjectID != *projectID) {
			continue
		}
		if aggs[a.UserID] == nil {
			aggs[a.UserID] = &agg{}
		}
		g := aggs[a.UserID]
		g.total++
		if a.Status == "completed" {
			g.completed++
		}
		if t.Status == "created" || t.Status == "in_progress" {
			g.inProgress++
		}
		if t.IsOverdue {
			g.overdue++
		}
	}
	out := []types.MemberTrackItem{}
	for uid, g := range aggs {
		it := types.MemberTrackItem{UserID: uid, Total: g.total, Completed: g.completed, InProgress: g.inProgress, Overdue: g.overdue}
		if u, _ := m.GetUserByID(context.Background(), uid); u != nil {
			it.Username = u.Username
			it.Nick = u.Nick
			if it.Nick == "" {
				it.Nick = it.Username
			}
		}
		out = append(out, it)
	}
	return out, nil
}
