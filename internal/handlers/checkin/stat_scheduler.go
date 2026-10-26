package checkin

// 月度考勤统计：每月最后一天 23:59:00 为每个公司每个在职成员生成当月考勤统计 CSV，
// 生成后推送给公司 owner/admin（其余成员不提醒）。财务/人事在客户端私聊发送给员工。
//
// 参考 report 包未交提醒的调度/推送模式（goroutine + ticker + recover + EmitTo user room）。

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mochi-oa-server-go/internal/config"
	"mochi-oa-server-go/internal/db"
	"mochi-oa-server-go/internal/logger"
	"mochi-oa-server-go/internal/socketapi"
	"mochi-oa-server-go/internal/types"
	"mochi-oa-server-go/internal/util"
)

// StartMonthlyStatScheduler 启动月度考勤统计定时器：每月最后一天 23:59:00 触发（分钟级、幂等）。
func StartMonthlyStatScheduler(srv socketapi.Server, store db.Store, cfg *config.ServerConfig) {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		ctx := context.Background()
		var lastRunYM string
		logger.Info(mod, "月度考勤统计定时器已启动")
		for range ticker.C {
			func() {
				defer func() {
					if r := recover(); r != nil {
						logger.Error(mod, "月度考勤统计定时器 panic（已恢复）", "recover", r)
					}
				}()
				now := time.Now().In(localTZ)
				ym := now.Format("2006-01")
				if now.Day() != lastDayOfMonth(now) || now.Hour() != 23 || now.Minute() != 59 {
					return
				}
				if lastRunYM == ym {
					return // 本月已生成，幂等
				}
				lastRunYM = ym
				logger.Info(mod, "月度考勤统计触发", "month", ym)
				tickMonthlyStats(ctx, srv, store, cfg, ym)
			}()
		}
	}()
}

// lastDayOfMonth 当月最后一天（1..31）。
func lastDayOfMonth(t time.Time) int {
	firstNext := time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, t.Location())
	return firstNext.AddDate(0, 0, -1).Day()
}

// tickMonthlyStats 逐公司生成当月统计。
func tickMonthlyStats(ctx context.Context, srv socketapi.Server, store db.Store, cfg *config.ServerConfig, ym string) {
	companies, err := store.ListCompanies(ctx, "")
	if err != nil {
		logger.Error(mod, "月度统计：列出公司失败", "error", err.Error())
		return
	}
	for _, comp := range companies {
		genCompanyMonthlyStats(ctx, srv, store, cfg, comp.ID, ym)
	}
}

// genCompanyMonthlyStats 单公司：为每个成员生成当月统计 CSV 并通知 owner/admin。
func genCompanyMonthlyStats(ctx context.Context, srv socketapi.Server, store db.Store, cfg *config.ServerConfig, companyID int64, ym string) {
	members, err := store.GetMembers(ctx, companyID)
	if err != nil {
		logger.Error(mod, "月度统计：列出公司成员失败", "companyId", companyID, "error", err.Error())
		return
	}
	if len(members) == 0 {
		return
	}
	rules, err := store.GetCheckinRules(ctx, companyID)
	if err != nil {
		logger.Error(mod, "月度统计：取排班规则失败", "companyId", companyID, "error", err.Error())
		return
	}
	mode, graceLate, graceEarly, _, week, alt := loadRules(rules)

	outDir := filepath.Join(cfg.File.Dir, "attendance_stats", fmt.Sprintf("%d", companyID), ym)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		logger.Error(mod, "月度统计：创建目录失败", "dir", outDir, "error", err.Error())
		return
	}

	var adminIDs []int64
	generated := []map[string]any{}
	for _, m := range members {
		if m.Role == "owner" || m.Role == "admin" {
			adminIDs = append(adminIDs, m.UserID)
		}
		nick := memberNick(m)
		ms := computeMonthlyStat(ctx, store, companyID, m.UserID, ym, mode, graceLate, graceEarly, week, alt)
		if ms == nil {
			continue
		}
		fname := fmt.Sprintf("%d_%s.csv", m.UserID, sanitizeFile(nick))
		if err := writeMonthlyCSV(filepath.Join(outDir, fname), ms, nick); err != nil {
			logger.Error(mod, "月度统计：写 CSV 失败", "companyId", companyID, "user", m.UserID, "error", err.Error())
			continue
		}
		generated = append(generated, map[string]any{"userId": m.UserID, "nick": nick, "file": fname})
	}

	// 通知 owner/admin（其余成员不提醒）。
	payload := map[string]any{"companyId": companyID, "month": ym, "count": len(generated), "files": generated}
	for _, uid := range adminIDs {
		if err := srv.EmitTo(util.UserRoom(uid), "attendance:stat:generated", payload); err != nil {
			logger.Error(mod, "月度统计通知推送失败", "companyId", companyID, "userId", uid, "error", err.Error())
		}
	}
	logger.Info(mod, "月度考勤统计已生成并通知",
		"companyId", companyID, "month", ym, "members", len(generated), "admins", len(adminIDs))
}

// dayStat 单日明细。
type dayStat struct {
	Date   string
	Week   string
	In     string
	Out    string
	Status string
}

// monthStat 单成员月度统计。
type monthStat struct {
	WorkDays, Actual, Normal, Late, Early, Missing int
	Makeup, LeaveDays, CompDays                     int
	OvertimeHours                                   float64
	Days                                            []dayStat
}

