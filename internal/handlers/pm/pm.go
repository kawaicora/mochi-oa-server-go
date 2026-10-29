// Package pm 是「TAPD 项目管理」socket 事件处理器，1:1 复刻自
// 源仓库 server/src/handlers/project-mgmt.ts（需求 / 缺陷 / 计划 / 文档 / Wiki / 仪表盘）。
//
// 行为对齐要点：
//   - 25 个入站事件：req:*(6) + bug:*(5) + plan:*(4) + doc:*(4) + wiki:*(4) + pm:*(2)，
//     事件名、字段名（camelCase）、ack 信封（util.OK/Fail 扁平）逐字一致；
//   - 每次数据变更后广播 req:updated / bug:updated / plan:updated / doc:updated / wiki:updated，
//     负载 { companyId, projectId }（projectId 为 null 表示未挂项目）——客户端靠它们刷新列表；
//   - 鉴权链（未登录 → 非本公司成员 → 公司管理员 / 处理人）与 TS 完全一致；
//   - 错误文案与 TS 逐字一致；存储层意外错误必打日志、绝不吞掉。
//
// 注册方式：Register(srv, store)；store 必须是基础层 db.GetStore() 的单例，本包不新建连接。
package pm

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
const mod = "handlers/pm"

// 与 TS 对齐的状态白名单（project-mgmt.ts 顶部 REQ_STATUS / BUG_STATUS）。
var (
	reqStatuses = []string{"planning", "in_progress", "done", "closed"}
	bugStatuses = []string{"pending", "processing", "verified", "closed"}
)

