// Package advance 实现备用金申请的 socket 事件。
//
// 事件（均 requireMember 公司隔离；审批须落在该条申请 approver_ids 内，含自审批）：
//   - advance:create / list / received:list / cc:list / cc:view
//   - advance:approve / reject / request:notification:stop
package advance

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"mochi-oa-server-go/internal/db"
	"mochi-oa-server-go/internal/logger"
	"mochi-oa-server-go/internal/socketapi"
	"mochi-oa-server-go/internal/types"
	"mochi-oa-server-go/internal/util"
)

// 日志模块前缀。
const mod = "handlers/advance"

// 备用金类型白名单。
var advanceCategories = map[string]struct{}{
	"travel":   {}, // 差旅
	"purchase": {}, // 采购
	"daily":    {}, // 日常
	"project":  {}, // 项目
	"other":    {}, // 其他
}

// ─── 共享小工具（本包私有副本，与 expense/attendance 包同款语义） ───

func getAuth(c socketapi.Conn) *types.Auth {
	v := c.Get("auth")
	if a, ok := v.(*types.Auth); ok && a != nil {
		return a
	}
	return nil
}

func parseData(data json.RawMessage) map[string]any {
	m := map[string]any{}
	if len(data) == 0 {
		return m
	}
	_ = json.Unmarshal(data, &m)
	return m
}

func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		if n != math.Trunc(n) {
			return 0, false
		}
		return int64(n), true
	case string:
		i, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
		if err != nil {
			return 0, false
		}
		return i, true
	}
	return 0, false
}

// toFloat64 把 JSON any 值转 float64。
func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f, err == nil
	}
	return 0, false
}

func strField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func roleRank(role string) int {
	switch role {
	case "owner":
		return 3
	case "admin":
		return 2
	default:
		return 1
	}
}

func requireMember(c socketapi.Conn, d map[string]any, st db.Store, ack socketapi.Ack) (*types.Auth, int64, string, bool) {
	a := getAuth(c)
	if a == nil {
		ack(util.Fail("未登录"))
		return nil, 0, "", false
	}
	cid, ok := toInt64(d["companyId"])
	if !ok || cid <= 0 {
		ack(util.Fail("参数不合法"))
		return nil, 0, "", false
	}
	role, err := st.GetMemberRole(context.Background(), cid, a.ID)
	if err != nil {
		logger.Error(mod, "查询成员角色失败", "companyId", cid, "userId", a.ID, "error", err.Error())
		ack(util.Fail("操作失败"))
		return nil, 0, "", false
	}
	if role == "" {
		ack(util.Fail("未加入该公司"))
		return nil, 0, "", false
	}
	return a, cid, role, true
}

