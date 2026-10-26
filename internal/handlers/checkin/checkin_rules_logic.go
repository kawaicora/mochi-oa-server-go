package checkin

// 本文件实现打卡排班规则的默认值、校验与当天状态判断逻辑（纯函数，便于单测）。

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"mochi-oa-server-go/internal/types"
)

var dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
var hhmmRe = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

// localTZ 业务本地时区：打卡日期/上下班时间一律按本地口径，避免 UTC 与规则时间错位。
// 可用 TIMEZONE 环境变量覆盖，缺省 Asia/Shanghai。
var localTZ = mustLocalTZ()

func mustLocalTZ() *time.Location {
	name := os.Getenv("TIMEZONE")
	if name == "" {
		name = "Asia/Shanghai"
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		loc = time.Local
	}
	return loc
}

func hhmm(s string) bool { return hhmmRe.MatchString(s) }

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return fmt.Sprintf("%.0f", t)
	}
	return ""
}

// validRuleMode 排班模式白名单。
func validRuleMode(m string) bool {
	switch m {
	case "standard", "sixday", "nosleep", "bigsmall":
		return true
	}
	return false
}

// dayRule 解析后的单日规则。
type dayRule struct {
	dow   int
	start string // HH:MM
	end   string // HH:MM
	rest  bool
}

// defaultStandardRules 默认双休规则（懒播种用）：周一~五 09:30-18:30，周六日休息。
func defaultStandardRules() map[string]any {
	week := []map[string]any{
		{"dow": 0, "start": "", "end": "", "rest": true}, // 周日
		{"dow": 1, "start": "09:30", "end": "18:30", "rest": false},
		{"dow": 2, "start": "09:30", "end": "18:30", "rest": false},
		{"dow": 3, "start": "09:30", "end": "18:30", "rest": false},
		{"dow": 4, "start": "09:30", "end": "18:30", "rest": false},
		{"dow": 5, "start": "09:30", "end": "18:30", "rest": false},
		{"dow": 6, "start": "", "end": "", "rest": true}, // 周六
	}
	b, _ := json.Marshal(week)
	return map[string]any{
		"mode": "standard", "grace_late_min": 0, "grace_early_min": 0,
		"week_rules": string(b), "alt_rule": nil, "offwork_deadline": "04:00",
	}
}

// rulesRowToMap 把行转成契约 JSON（snake 字段名）。
func rulesRowToMap(r *types.CompanyCheckinRulesRow) map[string]any {
	if r == nil {
		d := defaultStandardRules()
		return d
	}
	return map[string]any{
		"mode": r.Mode, "grace_late_min": r.GraceLateMin, "grace_early_min": r.GraceEarlyMin,
		"week_rules": r.WeekRules, "alt_rule": r.AltRule, "offwork_deadline": defaultDeadline(r.OffworkDeadline),
	}
}

// defaultDeadline 下班卡次日截止时刻；空值回退 04:00。
func defaultDeadline(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "04:00"
	}
	return s
}

// loadRules 从行加载（无行则默认），返回解析后的周规则与下班卡截止。
func loadRules(r *types.CompanyCheckinRulesRow) (mode string, graceLate, graceEarly int64, offworkDeadline string, week []dayRule, alt *dayRule) {
	if r == nil {
		d := defaultStandardRules()
		mode = "standard"
		offworkDeadline = "04:00"
		week = parseWeekRules(d["week_rules"].(string))
		return
	}
	mode = r.Mode
	graceLate = r.GraceLateMin
	graceEarly = r.GraceEarlyMin
	offworkDeadline = defaultDeadline(r.OffworkDeadline)
	week = parseWeekRules(r.WeekRules)
	if r.AltRule != nil && *r.AltRule != "" && *r.AltRule != "null" {
		var a struct {
			Dow      int    `json:"dow"`
			AltStart string `json:"altStart"`
			AltEnd   string `json:"altEnd"`
		}
		if json.Unmarshal([]byte(*r.AltRule), &a) == nil {
			alt = &dayRule{dow: a.Dow, start: a.AltStart, end: a.AltEnd}
		}
	}
	return
}

func parseWeekRules(s string) []dayRule {
	var raw []struct {
		Dow   int    `json:"dow"`
		Start string `json:"start"`
		End   string `json:"end"`
		Rest  bool   `json:"rest"`
	}
	_ = json.Unmarshal([]byte(s), &raw)
	out := make([]dayRule, 0, len(raw))
	for _, w := range raw {
		out = append(out, dayRule{dow: w.Dow, start: w.Start, end: w.End, rest: w.Rest})
	}
	return out
}

