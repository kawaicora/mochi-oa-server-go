// Package report 是「日报/周报」socket 事件处理器（钉钉基础功能子集）。
//
// 冻结协议（客户端按此对接，事件名/字段名不可改）：
//   - 全部事件需登录，payload 必带 companyId；先 GetMemberRole，非成员 Fail("未加入该公司")；
//   - 报告事件：report:current / create / update / submit / delete / list；
//   - 模板事件：report:template:list / create / update / delete（admin/owner）；
//   - 统计事件：report:stats（admin/owner）；
//   - 服务端推送：report:reminder → 该公司 admin/owner 的 user room（未交提醒）。
//
// 公司隔离：所有读写先校验 company_members 成员身份；非 admin 列表只能看自己。
// 硬性纪律：每个失败路径 logger.Error 带上下文 key，绝不静默吞错。
//
// 注册方式：RegisterReportHandlers(srv, store, cfg)（Register 内启动未交提醒 goroutine）。
package report

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"mochi-oa-server-go/internal/config"
	"mochi-oa-server-go/internal/db"
	"mochi-oa-server-go/internal/logger"
	"mochi-oa-server-go/internal/socketapi"
	"mochi-oa-server-go/internal/types"
	"mochi-oa-server-go/internal/util"
)

// 日志模块前缀（硬性约束）。
const mod = "handlers/report"

// 周期类型白名单。
var reportTypes = map[string]struct{}{"daily": {}, "weekly": {}}

// ─── 共享小工具（本包私有副本，与 org/task 包同款语义） ───

// getAuth 从连接取认证用户；未登录返回 nil。
func getAuth(c socketapi.Conn) *types.Auth {
	v := c.Get("auth")
	if a, ok := v.(*types.Auth); ok && a != nil {
		return a
	}
	return nil
}

// parseData 把 payload JSON 解析为 map；空/非法给空 map。
func parseData(data json.RawMessage) map[string]any {
	m := map[string]any{}
	if len(data) == 0 {
		return m
	}
	_ = json.Unmarshal(data, &m)
	return m
}

// toInt64 把 JSON any 值转 int64；第二返回值 false 表示不是合法整数。
func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		if n != math.Trunc(n) {
			return 0, false
		}
		return int64(n), true
	case string:
		var i int64
		_, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
		if err != nil {
			return 0, false
		}
		return i, true
	}
	return 0, false
}

// strField 取字符串字段并 trim。
func strField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// roleRank 角色比较：owner(3) > admin(2) > member(1)（与 company.go 同语义）。
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

// validPeriodDate 校验 YYYY-MM-DD（严格重排比较，拒绝 2026-1-2 这类写法）。
func validPeriodDate(s string) bool {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return false
	}
	return t.Format("2006-01-02") == s
}

// weekMonday 返回当地时间 now 所在周的周一 00:00（本地时区）。
// 约定：周报 periodDate = 本周周一；与定时器提醒的口径一致。
func weekMonday(now time.Time) time.Time {
	// time.Weekday(): Sunday=0 … Monday=1 …
	daysFromMonday := (int(now.Weekday()) + 6) % 7 // Monday→0 … Sunday→6
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -daysFromMonday)
}

// resolvePeriodDate 按类型算「当前周期」periodDate：daily=今天，weekly=本周周一。
func resolvePeriodDate(typ string, now time.Time) string {
	if typ == "weekly" {
		return weekMonday(now).Format("2006-01-02")
	}
	return now.Format("2006-01-02")
}

// requireMember 是事件公共前置：登录 + companyId 合法 + 本公司成员。
// 返回 (auth用户, 公司ID, 角色)；失败时已 ack，调用方直接 return。
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

// requireAdmin 是模板/统计等管理员事件前置（owner/admin）。
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

// ==================== 注册入口 ====================