// idList 从 payload 取 []int64；key 缺失/null → 空切片；非法元素 → false。
func idList(m map[string]any, key string) ([]int64, bool) {
	v, present := m[key]
	if !present || v == nil {
		return []int64{}, true
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := []int64{}
	for _, x := range arr {
		n, ok := toInt64(x)
		if !ok || n <= 0 {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// strList 从 payload 取 []string（用于 evidence 附件 URL 数组）。
func strList(m map[string]any, key string) ([]string, bool) {
	v, present := m[key]
	if !present || v == nil {
		return []string{}, true
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := []string{}
	for _, x := range arr {
		s, ok := x.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ─── 注册入口 ───

// RegisterAdvanceHandlers 注册 advance:* 事件。
func RegisterAdvanceHandlers(srv socketapi.Server, store db.Store) {
	ctx := context.Background()

	// ---- advance:create：发起备用金申请 ----
	srv.On("advance:create", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		a, cid, _, ok := requireMember(c, d, store, ack)
		if !ok {
			return
		}
		category := strField(d, "category")
		if _, ok := advanceCategories[category]; !ok {
			ack(util.Fail("备用金类型不合法"))
			return
		}
		amount, ok := toFloat64(d["amount"])
		if !ok {
			ack(util.Fail("金额不合法"))
			return
		}
		if amount <= 0 {
			ack(util.Fail("金额必须大于0"))
			return
		}
		reason := strField(d, "reason")
		if reason == "" {
			ack(util.Fail("事由不能为空"))
			return
		}
		evidence, ok := strList(d, "evidence")
		if !ok {
			logger.Error(mod, "advance:create 凭证附件不合法", "companyId", cid)
			ack(util.Fail("凭证附件不合法"))
			return
		}
		// 审批人逐 id 校验本公司成员。
		approverIDs, ok := idList(d, "approverIds")
		if !ok {
			logger.Error(mod, "advance:create 审批人 id 非法", "companyId", cid)
			ack(util.Fail("审批人不在该公司"))
			return
		}
		for _, uid := range approverIDs {
			role, err := store.GetMemberRole(context.Background(), cid, uid)
			if err != nil || role == "" {
				logger.Error(mod, "advance:create 审批人不在该公司", "companyId", cid, "userId", uid, "err", errStr(err))
				ack(util.Fail("审批人不在该公司"))
				return
			}
		}
		// 抄送人逐 id 校验本公司成员。
		ccIDs, ok := idList(d, "ccIds")
		if !ok {
			logger.Error(mod, "advance:create 抄送人 id 非法", "companyId", cid)
			ack(util.Fail("抄送人不在该公司"))
			return
		}
		for _, uid := range ccIDs {
			role, err := store.GetMemberRole(context.Background(), cid, uid)
			if err != nil || role == "" {
				logger.Error(mod, "advance:create 抄送人不在该公司", "companyId", cid, "userId", uid, "err", errStr(err))
				ack(util.Fail("抄送人不在该公司"))
				return
			}
		}
		req, err := store.CreateAdvanceRequest(ctx, db.CreateAdvanceRequestInput{
			CompanyID:   cid,
			UserID:      a.ID,
			Category:    category,
			Amount:      amount,
			Reason:      reason,
			Evidence:    evidence,
			ApproverIDs: approverIDs,
			CcIDs:       ccIDs,
		})
		if err != nil {
			logger.Error(mod, "advance:create 失败", "companyId", cid, "userId", a.ID, "error", err.Error())
			ack(util.Fail("创建失败"))
			return
		}
		// 通知：审批人全量 + 抄送人（含自审批/自抄送，不去重不排除自己）。
		notifyUsers := append([]int64{}, approverIDs...)
		notifyUsers = append(notifyUsers, ccIDs...)
		for _, uid := range notifyUsers {
			_ = srv.EmitTo(util.UserRoom(uid), "advance:request:notification", map[string]any{
				"kind": "company", "type": "advance_request_created", "companyId": cid,
				"requestId": req.ID, "title": "备用金申请",
				"body":      "您有一条待审批的备用金申请",
				"byUserId":  a.ID, "byNick": req.Nick,
			})
		}
		ack(util.OK(map[string]any{"request": req}))
	})

	// ---- advance:list：本人历史分页 ----
	srv.On("advance:list", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		a, cid, _, ok := requireMember(c, d, store, ack)
		if !ok {
			return
		}
		category := strField(d, "category")
		if category != "" {
			if _, ok := advanceCategories[category]; !ok {
				ack(util.Fail("备用金类型不合法"))
				return
			}
		}
		page := int64(1)
		if v, present := d["page"]; present && v != nil {
			if n, ok := toInt64(v); ok && n > 0 {
				page = n
			}
		}
		pageSize := int64(10)
		if v, present := d["pageSize"]; present && v != nil {
			if n, ok := toInt64(v); ok && n > 0 {
				pageSize = n
			}
		}
		if pageSize > 100 {
			pageSize = 100
		}
		list, total, err := store.ListAdvanceRequestsByUser(ctx, cid, a.ID, category, page, pageSize)
		if err != nil {
			logger.Error(mod, "advance:list 失败", "companyId", cid, "userId", a.ID, "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		if list == nil {
			list = []types.AdvanceRequest{}
		}
		ack(util.OK(map[string]any{"list": list, "total": total, "page": page, "pageSize": pageSize}))
	})

	// ---- advance:received:list：收到审批（拆 已处理/未处理）----
	srv.On("advance:received:list", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		a, cid, _, ok := requireMember(c, d, store, ack)
		if !ok {
			return
		}
		rows, err := store.ListReceivedAdvance(ctx, cid, a.ID)
		if err != nil {
			logger.Error(mod, "advance:received:list 失败", "companyId", cid, "userId", a.ID, "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		pending := []types.AdvanceRequest{}
		processed := []types.AdvanceRequest{}
		for _, r := range rows {
			if r.Status == "pending" {
				pending = append(pending, r)
			} else {
				processed = append(processed, r)
			}
		}
		ack(util.OK(map[string]any{"pending": pending, "processed": processed}))
	})

	// ---- advance:cc:list：收到抄送（拆 未查看/已查看）----
	srv.On("advance:cc:list", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		a, cid, _, ok := requireMember(c, d, store, ack)
		if !ok {
			return
		}
		rows, err := store.ListReceivedAdvanceCC(ctx, cid, a.ID)
		if err != nil {
			logger.Error(mod, "advance:cc:list 失败", "companyId", cid, "userId", a.ID, "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		type ccItem struct {
			types.AdvanceRequest
			Viewed bool `json:"viewed"`
		}
		unviewed := []ccItem{}
		viewed := []ccItem{}
		for _, r := range rows {
			isViewed := false
			for _, v := range r.CcViewedIds {
				if v == a.ID {
					isViewed = true
					break
				}
			}
			item := ccItem{AdvanceRequest: r, Viewed: isViewed}
			if isViewed {
				viewed = append(viewed, item)
			} else {
				unviewed = append(unviewed, item)
			}
		}
		ack(util.OK(map[string]any{"unviewed": unviewed, "viewed": viewed}))
	})

	// ---- advance:cc:view：把当前用户标记为已查看该抄送（幂等）----
	srv.On("advance:cc:view", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		a, cid, _, ok := requireMember(c, d, store, ack)
		if !ok {
			return
		}
		id, ok := toInt64(d["requestId"])
		if !ok || id <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		req, err := store.GetAdvanceByID(ctx, id)
		if err != nil {
			logger.Error(mod, "advance:cc:view 查申请失败", "id", id, "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		if req == nil || req.CompanyID != cid {
			ack(util.Fail("申请不存在"))
			return
		}
		isCC := false
		for _, v := range req.CcIDs {
			if v == a.ID {
				isCC = true
				break
			}
		}
		if !isCC {
			ack(util.Fail("您不是该申请的抄送人"))
			return
		}
		if err := store.MarkAdvanceCCViewed(ctx, id, a.ID); err != nil {
			logger.Error(mod, "advance:cc:view 失败", "id", id, "userId", a.ID, "error", err.Error())
			ack(util.Fail("操作失败"))
			return
		}
		ack(util.OK(map[string]any{"ok": true}))
	})

	// ---- advance:approve ----
	srv.On("advance:approve", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		a, cid, role, ok := requireMember(c, d, store, ack)
		if !ok {
			return
		}
		id, ok := toInt64(d["id"])
		if !ok || id <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		req, err := store.GetAdvanceByID(ctx, id)
		if err != nil {
			logger.Error(mod, "advance:approve 查申请失败", "id", id, "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		if req == nil || req.CompanyID != cid {
			ack(util.Fail("申请不存在"))
			return
		}
		if !approveAllowed(req.ApproverIDs, role, a.ID) {
			logger.Error(mod, "advance:approve 无权审批", "companyId", cid, "userId", a.ID, "requestId", id)
			ack(util.Fail("无权审批该申请"))
			return
		}
		updated, err := store.ApproveAdvanceRequest(ctx, id, a.ID)
		if err != nil {
			logger.Error(mod, "advance:approve 失败", "id", id, "error", err.Error())
			ack(util.Fail("操作失败"))
			return
		}
		if err := store.StopAdvanceNotification(ctx, id, cid); err != nil {
			logger.Error(mod, "advance:approve 置 notify_stopped 失败", "id", id, "error", err.Error())
		}
		updated.NotifyStopped = true
		_ = srv.EmitTo(util.UserRoom(req.UserID), "advance:request:notification", map[string]any{
			"kind": "company", "type": "advance_request_approved", "companyId": cid,
			"requestId": updated.ID, "title": "备用金申请",
			"body":      "您的备用金申请已通过",
			"byUserId":  a.ID, "byNick": a.Nick,
		})
		ack(util.OK(map[string]any{"request": updated}))
	})

	// ---- advance:reject ----
	srv.On("advance:reject", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		a, cid, role, ok := requireMember(c, d, store, ack)
		if !ok {
			return
		}
		id, ok := toInt64(d["id"])
		if !ok || id <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		req, err := store.GetAdvanceByID(ctx, id)
		if err != nil {
			logger.Error(mod, "advance:reject 查申请失败", "id", id, "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		if req == nil || req.CompanyID != cid {
			ack(util.Fail("申请不存在"))
			return
		}
		if !approveAllowed(req.ApproverIDs, role, a.ID) {
			logger.Error(mod, "advance:reject 无权审批", "companyId", cid, "userId", a.ID, "requestId", id)
			ack(util.Fail("无权审批该申请"))
			return
		}
		note := strField(d, "note")
		if note == "" {
			ack(util.Fail("拒绝理由不能为空"))
			return
		}
		updated, err := store.RejectAdvanceRequest(ctx, id, note)
		if err != nil {
			logger.Error(mod, "advance:reject 失败", "id", id, "error", err.Error())
			ack(util.Fail("操作失败"))
			return
		}
		if err := store.StopAdvanceNotification(ctx, id, cid); err != nil {
			logger.Error(mod, "advance:reject 置 notify_stopped 失败", "id", id, "error", err.Error())
		}
		updated.NotifyStopped = true
		_ = srv.EmitTo(util.UserRoom(req.UserID), "advance:request:notification", map[string]any{
			"kind": "company", "type": "advance_request_rejected", "companyId": cid,
			"requestId": updated.ID, "title": "备用金申请",
			"body":      "您的备用金申请已被拒绝",
			"byUserId":  a.ID, "byNick": a.Nick, "note":      note,
		})
		ack(util.OK(map[string]any{"request": updated}))
	})

	// ---- advance:request:notification:stop：幂等停止循环提醒 ----
	srv.On("advance:request:notification:stop", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		a, cid, _, ok := requireMember(c, d, store, ack)
		if !ok {
			return
		}
		id, ok := toInt64(d["requestId"])
		if !ok || id <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		req, err := store.GetAdvanceByID(ctx, id)
		if err != nil || req == nil || req.CompanyID != cid {
			logger.Warn(mod, "advance:request:notification:stop 申请不存在", "id", id, "error", err)
			ack(util.Fail("申请不存在"))
			return
		}
		isSelf := req.UserID == a.ID
		isApprover := false
		for _, v := range req.ApproverIDs {
			if v == a.ID {
				isApprover = true
				break
			}
		}
		if !isSelf && !isApprover {
			ack(util.Fail("无权操作该申请"))
			return
		}
		if err := store.StopAdvanceNotification(ctx, id, cid); err != nil {
			logger.Error(mod, "advance:request:notification:stop 失败", "id", id, "error", err.Error())
			ack(util.Fail("停止提醒失败"))
			return
		}
		ack(util.OK(map[string]any{"ok": true, "notifyStopped": true}))
	})
}

// approveAllowed：admin/owner 恒可审批；否则须在该条申请的 approver_ids 快照内（含自审批）。
func approveAllowed(approverIDs []int64, role string, currentUID int64) bool {
	if roleRank(role) >= roleRank("admin") {
		return true
	}
	for _, uid := range approverIDs {
		if uid == currentUID {
			return true
		}
	}
	return false
}
