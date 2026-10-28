package checkin

// 打卡提醒定时器：上班前 30 分钟、下班后 30 分钟，给未打卡员工推送聊天卡片提醒（含消息音）。
//   - 上班提醒：当天应出勤且未打上班卡 → 「还有一会儿上班，别忘记打卡」
//   - 下班提醒：当天已打上班卡但未打下班卡 → 「该下班了，别忘记打卡」
//
// 调度模式沿用月度统计定时器：goroutine + 每分钟 tick + recover + 逐公司遍历 + EmitTo user room。
// 幂等：内存 map 记录「公司|日期|类型」已提醒，服务内存生命周期内同窗口只发一次。

import (
	"context"
	"fmt"
	"time"

	"mochi-oa-server-go/internal/db"
	"mochi-oa-server-go/internal/logger"
	"mochi-oa-server-go/internal/socketapi"
	"mochi-oa-server-go/internal/util"
)

// StartCheckinReminderScheduler 启动打卡提醒定时器。
func StartCheckinReminderScheduler(srv socketapi.Server, store db.Store) {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		ctx := context.Background()
		sent := map[string]bool{}
		logger.Info(mod, "打卡提醒定时器已启动")
		for range ticker.C {
			func() {
				defer func() {
					if r := recover(); r != nil {
						logger.Error(mod, "打卡提醒定时器 panic（已恢复）", "recover", r)
					}
				}()
				now := time.Now().In(localTZ)
				nowMin := now.Hour()*60 + now.Minute()
				today := now.Format("2006-01-02")
				companies, err := store.ListCompanies(ctx, "")
				if err != nil {
					logger.Error(mod, "打卡提醒：列出公司失败", "error", err.Error())
					return
				}
				for _, comp := range companies {
					rules, err := store.GetCheckinRules(ctx, comp.ID)
					if err != nil {
						logger.Error(mod, "打卡提醒：取排班规则失败", "companyId", comp.ID, "error", err.Error())
						continue
					}
					mode, _, _, week, alt := loadRules(rules)
					rest, ws, we := dayScheduleFor(now, mode, week, alt)
					if rest {
						continue // 休息日不发提醒
					}
					wsMin := minutesSinceMidnight(ws)
					weMin := minutesSinceMidnight(we)
					if ws != "" && nowMin >= wsMin-30 && nowMin < wsMin {
						remindPunch(ctx, srv, store, comp.ID, now, today, "work", sent,
							"还有一会儿上班，别忘记打卡", "上班打卡提醒")
					}
					if we != "" && nowMin >= weMin && nowMin <= weMin+30 {
						remindPunch(ctx, srv, store, comp.ID, now, today, "off", sent,
							"该下班了，别忘记打卡", "下班打卡提醒")
					}
				}
			}()
		}
	}()
}

// dayScheduleFor 按业务本地日期取当天排班（dow + bigsmall 交替）。rest=true 表示休息。
func dayScheduleFor(t time.Time, mode string, week []dayRule, alt *dayRule) (rest bool, start, end string) {
	dow := int(t.Weekday())
	dr := dayRule{dow: dow, rest: true}
	for _, w := range week {
		if w.dow == dow {
			dr = w
			break
		}
	}
	start, end = dr.start, dr.end
	if mode == "bigsmall" && alt != nil && !alt.rest && dow == alt.dow {
		_, weekISO := t.ISOWeek()
		if weekISO%2 == 0 {
			start, end = alt.start, alt.end
		}
	}
	return dr.rest, start, end
}

// remindPunch 给当天该提醒但未打卡/未打下班卡的员工推送一条打卡提醒（幂等，每公司每日期每类型一次）。
func remindPunch(ctx context.Context, srv socketapi.Server, store db.Store, companyID int64, now time.Time, today, kind string, sent map[string]bool, body, title string) {
	key := fmt.Sprintf("%d|%s|%s", companyID, today, kind)
	if sent[key] {
		return
	}
	sent[key] = true // 先标记，避免失败重发风暴；服务重启会重发（可接受）
	members, err := store.GetMembers(ctx, companyID)
	if err != nil {
		logger.Error(mod, "打卡提醒：列出成员失败", "companyId", companyID, "error", err.Error())
		return
	}
	for _, m := range members {
		if !punchRemindEligible(ctx, store, companyID, m.UserID, today, kind) {
			continue
		}
		payload := map[string]any{
			"type": kind, "title": title, "body": body,
			"companyId": companyID, "date": today,
		}
		if err := srv.EmitTo(util.UserRoom(m.UserID), "checkin:reminder", payload); err != nil {
			logger.Error(mod, "打卡提醒推送失败", "companyId", companyID, "userId", m.UserID, "error", err.Error())
		}
	}
	logger.Info(mod, "打卡提醒已推送", "companyId", companyID, "date", today, "kind", kind)
}

// punchRemindEligible 判断员工是否命中提醒：上班提醒=当天无任何打卡；下班提醒=当天只有上班卡无下班卡。
func punchRemindEligible(ctx context.Context, store db.Store, companyID, userID int64, today, kind string) bool {
	punches, err := store.ListCheckinsByMonth(ctx, companyID, userID, today[:7])
	if err != nil {
		return false
	}
	var in, out string
	for _, p := range punches {
		if p.CheckDate == today {
			if in == "" {
				in = p.CheckTime
			}
			out = p.CheckTime
		}
	}
	switch kind {
	case "work":
		return in == "" // 未打上班卡
	case "off":
		return in != "" && out == "" // 已上班但未下班
	}
	return false
}