// RegisterReportHandlers 注册全部 report:* 事件并启动未交提醒定时器。
func RegisterReportHandlers(srv socketapi.Server, store db.Store, cfg *config.ServerConfig) {
	ctx := context.Background()

	// ---- report:current：本人当前周期报告（daily=今天，weekly=本周周一） ----
	srv.On("report:current", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		a, cid, _, ok := requireMember(c, d, store, ack)
		if !ok {
			return
		}
		typ := strField(d, "type")
		if _, ok := reportTypes[typ]; !ok {
			ack(util.Fail("报告类型不合法"))
			return
		}
		pd := resolvePeriodDate(typ, time.Now())
		report, err := store.GetReportByPeriod(ctx, cid, a.ID, typ, pd)
		if err != nil {
			logger.Error(mod, "report:current 查询失败", "companyId", cid, "userId", a.ID, "type", typ, "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		// 不存在返回 report=null（客户端据此引导创建）。
		ack(util.OK(map[string]any{"report": report}))
	})

	// ---- report:create：创建本人某周期报告 ----
	srv.On("report:create", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		a, cid, _, ok := requireMember(c, d, store, ack)
		if !ok {
			return
		}
		typ := strField(d, "type")
		if _, ok := reportTypes[typ]; !ok {
			ack(util.Fail("报告类型不合法"))
			return
		}
		pd := strField(d, "periodDate")
		if !validPeriodDate(pd) {
			ack(util.Fail("周期日期不合法"))
			return
		}
		content := ""
		if v, ok := d["content"].(string); ok {
			content = v
		}
		if strings.TrimSpace(content) == "" {
			ack(util.Fail("内容不能为空"))
			return
		}
		title := ""
		if v, ok := d["title"].(string); ok {
			title = strings.TrimSpace(v)
		}
		templateID := int64(0)
		if v, has := d["templateId"]; has && v != nil {
			if n, ok := toInt64(v); ok && n > 0 {
				templateID = n
			}
		}
		// recipients 可选：传了就逐个校验是本公司成员；空数组=不发送给任何人。
		recipients := []int64{}
		if _, present := d["recipients"]; present {
			rs, ok := parseRecipients(d, cid, store)
			if !ok {
				ack(util.Fail("接收人不在该公司"))
				return
			}
			recipients = rs
		}
		// 客户端会先调 report:current；这里再兜底查一次防并发重复。
		if existing, _ := store.GetReportByPeriod(ctx, cid, a.ID, typ, pd); existing != nil {
			ack(util.Fail("该周期的日报已存在"))
			return
		}
		report, err := store.CreateReport(ctx, db.CreateReportInput{
			CompanyID:  cid,
			UserID:     a.ID,
			Type:       typ,
			PeriodDate: pd,
			Title:      title,
			Content:    content,
			TemplateID: templateID,
			Recipients: recipients,
		})
		if err != nil {
			if _, ok := asAlreadyExists(err); ok {
				logger.Error(mod, "report:create 周期冲突", "companyId", cid, "userId", a.ID, "type", typ, "periodDate", pd)
				ack(util.Fail("该周期的日报已存在"))
				return
			}
			logger.Error(mod, "report:create 失败", "companyId", cid, "userId", a.ID, "error", err.Error())
			ack(util.Fail("创建失败"))
			return
		}
		ack(util.OK(map[string]any{"report": report}))
	})

	// ---- report:update：本人改内容（已提交也可改，submitted 状态不变） ----
	srv.On("report:update", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		a, cid, _, ok := requireMember(c, d, store, ack)
		if !ok {
			return
		}
		id, ok := toInt64(d["id"])
		if !ok || id <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		report, err := store.GetReportByID(ctx, id)
		if err != nil {
			logger.Error(mod, "report:update 查报告失败", "id", id, "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		if report == nil || report.CompanyID != cid {
			ack(util.Fail("报告不存在"))
			return
		}
		if report.UserID != a.ID {
			ack(util.Fail("无权修改"))
			return
		}
		patch := db.UpdateReportInput{}
		changed := false
		if v, present := d["title"]; present {
			s := strings.TrimSpace(anyStr(v))
			patch.Title = &s
			changed = true
		}
		if v, present := d["content"]; present {
			s := anyStr(v)
			patch.Content = &s
			changed = true
		}
		if v, present := d["templateId"]; present && v != nil {
			if n, ok := toInt64(v); ok {
				patch.TemplateID = &n
				changed = true
			}
		}
		// recipients 可选：传了则全量替换接收人（空数组=清空）；逐个校验为本公司成员。
		if v, present := d["recipients"]; present && v != nil {
			rs, ok := parseRecipients(d, cid, store)
			if !ok {
				ack(util.Fail("接收人不在该公司"))
				return
			}
			patch.Recipients = rs
			changed = true
		}
		if !changed {
			ack(util.OK(map[string]any{"report": report}))
			return
		}
		updated, err := store.UpdateReport(ctx, id, patch)
		if err != nil {
			logger.Error(mod, "report:update 失败", "id", id, "error", err.Error())
			ack(util.Fail("保存失败"))
			return
		}
		ack(util.OK(map[string]any{"report": updated}))
	})

	// ---- report:submit：本人提交（置 submitted=true、submittedAt=now） ----
	srv.On("report:submit", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		a, cid, _, ok := requireMember(c, d, store, ack)
		if !ok {
			return
		}
		id, ok := toInt64(d["id"])
		if !ok || id <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		report, err := store.GetReportByID(ctx, id)
		if err != nil {
			logger.Error(mod, "report:submit 查报告失败", "id", id, "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		if report == nil || report.CompanyID != cid {
			ack(util.Fail("报告不存在"))
			return
		}
		if report.UserID != a.ID {
			ack(util.Fail("无权操作"))
			return
		}
		submitted, err := store.SubmitReport(ctx, id)
		if err != nil {
			logger.Error(mod, "report:submit 失败", "id", id, "error", err.Error())
			ack(util.Fail("提交失败"))
			return
		}
		ack(util.OK(map[string]any{"report": submitted}))
	})

	// ---- report:delete：本人或 admin 删除 ----
	srv.On("report:delete", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
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
		report, err := store.GetReportByID(ctx, id)
		if err != nil {
			logger.Error(mod, "report:delete 查报告失败", "id", id, "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		if report == nil || report.CompanyID != cid {
			ack(util.Fail("报告不存在"))
			return
		}
		if report.UserID != a.ID && roleRank(role) < roleRank("admin") {
			ack(util.Fail("无权删除"))
			return
		}
		if err := store.DeleteReport(ctx, id); err != nil {
			logger.Error(mod, "report:delete 失败", "id", id, "error", err.Error())
			ack(util.Fail("删除失败"))
			return
		}
		ack(util.OK(nil))
	})

	// ---- report:list：非 admin 只能看自己；admin 可按 userId 过滤 ----
	srv.On("report:list", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		a, cid, role, ok := requireMember(c, d, store, ack)
		if !ok {
			return
		}
		isAdmin := roleRank(role) >= roleRank("admin")
		in := db.ListReportsInput{CompanyID: cid}
		in.Type = strField(d, "type")
		if in.Type != "" {
			if _, ok := reportTypes[in.Type]; !ok {
				ack(util.Fail("报告类型不合法"))
				return
			}
		}
		// 非 admin 强制看自己；admin 传了 userId 才过滤。
		in.UserID = a.ID
		if uidV, present := d["userId"]; present && uidV != nil && isAdmin {
			if n, ok := toInt64(uidV); ok && n > 0 {
				in.UserID = n
			}
		}
		in.PeriodStart = strField(d, "periodStart")
		in.PeriodEnd = strField(d, "periodEnd")
		if in.PeriodStart != "" && !validPeriodDate(in.PeriodStart) {
			ack(util.Fail("periodStart 不合法"))
			return
		}
		if in.PeriodEnd != "" && !validPeriodDate(in.PeriodEnd) {
			ack(util.Fail("periodEnd 不合法"))
			return
		}
		in.Page = 1
		in.PageSize = 20
		if v, present := d["page"]; present && v != nil {
			if n, ok := toInt64(v); ok && n > 0 {
				in.Page = n
			}
		}
		if v, present := d["pageSize"]; present && v != nil {
			if n, ok := toInt64(v); ok && n > 0 {
				in.PageSize = n
			}
		}
		// store 层兜底 pageSize 上限 100，这里也显式夹一次。
		if in.PageSize > 100 {
			in.PageSize = 100
		}
		reports, total, err := store.ListReports(ctx, in)
		if err != nil {
			logger.Error(mod, "report:list 查询失败", "companyId", cid, "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		if reports == nil {
			reports = []types.ReportItem{}
		}
		ack(util.OK(map[string]any{"reports": reports, "total": total}))
	})

	// ---- report:inbox：我收到的（别人发给我的），createdAt 倒序分页 ----
	srv.On("report:inbox", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		a, cid, _, ok := requireMember(c, d, store, ack)
		if !ok {
			return
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
		reports, total, err := store.ListInboxReports(ctx, cid, a.ID, page, pageSize)
		if err != nil {
			logger.Error(mod, "report:inbox 查询失败", "companyId", cid, "userId", a.ID, "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		if reports == nil {
			reports = []types.ReportItem{}
		}
		ack(util.OK(map[string]any{"reports": reports, "total": total}))
	})

	// ---- report:template:list：成员可读 ----
	srv.On("report:template:list", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		_, cid, _, ok := requireMember(c, d, store, ack)
		if !ok {
			return
		}
		templates, err := store.ListReportTemplates(ctx, cid)
		if err != nil {
			logger.Error(mod, "report:template:list 失败", "companyId", cid, "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		if templates == nil {
			templates = []types.ReportTemplate{}
		}
		ack(util.OK(map[string]any{"templates": templates}))
	})

	// ---- report:template:create：admin/owner ----
	srv.On("report:template:create", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		a, cid, ok := requireAdmin(c, d, store, ack)
		if !ok {
			return
		}
		name := strField(d, "name")
		if name == "" {
			ack(util.Fail("模板名称不能为空"))
			return
		}
		desc := ""
		if v, ok := d["description"].(string); ok {
			desc = strings.TrimSpace(v)
		}
		sections := []string{}
		if arr, ok := d["sections"].([]any); ok {
			for _, x := range arr {
				if s, isStr := x.(string); isStr && strings.TrimSpace(s) != "" {
					sections = append(sections, strings.TrimSpace(s))
				}
			}
		}
		tpl, err := store.CreateReportTemplate(ctx, db.CreateReportTemplateInput{
			CompanyID:   cid,
			Name:        name,
			Description: desc,
			Sections:    sections,
			CreatedBy:   a.ID,
		})
		if err != nil {
			logger.Error(mod, "report:template:create 失败", "companyId", cid, "error", err.Error())
			ack(util.Fail("创建失败"))
			return
		}
		ack(util.OK(map[string]any{"template": tpl}))
	})

	// ---- report:template:update：admin/owner ----
	srv.On("report:template:update", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		_, cid, ok := requireAdmin(c, d, store, ack)
		if !ok {
			return
		}
		id, ok := toInt64(d["id"])
		if !ok || id <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		// 先校验模板属于本公司（防跨公司改）。
		tpl, err := store.GetReportTemplateByID(ctx, id)
		if err != nil {
			logger.Error(mod, "report:template:update 查模板失败", "id", id, "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		if tpl == nil || tpl.CompanyID != cid {
			ack(util.Fail("模板不存在"))
			return
		}
		in := db.UpdateReportTemplateInput{}
		changed := false
		if v, present := d["name"]; present {
			s := strings.TrimSpace(anyStr(v))
			if s == "" {
				ack(util.Fail("模板名称不能为空"))
				return
			}
			in.Name = &s
			changed = true
		}
		if v, present := d["description"]; present {
			s := strings.TrimSpace(anyStr(v))
			in.Description = &s
			changed = true
		}
		if v, present := d["sections"]; present && v != nil {
			sections := []string{}
			if arr, ok := v.([]any); ok {
				for _, x := range arr {
					if s, isStr := x.(string); isStr && strings.TrimSpace(s) != "" {
						sections = append(sections, strings.TrimSpace(s))
					}
				}
			}
			in.Sections = sections
			changed = true
		}
		if !changed {
			ack(util.OK(map[string]any{"template": tpl}))
			return
		}
		updated, err := store.UpdateReportTemplate(ctx, id, in)
		if err != nil {
			logger.Error(mod, "report:template:update 失败", "id", id, "error", err.Error())
			ack(util.Fail("保存失败"))
			return
		}
		ack(util.OK(map[string]any{"template": updated}))
	})

	// ---- report:template:delete：admin/owner ----
	srv.On("report:template:delete", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		_, cid, ok := requireAdmin(c, d, store, ack)
		if !ok {
			return
		}
		id, ok := toInt64(d["id"])
		if !ok || id <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		tpl, err := store.GetReportTemplateByID(ctx, id)
		if err != nil {
			logger.Error(mod, "report:template:delete 查模板失败", "id", id, "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		if tpl == nil || tpl.CompanyID != cid {
			ack(util.Fail("模板不存在"))
			return
		}
		if err := store.DeleteReportTemplate(ctx, id); err != nil {
			logger.Error(mod, "report:template:delete 失败", "id", id, "error", err.Error())
			ack(util.Fail("删除失败"))
			return
		}
		ack(util.OK(nil))
	})

	// ---- report:stats：admin/owner 看区间内全员提交统计 ----
	srv.On("report:stats", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		_, cid, ok := requireAdmin(c, d, store, ack)
		if !ok {
			return
		}
		typ := strField(d, "type")
		if _, ok := reportTypes[typ]; !ok {
			ack(util.Fail("报告类型不合法"))
			return
		}
		ps := strField(d, "periodStart")
		pe := strField(d, "periodEnd")
		if !validPeriodDate(ps) || !validPeriodDate(pe) {
			ack(util.Fail("统计区间不合法"))
			return
		}
		stats, err := buildReportStats(ctx, store, cid, typ, ps, pe)
		if err != nil {
			logger.Error(mod, "report:stats 失败", "companyId", cid, "type", typ, "error", err.Error())
			ack(util.Fail("统计失败"))
			return
		}
		ack(util.OK(map[string]any{"stats": stats}))
	})

	// ---- 未交提醒定时器：Register 时启动 goroutine，每 5 分钟 tick ----
	startReminderLoop(srv, store, cfg)
}