// computeMonthlyStat 计算某成员某月统计（纯函数，复用 checkin 状态判断）。
func computeMonthlyStat(ctx context.Context, store db.Store, companyID, userID int64, ym, mode string, graceLate, graceEarly int64, week []dayRule, alt *dayRule) *monthStat {
	punches, err := store.ListCheckinsByMonth(ctx, companyID, userID, ym)
	if err != nil {
		logger.Error(mod, "月度统计：取打卡记录失败", "companyId", companyID, "user", userID, "month", ym, "error", err.Error())
		return nil
	}
	base, err := time.Parse("2006-01", ym)
	if err != nil {
		return nil
	}
	lastDay := lastDayOfMonth(time.Date(base.Year(), base.Month()+1, 1, 0, 0, 0, 0, localTZ))
	var ms monthStat
	for d := 1; d <= lastDay; d++ {
		dateStr := fmt.Sprintf("%s-%02d", ym, d)
		in, out := punchInOut(punches, dateStr)
		resp := computeDayStatus(dateStr, mode, graceLate, graceEarly, week, alt, in, out)
		rest, _ := resp["rest"].(bool)
		if rest {
			continue
		}
		ms.WorkDays++
		st, _ := resp["status"].(string)
		switch st {
		case "normal":
			ms.Normal++
			ms.Actual++
		case "late":
			ms.Late++
			ms.Actual++
		case "early":
			ms.Early++
			ms.Actual++
		case "late_early":
			ms.Late++
			ms.Early++
			ms.Actual++
		case "none":
			ms.Missing++ // 应出勤但无打卡 = 缺卡
		}
		weekCN := ""
		if dt, e := time.Parse("2006-01-02", dateStr); e == nil {
			weekCN = weekdayCN(int(dt.Weekday()))
		}
		ms.Days = append(ms.Days, dayStat{Date: dateStr, Week: weekCN, In: emptyOrNil(in), Out: emptyOrNil(out), Status: st})
	}

	// 申请统计：补卡/请假/调休 approved。
	reqs, _, err := store.ListAttendanceRequestsByUser(ctx, companyID, userID, "", ym+"-01", ym+"-31", 1, 100)
	if err == nil {
		for _, r := range reqs {
			if r.Status != "approved" {
				continue
			}
			switch r.Type {
			case "make_up":
				ms.Makeup++
			case "leave":
				ms.LeaveDays++
				if r.DurationHours != nil {
					ms.OvertimeHours += *r.DurationHours
				}
			case "comp_time":
				ms.CompDays++
			}
		}
	}
	return &ms
}

// writeMonthlyCSV 写 CSV：表头 + 每日明细 + 月度汇总。
func writeMonthlyCSV(path string, ms *monthStat, nick string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()

	w.Write([]string{"月份", "姓名", "应出勤", "实出勤", "正常", "迟到", "早退", "缺卡", "补卡", "请假", "调休", "异常天数"})
	// 每日明细
	abnormal := ms.Late + ms.Early + ms.Missing
	w.Write([]string{"【每日明细】", "", "", "", "", "", "", "", "", "", "", ""})
	for _, d := range ms.Days {
		w.Write([]string{d.Date, d.Week, d.In, d.Out, d.Status, "", "", "", "", "", "", ""})
	}
	// 月度汇总
	w.Write([]string{"【月度汇总】", "", "", "", "", "", "", "", "", "", "", ""})
	w.Write([]string{
		"", nick,
		fmt.Sprintf("%d", ms.WorkDays), fmt.Sprintf("%d", ms.Actual),
		fmt.Sprintf("%d", ms.Normal), fmt.Sprintf("%d", ms.Late),
		fmt.Sprintf("%d", ms.Early), fmt.Sprintf("%d", ms.Missing),
		fmt.Sprintf("%d", ms.Makeup), fmt.Sprintf("%d", ms.LeaveDays),
		fmt.Sprintf("%d", ms.CompDays), fmt.Sprintf("%d", abnormal),
	})
	return w.Error()
}

func memberNick(m types.Member) string {
	if m.Nick != nil && *m.Nick != "" {
		return *m.Nick
	}
	if m.Username != nil {
		return *m.Username
	}
	return fmt.Sprintf("%d", m.UserID)
}

// sanitizeFile 去掉文件名非法字符。
func sanitizeFile(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" {
		out = "user"
	}
	return out
}

func emptyOrNil(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func weekdayCN(w int) string {
	return []string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}[w]
}

// RegisterStatHandlers 注册考勤统计相关 socket 事件。
func RegisterStatHandlers(srv socketapi.Server, store db.Store, cfg *config.ServerConfig) {
	// ---- attendance:stat:list：admin/owner 查某公司某月已生成的考勤统计 CSV（含下载 URL）----
	srv.On("attendance:stat:list", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		a, cid, role, ok := requireMember(c, d, store, ack)
		if !ok {
			return
		}
		if role != "owner" && role != "admin" {
			ack(util.Fail("仅管理员可查看考勤统计"))
			return
		}
		month := strField(d, "month")
		if month == "" {
			month = time.Now().In(localTZ).Format("2006-01")
		}
		dir := filepath.Join(cfg.File.Dir, "attendance_stats", fmt.Sprintf("%d", cid), month)
		entries, err := os.ReadDir(dir)
		if err != nil {
			ack(util.OK(map[string]any{"files": []any{}, "month": month}))
			return
		}
		files := []map[string]any{}
		for _, e := range entries {
			if e.IsDir() || strings.ToLower(filepath.Ext(e.Name())) != ".csv" {
				continue
			}
			rel := "attendance_stats/" + fmt.Sprintf("%d", cid) + "/" + month + "/" + e.Name()
			files = append(files, map[string]any{"name": e.Name(), "url": "/files/" + rel})
		}
		logger.Info(mod, "attendance:stat:list", "companyId", cid, "userId", a.ID, "month", month, "files", len(files))
		ack(util.OK(map[string]any{"files": files, "month": month}))
	})
}
