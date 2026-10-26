// Package checkin 实现打卡（check-in）与公司打卡白名单配置的 socket 事件。
//
// 事件（均 requireMember 公司隔离；白名单写操作需 admin/owner）：
//   - checkin:create / list
//   - checkin:settings:get / save
package checkin

import (
	"context"
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"mochi-oa-server-go/internal/db"
	"mochi-oa-server-go/internal/logger"
	"mochi-oa-server-go/internal/socketapi"
	"mochi-oa-server-go/internal/types"
	"mochi-oa-server-go/internal/util"
)

const mod = "handlers/checkin"

// ─── 共享小工具（本包私有副本，与 report/attendance 包同款语义） ───

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

// headerGet 从 handshakeHeaders 取头值（与 auth.go 同款：逐 key 大小写不敏感扫描）。
// 注意：main.go 构建 handshakeHeaders 时直接遍历 http.Header，key 是 Go canonical 形式
// （如 "X-Real-Ip"），不能用 m[strings.ToLower(name)] 直查（会永远 miss 导致 realIp 恒空）。
// 兼容 string / []string / []any 多值。
func headerGet(m map[string]any, name string) string {
	n := strings.ToLower(name)
	for k, v := range m {
		if strings.ToLower(k) != n {
			continue
		}
		switch t := v.(type) {
		case string:
			return strings.TrimSpace(t)
		case []string:
			if len(t) > 0 {
				return strings.TrimSpace(t[0])
			}
		case []any:
			if len(t) > 0 {
				if s, ok := t[0].(string); ok {
					return strings.TrimSpace(s)
				}
			}
		}
	}
	return ""
}