// ─── 小工具补充 ───

// anyStr 容忍 nil/非字符串取值（给更新字段用）。
func anyStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// asAlreadyExists 判断是否唯一冲突错误（errors.As 兼容包装层）。
func asAlreadyExists(err error) (*db.AlreadyExistsError, bool) {
	var ae *db.AlreadyExistsError
	ok := errors.As(err, &ae)
	return ae, ok
}

// parseRecipients 解析 payload 的 recipients 数组并逐个校验为本公司成员。
// 任一非法 id 或非本公司成员都打日志并返回 false（调用方 Fail("接收人不在该公司")）。
// 空数组（传了 []）合法，表示不发送给任何人。
func parseRecipients(m map[string]any, cid int64, st db.Store) ([]int64, bool) {
	arr, ok := m["recipients"].([]any)
	if !ok {
		logger.Error(mod, "report recipients 不是数组", "companyId", cid)
		return nil, false
	}
	out := []int64{}
	for _, x := range arr {
		uid, ok := toInt64(x)
		if !ok || uid <= 0 {
			logger.Error(mod, "report recipients 含非法 user id", "companyId", cid, "value", fmt.Sprintf("%v", x))
			return nil, false
		}
		role, err := st.GetMemberRole(context.Background(), cid, uid)
		if err != nil {
			logger.Error(mod, "report recipients 校验成员失败", "companyId", cid, "userId", uid, "error", err.Error())
			return nil, false
		}
		if role == "" {
			logger.Error(mod, "report recipients 接收人不在该公司", "companyId", cid, "userId", uid)
			return nil, false
		}
		out = append(out, uid)
	}
	return out, true
}

