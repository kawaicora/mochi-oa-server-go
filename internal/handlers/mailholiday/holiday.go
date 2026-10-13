// holiday.go —— 假期 CRUD handler + 内置法定假期播种（对标 server/src/handlers/holiday.ts）。
//
// 事件（全部要求登录，ack 信封 util.OK/Fail 扁平）：
//   - holiday:list        假期列表（year 省略/0=全部）
//   - holiday:add         新增/修改假期（date 唯一，upsert）→ 广播 holiday:updated
//   - holiday:remove      删除假期 → 广播 holiday:updated
//   - holiday:addMany     批量新增/修改 → 广播 holiday:updated
//   - holiday:removeMany  批量删除 → 广播 holiday:updated
//   - holiday:reset       清空全部后重新种入官方法定安排 → 广播 holiday:updated
//
// 导出 SeedHolidays 供 main.go 启动时播种（对标 index.ts 的 seedHolidays 调用：
// 失败只打 error 不中断启动——本函数单条失败静默跳过，整体返回 nil，与 TS try/catch 语义一致）。
package mailholiday

import (
	"context"
	"encoding/json"

	"mochi-oa-server-go/internal/db"
	"mochi-oa-server-go/internal/logger"
	"mochi-oa-server-go/internal/socketapi"
	"mochi-oa-server-go/internal/types"
	"mochi-oa-server-go/internal/util"
)

// holidaySeed 单条种子：date=YYYY-MM-DD，type=legal（法定放假）|workday（调休补班上班日）。
type holidaySeed struct {
	Date string
	Name string
	Type string
}

// builtinHolidays 内置法定假期种子（依据《国务院办公厅关于2026年部分节假日安排的通知》
// 国办发明电〔2025〕7号），与 holiday.ts 的 seed 数组逐字一致。
// legal=法定放假日期（含假期内穿插的周六/周日），workday=调休补班上班日（周末上班）。
var builtinHolidays = []holidaySeed{
	// 元旦：1/1(四)-1/3(六)，1/4(日)补班
	{"2026-01-01", "元旦", "legal"},
	{"2026-01-02", "元旦", "legal"},
	{"2026-01-03", "元旦", "legal"},
	{"2026-01-04", "元旦调休上班", "workday"},
	// 春节：2/15(腊月廿八、日)-2/23(正月初七、一)，2/14(六)、2/28(六)补班
	{"2026-02-14", "春节调休上班", "workday"},
	{"2026-02-15", "春节", "legal"},
	{"2026-02-16", "春节", "legal"},
	{"2026-02-17", "春节", "legal"},
	{"2026-02-18", "春节", "legal"},
	{"2026-02-19", "春节", "legal"},
	{"2026-02-20", "春节", "legal"},
	{"2026-02-21", "春节", "legal"},
	{"2026-02-22", "春节", "legal"},
	{"2026-02-23", "春节", "legal"},
	{"2026-02-28", "春节调休上班", "workday"},
	// 清明节：4/4(六)-4/6(一)，无调休
	{"2026-04-04", "清明节", "legal"},
	{"2026-04-05", "清明节", "legal"},
	{"2026-04-06", "清明节", "legal"},
	// 劳动节：5/1(五)-5/5(二)，5/9(六)补班
	{"2026-05-01", "劳动节", "legal"},
	{"2026-05-02", "劳动节", "legal"},
	{"2026-05-03", "劳动节", "legal"},
	{"2026-05-04", "劳动节", "legal"},
	{"2026-05-05", "劳动节", "legal"},
	{"2026-05-09", "劳动节调休上班", "workday"},
	// 端午节：6/19(五)-6/21(日)，无调休
	{"2026-06-19", "端午节", "legal"},
	{"2026-06-20", "端午节", "legal"},
	{"2026-06-21", "端午节", "legal"},
	// 中秋节：9/25(五)-9/27(日)，无调休
	{"2026-09-25", "中秋节", "legal"},
	{"2026-09-26", "中秋节", "legal"},
	{"2026-09-27", "中秋节", "legal"},
	// 国庆节：10/1(四)-10/7(三)，9/20(日)、10/10(六)补班
	{"2026-09-20", "国庆调休上班", "workday"},
	{"2026-10-01", "国庆节", "legal"},
	{"2026-10-02", "国庆节", "legal"},
	{"2026-10-03", "国庆节", "legal"},
	{"2026-10-04", "国庆节", "legal"},
	{"2026-10-05", "国庆节", "legal"},
	{"2026-10-06", "国庆节", "legal"},
	{"2026-10-07", "国庆节", "legal"},
	{"2026-10-10", "国庆调休上班", "workday"},
}