// realIp 解析客户端真实 IP（与 auth.go 同款头链降级方案）：
// CF-Connecting-IP → X-Forwarded-For 最左 → X-Real-IP → X-Client-IP → TCP 直连地址。
// 注：无条件信任代理头（与 TS/生产反代一致）。
func realIp(c socketapi.Conn) string {
	headers, _ := c.Get("handshakeHeaders").(map[string]any)
	first := func(s string) string {
		if i := strings.Index(s, ","); i >= 0 {
			s = s[:i]
		}
		return strings.TrimSpace(s)
	}
	if headers != nil {
		if v := headerGet(headers, "cf-connecting-ip"); v != "" {
			return first(v)
		}
		if v := headerGet(headers, "x-forwarded-for"); v != "" {
			return first(v)
		}
		if v := headerGet(headers, "x-real-ip"); v != "" {
			return first(v)
		} else if v := headerGet(headers, "x-client-ip"); v != "" {
			return first(v)
		}
	}
	if v, ok := c.Get("remoteAddr").(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
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

var monthRe = regexp.MustCompile(`^\d{4}-\d{2}$`)

// strList 从 payload 取字符串数组；非法返回 false。
func strList(m map[string]any, key string) ([]string, bool) {
	arr, ok := m[key].([]any)
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

// macInList 大小写不敏感匹配 MAC。
func macInList(list []string, mac string) bool {
	target := strings.ToLower(strings.TrimSpace(mac))
	if target == "" {
		return false
	}
	for _, m := range list {
		if strings.ToLower(strings.TrimSpace(m)) == target {
			return true
		}
	}
	return false
}

// ─── 注册入口 ───

// RegisterCheckinHandlers 注册 checkin:* 与 checkin:settings:* 事件。
func RegisterCheckinHandlers(srv socketapi.Server, store db.Store) {
	ctx := context.Background()

	// ---- checkin:create：打卡 ----
	srv.On("checkin:create", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		a, cid, _, ok := requireMember(c, d, store, ack)
		if !ok {
			return
		}
		mac := strings.TrimSpace(strField(d, "mac"))
		ip := realIp(c)

		// 白名单校验：未配置=该类别不限制；配置了才逐项比对。
		settings, err := store.GetCheckinSettings(ctx, cid)
		if err != nil {
			logger.Error(mod, "checkin:create 读白名单失败", "companyId", cid, "userId", a.ID, "error", err.Error())
			ack(util.Fail("操作失败"))
			return
		}
		if settings != nil {
			// 仅当「只允许指定 MAC 打卡」开关开启时才启用 MAC 限制。
			if settings.MacEnabled {
				if len(settings.MacWhitelist) == 0 {
					logger.Warn(mod, "checkin:create MAC 开关开启但未配置列表", "companyId", cid, "userId", a.ID, "mac", mac)
					ack(util.Fail("已开启只允许指定MAC打卡，但尚未配置可打卡MAC列表"))
					return
				}
				if !macInList(settings.MacWhitelist, mac) {
					logger.Warn(mod, "checkin:create MAC 不在白名单", "companyId", cid, "userId", a.ID, "mac", mac)
					ack(util.Fail("MAC 不在可打卡白名单"))
					return
				}
			}
			if len(settings.IpWhitelist) > 0 {
				allow := false
				for _, w := range settings.IpWhitelist {
					if strings.TrimSpace(w) == ip {
						allow = true
						break
					}
				}
				if !allow {
					logger.Warn(mod, "checkin:create IP 不在白名单", "companyId", cid, "userId", a.ID, "ip", ip)
					ack(util.Fail("IP 不在白名单"))
					return
				}
			}
		}

		// check_date 归属：次日下班截止前（默认 04:00）打的卡归前一天（补前天下班卡），
		// 其余时刻归当天。避免 UTC 日期错位，一律按业务本地口径。
		checkDate := time.Now().In(localTZ).Format("2006-01-02")
		if r, e := store.GetCheckinRules(ctx, cid); e == nil {
			_, _, _, offDeadline, _, _ := loadRules(r)
			checkDate, _ = decidePunchKind(time.Now().In(localTZ), offDeadline)
		}
		var macPtr *string
		if mac != "" {
			macPtr = &mac
		}
		item, kind, err := store.CreateCheckin(ctx, cid, a.ID, checkDate, ip, macPtr)
		if err != nil {
			logger.Error(mod, "checkin:create 写入失败", "companyId", cid, "userId", a.ID, "error", err.Error())
			ack(util.Fail("打卡失败"))
			return
		}
		logger.Info(mod, "checkin:create 成功", "kind", kind, "companyId", cid, "userId", a.ID, "date", checkDate, "ip", ip, "mac", mac)
		ack(util.OK(map[string]any{"checkin": item, "kind": kind}))
	})

	// ---- checkin:list：本人某月，按天分组（date 倒序、组内 check_time 升序） ----
	srv.On("checkin:list", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		a, cid, _, ok := requireMember(c, d, store, ack)
		if !ok {
			return
		}
		month := strField(d, "month")
		if !monthRe.MatchString(month) {
			ack(util.Fail("月份格式不合法"))
			return
		}
		items, err := store.ListCheckinsByMonth(ctx, cid, a.ID, month)
		if err != nil {
			logger.Error(mod, "checkin:list 查询失败", "companyId", cid, "userId", a.ID, "month", month, "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		// 按 check_date 分组。
		byDate := map[string][]types.CheckinItem{}
		for _, it := range items {
			byDate[it.CheckDate] = append(byDate[it.CheckDate], it)
		}
		days := []types.CheckinDay{}
		// 日期倒序。
		dates := make([]string, 0, len(byDate))
		for k := range byDate {
			dates = append(dates, k)
		}
		sort.Sort(sort.Reverse(sort.StringSlice(dates)))
		for _, date := range dates {
			recs := byDate[date] // store 已按 check_time 升序
			day := types.CheckinDay{
				Date:      date,
				Count:     int64(len(recs)),
				Records:   recs,
				FirstTime: recs[0].CheckTime,
				LastTime:  recs[len(recs)-1].CheckTime,
			}
			days = append(days, day)
		}
		ack(util.OK(map[string]any{"days": days}))
	})

	// ---- checkin:settings:get：成员可读 ----
	srv.On("checkin:settings:get", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		_, cid, _, ok := requireMember(c, d, store, ack)
		if !ok {
			return
		}
		s, err := store.GetCheckinSettings(ctx, cid)
		if err != nil {
			logger.Error(mod, "checkin:settings:get 失败", "companyId", cid, "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		// 未配置返回空白名单结构。
		macList := []string{}
		ipList := []string{}
		if s != nil {
			macList = s.MacWhitelist
			ipList = s.IpWhitelist
		}
		ack(util.OK(map[string]any{"settings": map[string]any{
			"macEnabled":   s != nil && s.MacEnabled,
			"macWhitelist": macList,
			"ipWhitelist":  ipList,
		}}))
	})

	// ---- checkin:settings:save：admin/owner，trim + 去重 ----
	srv.On("checkin:settings:save", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		_, cid, ok := requireAdmin(c, d, store, ack)
		if !ok {
			return
		}
		macRaw, ok := strList(d, "macWhitelist")
		if !ok {
			ack(util.Fail("MAC 白名单格式不合法"))
			return
		}
		ipRaw, ok := strList(d, "ipWhitelist")
		if !ok {
			ack(util.Fail("IP 白名单格式不合法"))
			return
		}
		macEnabled := false
		if v, ok := d["macEnabled"]; ok {
			switch t := v.(type) {
			case bool:
				macEnabled = t
			case string:
				macEnabled = t == "true" || t == "1"
			case float64:
				macEnabled = t != 0
			}
		}
		macList := dedupMac(macRaw)
		ipList := dedupPlain(ipRaw)
		s, err := store.SaveCheckinSettings(ctx, cid, macEnabled, macList, ipList)
		if err != nil {
			logger.Error(mod, "checkin:settings:save 失败", "companyId", cid, "error", err.Error())
			ack(util.Fail("保存失败"))
			return
		}
		ack(util.OK(map[string]any{"settings": s}))
	})

	// ---- checkin:rules:get（成员；无配置返回默认规则并懒播种） ----
	srv.On("checkin:rules:get", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		_, cid, _, ok := requireMember(c, d, store, ack)
		if !ok {
			return
		}
		r, err := store.GetCheckinRules(ctx, cid)
		if err != nil {
			logger.Error(mod, "checkin:rules:get 查询失败", "companyId", cid, "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		if r == nil {
			// 懒播种默认规则：standard 双休，周一~五 09:30-18:30，周六日休息。
			def := defaultStandardRules()
			graceLate, _ := toInt64(def["grace_late_min"])
			graceEarly, _ := toInt64(def["grace_early_min"])
			if err := store.SaveCheckinRules(ctx, cid, def["mode"].(string),
				graceLate, graceEarly, def["week_rules"].(string), nil, def["offwork_deadline"].(string)); err != nil {
				logger.Error(mod, "checkin:rules:get 懒播种失败", "companyId", cid, "error", err.Error())
				ack(util.Fail("获取失败"))
				return
			}
			ack(util.OK(map[string]any{"rules": def}))
			return
		}
		ack(util.OK(map[string]any{"rules": rulesRowToMap(r)}))
	})

	// ---- checkin:rules:save（admin/owner，校验后保存） ----
	srv.On("checkin:rules:save", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		_, cid, ok := requireAdmin(c, d, store, ack)
		if !ok {
			return
		}
		mode := strings.TrimSpace(strField(d, "mode"))
		if !validRuleMode(mode) {
			logger.Warn(mod, "checkin:rules:save mode 非法", "companyId", cid, "mode", mode)
			ack(util.Fail("排班模式不合法"))
			return
		}
		graceLate := int64(0)
		if v, has := d["grace_late_min"]; has {
			if n, ok := toInt64(v); ok {
				graceLate = n
			}
		}
		graceEarly := int64(0)
		if v, has := d["grace_early_min"]; has {
			if n, ok := toInt64(v); ok {
				graceEarly = n
			}
		}
		// week_rules 校验：7 项完整、HH:MM 格式、start<end。
		wr, has := d["week_rules"].([]any)
		if !has || len(wr) != 7 {
			ack(util.Fail("周规则需为 7 项"))
			return
		}
		weekOut := make([]map[string]any, 0, 7)
		seenDow := map[int]bool{}
		for _, item := range wr {
			m, ok := item.(map[string]any)
			if !ok {
				ack(util.Fail("周规则项格式不合法"))
				return
			}
			dow, ok := toInt64(m["dow"])
			if !ok || dow < 0 || dow > 6 {
				ack(util.Fail("周规则 dow 不合法"))
				return
			}
			if seenDow[int(dow)] {
				ack(util.Fail("周规则 dow 重复"))
				return
			}
			seenDow[int(dow)] = true
			start := strings.TrimSpace(toString(m["start"]))
			end := strings.TrimSpace(toString(m["end"]))
			rest, _ := m["rest"].(bool)
			if !rest {
				if !hhmm(start) || !hhmm(end) || start >= end {
					ack(util.Fail("工作时间格式不合法（需 HH:MM 且 start<end）"))
					return
				}
			}
			weekOut = append(weekOut, map[string]any{"dow": dow, "start": start, "end": end, "rest": rest})
		}
		weekBytes, _ := json.Marshal(weekOut)
		// alt_rule 校验（可选）。
		var altJSON *string
		if altRaw, has := d["alt_rule"]; has && altRaw != nil {
			am, ok := altRaw.(map[string]any)
			if !ok {
				ack(util.Fail("交替规则格式不合法"))
				return
			}
			dow, ok := toInt64(am["dow"])
			if !ok || dow < 0 || dow > 6 {
				ack(util.Fail("交替规则 dow 不合法"))
				return
			}
			as := strings.TrimSpace(toString(am["altStart"]))
			ae := strings.TrimSpace(toString(am["altEnd"]))
			if !hhmm(as) || !hhmm(ae) || as >= ae {
				ack(util.Fail("交替工作时间格式不合法（需 HH:MM 且 altStart<altEnd）"))
				return
			}
			b, _ := json.Marshal(map[string]any{"dow": dow, "altStart": as, "altEnd": ae})
			s := string(b)
			altJSON = &s
		}
		// 下班卡次日截止（HH:MM，默认 04:00；管理员可调）。
		offDeadline := strings.TrimSpace(strField(d, "offwork_deadline"))
		if offDeadline == "" {
			offDeadline = "04:00"
		}
		if !hhmm(offDeadline) {
			ack(util.Fail("下班卡截止时间格式不合法（需 HH:MM）"))
			return
		}
		if err := store.SaveCheckinRules(ctx, cid, mode, graceLate, graceEarly, string(weekBytes), altJSON, offDeadline); err != nil {
			logger.Error(mod, "checkin:rules:save 失败", "companyId", cid, "error", err.Error())
			ack(util.Fail("保存失败"))
			return
		}
		r, _ := store.GetCheckinRules(ctx, cid)
		ack(util.OK(map[string]any{"rules": rulesRowToMap(r)}))
	})

	// ---- checkin:status:get（成员；按规则+打卡记录判断当天状态） ----
	srv.On("checkin:status:get", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		a, cid, _, ok := requireMember(c, d, store, ack)
		if !ok {
			return
		}
		dateStr := strings.TrimSpace(strField(d, "date"))
		if !dateRe.MatchString(dateStr) {
			ack(util.Fail("日期格式不合法（YYYY-MM-DD）"))
			return
		}
		// 取公司规则（无则用默认）。
		r, err := store.GetCheckinRules(ctx, cid)
		if err != nil {
			logger.Error(mod, "checkin:status:get 查询规则失败", "companyId", cid, "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		mode, graceLate, graceEarly, _, weekRules, altRule := loadRules(r)
		// 当天打卡记录（复用 list 同源查询，按月取后过滤当天）。
		punches, _ := store.ListCheckinsByMonth(ctx, cid, a.ID, dateStr[:7])
		in, out := punchInOut(punches, dateStr)
		resp := computeDayStatus(dateStr, mode, graceLate, graceEarly, weekRules, altRule, in, out)
		// 缺卡细分：上班/下班未打卡（客户端用于「缺卡：上班未打卡、下班未打卡」提示）。
		resp["missingWork"] = in == ""
		resp["missingOff"] = out == ""
		ack(util.OK(map[string]any{"status": resp}))
	})
}

// dedupMac 逐条 trim、大小写不敏感去重。
func dedupMac(in []string) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		k := strings.ToLower(s)
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, s)
	}
	return out
}

// dedupPlain 逐条 trim、原样去重（IP）。
func dedupPlain(in []string) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