// ==================== 统计口径 ====================

// reportStatsView 是 report:stats 的 stats 负载。
type reportStatsView struct {
	Type        string                   `json:"type"`
	PeriodStart string                   `json:"periodStart"`
	PeriodEnd   string                   `json:"periodEnd"`
	TotalDays   int64                    `json:"totalDays"`
	Members     []types.ReportMemberStat `json:"members"`
}

// buildReportStats 计算区间统计。
//
// 口径（写死在注释，与前端约定一致）：
//   - totalDays = 区间 [periodStart, periodEnd] 内「已结束或今天」的周期单位数：
//     daily=天数（右边界取 min(end, 今天)），weekly=周一数（右边界取 min(end, 本周周一)）；
//   - submittedDays = 该成员在范围内的实际提交条数；
//   - missingDays = max(0, totalDays - submittedDays)。
func buildReportStats(ctx context.Context, store db.Store, companyID int64, typ, ps, pe string) (*reportStatsView, error) {
	members, err := store.GetMembers(ctx, companyID)
	if err != nil {
		return nil, err
	}
	// 成员 userID 列表（统计只覆盖公司成员）。
	userIDs := make([]int64, 0, len(members))
	for _, m := range members {
		userIDs = append(userIDs, m.UserID)
	}
	reports, err := store.ListReportsByPeriod(ctx, companyID, typ, ps, pe, userIDs)
	if err != nil {
		return nil, err
	}
	// 按成员聚合。
	submittedCount := map[int64]int64{}
	lastSubmitted := map[int64]string{}
	for _, r := range reports {
		if !r.Submitted {
			continue
		}
		submittedCount[r.UserID]++
		if r.SubmittedAt != nil {
			if cur, ok := lastSubmitted[r.UserID]; !ok || *r.SubmittedAt > cur {
				lastSubmitted[r.UserID] = *r.SubmittedAt
			}
		}
	}
	// 右边界收敛到「已结束或今天」。
	now := time.Now()
	var effectiveEnd string
	if typ == "weekly" {
		// 周报右边界 = min(end, 本周周一)：本周才算「已结束」的一周单位。
		monday := weekMonday(now).Format("2006-01-02")
		if pe > monday {
			effectiveEnd = monday
		} else {
			effectiveEnd = pe
		}
	} else {
		today := now.Format("2006-01-02")
		if pe > today {
			effectiveEnd = today
		} else {
			effectiveEnd = pe
		}
	}
	totalDays := countPeriodUnits(typ, ps, effectiveEnd)

	out := []types.ReportMemberStat{}
	for _, m := range members {
		submitted := submittedCount[m.UserID]
		missing := totalDays - submitted
		if missing < 0 {
			missing = 0
		}
		nick := ""
		if m.Nick != nil {
			nick = *m.Nick
		}
		username := ""
		if m.Username != nil {
			username = *m.Username
		}
		var last *string
		if s, ok := lastSubmitted[m.UserID]; ok {
			last = &s
		}
		out = append(out, types.ReportMemberStat{
			UserID:          m.UserID,
			Nick:            nick,
			Username:        username,
			SubmittedDays:   submitted,
			MissingDays:     missing,
			LastSubmittedAt: last,
		})
	}
	return &reportStatsView{
		Type:        typ,
		PeriodStart: ps,
		PeriodEnd:   pe,
		TotalDays:   totalDays,
		Members:     out,
	}, nil
}

