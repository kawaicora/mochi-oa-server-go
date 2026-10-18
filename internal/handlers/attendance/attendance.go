// Package attendance 实现考勤申请（补卡/请假/调休）的 socket 事件。
//
// 事件（均 requireMember 公司隔离；审批需 admin/owner 或落在该条申请 approverIds 内）：
//   - attendance:create / list / approve / reject
//
// 审批人/抄送人由申请人发起时动态选择（approverIds/ccIds），不再有公司级预设审批流配置。
package attendance

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

// 日志模块前缀（硬性约束）。
const mod = "handlers/attendance"

// 申请类型白名单。
var attendanceTypes = map[string]struct{}{
	"make_up":   {}, // 补卡
	"leave":     {}, // 请假
	"comp_time": {}, // 调休
}

// attendanceTypeLabel 申请类型中文显示名（业务通知标题/正文用）。
func attendanceTypeLabel(typ string) string {
	switch typ {
	case "leave":
		return "请假"
	case "comp_time":
		return "调休"
	case "make_up":
		return "补卡"
	default:
		return "考勤"
	}
}

// ─── 共享小工具（本包私有副本，与 report/org/task 包同款语义） ───

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

// strPtr 取可选字符串字段；未传/空 → nil，否则 trim 后指针（空串也保留为 *string("")）。
func strPtr(m map[string]any, key string) *string {
	v, present := m[key]
	if !present || v == nil {
		return nil
	}
	s, _ := v.(string)
	return &s
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

func requireAdmin(c socketapi.Conn, d map[string]any, st db.Store, ack socketapi.Ack) (*types.Auth, int64, bool) {
	a, cid, role, ok := requireMember(c, d, st, ack)
	if !ok {
		return nil, 0, false
	}
	if roleRank(role) < roleRank("admin") {
		ack(util.Fail("仅管理员可操作"))
		return nil, 0, false
	}
	return a, cid, true
}

// parseTimePtr 解析可选时间字段（ISO8601/RFC3339）；未传→nil；非法→(nil,false)。
func parseTimePtr(m map[string]any, key string) (*time.Time, bool) {
	v, present := m[key]
	if !present || v == nil {
		return nil, true
	}
	s, _ := v.(string)
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, true
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, false
	}
	utc := t.UTC()
	return &utc, true
}

// idList 从 payload 取 []int64（用于 approverIds/ccIds）；非法元素返回 false。
// idList 取 number[]。key 缺失或为 null → 返回空切片（合法，默认 []）；
// key 存在但类型非法或含非法元素 → 返回 false（由 handler 按字段报对应错误）。
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

// ─── 注册入口 ───