// punchInOut 从当月记录中取当天首末打卡（复用 list 同源数据）。
// in/out 为当天"HH:MM"字符串；无记录则为空串。
// 仅一条记录时只算上班卡（in），下班未打卡（out 空），不得把同一次显示成上班+下班。
func punchInOut(items []types.CheckinItem, dateStr string) (in, out string) {
	var first, last time.Time
	has := false
	count := 0
	for _, it := range items {
		if it.CheckDate != dateStr {
			continue
		}
		t, err := time.Parse(time.RFC3339, it.CheckTime)
		if err != nil {
			continue
		}
		// 转业务本地时区再取时分，与本地口径规则时间比较。
		t = t.In(localTZ)
		count++
		if !has || t.Before(first) {
			first = t
		}
		if !has || t.After(last) {
			last = t
		}
		has = true
	}
	if !has {
		return "", ""
	}
	if count == 1 {
		// 只打一次卡 = 上班卡，下班未打卡。
		return first.Format("15:04"), ""
	}
	return first.Format("15:04"), last.Format("15:04")
}

// computeDayStatus 内聚的当天状态判断。
//   - bigsmall 模式：交替日（alt.dow）按 ISO 周序号奇偶选择——week%2==0 用 alt 时间，否则用主规则。
//   - 休息日 → rest；无打卡 → none；
//   - punchIn > start+graceLate → late；punchOut < end-graceEarly → early；两者都超 → late_early；否则 normal。
func computeDayStatus(dateStr, mode string, graceLate, graceEarly int64, week []dayRule, alt *dayRule, in, out string) map[string]any {
	resp := map[string]any{
		"date": dateStr, "mode": mode, "punchIn": emptyNil(in), "punchOut": emptyNil(out),
	}
	t, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		resp["status"] = "none"
		resp["rest"] = false
		return resp
	}
	dow := int(t.Weekday()) // Sunday=0
	// 找当天主规则。
	dr := dayRule{dow: dow, rest: true}
	for _, w := range week {
		if w.dow == dow {
			dr = w
			break
		}
	}
	// bigsmall 交替日按 ISO 周奇偶切换时间。
	workStart, workEnd := dr.start, dr.end
	if mode == "bigsmall" && alt != nil && !alt.rest && dow == alt.dow {
		_, weekISO := t.ISOWeek()
		if weekISO%2 == 0 {
			workStart, workEnd = alt.start, alt.end
		}
	}
	resp["rest"] = dr.rest
	resp["workStart"] = workStart
	resp["workEnd"] = workEnd

	if dr.rest {
		resp["status"] = "rest"
		return resp
	}
	if in == "" && out == "" {
		resp["status"] = "none"
		return resp
	}

	late, early := false, false
	// 迟到：punchIn 晚于 start+豁免。
	if in != "" {
		if minutesSinceMidnight(in) > minutesSinceMidnight(workStart)+int(graceLate) {
			late = true
		}
	}
	// 早退：punchOut 早于 end-豁免。
	if out != "" && workEnd != "" {
		if minutesSinceMidnight(out) < minutesSinceMidnight(workEnd)-int(graceEarly) {
			early = true
		}
	}
	switch {
	case late && early:
		resp["status"] = "late_early"
	case late:
		resp["status"] = "late"
	case early:
		resp["status"] = "early"
	default:
		resp["status"] = "normal"
	}
	return resp
}

func emptyNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// minutesSinceMidnight "HH:MM" → 当天分钟数。
func minutesSinceMidnight(hhmm string) int {
	var h, m int
	fmt.Sscanf(hhmm, "%d:%d", &h, &m)
	return h*60 + m
}

// decidePunchKind 按当前业务本地时刻判定本次打卡的归属日期与类型。
// 规则：
//   - 次日凌晨段 [00:00, offworkDeadline)（默认 04:00）：打的卡归属**前一天**，算前一天下班卡（补下班）；
//   - 其余时刻：归属**当天**（kind 由当天已打次数决定：0 次=上班卡、≥1=下班卡，第一次打卡永不拒绝，迟到仅作状态标记）。
//
// 返回 targetDate（业务本地 YYYY-MM-DD）与 isMidnight（是否处于次日凌晨补下班段）。
func decidePunchKind(now time.Time, offworkDeadline string) (targetDate string, isMidnight bool) {
	nowMin := now.Hour()*60 + now.Minute()
	deadMin := minutesSinceMidnight(defaultDeadline(offworkDeadline))
	if nowMin < deadMin {
		return now.AddDate(0, 0, -1).Format("2006-01-02"), true
	}
	return now.Format("2006-01-02"), false
}