// countPeriodUnits 数 [start,end]（YYYY-MM-DD，闭区间）内的周期单位数。
// daily=自然日数；weekly=周一数。end 早于 start 返回 0。
func countPeriodUnits(typ, start, end string) int64 {
	s, err1 := time.Parse("2006-01-02", start)
	e, err2 := time.Parse("2006-01-02", end)
	if err1 != nil || err2 != nil || e.Before(s) {
		return 0
	}
	if typ == "weekly" {
		n := int64(0)
		for d := s; !d.After(e); d = d.AddDate(0, 0, 1) {
			if d.Weekday() == time.Monday {
				n++
			}
		}
		return n
	}
	return int64(e.Sub(s).Hours()/24) + 1
}

// ==================== 未交提醒定时器 ====================

// reminderWindowMin 是触发窗口长度：当前时间落在 [deadline, deadline+10min) 内即触发一次。
// 无状态窗口法——不需要记录「今天是否已推过」，每 5 分钟 tick 一次只要命中窗口就推，
// 窗口只有 10 分钟，天然保证同一天/同一周只推一次（服务重启也不会重复推到第二天）。
const reminderWindowMin = 10

// parseClock 解析 "HH:MM"；非法回退默认 18:00。
func parseClock(s string) (hh, mm int) {
	hh, mm = 18, 0
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return
	}
	h, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	m, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return
	}
	return h, m
}

