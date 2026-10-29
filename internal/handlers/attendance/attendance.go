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
	"mochi-oa-server-go/internal/handlers/checkin"
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
	"overtime":  {}, // 加班（申请通过才在打卡记录显示，绝不因打卡时间晚推断）
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
	case "overtime":
		return "加班"
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
	v, ok := toFloat64(d["companyId"])
	if !ok {

		return nil, 0, "", false
	}
	cid, ok := toInt64(v)
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

// parseTimePtr 解析可选时间字段（空格完整时间或 ISO/RFC3339）；未传→nil；非法→(nil,false)。
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
	t, ok := util.ParseLocalTime(s)
	if !ok {
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
			ack(util.Fail("requireMember 失败"))
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
		if typ == "overtime" {
			// 加班时长由服务端按开始~结束时间段计算（申请通过才显示，时长准确，不信任客户端）。
			if startT == nil || endT == nil {
				ack(util.Fail("加班申请必须选择开始与结束时间"))
				return
			}
			h := endT.Sub(*startT).Hours()
			if h <= 0 {
				ack(util.Fail("加班结束时间必须晚于开始时间"))
				return
			}
			dur = &h
		} else if v, present := d["durationHours"]; present && v != nil {
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
		if len(approverIDs) == 0 {
			logger.Error(mod, "attendance:create 未选审批人", "companyId", cid, "user", a.ID)
			ack(util.Fail("请选择审批人"))
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
			MakeupDate:    strField(d, "makeupDate"), // 补卡目标日期（被补那天）
			MakeupSlot:    strField(d, "makeupSlot"), // 补卡槽位 work|off
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
				"body":     req.Nick + " 提交了" + typeLabel + "申请，等待您审批",
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
			ack(util.Fail("requireMember 失败"))
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
			ack(util.Fail("requireMember 失败"))
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
			ack(util.Fail("requireMember 失败"))
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
			ack(util.Fail("requireMember 失败"))
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
			ack(util.Fail("requireMember 失败"))
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
			ack(util.Fail("requireMember 失败"))
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
		// 补卡也算打卡：make_up 批准后把补卡时间写入 checkins（解析失败仅记日志，不影响审批）。
		// 同步固化排班快照：优先当天已有打卡记录的快照（同一天历史判定一致），否则用当前公司规则。
		if updated.Type == "make_up" && updated.PunchTime != nil && *updated.PunchTime != "" {
			punch := *updated.PunchTime
			// 被补日：客户端已传 MakeupDate（从打卡页补卡预填）则直接采用；
			// 未传（单独发起）则用 PunchTime 自动推断被补日与槽位。
			pDate := ""
			if updated.MakeupDate != nil {
				pDate = *updated.MakeupDate
			}
			// 查某日排班快照/规则的闭包（优先当天已有打卡快照，历史判定一致；否则用当前公司规则）。
			ruleOfDay := func(d string) (ws, we string, gl, ge int64) {
				if d == "" {
					return "", "", 0, 0
				}
				if existing, e := store.ListCheckinsByMonth(ctx, cid, req.UserID, d[:7]); e == nil {
					for _, it := range existing {
						if it.CheckDate == d && it.StdStart != "" && it.StdEnd != "" {
							return it.StdStart, it.StdEnd, it.GraceLateMin, it.GraceEarlyMin
						}
					}
				}
				if rule, e := store.GetCheckinRules(ctx, cid); e == nil {
					return checkin.RuleSnapshotForDay(rule, d)
				}
				return "", "", 0, 0
			}
			workStart, workEnd, gl, ge := "", "", int64(0), int64(0)
			inferredSlot := int64(0)
			// 单独申请（无被补日）：PunchTime 落在「某日下班 ~ 下班+8h」区间 → 补该日下班卡
			// （跨天补下班也算，如 26 号下班补到次日 02:00 → 补 26 号下班）；更贴近某日上班 → 补该日上班卡。
			// 先试 punch 前一工作日（跨天补下班场景），再试 punch 当日。
			if pDate == "" && len(punch) >= 10 {
				d := punch[:10]
				for _, cand := range []string{prevDateStr(d), d} {
					if cand == "" {
						continue
					}
					ws, we, g2, e2 := ruleOfDay(cand)
					if ws == "" || we == "" {
						continue
					}
					slotC := makeupSlotForDay(punch, cand, ws, we)
					if slotC == 2 {
						pDate = cand
						inferredSlot = 2
						workStart, workEnd, gl, ge = ws, we, g2, e2
						break
					}
					if inferredSlot == 0 {
						pDate = cand
						inferredSlot = slotC
						workStart, workEnd, gl, ge = ws, we, g2, e2
					}
				}
			}
			if pDate == "" && len(punch) >= 10 {
				pDate = punch[:10]
			}
			if workStart == "" {
				workStart, workEnd, gl, ge = ruleOfDay(pDate)
			}
			// 槽位最终判定：客户端已明确 MakeupSlot（补上班/补下班）则直接采用；
			// 否则用单独申请的推断结果，或按 PunchTime 距上下班邻近兜底。
			slot := int64(1)
			if updated.MakeupSlot == "off" {
				slot = 2
			} else if updated.MakeupSlot != "work" {
				if inferredSlot == 1 || inferredSlot == 2 {
					slot = inferredSlot
				} else if workStart != "" && workEnd != "" {
					if s := makeupSlotForDay(punch, pDate, workStart, workEnd); s == 1 || s == 2 {
						slot = s
					}
				}
			}
			if err := store.InsertMakeupCheckin(ctx, cid, req.UserID, pDate, punch, workStart, workEnd, gl, ge, slot); err != nil {
				logger.Error(mod, "attendance:approve 补卡写入 checkins 失败", "id", id, "punchTime", punch, "error", err.Error())
			}
		}
		// 业务通知：审批通过 → 推给申请人本人。
		typeLabel := attendanceTypeLabel(updated.Type)
		_ = srv.EmitTo(util.UserRoom(req.UserID), "attendance:request:notification", map[string]any{
			"kind": "company", "type": "attendance_request_approved", "companyId": cid,
			"requestId": updated.ID, "title": typeLabel + "申请已通过",
			"body":     "您的" + typeLabel + "申请已被 " + a.Nick + " 通过",
			"byUserId": a.ID, "byNick": a.Nick, "note": "",
		})
		ack(util.OK(map[string]any{"request": updated}))
	})

	// ---- attendance:reject ----
	srv.On("attendance:reject", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		a, cid, role, ok := requireMember(c, d, store, ack)
		if !ok {
			ack(util.Fail("requireMember 失败"))
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
			"body":     "您的" + typeLabel + "申请被 " + a.Nick + " 拒绝",
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

// clockMinutes 把 "HH:MM"（或 "HH:MM:SS"）转当天分钟数；失败返回 -1。
func clockMinutes(hhmm string) int {
	hm := hhmm
	if len(hm) > 5 {
		hm = hm[:5]
	}
	t, err := time.Parse("15:04", hm)
	if err != nil {
		return -1
	}
	return t.Hour()*60 + t.Minute()
}

// punchMinutesOf 从 "YYYY-MM-DD HH:mm[:ss]"（兼容 ISO "YYYY-MM-DDTHH:mm"）提取分钟数；失败返回 -1。
func punchMinutesOf(p string) int {
	idx := strings.Index(p, " ")
	if idx < 0 {
		if ti := strings.Index(p, "T"); ti >= 0 {
			idx = ti
		}
	}
	if idx < 0 || idx+1 >= len(p) {
		return -1
	}
	return clockMinutes(p[idx+1:])
}

// absInt 绝对值。
func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// prevDateStr 返回 dateStr（YYYY-MM-DD）的前一天；格式不合法返回空串。
func prevDateStr(dateStr string) string {
	t, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		return ""
	}
	return t.AddDate(0, 0, -1).Format("2006-01-02")
}

// makeupSlotForDay 判定 punch（实际打卡时刻，含日期）相对候选被补日 dateStr 的槽位。
// 下班区间 = 该日下班 ~ 下班+8 小时（含跨天到次日凌晨，如 26 号下班补到次日 02:00 仍算补 26 号下班）。
// 返回 2=补下班、1=补上班、-1=排班不可用无法判定。
func makeupSlotForDay(punch, dateStr, workStart, workEnd string) int64 {
	ws, we := clockMinutes(workStart), clockMinutes(workEnd)
	pm := punchMinutesOf(punch)
	if ws < 0 || we < 0 || pm < 0 {
		return -1
	}
	// punch 相对候选日按天偏移（punch 属次日 → +24h，跨天补下班）。
	off := pm
	if len(punch) >= 10 {
		if pt, e1 := time.Parse("2006-01-02", punch[:10]); e1 == nil {
			if dt, e2 := time.Parse("2006-01-02", dateStr); e2 == nil {
				off += int(pt.Sub(dt).Hours()/24) * 24 * 60
			}
		}
	}
	// 落在「下班 ~ 下班+8h」区间 → 补下班。
	if off >= we && off <= we+8*60 {
		return 2
	}
	// 距下班更近 → 补下班；距上班更近 → 补上班。
	if absInt(off-we) < absInt(off-ws) {
		return 2
	}
	return 1
}
