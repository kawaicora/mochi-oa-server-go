// Package task 是「任务流程系统」socket 事件处理器，1:1 复刻自
// 源仓库 server/src/handlers/task.ts（项目 / 任务 / 分配 / 留言 / 问题 / 延期）。
//
// 行为对齐要点：
//   - 20 个 task:* 入站事件，事件名、字段名（camelCase）、ack 信封（util.OK/Fail 扁平）逐字一致；
//   - 每次任务/项目数据变更后广播 task:tasksUpdated / task:projectsUpdated（客户端靠它刷新列表）；
//   - 鉴权链（未登录 → 非本公司成员 → 公司管理员/项目 PM/创建人）与 TS 完全一致；
//   - 错误文案与 TS 逐字一致；存储层意外错误必打日志、绝不吞掉。
//
// 注册方式：Register(srv, store)；store 必须是基础层 db.GetStore() 的单例，本包不新建连接。
package task

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"

	"mochi-oa-server-go/internal/db"
	"mochi-oa-server-go/internal/logger"
	"mochi-oa-server-go/internal/socketapi"
	"mochi-oa-server-go/internal/types"
	"mochi-oa-server-go/internal/util"
)

// 日志模块前缀（硬性约束：统一 internal/logger）。
const mod = "handlers/task"

// 与 TS 对齐的枚举白名单（task.ts 顶部常量）。
var (
	taskStatuses = []string{"created", "in_progress", "completed", "pending_extension", "extended", "overdue"}
	projRoles    = []string{"pm", "leader", "member"}
	assignStates = []string{"created", "in_progress", "completed"}
)

// transitions 任务状态合法流转表（与 TS TRANSITIONS 逐字一致）。
var transitions = map[string][]string{
	"created":          {"in_progress", "completed", "pending_extension"},
	"in_progress":      {"completed", "pending_extension"},
	"completed":        {},
	"pending_extension": {"extended", "in_progress"},
	"extended":         {"in_progress", "completed"},
	"overdue":          {"in_progress", "completed", "pending_extension"},
}

// attachmentKinds 留言附件合法 kind 白名单（TS: ['image','video','audio','folder','file']）。
var attachmentKinds = []string{"image", "video", "audio", "folder", "file"}