// SeedHolidays 播种内置法定假期（对标 holiday.ts: seedHolidays）。
// 幂等：saveHoliday 按 date upsert，重复播种同日是预期行为，不会产生重复行；
// 单条失败（已存在/并发冲突）静默跳过——与 TS 的 try/catch 完全一致。
// 整体不向 main.go 返回错误：index.ts 里 seed 失败只 console.error 不中断启动，
// 此处恒返回 nil，由 main.go 自行决定是否兜底打日志。
func SeedHolidays(st db.Store) error {
	for _, s := range builtinHolidays {
		// 忽略单条失败（已存在/并发）——幂等 upsert，重跑安全。
		if _, err := st.SaveHoliday(context.Background(), db.SaveHolidayInput{
			Date: s.Date,
			Name: s.Name,
			Type: s.Type,
		}); err != nil {
			logger.Warn(mod, "seedHolidays 单条写入失败（忽略）", "date", s.Date, "error", err.Error())
		}
	}
	return nil
}

// RegisterHoliday 注册 holiday:* 事件处理器（对标 TS registerHolidayHandlers(ctx)，ctx.io/store 由参数注入）。
func RegisterHoliday(srv socketapi.Server, store db.Store) {
	// ---- 假期列表（year 省略/0=全部） ----
	srv.On("holiday:list", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		if _, ok := authedUser(c); !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		// 对标 TS: Number(data?.year) || 0；0/NaN → 全部。
		year := intYear(m["year"])
		holidays, err := store.ListHolidays(context.Background(), year)
		if err != nil {
			logger.Error(mod, "holiday:list 查询失败", "year", year, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		if holidays == nil {
			holidays = []types.Holiday{}
		}
		ack(util.OK(map[string]any{"holidays": holidays}))
	})

	// ---- 新增/修改假期（date 唯一，upsert） ----
	srv.On("holiday:add", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		if _, ok := authedUser(c); !ok {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(raw)
		// 对标 TS: typeof d.date === 'string' ? d.date.trim() : ''；name 同理再 slice(0,32)。
		date := strTrim(d["date"])
		name := strTrim(d["name"])
		if len(name) > 32 {
			name = name[:32]
		}
		typ := holidayTypeOf(d["type"])
		if !dateRe.MatchString(date) {
			ack(util.Fail("日期格式不合法"))
			return
		}
		if name == "" {
			ack(util.Fail("名称不能为空"))
			return
		}
		if _, ok := holidayTypes[typ]; !ok {
			ack(util.Fail("类型不合法"))
			return
		}
		h, err := store.SaveHoliday(context.Background(), db.SaveHolidayInput{Date: date, Name: name, Type: typ})
		if err != nil {
			logger.Error(mod, "holiday:add 保存失败", "date", date, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		// 客户端靠此事件刷新日历，一个都不能少。
		_ = srv.Broadcast("holiday:updated", map[string]any{})
		ack(util.OK(map[string]any{"holiday": h}))
	})

	// ---- 删除假期 ----
	srv.On("holiday:remove", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		if _, ok := authedUser(c); !ok {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(raw)
		date := str(d["date"])
		if !dateRe.MatchString(date) {
			ack(util.Fail("日期格式不合法"))
			return
		}
		if err := store.DeleteHoliday(context.Background(), date); err != nil {
			logger.Error(mod, "holiday:remove 删除失败", "date", date, "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		_ = srv.Broadcast("holiday:updated", map[string]any{})
		ack(util.OK(nil))
	})

	// ---- 批量新增/修改假期（多选批量设假期） ----
	srv.On("holiday:addMany", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		if _, ok := authedUser(c); !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		items, _ := m["items"].([]any)
		if len(items) == 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		added := 0
		for _, it := range items {
			d, _ := it.(map[string]any)
			if d == nil {
				continue
			}
			date := strTrim(d["date"])
			name := strCut(d["name"], 32)
			typ := holidayTypeOf(d["type"])
			if !dateRe.MatchString(date) || name == "" {
				continue
			}
			if _, ok := holidayTypes[typ]; !ok {
				continue
			}
			if _, err := store.SaveHoliday(context.Background(), db.SaveHolidayInput{Date: date, Name: name, Type: typ}); err != nil {
				logger.Error(mod, "holiday:addMany 单条保存失败（跳过）", "date", date, "error", err.Error())
				continue
			}
			added++
		}
		_ = srv.Broadcast("holiday:updated", map[string]any{})
		ack(util.OK(map[string]any{"added": added}))
	})

	// ---- 批量删除假期（多选取消） ----
	srv.On("holiday:removeMany", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		if _, ok := authedUser(c); !ok {
			ack(util.Fail("未登录"))
			return
		}
		m := parseData(raw)
		dates, _ := m["dates"].([]any)
		if len(dates) == 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		removed := 0
		for _, dd := range dates {
			s, isStr := dd.(string)
			if !isStr || !dateRe.MatchString(s) {
				continue
			}
			if err := store.DeleteHoliday(context.Background(), s); err != nil {
				logger.Error(mod, "holiday:removeMany 单条删除失败（跳过）", "date", s, "error", err.Error())
				continue
			}
			removed++
		}
		_ = srv.Broadcast("holiday:updated", map[string]any{})
		ack(util.OK(map[string]any{"removed": removed}))
	})

	// ---- 恢复默认：清空全部假期 → 重新种入官方法定安排 ----
	srv.On("holiday:reset", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		if _, ok := authedUser(c); !ok {
			ack(util.Fail("未登录"))
			return
		}
		all, err := store.ListHolidays(context.Background(), 0)
		if err != nil {
			logger.Error(mod, "holiday:reset 查询失败", "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		for _, h := range all {
			if err := store.DeleteHoliday(context.Background(), h.Date); err != nil {
				logger.Error(mod, "holiday:reset 清空单条失败（跳过）", "date", h.Date, "error", err.Error())
			}
		}
		// 重种内置法定假期（单条失败内部静默跳过）。
		_ = SeedHolidays(store)
		holidays, err := store.ListHolidays(context.Background(), 0)
		if err != nil {
			logger.Error(mod, "holiday:reset 重查失败", "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		if holidays == nil {
			holidays = []types.Holiday{}
		}
		_ = srv.Broadcast("holiday:updated", map[string]any{})
		ack(util.OK(map[string]any{"holidays": holidays, "count": len(holidays)}))
	})
}

// holidayTypeOf 对标 TS: typeof d.type === 'string' ? d.type : 'legal'（不 trim，逐字比对白名单）。
func holidayTypeOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return "legal"
}

// intYear 对标 TS: Number(v) || 0（NaN/<=0 → 0=全部）。
func intYear(v any) int64 {
	switch n := v.(type) {
	case float64:
		if n <= 0 {
			return 0
		}
		return int64(n)
	case int:
		if n <= 0 {
			return 0
		}
		return int64(n)
	case int64:
		if n <= 0 {
			return 0
		}
		return n
	}
	return 0
}