// RegisterAttendanceHandlers 注册 attendance:* 事件。
func RegisterAttendanceHandlers(srv socketapi.Server, store db.Store) {
	ctx := context.Background()

	// ---- attendance:create：发起申请 ----
	srv.On("attendance:create", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		a, cid, _, ok := requireMember(c, d, store, ack)
		if !ok {
			return
		}
		typ := strField(d, "type")
		if _, ok := attendanceTypes[typ]; !ok {
			ack(util.Fail("申请类型不合法"))
			return
		}
		reason := strField(d, "reason")
		if reason == "" {
			ack(util.Fail("理由不能为空"))
			return
		}
		startT, ok := parseTimePtr(d, "startTime")
		if !ok {
			ack(util.Fail("开始时间格式不合法"))
			return
		}
		endT, ok := parseTimePtr(d, "endTime")
		if !ok {
			ack(util.Fail("结束时间格式不合法"))
			return
		}
		if startT != nil && endT != nil && startT.After(*endT) {
			ack(util.Fail("开始时间不能晚于结束时间"))
			return
		}
		var dur *float64
		if v, present := d["durationHours"]; present && v != nil {
			f, ok := toFloat64(v)
			if !ok {
				ack(util.Fail("时长不合法"))
				return
			}
			if f <= 0 {
				ack(util.Fail("时长必须大于0"))
				return
			}
			dur = &f
		}
		// 审批人/抄送人由申请人发起时动态选择（不再读公司级预设审批流）。
		// 逐 id 校验本公司成员；不合法 → Fail。不传则 []（此时由 admin/owner 兜底审批）。
		approverIDs, ok := idList(d, "approverIds")
		if !ok {
			logger.Error(mod, "attendance:create 审批人 id 非法", "companyId", cid)
			ack(util.Fail("审批人不在该公司"))
			return
		}
		for _, uid := range approverIDs {
			role, err := store.GetMemberRole(context.Background(), cid, uid)
			if err != nil || role == "" {
				logger.Error(mod, "attendance:create 审批人不在该公司", "companyId", cid, "userId", uid, "err", errStr(err))
				ack(util.Fail("审批人不在该公司"))
				return
			}
		}
		ccIDs, ok := idList(d, "ccIds")
		if !ok {
			logger.Error(mod, "attendance:create 抄送人 id 非法", "companyId", cid)
			ack(util.Fail("抄送人不在该公司"))
			return
		}
		for _, uid := range ccIDs {
			role, err := store.GetMemberRole(context.Background(), cid, uid)
			if err != nil || role == "" {
				logger.Error(mod, "attendance:create 抄送人不在该公司", "companyId", cid, "userId", uid, "err", errStr(err))
				ack(util.Fail("抄送人不在该公司"))
				return
			}
		}
		req, err := store.CreateAttendanceRequest(ctx, db.CreateAttendanceRequestInput{
			CompanyID:     cid,
			UserID:        a.ID,
			Type:          typ,
			PunchTime:     strPtr(d, "punchTime"),
			LeaveType:     strPtr(d, "leaveType"),
			CompType:      strPtr(d, "compType"),
			StartTime:     startT,
			EndTime:       endT,
			DurationHours: dur,
			Reason:        reason,
			ApproverIDs:   approverIDs,
			CcIDs:         ccIDs,
		})
		if err != nil {
			logger.Error(mod, "attendance:create 失败", "companyId", cid, "userId", a.ID, "error", err.Error())
			ack(util.Fail("创建失败"))
			return
		}
		// 业务通知（kind=company）：申请成功 → 审批人全量 + 抄送人（含自审批/自抄送）。
		typeLabel := attendanceTypeLabel(typ)
		notifyUsers := append([]int64{}, approverIDs...)
		notifyUsers = append(notifyUsers, ccIDs...)
		for _, uid := range notifyUsers {
			_ = srv.EmitTo(util.UserRoom(uid), "attendance:request:notification", map[string]any{
				"kind": "company", "type": "attendance_request_created", "companyId": cid,
				"requestId": req.ID, "title": "新的" + typeLabel + "申请",
				"body":  req.Nick + " 提交了" + typeLabel + "申请，等待您审批",
				"byUserId": a.ID, "byNick": req.Nick, "note": reason,
			})
		}
		ack(util.OK(map[string]any{"request": req}))
	})

	// ---- attendance:list：只看本人历史 ----
	srv.On("attendance:list", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		a, cid, _, ok := requireMember(c, d, store, ack)
		if !ok {
			return
		}
		typ := strField(d, "type")
		if typ != "" {
			if _, ok := attendanceTypes[typ]; !ok {
				ack(util.Fail("申请类型不合法"))
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
		// 可选日期区间（YYYY-MM-DD 含两端）：日历紫色点用；不传=行为不变。
		dateStart := strField(d, "dateStart")
		dateEnd := strField(d, "dateEnd")
		rows, total, err := store.ListAttendanceRequestsByUser(ctx, cid, a.ID, typ, dateStart, dateEnd, page, pageSize)
		if err != nil {
			logger.Error(mod, "attendance:list 失败", "companyId", cid, "userId", a.ID, "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		if rows == nil {
			rows = []types.AttendanceRequest{}
		}
		ack(util.OK(map[string]any{"requests": rows, "total": total}))
	})

	// ---- attendance:received:list：收到审批（本人提交 OR 本人为审批人，含自审批）----
	srv.On("attendance:received:list", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		a, cid, _, ok := requireMember(c, d, store, ack)
		if !ok {
			return
		}
		rows, err := store.ListReceivedAttendance(ctx, cid, a.ID)
		if err != nil {
			logger.Error(mod, "attendance:received:list 失败", "companyId", cid, "userId", a.ID, "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		pending := []types.AttendanceRequest{}
		processed := []types.AttendanceRequest{}
		for _, r := range rows {
			if r.Status == "pending" {
				pending = append(pending, r)
			} else {
				processed = append(processed, r)
			}
		}
		ack(util.OK(map[string]any{"pending": pending, "processed": processed}))
	})

	// ---- attendance:cc:list：收到抄送（本人提交 OR 本人为抄送人，含自抄送）----
	srv.On("attendance:cc:list", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		a, cid, _, ok := requireMember(c, d, store, ack)
		if !ok {
			return
		}
		rows, err := store.ListReceivedCC(ctx, cid, a.ID)
		if err != nil {
			logger.Error(mod, "attendance:cc:list 失败", "companyId", cid, "userId", a.ID, "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		type ccItem struct {
			types.AttendanceRequest
			Viewed bool `json:"viewed"`
		}
		out := []ccItem{}
		for _, r := range rows {
			viewed := false
			for _, v := range r.CcViewedIds {
				if v == a.ID {
					viewed = true
					break
				}
			}
			out = append(out, ccItem{AttendanceRequest: r, Viewed: viewed})
		}
		ack(util.OK(map[string]any{"items": out}))
	})

	// ---- attendance:cc:view：把当前用户标记为已查看该抄送（幂等）----
	srv.On("attendance:cc:view", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
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
		req, err := store.GetAttendanceRequestByID(ctx, id)
		if err != nil {
			logger.Error(mod, "attendance:cc:view 查申请失败", "id", id, "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		if req == nil || req.CompanyID != cid {
			ack(util.Fail("申请不存在"))
			return
		}
		// 仅抄送人（含本人提交即自抄送）可标记；非抄送人拒绝。
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
		if err := store.MarkCCViewed(ctx, id, a.ID); err != nil {
			logger.Error(mod, "attendance:cc:view 失败", "id", id, "userId", a.ID, "error", err.Error())
			ack(util.Fail("操作失败"))
			return
		}
		ack(util.OK(map[string]any{"ok": true}))
	})

	// ---- attendance:request:notification:stop：申请人或审批人主动停止循环提醒（幂等）----
	srv.On("attendance:request:notification:stop", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
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
		req, err := store.GetAttendanceRequestByID(ctx, id)
		if err != nil || req == nil || req.CompanyID != cid {
			logger.Warn(mod, "attendance:request:notification:stop 申请不存在", "id", id, "error", err)
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
		if err := store.StopAttendanceNotification(ctx, id, cid); err != nil {
			logger.Error(mod, "attendance:request:notification:stop 失败", "id", id, "error", err.Error())
			ack(util.Fail("停止提醒失败"))
			return
		}
		ack(util.OK(map[string]any{"ok": true, "notifyStopped": true}))
	})

	// ---- attendance:approve ----
	srv.On("attendance:approve", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
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
		req, err := store.GetAttendanceRequestByID(ctx, id)
		if err != nil {
			logger.Error(mod, "attendance:approve 查申请失败", "id", id, "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		if req == nil || req.CompanyID != cid {
			ack(util.Fail("申请不存在"))
			return
		}
		if !approveAllowed(req, role, a.ID) {
			logger.Error(mod, "attendance:approve 无权审批", "companyId", cid, "userId", a.ID, "requestId", id)
			ack(util.Fail("无权审批该申请"))
			return
		}
		updated, err := store.ApproveAttendanceRequest(ctx, id, a.ID)
		if err != nil {
			logger.Error(mod, "attendance:approve 失败", "id", id, "error", err.Error())
			ack(util.Fail("操作失败"))
			return
		}
		// 处理时自动停止循环提醒（幂等）。
		if err := store.StopAttendanceNotification(ctx, id, cid); err != nil {
			logger.Error(mod, "attendance:approve 置 notify_stopped 失败", "id", id, "error", err.Error())
		}
		updated.NotifyStopped = true
		// 业务通知：审批通过 → 推给申请人本人。
		typeLabel := attendanceTypeLabel(updated.Type)
		_ = srv.EmitTo(util.UserRoom(req.UserID), "attendance:request:notification", map[string]any{
			"kind": "company", "type": "attendance_request_approved", "companyId": cid,
			"requestId": updated.ID, "title": typeLabel + "申请已通过",
			"body":  "您的" + typeLabel + "申请已被 " + a.Nick + " 通过",
			"byUserId": a.ID, "byNick": a.Nick, "note": "",
		})
		ack(util.OK(map[string]any{"request": updated}))
	})

	// ---- attendance:reject ----
	srv.On("attendance:reject", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
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
		req, err := store.GetAttendanceRequestByID(ctx, id)
		if err != nil {
			logger.Error(mod, "attendance:reject 查申请失败", "id", id, "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		if req == nil || req.CompanyID != cid {
			ack(util.Fail("申请不存在"))
			return
		}
		if !approveAllowed(req, role, a.ID) {
			logger.Error(mod, "attendance:reject 无权审批", "companyId", cid, "userId", a.ID, "requestId", id)
			ack(util.Fail("无权审批该申请"))
			return
		}
		note := strField(d, "note")
		updated, err := store.RejectAttendanceRequest(ctx, id, note)
		if err != nil {
			logger.Error(mod, "attendance:reject 失败", "id", id, "error", err.Error())
			ack(util.Fail("操作失败"))
			return
		}
		// 处理时自动停止循环提醒（幂等）。
		if err := store.StopAttendanceNotification(ctx, id, cid); err != nil {
			logger.Error(mod, "attendance:reject 置 notify_stopped 失败", "id", id, "error", err.Error())
		}
		updated.NotifyStopped = true
		// 业务通知：审批拒绝 → 推给申请人本人（含拒绝原因摘要）。
		typeLabel := attendanceTypeLabel(updated.Type)
		_ = srv.EmitTo(util.UserRoom(req.UserID), "attendance:request:notification", map[string]any{
			"kind": "company", "type": "attendance_request_rejected", "companyId": cid,
			"requestId": updated.ID, "title": typeLabel + "申请被拒绝",
			"body":  "您的" + typeLabel + "申请被 " + a.Nick + " 拒绝",
			"byUserId": a.ID, "byNick": a.Nick, "note": note,
		})
		ack(util.OK(map[string]any{"request": updated}))
	})
}

// approveAllowed：admin/owner 恒可审批；否则须在该条申请的 approverIds 快照内。
func approveAllowed(req *types.AttendanceRequest, role string, currentUID int64) bool {
	if roleRank(role) >= roleRank("admin") {
		return true
	}
	for _, uid := range req.ApproverIDs {
		if uid == currentUID {
			return true
		}
	}
	return false
}

// errStr 把 error 安全转字符串（nil → ""）。
func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