// Register 注册全部 PM 域事件处理器（对标 TS registerProjectMgmtHandlers(ctx)，ctx.io/ctx.store 由参数注入）。
func Register(srv socketapi.Server, store db.Store) {
	// ==================== 需求 ====================
	srv.On("req:list", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		_, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		companyID := asInt(m["companyId"])
		if _, ok := guardCompany(context.Background(), store, c, companyID, ack); !ok {
			return
		}
		requirements, err := store.ListRequirements(context.Background(), companyID, projectFilter(m))
		if err != nil {
			logger.Error(mod, "req:list 查询失败", "companyId", companyID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		if requirements == nil {
			requirements = []types.Requirement{}
		}
		ack(util.OK(map[string]any{"requirements": requirements}))
	})

	srv.On("req:create", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		uid, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		companyID := asInt(m["companyId"])
		if _, ok := guardCompany(context.Background(), store, c, companyID, ack); !ok {
			return
		}
		title := asStr(m["title"], 128)
		if title == "" {
			ack(util.Fail("标题不能为空"))
			return
		}
		st := asIso(m["startTime"])
		dt := asIso(m["dueTime"])
		if st == "" || dt == "" {
			ack(util.Fail("请填写开始与结束时间"))
			return
		}
		// TS: Date.parse(due) < Date.parse(start) —— 两个时间均已被 asIso 校验为同一格式，字符串序即时间序。
		if dt < st {
			ack(util.Fail("结束时间不能早于开始"))
			return
		}
		requirement, err := store.CreateRequirement(context.Background(), db.CreateRequirementInput{
			CompanyID:   companyID,
			ProjectID:   projectFilter(m),
			Title:       title,
			Description: asStr(m["description"], 8000),
			Category:    asStr(m["category"], 32),
			Priority:    asStr(m["priority"], 32),
			HandlerID:   intPtr(asInt(m["handlerId"])), // TS: int(d.handlerId) || null
			StartTime:   st,
			DueTime:     dt,
			CreatorID:   uid,
		})
		if err != nil {
			logger.Error(mod, "req:create 失败", "companyId", companyID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		// TS: 仅当传入了非空 taskIds 数组时关联任务；关联失败不吞，记日志并按存储错误回失败。
		if taskIDs := asIntArr(m["taskIds"]); len(taskIDs) > 0 {
			if _, err := store.LinkRequirementTasks(context.Background(), db.LinkRequirementTasksInput{
				RequirementID: requirement.ID, TaskIDs: taskIDs, UserID: uid, CompanyID: companyID,
			}); err != nil {
				logger.Error(mod, "req:create 关联任务失败", "requirementId", requirement.ID, "error", err.Error())
				ack(util.Fail(err.Error()))
				return
			}
		}
		_ = srv.Broadcast("req:updated", map[string]any{"companyId": companyID, "projectId": requirement.ProjectID})
		ack(util.OK(map[string]any{"requirement": requirement}))
	})

	srv.On("req:update", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		uid, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		req, err := store.GetRequirement(context.Background(), asInt(m["id"]))
		if err != nil {
			logger.Error(mod, "req:update 查询需求失败", "id", asInt(m["id"]), "error", err.Error())
		}
		if req == nil {
			ack(util.Fail("需求不存在"))
			return
		}
		if !isAdmin(memberRoleOf(context.Background(), store, req.CompanyID, uid)) {
			ack(util.Fail("仅管理员可编辑"))
			return
		}
		// 仅收录「客户端显式传了」的键（TS: d.title !== undefined 才入 v）；
		// toIsoStr 解析失败（''）按 TS 的 `|| undefined` 语义跳过该字段。
		in := db.UpdateRequirementInput{ID: req.ID}
		if v, exists := m["title"]; exists {
			s := asStr(v, 128)
			in.Title = &s
		}
		if v, exists := m["description"]; exists {
			s := asStr(v, 8000)
			in.Description = &s
		}
		if v, exists := m["category"]; exists {
			s := asStr(v, 32)
			in.Category = &s
		}
		if v, exists := m["priority"]; exists {
			s := asStr(v, 32)
			in.Priority = &s
		}
		if v, exists := m["handlerId"]; exists {
			in.HandlerID = intPtr(asInt(v)) // TS: int(d.handlerId) || null
		}
		if v, exists := m["startTime"]; exists {
			if s := asIso(v); s != "" {
				in.StartTime = &s
			}
		}
		if v, exists := m["dueTime"]; exists {
			if s := asIso(v); s != "" {
				in.DueTime = &s
			}
		}
		updated, err := store.UpdateRequirement(context.Background(), in)
		if err != nil {
			logger.Error(mod, "req:update 失败", "id", req.ID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		_ = srv.Broadcast("req:updated", map[string]any{"companyId": req.CompanyID, "projectId": req.ProjectID})
		ack(util.OK(map[string]any{"requirement": updated}))
	})

	srv.On("req:status", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		uid, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		req, err := store.GetRequirement(context.Background(), asInt(m["id"]))
		if err != nil {
			logger.Error(mod, "req:status 查询需求失败", "id", asInt(m["id"]), "error", err.Error())
		}
		if req == nil {
			ack(util.Fail("需求不存在"))
			return
		}
		status := asStr(m["status"], 32)
		if !inList(status, reqStatuses) {
			ack(util.Fail("非法状态"))
			return
		}
		role := memberRoleOf(context.Background(), store, req.CompanyID, uid)
		handler := req.HandlerID != nil && *req.HandlerID == uid
		if !isAdmin(role) && !handler {
			ack(util.Fail("仅管理员或处理人可流转"))
			return
		}
		updated, err := store.SetRequirementStatus(context.Background(), db.SetRequirementStatusInput{
			RequirementID: req.ID, Status: status, UserID: uid,
		})
		if err != nil {
			logger.Error(mod, "req:status 失败", "id", req.ID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		_ = srv.Broadcast("req:updated", map[string]any{"companyId": req.CompanyID, "projectId": req.ProjectID})
		ack(util.OK(map[string]any{"requirement": updated}))
	})

	srv.On("req:delete", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		uid, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		req, err := store.GetRequirement(context.Background(), asInt(m["id"]))
		if err != nil {
			logger.Error(mod, "req:delete 查询需求失败", "id", asInt(m["id"]), "error", err.Error())
		}
		if req == nil {
			ack(util.Fail("需求不存在"))
			return
		}
		if !isAdmin(memberRoleOf(context.Background(), store, req.CompanyID, uid)) {
			ack(util.Fail("仅管理员可删除"))
			return
		}
		if err := store.DeleteRequirement(context.Background(), req.ID); err != nil {
			logger.Error(mod, "req:delete 失败", "id", req.ID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		_ = srv.Broadcast("req:updated", map[string]any{"companyId": req.CompanyID, "projectId": req.ProjectID})
		ack(util.OK(nil))
	})

	srv.On("req:linkTasks", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		uid, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		req, err := store.GetRequirement(context.Background(), asInt(m["id"]))
		if err != nil {
			logger.Error(mod, "req:linkTasks 查询需求失败", "id", asInt(m["id"]), "error", err.Error())
		}
		if req == nil {
			ack(util.Fail("需求不存在"))
			return
		}
		if !isAdmin(memberRoleOf(context.Background(), store, req.CompanyID, uid)) {
			ack(util.Fail("仅管理员可关联任务"))
			return
		}
		// TS: taskIds 即便为空数组也照调 linkRequirementTasks（先清后写）。
		taskIDs := asIntArr(m["taskIds"])
		linked, err := store.LinkRequirementTasks(context.Background(), db.LinkRequirementTasksInput{
			RequirementID: req.ID, TaskIDs: taskIDs, UserID: uid, CompanyID: req.CompanyID,
		})
		if err != nil {
			logger.Error(mod, "req:linkTasks 失败", "id", req.ID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		if linked == nil {
			linked = []int64{}
		}
		_ = srv.Broadcast("req:updated", map[string]any{"companyId": req.CompanyID, "projectId": req.ProjectID})
		ack(util.OK(map[string]any{"linkedTaskIds": linked}))
	})

	// ==================== 缺陷 ====================
	srv.On("bug:list", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		_, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		companyID := asInt(m["companyId"])
		if _, ok := guardCompany(context.Background(), store, c, companyID, ack); !ok {
			return
		}
		bugs, err := store.ListBugs(context.Background(), companyID, projectFilter(m))
		if err != nil {
			logger.Error(mod, "bug:list 查询失败", "companyId", companyID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		if bugs == nil {
			bugs = []types.Bug{}
		}
		ack(util.OK(map[string]any{"bugs": bugs}))
	})

	srv.On("bug:create", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		uid, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		companyID := asInt(m["companyId"])
		if _, ok := guardCompany(context.Background(), store, c, companyID, ack); !ok {
			return
		}
		title := asStr(m["title"], 128)
		if title == "" {
			ack(util.Fail("标题不能为空"))
			return
		}
		bug, err := store.CreateBug(context.Background(), db.CreateBugInput{
			CompanyID:     companyID,
			ProjectID:     projectFilter(m),
			RequirementID: intPtr(asInt(m["requirementId"])), // TS: int(...) || null
			Title:         title,
			Description:   asStr(m["description"], 8000),
			Severity:      asStr(m["severity"], 32),
			Priority:      asStr(m["priority"], 32),
			HandlerID:     intPtr(asInt(m["handlerId"])),
			FoundVersion:  asStr(m["foundVersion"], 64),
			CreatorID:     uid,
		})
		if err != nil {
			logger.Error(mod, "bug:create 失败", "companyId", companyID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		_ = srv.Broadcast("bug:updated", map[string]any{"companyId": companyID, "projectId": bug.ProjectID})
		ack(util.OK(map[string]any{"bug": bug}))
	})

	srv.On("bug:update", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		uid, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		bug, err := store.GetBug(context.Background(), asInt(m["id"]))
		if err != nil {
			logger.Error(mod, "bug:update 查询缺陷失败", "id", asInt(m["id"]), "error", err.Error())
		}
		if bug == nil {
			ack(util.Fail("缺陷不存在"))
			return
		}
		if !isAdmin(memberRoleOf(context.Background(), store, bug.CompanyID, uid)) {
			ack(util.Fail("仅管理员可编辑"))
			return
		}
		in := db.UpdateBugInput{ID: bug.ID}
		if v, exists := m["title"]; exists {
			s := asStr(v, 128)
			in.Title = &s
		}
		if v, exists := m["description"]; exists {
			s := asStr(v, 8000)
			in.Description = &s
		}
		if v, exists := m["severity"]; exists {
			s := asStr(v, 32)
			in.Severity = &s
		}
		if v, exists := m["priority"]; exists {
			s := asStr(v, 32)
			in.Priority = &s
		}
		if v, exists := m["handlerId"]; exists {
			in.HandlerID = intPtr(asInt(v))
		}
		if v, exists := m["foundVersion"]; exists {
			s := asStr(v, 64)
			in.FoundVersion = &s
		}
		updated, err := store.UpdateBug(context.Background(), in)
		if err != nil {
			logger.Error(mod, "bug:update 失败", "id", bug.ID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		_ = srv.Broadcast("bug:updated", map[string]any{"companyId": bug.CompanyID, "projectId": bug.ProjectID})
		ack(util.OK(map[string]any{"bug": updated}))
	})

	srv.On("bug:status", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		uid, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		bug, err := store.GetBug(context.Background(), asInt(m["id"]))
		if err != nil {
			logger.Error(mod, "bug:status 查询缺陷失败", "id", asInt(m["id"]), "error", err.Error())
		}
		if bug == nil {
			ack(util.Fail("缺陷不存在"))
			return
		}
		status := asStr(m["status"], 32)
		if !inList(status, bugStatuses) {
			ack(util.Fail("非法状态"))
			return
		}
		role := memberRoleOf(context.Background(), store, bug.CompanyID, uid)
		handler := bug.HandlerID != nil && *bug.HandlerID == uid
		if !isAdmin(role) && !handler {
			ack(util.Fail("仅管理员或处理人可流转"))
			return
		}
		updated, err := store.SetBugStatus(context.Background(), db.SetBugStatusInput{
			BugID: bug.ID, Status: status, UserID: uid,
		})
		if err != nil {
			logger.Error(mod, "bug:status 失败", "id", bug.ID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		_ = srv.Broadcast("bug:updated", map[string]any{"companyId": bug.CompanyID, "projectId": bug.ProjectID})
		ack(util.OK(map[string]any{"bug": updated}))
	})

	srv.On("bug:delete", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		uid, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		bug, err := store.GetBug(context.Background(), asInt(m["id"]))
		if err != nil {
			logger.Error(mod, "bug:delete 查询缺陷失败", "id", asInt(m["id"]), "error", err.Error())
		}
		if bug == nil {
			ack(util.Fail("缺陷不存在"))
			return
		}
		if !isAdmin(memberRoleOf(context.Background(), store, bug.CompanyID, uid)) {
			ack(util.Fail("仅管理员可删除"))
			return
		}
		if err := store.DeleteBug(context.Background(), bug.ID); err != nil {
			logger.Error(mod, "bug:delete 失败", "id", bug.ID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		_ = srv.Broadcast("bug:updated", map[string]any{"companyId": bug.CompanyID, "projectId": bug.ProjectID})
		ack(util.OK(nil))
	})

	// ==================== 计划 ====================
	srv.On("plan:list", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		_, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		companyID := asInt(m["companyId"])
		if _, ok := guardCompany(context.Background(), store, c, companyID, ack); !ok {
			return
		}
		plans, err := store.ListPlans(context.Background(), companyID, projectFilter(m))
		if err != nil {
			logger.Error(mod, "plan:list 查询失败", "companyId", companyID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		if plans == nil {
			plans = []types.Plan{}
		}
		ack(util.OK(map[string]any{"plans": plans}))
	})

	srv.On("plan:create", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		uid, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		companyID := asInt(m["companyId"])
		role, ok := guardCompany(context.Background(), store, c, companyID, ack)
		if !ok {
			return
		}
		if !isAdmin(role) { // TS: plan:create 额外要求公司管理员
			ack(util.Fail("仅管理员可创建计划"))
			return
		}
		name := asStr(m["name"], 128)
		if name == "" {
			ack(util.Fail("计划名称不能为空"))
			return
		}
		st := asIso(m["startTime"])
		dt := asIso(m["dueTime"])
		if st == "" || dt == "" {
			ack(util.Fail("请填写开始与结束时间"))
			return
		}
		if dt < st {
			ack(util.Fail("结束时间不能早于开始"))
			return
		}
		plan, err := store.CreatePlan(context.Background(), db.CreatePlanInput{
			CompanyID:   companyID,
			ProjectID:   projectFilter(m),
			Name:        name,
			Description: asStr(m["description"], 8000),
			StartTime:   st,
			DueTime:     dt,
			CreatorID:   uid,
		})
		if err != nil {
			logger.Error(mod, "plan:create 失败", "companyId", companyID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		_ = srv.Broadcast("plan:updated", map[string]any{"companyId": companyID, "projectId": plan.ProjectID})
		ack(util.OK(map[string]any{"plan": plan}))
	})

	srv.On("plan:update", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		uid, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		planID := asInt(m["id"])
		// TS 原样：store 无按 id 取 plan 的方法，listPlans(0) 全量拉取后在内存里找归属。
		all, err := store.ListPlans(context.Background(), 0, nil)
		if err != nil {
			logger.Error(mod, "plan:update 查询计划失败", "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		target := findPlan(all, planID)
		if target == nil {
			ack(util.Fail("计划不存在"))
			return
		}
		if !isAdmin(memberRoleOf(context.Background(), store, target.CompanyID, uid)) {
			ack(util.Fail("仅管理员可编辑"))
			return
		}
		in := db.UpdatePlanInput{ID: planID}
		if v, exists := m["name"]; exists {
			s := asStr(v, 128)
			in.Name = &s
		}
		if v, exists := m["description"]; exists {
			s := asStr(v, 8000)
			in.Description = &s
		}
		if v, exists := m["startTime"]; exists {
			if s := asIso(v); s != "" {
				in.StartTime = &s
			}
		}
		if v, exists := m["dueTime"]; exists {
			if s := asIso(v); s != "" {
				in.DueTime = &s
			}
		}
		if v, exists := m["status"]; exists {
			s := asStr(v, 32)
			in.Status = &s
		}
		updated, err := store.UpdatePlan(context.Background(), in)
		if err != nil {
			logger.Error(mod, "plan:update 失败", "id", planID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		_ = srv.Broadcast("plan:updated", map[string]any{"companyId": target.CompanyID, "projectId": target.ProjectID})
		ack(util.OK(map[string]any{"plan": updated}))
	})

	srv.On("plan:delete", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		uid, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		planID := asInt(m["id"])
		all, err := store.ListPlans(context.Background(), 0, nil)
		if err != nil {
			logger.Error(mod, "plan:delete 查询计划失败", "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		target := findPlan(all, planID)
		if target == nil {
			ack(util.Fail("计划不存在"))
			return
		}
		if !isAdmin(memberRoleOf(context.Background(), store, target.CompanyID, uid)) {
			ack(util.Fail("仅管理员可删除"))
			return
		}
		if err := store.DeletePlan(context.Background(), planID); err != nil {
			logger.Error(mod, "plan:delete 失败", "id", planID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		_ = srv.Broadcast("plan:updated", map[string]any{"companyId": target.CompanyID, "projectId": target.ProjectID})
		ack(util.OK(nil))
	})

	// ==================== 文档 ====================
	srv.On("doc:list", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		_, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		companyID := asInt(m["companyId"])
		if _, ok := guardCompany(context.Background(), store, c, companyID, ack); !ok {
			return
		}
		docs, err := store.ListDocuments(context.Background(), companyID, projectFilter(m))
		if err != nil {
			logger.Error(mod, "doc:list 查询失败", "companyId", companyID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		if docs == nil {
			docs = []types.ProjectDocument{}
		}
		ack(util.OK(map[string]any{"docs": docs}))
	})

	srv.On("doc:create", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		uid, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		companyID := asInt(m["companyId"])
		if _, ok := guardCompany(context.Background(), store, c, companyID, ack); !ok { // TS: 仅校验成员身份，不要求管理员
			return
		}
		title := asStr(m["title"], 128)
		if title == "" {
			ack(util.Fail("标题不能为空"))
			return
		}
		doc, err := store.CreateDocument(context.Background(), db.CreateDocumentInput{
			CompanyID: companyID, ProjectID: projectFilter(m),
			Title: title, Content: asStr(m["content"], 100000), CreatorID: uid,
		})
		if err != nil {
			logger.Error(mod, "doc:create 失败", "companyId", companyID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		_ = srv.Broadcast("doc:updated", map[string]any{"companyId": companyID, "projectId": doc.ProjectID})
		ack(util.OK(map[string]any{"doc": doc}))
	})

	srv.On("doc:update", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		uid, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		docID := asInt(m["id"])
		// TS 原样：listDocuments(0) 全量拉取后内存找。
		docs, err := store.ListDocuments(context.Background(), 0, nil)
		if err != nil {
			logger.Error(mod, "doc:update 查询文档失败", "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		target := findDoc(docs, docID)
		if target == nil {
			ack(util.Fail("文档不存在"))
			return
		}
		if !isAdmin(memberRoleOf(context.Background(), store, target.CompanyID, uid)) {
			ack(util.Fail("仅管理员可编辑"))
			return
		}
		in := db.UpdateDocumentInput{ID: docID}
		if v, exists := m["title"]; exists {
			s := asStr(v, 128)
			in.Title = &s
		}
		if v, exists := m["content"]; exists {
			s := asStr(v, 100000)
			in.Content = &s
		}
		updated, err := store.UpdateDocument(context.Background(), in)
		if err != nil {
			logger.Error(mod, "doc:update 失败", "id", docID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		_ = srv.Broadcast("doc:updated", map[string]any{"companyId": target.CompanyID, "projectId": target.ProjectID})
		ack(util.OK(map[string]any{"doc": updated}))
	})

	srv.On("doc:delete", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		uid, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		docID := asInt(m["id"])
		docs, err := store.ListDocuments(context.Background(), 0, nil)
		if err != nil {
			logger.Error(mod, "doc:delete 查询文档失败", "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		target := findDoc(docs, docID)
		if target == nil {
			ack(util.Fail("文档不存在"))
			return
		}
		if !isAdmin(memberRoleOf(context.Background(), store, target.CompanyID, uid)) {
			ack(util.Fail("仅管理员可删除"))
			return
		}
		if err := store.DeleteDocument(context.Background(), docID); err != nil {
			logger.Error(mod, "doc:delete 失败", "id", docID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		_ = srv.Broadcast("doc:updated", map[string]any{"companyId": target.CompanyID, "projectId": target.ProjectID})
		ack(util.OK(nil))
	})

	// ==================== Wiki ====================
	srv.On("wiki:list", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		_, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		companyID := asInt(m["companyId"])
		if _, ok := guardCompany(context.Background(), store, c, companyID, ack); !ok {
			return
		}
		pages, err := store.ListWikiPages(context.Background(), companyID, projectFilter(m))
		if err != nil {
			logger.Error(mod, "wiki:list 查询失败", "companyId", companyID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		if pages == nil {
			pages = []types.WikiPage{}
		}
		ack(util.OK(map[string]any{"pages": pages}))
	})

	srv.On("wiki:create", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		uid, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		companyID := asInt(m["companyId"])
		if _, ok := guardCompany(context.Background(), store, c, companyID, ack); !ok { // TS: 仅校验成员身份
			return
		}
		title := asStr(m["title"], 128)
		if title == "" {
			ack(util.Fail("标题不能为空"))
			return
		}
		page, err := store.CreateWikiPage(context.Background(), db.CreateWikiInput{
			CompanyID: companyID, ProjectID: projectFilter(m),
			Title: title, Content: asStr(m["content"], 100000), CreatorID: uid,
		})
		if err != nil {
			logger.Error(mod, "wiki:create 失败", "companyId", companyID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		_ = srv.Broadcast("wiki:updated", map[string]any{"companyId": companyID, "projectId": page.ProjectID})
		ack(util.OK(map[string]any{"page": page}))
	})

	srv.On("wiki:update", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		uid, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		wikiID := asInt(m["id"])
		// TS 原样：listWikiPages(0) 全量拉取后内存找。
		pages, err := store.ListWikiPages(context.Background(), 0, nil)
		if err != nil {
			logger.Error(mod, "wiki:update 查询 Wiki 失败", "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		target := findWiki(pages, wikiID)
		if target == nil {
			ack(util.Fail("Wiki 页面不存在"))
			return
		}
		if !isAdmin(memberRoleOf(context.Background(), store, target.CompanyID, uid)) {
			ack(util.Fail("仅管理员可编辑"))
			return
		}
		in := db.UpdateWikiInput{ID: wikiID}
		if v, exists := m["title"]; exists {
			s := asStr(v, 128)
			in.Title = &s
		}
		if v, exists := m["content"]; exists {
			s := asStr(v, 100000)
			in.Content = &s
		}
		updated, err := store.UpdateWikiPage(context.Background(), in)
		if err != nil {
			logger.Error(mod, "wiki:update 失败", "id", wikiID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		_ = srv.Broadcast("wiki:updated", map[string]any{"companyId": target.CompanyID, "projectId": target.ProjectID})
		ack(util.OK(map[string]any{"page": updated}))
	})

	srv.On("wiki:delete", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		uid, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		wikiID := asInt(m["id"])
		pages, err := store.ListWikiPages(context.Background(), 0, nil)
		if err != nil {
			logger.Error(mod, "wiki:delete 查询 Wiki 失败", "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		target := findWiki(pages, wikiID)
		if target == nil {
			ack(util.Fail("Wiki 页面不存在"))
			return
		}
		if !isAdmin(memberRoleOf(context.Background(), store, target.CompanyID, uid)) {
			ack(util.Fail("仅管理员可删除"))
			return
		}
		if err := store.DeleteWikiPage(context.Background(), wikiID); err != nil {
			logger.Error(mod, "wiki:delete 失败", "id", wikiID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		_ = srv.Broadcast("wiki:updated", map[string]any{"companyId": target.CompanyID, "projectId": target.ProjectID})
		ack(util.OK(nil))
	})

	// ==================== 仪表盘 + 成员跟踪 ====================
	srv.On("pm:dashboard", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		_, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		companyID := asInt(m["companyId"])
		if _, ok := guardCompany(context.Background(), store, c, companyID, ack); !ok {
			return
		}
		stats, err := store.DashboardStats(context.Background(), companyID, projectFilter(m))
		if err != nil {
			logger.Error(mod, "pm:dashboard 查询失败", "companyId", companyID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		ack(util.OK(map[string]any{"stats": stats}))
	})

	srv.On("pm:memberTracking", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		_, ok := authed(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		companyID := asInt(m["companyId"])
		if _, ok := guardCompany(context.Background(), store, c, companyID, ack); !ok {
			return
		}
		members, err := store.MemberTracking(context.Background(), companyID, projectFilter(m))
		if err != nil {
			logger.Error(mod, "pm:memberTracking 查询失败", "companyId", companyID, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		if members == nil {
			members = []types.MemberTrackItem{}
		}
		ack(util.OK(map[string]any{"members": members}))
	})
}

// ==================== 内部小工具（对标 project-mgmt.ts 顶部的 authed/str/toIsoStr/int/intArr/memberRoleOf/isAdmin/guardCompany） ====================

// authed 取连接上的登录身份（对标 TS: socket.data.auth?.id）。
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

// asIntArr 对标 TS intArr(v)：Array.isArray ? map(int).filter(>0) : []。
func asIntArr(v any) []int64 {
	arr, ok := v.([]any)
	if !ok {
		return []int64{}
	}
	out := []int64{}
	for _, x := range arr {
		if n := asInt(x); n > 0 {
			out = append(out, n)
		}
	}
	return out
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
			return util.LocalTimeFormat(t)
		}
	}
	return ""
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

// intPtr 对标 TS `int(v) || null`：>0 转指针，否则 nil。
func intPtr(v int64) *int64 {
	if v <= 0 {
		return nil
	}
	return &v
}

// projectFilter 对标 TS `int(d.projectId) || undefined`：>0 才带项目过滤。
func projectFilter(m map[string]any) *int64 {
	if pid := asInt(m["projectId"]); pid > 0 {
		return &pid
	}
	return nil
}

// memberRoleOf 对标 TS memberRoleOf：companyID<=0 → ""（非成员）；查询出错记日志并按非成员处理。
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

// isAdmin 对标 TS：owner 或 admin。
func isAdmin(role string) bool {
	return role == "owner" || role == "admin"
}

// guardCompany 对标 TS guardCompany：未登录 / 非本公司成员时已 ack 失败并返回 ("", false)；
// 成功返回当前用户在该公司的角色（plan:create 还需据此判管理员）。
func guardCompany(ctx context.Context, store db.Store, c socketapi.Conn, companyID int64, ack socketapi.Ack) (string, bool) {
	uid, ok := authed(c)
	if !ok {
		ack(util.Fail("未登录"))
		return "", false
	}
	role := memberRoleOf(ctx, store, companyID, uid)
	if role == "" {
		ack(util.Fail("非本公司成员"))
		return "", false
	}
	return role, true
}

// findPlan 在全量计划中按 id 定位（对标 TS all.find(p => p.id === planId)）。
func findPlan(all []types.Plan, id int64) *types.Plan {
	for i := range all {
		if all[i].ID == id {
			return &all[i]
		}
	}
	return nil
}

// findDoc 在全量文档中按 id 定位（对标 TS docs.find(x => x.id === docId)）。
func findDoc(all []types.ProjectDocument, id int64) *types.ProjectDocument {
	for i := range all {
		if all[i].ID == id {
			return &all[i]
		}
	}
	return nil
}

// findWiki 在全量 Wiki 页中按 id 定位（对标 TS pages.find(x => x.id === wikiId)）。
func findWiki(all []types.WikiPage, id int64) *types.WikiPage {
	for i := range all {
		if all[i].ID == id {
			return &all[i]
		}
	}
	return nil
}