// deadlineToday 把 "HH:MM" 落到「今天」当地时间。
func deadlineToday(now time.Time, clock string) time.Time {
	hh, mm := parseClock(clock)
	return time.Date(now.Year(), now.Month(), now.Day(), hh, mm, 0, 0, now.Location())
}

// inReminderWindow now 是否在 [start, start+10min) 内。
func inReminderWindow(now, start time.Time) bool {
	return !now.Before(start) && now.Before(start.Add(reminderWindowMin*time.Minute))
}

// startReminderLoop 每 5 分钟 tick 一次，命中窗口则逐公司发未交提醒。
func startReminderLoop(srv socketapi.Server, store db.Store, cfg *config.ServerConfig) {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		ctx := context.Background()
		logger.Info(mod, "未交提醒定时器已启动",
			"daily", cfg.ReportRemindDaily, "weekly", cfg.ReportRemindWeekly, "windowMin", reminderWindowMin)
		for range ticker.C {
			// 定时器任何 panic 都不能炸掉整个 goroutine。
			func() {
				defer func() {
					if r := recover(); r != nil {
						logger.Error(mod, "未交提醒定时器 panic（已恢复）", "recover", r)
					}
				}()
				tickReminders(ctx, srv, store, cfg)
			}()
		}
	}()
}