// Register 注册全部 task:* 事件处理器（对标 TS registerTaskHandlers(ctx)，ctx.io/ctx.store 由参数注入）。
func Register(srv socketapi.Server, store db.Store) {
	// ==================== 项目 ====================
	srv.On("task:projectList", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		uid, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		companyID := asInt(m["companyId"])
		if memberRole(context.Background(), store, companyID, uid) == "" {
			ack(util.Fail("非本公司成员"))
			return
		}
		projects, err := store.ListProjects(context.Background(), companyID)
		if err != nil {
			logger.Error(mod, "task:projectList 查询失败", "companyId", companyID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		if projects == nil {
			projects = []types.Project{}
		}
		ack(util.OK(map[string]any{"projects": projects}))
	})

	srv.On("task:projectCreate", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		uid, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		companyID := asInt(m["companyId"])
		name := asStr(m["name"], 64)
		if !isCompanyAdmin(context.Background(), store, companyID, uid) {
			ack(util.Fail("无权限：仅公司 owner/admin 可创建项目"))
			return
		}
		if name == "" {
			ack(util.Fail("项目名称不能为空"))
			return
		}
		project, err := store.CreateProject(context.Background(), db.CreateProjectInput{CompanyID: companyID, Name: name, PmID: uid})
		if err != nil {
			logger.Error(mod, "task:projectCreate 失败", "companyId", companyID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		_ = srv.Broadcast("task:projectsUpdated", map[string]any{"companyId": companyID})
		ack(util.OK(map[string]any{"project": project}))
	})

	srv.On("task:projectDelete", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		uid, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		companyID := asInt(m["companyId"])
		projectID := asInt(m["projectId"])
		if memberRole(context.Background(), store, companyID, uid) == "" {
			ack(util.Fail("非本公司成员"))
			return
		}
		projects, err := store.ListProjects(context.Background(), companyID)
		if err != nil {
			logger.Error(mod, "task:projectDelete 查询项目失败", "companyId", companyID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		var project *types.Project
		for i := range projects {
			if projects[i].ID == projectID {
				project = &projects[i]
				break
			}
		}
		if project == nil {
			ack(util.Fail("项目不存在"))
			return
		}
		if !isCompanyAdmin(context.Background(), store, companyID, uid) && project.PmID != uid {
			ack(util.Fail("无权限：仅公司管理员或 PM 可删除项目"))
			return
		}
		if err := store.DeleteProject(context.Background(), projectID); err != nil {
			logger.Error(mod, "task:projectDelete 失败", "projectId", projectID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		_ = srv.Broadcast("task:projectsUpdated", map[string]any{"companyId": companyID})
		ack(util.OK(nil))
	})

	srv.On("task:projectSetRole", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		uid, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		companyID := asInt(m["companyId"])
		projectID := asInt(m["projectId"])
		userID := asInt(m["userId"])
		role := asStr(m["role"], 16)
		if memberRole(context.Background(), store, companyID, uid) == "" {
			ack(util.Fail("非本公司成员"))
			return
		}
		projects, err := store.ListProjects(context.Background(), companyID)
		if err != nil {
			logger.Error(mod, "task:projectSetRole 查询项目失败", "companyId", companyID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		var project *types.Project
		for i := range projects {
			if projects[i].ID == projectID {
				project = &projects[i]
				break
			}
		}
		if project == nil {
			ack(util.Fail("项目不存在"))
			return
		}
		if project.PmID != uid {
			ack(util.Fail("无权限：仅 PM 可下放组长权限"))
			return
		}
		if !inList(role, projRoles) {
			ack(util.Fail("角色不合法"))
			return
		}
		if err := store.SetProjectRole(context.Background(), projectID, userID, role); err != nil {
			logger.Error(mod, "task:projectSetRole 失败", "projectId", projectID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		_ = srv.Broadcast("task:projectsUpdated", map[string]any{"companyId": companyID})
		ack(util.OK(nil))
	})

	srv.On("task:projectMembers", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		_, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		projectID := asInt(m["projectId"])
		members, err := store.ListProjectMembers(context.Background(), projectID)
		if err != nil {
			logger.Error(mod, "task:projectMembers 查询失败", "projectId", projectID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		if members == nil {
			members = []types.ProjectMember{}
		}
		ack(util.OK(map[string]any{"members": members}))
	})

	// ==================== 任务 ====================
	srv.On("task:list", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		uid, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		companyID := asInt(m["companyId"])
		// TS: d.projectId ? int(d.projectId) : null —— 假值（null/undefined/0/""/false）一律 nil
		var projectID *int64
		if v, exists := m["projectId"]; exists && truthy(v) {
			pid := asInt(v)
			projectID = &pid
		}
		if memberRole(context.Background(), store, companyID, uid) == "" {
			ack(util.Fail("非本公司成员"))
			return
		}
		tasks, err := store.ListTasks(context.Background(), companyID, projectID)
		if err != nil {
			logger.Error(mod, "task:list 查询失败", "companyId", companyID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		if tasks == nil {
			tasks = []types.TaskItem{}
		}
		ack(util.OK(map[string]any{"tasks": tasks}))
	})

	srv.On("task:detail", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		uid, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		taskID := asInt(m["taskId"])
		detail, err := store.GetTaskDetail(context.Background(), taskID)
		if err != nil {
			logger.Error(mod, "task:detail 查询失败", "taskId", taskID, "error", err.Error())
		}
		if detail == nil {
			ack(util.Fail("任务不存在"))
			return
		}
		if memberRole(context.Background(), store, detail.Task.CompanyID, uid) == "" {
			ack(util.Fail("非本公司成员"))
			return
		}
		ack(util.OK(map[string]any{"detail": detail}))
	})

	srv.On("task:create", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		uid, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		companyID := asInt(m["companyId"])
		var projectID *int64
		if v, exists := m["projectId"]; exists && truthy(v) {
			pid := asInt(v)
			projectID = &pid
		}
		title := asStr(m["title"], 128)
		description := asStr(m["description"], 2000)
		startTime := asIso(m["startTime"])
		dueTime := asIso(m["dueTime"])
		images := asStringSlice(m["images"], 9)
		if memberRole(context.Background(), store, companyID, uid) == "" {
			ack(util.Fail("非本公司成员"))
			return
		}
		admin := isCompanyAdmin(context.Background(), store, companyID, uid)
		pm := isProjectManager(context.Background(), store, projectID, uid)
		if !admin && !pm {
			ack(util.Fail("无权限：仅公司管理员或项目 PM/组长可创建任务"))
			return
		}
		if title == "" {
			ack(util.Fail("任务名称不能为空"))
			return
		}
		if startTime == "" || dueTime == "" {
			ack(util.Fail("开始/预期结束时间不能为空"))
			return
		}
		// TS: Date.parse(dueTime) < Date.parse(startTime) —— 两个时间均已被 asIso 校验合法
		if dueTime < startTime {
			ack(util.Fail("预期结束时间不能早于开始时间"))
			return
		}
		task, err := store.CreateTask(context.Background(), db.CreateTaskInput{
			CompanyID: companyID, ProjectID: projectID, Title: title, Description: description,
			StartTime: startTime, DueTime: dueTime, Images: images, CreatedBy: uid,
		})
		if err != nil {
			logger.Error(mod, "task:create 失败", "companyId", companyID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		// 初始指派（TS: for ... if (u > 0) await addAssignment(...)）
		if arr, ok := asArray(m["assignments"]); ok {
			for _, item := range arr {
				o, _ := item.(map[string]any)
				u := asInt(o["userId"])
				if u > 0 {
					content := asStr(o["content"], 500)
					if _, err := store.AddAssignment(context.Background(), db.AddAssignmentInput{TaskID: task.ID, UserID: u, Content: content}); err != nil {
						logger.Error(mod, "task:create 追加指派失败", "taskId", task.ID, "userId", u, "error", err.Error())
					}
				}
			}
		}
		_ = srv.Broadcast("task:tasksUpdated", map[string]any{"companyId": companyID})
		ack(util.OK(map[string]any{"task": task}))
	})

	srv.On("task:update", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		uid, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		taskID := asInt(m["taskId"])
		t, err := store.GetTask(context.Background(), taskID)
		if err != nil {
			logger.Error(mod, "task:update 查询任务失败", "taskId", taskID, "error", err.Error())
		}
		if t == nil {
			ack(util.Fail("任务不存在"))
			return
		}
		if memberRole(context.Background(), store, t.CompanyID, uid) == "" {
			ack(util.Fail("非本公司成员"))
			return
		}
		admin := isCompanyAdmin(context.Background(), store, t.CompanyID, uid)
		pm := isProjectManager(context.Background(), store, t.ProjectID, uid)
		if !admin && !pm && t.CreatedBy != uid {
			ack(util.Fail("无权限"))
			return
		}
		// 仅收录「客户端显式传了」的键（TS: d.title !== undefined 才入 v）
		in := db.UpdateTaskInput{ID: taskID}
		changed := false
		if v, exists := m["title"]; exists {
			s := asStr(v, 128)
			in.Title = &s
			changed = true
		}
		if v, exists := m["description"]; exists {
			s := asStr(v, 2000)
			in.Description = &s
			changed = true
		}
		if v, exists := m["startTime"]; exists {
			s := asIso(v)
			in.StartTime = &s
			changed = true
		}
		if v, exists := m["dueTime"]; exists {
			s := asIso(v)
			in.DueTime = &s
			changed = true
		}
		if v, exists := m["images"]; exists {
			// TS: 仅当 Array.isArray(d.images) 才入 v（images:null 等非数组不更新该字段）
			if arr, ok := asArray(v); ok {
				out := []string{}
				for _, x := range arr {
					if s, isStr := x.(string); isStr {
						out = append(out, s)
					}
				}
				if len(out) > 9 {
					out = out[:9]
				}
				in.Images = out
				changed = true
			}
		}
		if !changed {
			ack(util.Fail("无变更"))
			return
		}
		// TS: if (v.startTime === '' || v.dueTime === '') —— 仅检查本次传入的时间字段
		if in.StartTime != nil && *in.StartTime == "" {
			ack(util.Fail("时间不能为空"))
			return
		}
		if in.DueTime != nil && *in.DueTime == "" {
			ack(util.Fail("时间不能为空"))
			return
		}
		task, err := store.UpdateTask(context.Background(), in)
		if err != nil {
			logger.Error(mod, "task:update 失败", "taskId", taskID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		_ = srv.Broadcast("task:tasksUpdated", map[string]any{"companyId": t.CompanyID})
		ack(util.OK(map[string]any{"task": task}))
	})

	srv.On("task:delete", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		uid, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		taskID := asInt(m["taskId"])
		t, err := store.GetTask(context.Background(), taskID)
		if err != nil {
			logger.Error(mod, "task:delete 查询任务失败", "taskId", taskID, "error", err.Error())
		}
		if t == nil {
			ack(util.Fail("任务不存在"))
			return
		}
		admin := isCompanyAdmin(context.Background(), store, t.CompanyID, uid)
		pm := isProjectManager(context.Background(), store, t.ProjectID, uid)
		if !admin && !pm {
			ack(util.Fail("无权限：仅公司管理员或项目 PM/组长可删除任务"))
			return
		}
		if err := store.DeleteTask(context.Background(), taskID); err != nil {
			logger.Error(mod, "task:delete 失败", "taskId", taskID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		_ = srv.Broadcast("task:tasksUpdated", map[string]any{"companyId": t.CompanyID})
		ack(util.OK(nil))
	})

	srv.On("task:setStatus", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		uid, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		taskID := asInt(m["taskId"])
		status := asStr(m["status"], 32)
		note := asStr(m["note"], 500)
		t, err := store.GetTask(context.Background(), taskID)
		if err != nil {
			logger.Error(mod, "task:setStatus 查询任务失败", "taskId", taskID, "error", err.Error())
		}
		if t == nil {
			ack(util.Fail("任务不存在"))
			return
		}
		if memberRole(context.Background(), store, t.CompanyID, uid) == "" {
			ack(util.Fail("非本公司成员"))
			return
		}
		if !inList(status, taskStatuses) {
			ack(util.Fail("状态不合法"))
			return
		}
		if note == "" {
			ack(util.Fail("流转需填写说明"))
			return
		}
		if !inList(status, transitions[t.Status]) {
			ack(util.Fail("不可从「" + t.Status + "」流转到「" + status + "」"))
			return
		}
		task, err := store.SetTaskStatus(context.Background(), db.SetTaskStatusInput{TaskID: taskID, Status: status, UserID: uid, Note: note})
		if err != nil {
			logger.Error(mod, "task:setStatus 失败", "taskId", taskID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		_ = srv.Broadcast("task:tasksUpdated", map[string]any{"companyId": t.CompanyID})
		ack(util.OK(map[string]any{"task": task}))
	})

	srv.On("task:setReminder", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		uid, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		taskID := asInt(m["taskId"])
		reminderYellow := asInt(m["reminderYellow"])
		reminderRed := asInt(m["reminderRed"])
		t, err := store.GetTask(context.Background(), taskID)
		if err != nil {
			logger.Error(mod, "task:setReminder 查询任务失败", "taskId", taskID, "error", err.Error())
		}
		if t == nil {
			ack(util.Fail("任务不存在"))
			return
		}
		if memberRole(context.Background(), store, t.CompanyID, uid) == "" {
			ack(util.Fail("非本公司成员"))
			return
		}
		admin := isCompanyAdmin(context.Background(), store, t.CompanyID, uid)
		pm := isProjectManager(context.Background(), store, t.ProjectID, uid)
		if !admin && !pm && t.CreatedBy != uid {
			ack(util.Fail("无权限"))
			return
		}
		if reminderRed > reminderYellow {
			ack(util.Fail("红色提醒阈值不能大于黄色"))
			return
		}
		task, err := store.UpdateTaskReminder(context.Background(), db.TaskReminderInput{TaskID: taskID, ReminderYellow: reminderYellow, ReminderRed: reminderRed})
		if err != nil {
			logger.Error(mod, "task:setReminder 失败", "taskId", taskID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		_ = srv.Broadcast("task:tasksUpdated", map[string]any{"companyId": t.CompanyID})
		ack(util.OK(map[string]any{"task": task}))
	})

	// ==================== 分配（执行人） ====================
	srv.On("task:addAssignment", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		uid, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		taskID := asInt(m["taskId"])
		userID := asInt(m["userId"])
		content := asStr(m["content"], 500)
		t, err := store.GetTask(context.Background(), taskID)
		if err != nil {
			logger.Error(mod, "task:addAssignment 查询任务失败", "taskId", taskID, "error", err.Error())
		}
		if t == nil {
			ack(util.Fail("任务不存在"))
			return
		}
		admin := isCompanyAdmin(context.Background(), store, t.CompanyID, uid)
		pm := isProjectManager(context.Background(), store, t.ProjectID, uid)
		if !admin && !pm && t.CreatedBy != uid {
			ack(util.Fail("无权限"))
			return
		}
		if userID <= 0 {
			ack(util.Fail("执行人不能为空"))
			return
		}
		assignment, err := store.AddAssignment(context.Background(), db.AddAssignmentInput{TaskID: taskID, UserID: userID, Content: content})
		if err != nil {
			logger.Error(mod, "task:addAssignment 失败", "taskId", taskID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		_ = srv.Broadcast("task:tasksUpdated", map[string]any{"companyId": t.CompanyID})
		ack(util.OK(map[string]any{"assignment": assignment}))
	})

	srv.On("task:removeAssignment", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		_, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		id := asInt(m["id"])
		// TS 原样：不查任务、不做成员/权限校验，直接删
		if err := store.RemoveAssignment(context.Background(), id); err != nil {
			logger.Error(mod, "task:removeAssignment 失败", "id", id, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		ack(util.OK(nil))
	})

	srv.On("task:setAssignmentStatus", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		uid, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		id := asInt(m["id"])
		status := asStr(m["status"], 32)
		if !inList(status, assignStates) {
			ack(util.Fail("状态不合法"))
			return
		}
		assignment, err := store.SetAssignmentStatus(context.Background(), db.SetAssignmentStatusInput{ID: id, Status: status, UserID: uid})
		if err != nil {
			logger.Error(mod, "task:setAssignmentStatus 失败", "id", id, "error", err.Error())
		}
		// TS: store 返回 null 表示「非本人分配」——基础层需在 store 内校验
		if assignment == nil {
			ack(util.Fail("仅本人可提交分配状态"))
			return
		}
		ack(util.OK(map[string]any{"assignment": assignment}))
	})

	// ==================== 留言 ====================
	srv.On("task:comment", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		uid, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		taskID := asInt(m["taskId"])
		content := asStr(m["content"], 1000)
		// TS: attachments 优先；否则把 images 数组每项包成 { url: x }
		var rawAtt []any
		if arr, exists := asArray(m["attachments"]); exists {
			rawAtt = arr
		} else if arr, exists := asArray(m["images"]); exists {
			for _, x := range arr {
				rawAtt = append(rawAtt, map[string]any{"url": x})
			}
		}
		// TS: rawAtt.slice(0,9).map(...).filter(Boolean)
		atts := []types.TaskAttachment{}
		n := len(rawAtt)
		if n > 9 {
			n = 9
		}
		for _, x := range rawAtt[:n] {
			o, _ := x.(map[string]any)
			url := ""
			if s, isStr := o["url"].(string); isStr {
				url = strings.TrimSpace(s)
				if len(url) > 500 {
					url = url[:500]
				}
			}
			if url == "" {
				continue // TS: 无 url → null → filter 掉
			}
			kind, _ := o["kind"].(string)
			if !inList(kind, attachmentKinds) {
				kind = "file"
			}
			name, _ := o["name"].(string) // TS: 不 trim，直接 slice(0,255)
			if len(name) > 255 {
				name = name[:255]
			}
			atts = append(atts, types.TaskAttachment{Kind: kind, URL: url, Name: name})
		}
		t, err := store.GetTask(context.Background(), taskID)
		if err != nil {
			logger.Error(mod, "task:comment 查询任务失败", "taskId", taskID, "error", err.Error())
		}
		if t == nil {
			ack(util.Fail("任务不存在"))
			return
		}
		if memberRole(context.Background(), store, t.CompanyID, uid) == "" {
			ack(util.Fail("非本公司成员"))
			return
		}
		if content == "" && len(atts) == 0 {
			ack(util.Fail("留言不能为空"))
			return
		}
		comment, err := store.AddTaskComment(context.Background(), db.AddTaskCommentInput{TaskID: taskID, UserID: uid, Content: content, Attachments: atts})
		if err != nil {
			logger.Error(mod, "task:comment 失败", "taskId", taskID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		_ = srv.Broadcast("task:tasksUpdated", map[string]any{"companyId": t.CompanyID})
		ack(util.OK(map[string]any{"comment": comment}))
	})

	// ==================== 问题（QA） ====================
	srv.On("task:addIssue", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		uid, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		taskID := asInt(m["taskId"])
		title := asStr(m["title"], 128)
		content := asStr(m["content"], 1000)
		t, err := store.GetTask(context.Background(), taskID)
		if err != nil {
			logger.Error(mod, "task:addIssue 查询任务失败", "taskId", taskID, "error", err.Error())
		}
		if t == nil {
			ack(util.Fail("任务不存在"))
			return
		}
		if memberRole(context.Background(), store, t.CompanyID, uid) == "" {
			ack(util.Fail("非本公司成员"))
			return
		}
		if title == "" {
			ack(util.Fail("问题标题不能为空"))
			return
		}
		issue, err := store.AddTaskIssue(context.Background(), db.AddTaskIssueInput{TaskID: taskID, UserID: uid, Title: title, Content: content})
		if err != nil {
			logger.Error(mod, "task:addIssue 失败", "taskId", taskID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		_ = srv.Broadcast("task:tasksUpdated", map[string]any{"companyId": t.CompanyID})
		ack(util.OK(map[string]any{"issue": issue}))
	})

	srv.On("task:resolveIssue", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		_, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		issueID := asInt(m["issueId"])
		issue, err := store.ResolveTaskIssue(context.Background(), issueID)
		if err != nil {
			logger.Error(mod, "task:resolveIssue 失败", "issueId", issueID, "error", err.Error())
		}
		if issue == nil {
			ack(util.Fail("问题不存在"))
			return
		}
		ack(util.OK(map[string]any{"issue": issue}))
	})

	// ==================== 延期 ====================
	srv.On("task:requestExtension", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		uid, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		taskID := asInt(m["taskId"])
		requestedDueTime := asIso(m["requestedDueTime"])
		reason := asStr(m["reason"], 500)
		t, err := store.GetTask(context.Background(), taskID)
		if err != nil {
			logger.Error(mod, "task:requestExtension 查询任务失败", "taskId", taskID, "error", err.Error())
		}
		if t == nil {
			ack(util.Fail("任务不存在"))
			return
		}
		if memberRole(context.Background(), store, t.CompanyID, uid) == "" {
			ack(util.Fail("非本公司成员"))
			return
		}
		if requestedDueTime == "" {
			ack(util.Fail("新截止时间不能为空"))
			return
		}
		if reason == "" {
			ack(util.Fail("请填写延期原因"))
			return
		}
		extension, err := store.RequestTaskExtension(context.Background(), db.RequestExtensionInput{TaskID: taskID, UserID: uid, RequestedDueTime: requestedDueTime, Reason: reason})
		if err != nil {
			logger.Error(mod, "task:requestExtension 失败", "taskId", taskID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		_ = srv.Broadcast("task:tasksUpdated", map[string]any{"companyId": t.CompanyID})
		ack(util.OK(map[string]any{"extension": extension}))
	})

	srv.On("task:decideExtension", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		uid, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		companyID := asInt(m["companyId"])
		taskID := asInt(m["taskId"])
		extensionID := asInt(m["extensionId"])
		approved := asBool(m["approved"])
		if memberRole(context.Background(), store, companyID, uid) == "" {
			ack(util.Fail("非本公司成员"))
			return
		}
		t, err := store.GetTask(context.Background(), taskID)
		if err != nil {
			logger.Error(mod, "task:decideExtension 查询任务失败", "taskId", taskID, "error", err.Error())
		}
		if t == nil {
			ack(util.Fail("任务不存在"))
			return
		}
		admin := isCompanyAdmin(context.Background(), store, companyID, uid)
		pm := isProjectManager(context.Background(), store, t.ProjectID, uid)
		if !admin && !pm {
			ack(util.Fail("无权限：仅公司管理员或项目 PM/组长可审批"))
			return
		}
		extension, err := store.DecideTaskExtension(context.Background(), db.DecideExtensionInput{ExtensionID: extensionID, Approved: approved, DecidedBy: uid})
		if err != nil {
			logger.Error(mod, "task:decideExtension 失败", "extensionId", extensionID, "error", err.Error())
		}
		if extension == nil {
			ack(util.Fail("延期申请不存在"))
			return
		}
		_ = srv.Broadcast("task:tasksUpdated", map[string]any{"companyId": companyID})
		ack(util.OK(map[string]any{"extension": extension}))
	})
}

// ==================== 内部小工具（对标 task.ts 顶部的 authed/str/toIsoStr/int/memberRoleOf/isCompanyAdmin/isProjectManager） ====================

// authed 取连接上的登录身份（对标 TS: socket.data.auth?.id）。
// 认证中间件以 *types.Auth / types.Auth 写入 conn.Set("auth", ...)。
func authed(c socketapi.Conn) (int64, bool) {
	v := c.Get("auth")
	switch a := v.(type) {
	case *types.Auth:
		if a != nil {
			return a.ID, true
		}
	case types.Auth:
		return a.ID, true
	}
	return 0, false
}

// parseData 把事件负载 JSON 字节解析为 map；null/空/非法一律给空 map（对标 TS: data ?? {}）。
func parseData(raw json.RawMessage) map[string]any {
	m := map[string]any{}
	if len(raw) == 0 {
		return m
	}
	_ = json.Unmarshal(raw, &m)
	return m
}

// asStr 对标 TS str(v, max)：字符串 → trim + 截断到 max；否则空串。
func asStr(v any, max int) string {
	s, _ := v.(string)
	s = strings.TrimSpace(s)
	if len(s) > max {
		s = s[:max]
	}
	return s
}

// asInt 对标 TS int(v)：Number(v) 有限则 max(0, floor)，否则 0。
func asInt(v any) int64 {
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) || n <= 0 {
			return 0
		}
		return int64(n)
	case float32:
		if n <= 0 {
			return 0
		}
		return int64(n)
	case int:
		if n < 0 {
			return 0
		}
		return int64(n)
	case int64:
		if n < 0 {
			return 0
		}
		return n
	case string:
		x, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		if err != nil || math.IsNaN(x) || x <= 0 {
			return 0
		}
		return int64(x)
	}
	return 0
}

// asBool 对标 TS Boolean(v)：true / 非零数字 / 非空字符串 为真。
func asBool(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case float64:
		return b != 0
	case string:
		return b != ""
	}
	return false
}

// asIso 对标 TS toIsoStr(v)：合法时间串 → UTC ISO（对齐 JS toISOString 的 .000Z）；否则空串。
func asIso(v any) string {
	s, _ := v.(string)
	if s == "" {
		return ""
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Format("2006-01-02T15:04:05.000Z")
		}
	}
	return ""
}

// asStringSlice 对标 TS images.filter(isString).slice(0, max)：仅保留字符串项，截断到 max。
func asStringSlice(v any, max int) []string {
	arr, ok := v.([]any)
	if !ok {
		return []string{}
	}
	out := []string{}
	for _, x := range arr {
		if s, isStr := x.(string); isStr {
			out = append(out, s)
		}
	}
	if len(out) > max {
		out = out[:max]
	}
	return out
}

// asArray 对标 TS Array.isArray(v)。
func asArray(v any) ([]any, bool) {
	arr, ok := v.([]any)
	return arr, ok
}

// truthy 对标 JS 真值判断（用于 d.projectId ? int : null 这类三态）。
func truthy(v any) bool {
	switch b := v.(type) {
	case nil:
		return false
	case bool:
		return b
	case float64:
		return b != 0
	case string:
		return b != ""
	}
	return true
}

// inList 判断 s 是否在列表中（对标 TS arr.includes(s)）。
func inList(s string, list []string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// memberRoleOf 对标 TS memberRoleOf：companyID<=0 → ""（非成员）；否则返回角色串（非成员 store 返回 ""）。
func memberRoleOf(ctx context.Context, store db.Store, companyID, userID int64) string {
	if companyID <= 0 {
		return ""
	}
	role, err := store.GetMemberRole(ctx, companyID, userID)
	if err != nil {
		logger.Error(mod, "查询成员角色失败", "companyId", companyID, "userId", userID, "error", err.Error())
		return ""
	}
	return role
}

// memberRole 是 memberRoleOf 的短名（handler 内调用点更贴近 TS）。
func memberRole(ctx context.Context, store db.Store, companyID, userID int64) string {
	return memberRoleOf(ctx, store, companyID, userID)
}

// isCompanyAdmin 对标 TS：owner 或 admin。
func isCompanyAdmin(ctx context.Context, store db.Store, companyID, userID int64) bool {
	role := memberRoleOf(ctx, store, companyID, userID)
	return role == "owner" || role == "admin"
}

// isProjectManager 对标 TS：项目成员中 role 为 pm/leader。
func isProjectManager(ctx context.Context, store db.Store, projectID *int64, userID int64) bool {
	if projectID == nil || *projectID <= 0 {
		return false
	}
	members, err := store.ListProjectMembers(ctx, *projectID)
	if err != nil {
		logger.Error(mod, "查询项目成员失败", "projectId", *projectID, "error", err.Error())
		return false
	}
	for _, m := range members {
		if m.UserID == userID {
			return m.Role == "pm" || m.Role == "leader"
		}
	}
	return false
}