// tickReminders 一次 tick：判断今天该不该触发日报/周报提醒，然后逐公司计算缺交名单。
func tickReminders(ctx context.Context, srv socketapi.Server, store db.Store, cfg *config.ServerConfig) {
	now := time.Now()
	// 窗口判定（无状态）：当前时间落在 [deadline, deadline+10min) 即触发。
	fireDaily := inReminderWindow(now, deadlineToday(now, cfg.ReportRemindDaily))
	// 周报仅周五触发。
	fireWeekly := now.Weekday() == time.Friday && inReminderWindow(now, deadlineToday(now, cfg.ReportRemindWeekly))
	if !fireDaily && !fireWeekly {
		return
	}
	// periodDate：日报=当天，周报=本周周一（与 report:current 口径一致）。
	todayStr := now.Format("2006-01-02")
	mondayStr := weekMonday(now).Format("2006-01-02")

	companies, err := store.ListCompanies(ctx, "")
	if err != nil {
		logger.Error(mod, "未交提醒：列出公司失败", "error", err.Error())
		return
	}
	for _, comp := range companies {
		if fireDaily {
			fireCompanyReminder(ctx, srv, store, comp.ID, "daily", todayStr)
		}
		if fireWeekly {
			fireCompanyReminder(ctx, srv, store, comp.ID, "weekly", mondayStr)
		}
	}
}

// fireCompanyReminder 单公司单类型：算缺交名单 → 推给该公司全部 admin/owner 的 user room。
func fireCompanyReminder(ctx context.Context, srv socketapi.Server, store db.Store, companyID int64, typ, periodDate string) {
	members, err := store.GetMembers(ctx, companyID)
	if err != nil {
		logger.Error(mod, "未交提醒：列出公司成员失败", "companyId", companyID, "error", err.Error())
		return
	}
	if len(members) == 0 {
		return
	}
	userIDs := make([]int64, 0, len(members))
	for _, m := range members {
		userIDs = append(userIDs, m.UserID)
	}
	// 取该周期已提交的报告（统计用范围查询，start=end=periodDate）。
	reports, err := store.ListReportsByPeriod(ctx, companyID, typ, periodDate, periodDate, userIDs)
	if err != nil {
		logger.Error(mod, "未交提醒：查询周期报告失败", "companyId", companyID, "type", typ, "periodDate", periodDate, "error", err.Error())
		return
	}
	submitted := map[int64]bool{}
	for _, r := range reports {
		if r.Submitted {
			submitted[r.UserID] = true
		}
	}
	missing := []map[string]any{}
	adminIDs := []int64{}
	for _, m := range members {
		if m.Role == "owner" || m.Role == "admin" {
			adminIDs = append(adminIDs, m.UserID)
		}
		if !submitted[m.UserID] {
			nick := ""
			if m.Nick != nil {
				nick = *m.Nick
			}
			username := ""
			if m.Username != nil {
				username = *m.Username
			}
			missing = append(missing, map[string]any{
				"userId":   m.UserID,
				"nick":     nick,
				"username": username,
			})
		}
	}
	payload := map[string]any{
		"companyId":    companyID,
		"type":         typ,
		"periodDate":   periodDate,
		"missingUsers": missing,
	}
	for _, uid := range adminIDs {
		if err := srv.EmitTo(util.UserRoom(uid), "report:reminder", payload); err != nil {
			logger.Error(mod, "未交提醒推送失败", "companyId", companyID, "userId", uid, "error", err.Error())
		}
	}
	logger.Info(mod, "未交提醒已推送",
		"companyId", companyID, "type", typ, "periodDate", periodDate,
		"missing", len(missing), "admins", len(adminIDs))
}
