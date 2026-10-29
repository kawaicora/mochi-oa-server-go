package db

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"gorm.io/gorm"

	"mochi-oa-server-go/internal/types"
)

// 本文件实现打卡记录与公司打卡白名单配置的 MySQL 访问。
// JSON 列（mac/ip_whitelist）一律 gorm:"type:json" 可空无 default，写入侧空数组落 "[]"，
// 读取侧 decodeStrings 对 NULL/空/损坏兜底 []。

// state 位掩码（每条 checkins 记录的类型，单值非组合）：1=上班 2=下班 4=缺卡 8=补卡 16=请假 32=调休。
// 单条记录只含一个类型位（上班或下班），不存在 =3 的组合；补卡=卡类型|8 → 补上班=9、补下班=10。
// 请假(16)/调休(32)为申请状态：当天无打卡记录时由考勤申请表达，不落 checkins 行。
const (
	stateWork   = 1
	stateOff    = 2
	stateMiss   = 4
	stateMakeup = 8
	stateLeave  = 16
	stateComp   = 32
)

// checkinScanRow 联表行：打卡记录字段 + 发起人 nick/username。
type checkinScanRow struct {
	ID            int64     `gorm:"column:id"`
	CompanyID     int64     `gorm:"column:company_id"`
	UserID        int64     `gorm:"column:user_id"`
	CheckDate     string    `gorm:"column:check_date"`
	CheckTime     time.Time `gorm:"column:check_time"`
	IP            *string   `gorm:"column:ip"`
	MAC           *string   `gorm:"column:mac"`
	StdStart      string    `gorm:"column:std_start"`
	StdEnd        string    `gorm:"column:std_end"`
	GraceLateMin  int64     `gorm:"column:grace_late_min"`
	GraceEarlyMin int64     `gorm:"column:grace_early_min"`
	State         int64     `gorm:"column:state"`
	CreatedAt     time.Time `gorm:"column:created_at"`
	Username      string    `gorm:"column:username"`
	Nick          string    `gorm:"column:nick"`
}

func (s *MySqlStore) checkinBaseQuery(ctx context.Context) *gorm.DB {
	return s.gorm.WithContext(ctx).Table("checkins r").
		Joins("LEFT JOIN users u ON u.id = r.user_id")
}

const checkinSelect = "r.id, r.company_id, r.user_id, DATE_FORMAT(r.check_date, '%Y-%m-%d') AS check_date, r.check_time, r.ip, r.mac, DATE_FORMAT(r.std_start, '%H:%i') AS std_start, DATE_FORMAT(r.std_end, '%H:%i') AS std_end, r.grace_late_min, r.grace_early_min, r.state, r.created_at, u.username, u.nick"

// normHHMM 把 "HH:MM:SS"（或完整 datetime "YYYY-MM-DD HH:MM:SS" 经 DATE_FORMAT 已裁为 HH:MM）
// 规范为 "HH:MM"（空串/短串原样返回）。
func normHHMM(s string) string {
	if len(s) >= 5 {
		return s[:5]
	}
	return s
}

// normalizeDate 把任意常见的日期/时间戳字符串归一化为纯 "YYYY-MM-DD"（check_date 列为 date 类型）。
// 兼容：纯日期 "2026-10-26"、空格分隔 "2026-10-26 18:00"、ISO "2026-10-26T18:00:00"、
// UTC "2026-10-26T00:00:00Z" 等；解析失败原样返回（交由 SQL 层报错，不吞错误）。
func normalizeDate(v string) string {
	if v == "" {
		return v
	}
	s := strings.Replace(v, "T", " ", 1) // ISO → 空格分隔，统一格式
	s = strings.Replace(s, "Z", "", 1)
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, localTZ); err == nil {
			return t.In(localTZ).Format("2006-01-02")
		}
	}
	// 兜底：截取前 10 位（形如 "2026-10-26..."）
	if len(v) >= 10 {
		return v[:10]
	}
	return v
}

// fullDT 把排班时间（"09:30" 或 "09:30:00"）+ 当天日期拼成完整 datetime "2026-10-28 09:30:00"。
// 生产库 checkins.std_start/std_end 为 datetime 列，只写时间会报 1292；空返回 nil（落 NULL）。
func fullDT(checkDate, v string) *string {
	if v == "" {
		return nil
	}
	s := checkDate + " " + normHHMM(v) + ":00"
	return &s
}

func scanRowToCheckin(r checkinScanRow) types.CheckinItem {
	return types.CheckinItem{
		ID:            r.ID,
		CompanyID:     r.CompanyID,
		UserID:        r.UserID,
		CheckDate:     r.CheckDate,
		CheckTime:     toIso(r.CheckTime),
		IP:            r.IP,
		MAC:           r.MAC,
		StdStart:      normHHMM(r.StdStart),
		StdEnd:        normHHMM(r.StdEnd),
		GraceLateMin:  r.GraceLateMin,
		GraceEarlyMin: r.GraceEarlyMin,
		State:         r.State,
		CreatedAt:     toIso(r.CreatedAt),
		Nick:          r.Nick,
		Username:      r.Username,
	}
}

// encodeStrings 序列化字符串数组为 JSON；空/nil → "[]"。
func encodeStrings(arr []string) string {
	if len(arr) == 0 {
		return "[]"
	}
	b, err := json.Marshal(arr)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// decodeStrings 反序列化 JSON 字符串数组；NULL/空/损坏 → []。
func decodeStrings(s string) []string {
	out := []string{}
	if s == "" || s == "null" {
		return out
	}
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

// CreateCheckin 一天打卡规则（事务内计数+写，打卡并发低不加悲观锁）：
//   - 当天 0 条 → INSERT（第1次=上班时间），kind=first；
//   - 当天 1 条 → INSERT（第2次=下班时间），kind=second；
//   - 当天 ≥2 条 → UPDATE 当天 check_time 升序最后一条的 check_time=now（覆盖下班时间，不新增行），kind=override。
//
// workStart/workEnd/graceLate/graceEarly 为打卡时的排班规则快照，随记录落库供历史判定（改规则不影响历史）。
func (s *MySqlStore) CreateCheckin(ctx context.Context, companyID, userID int64, checkDate, ip string, mac *string, workStart, workEnd string, graceLate, graceEarly int64, offWindow ...bool) (*types.CheckinItem, string, error) {
	now := time.Now().UTC()
	tx := s.gorm.WithContext(ctx).Begin()
	isOffWindow := len(offWindow) > 0 && offWindow[0]

	// 数当天已有记录条数。
	var cnt int64
	if err := tx.Table("checkins").Where("company_id = ? AND user_id = ? AND check_date = ?", companyID, userID, checkDate).Count(&cnt).Error; err != nil {
		tx.Rollback()
		return nil, "", err
	}

	// ≥2 条：覆盖当天最后一条（check_time 升序最后一条=最近一次下班时间）。
	if cnt >= 2 {
		var last types.CheckinRow
		err := tx.Table("checkins").
			Where("company_id = ? AND user_id = ? AND check_date = ?", companyID, userID, checkDate).
			Order("check_time DESC").Order("id DESC").
			Take(&last).Error
		if err != nil {
			tx.Rollback()
			return nil, "", err
		}
		res := tx.Table("checkins").Where("id = ?", last.ID).
			Updates(map[string]any{"check_time": now, "ip": ip, "mac": mac,
				"std_start": fullDT(checkDate, workStart), "std_end": fullDT(checkDate, workEnd), "grace_late_min": graceLate, "grace_early_min": graceEarly,
				"state": stateOff})
		if res.Error != nil {
			tx.Rollback()
			return nil, "", res.Error
		}
		if err := tx.Commit().Error; err != nil {
			return nil, "", err
		}
		item, err := s.getCheckinByID(ctx, last.ID)
		return item, "override", err
	}

	// 0 或 1 条：INSERT 新记录。
	// isOffWindow=true 表示本次打卡命中「前一天下班 +8h 窗口」：归前一天，语义为补前天下班卡，
	// 因此即使当天 0 条也按下班卡（state=off）写入，而非上班卡。
	state := int64(stateWork)
	if cnt == 1 || isOffWindow {
		state = int64(stateOff)
	}
	row := types.CheckinRow{
		CompanyID:     companyID,
		UserID:        userID,
		CheckDate:     checkDate,
		CheckTime:     now,
		IP:            &ip,
		MAC:           mac,
		StdStart:      fullDT(checkDate, workStart),
		StdEnd:        fullDT(checkDate, workEnd),
		GraceLateMin:  graceLate,
		GraceEarlyMin: graceEarly,
		State:         state,
		CreatedAt:     now,
	}
	if err := tx.Table("checkins").Create(&row).Error; err != nil {
		tx.Rollback()
		return nil, "", err
	}
	if err := tx.Commit().Error; err != nil {
		return nil, "", err
	}
	kind := "first"
	if cnt == 1 || isOffWindow {
		kind = "second"
	}
	item, err := s.getCheckinByID(ctx, row.ID)
	return item, kind, err
}

// getCheckinByID 按 id 取单条。
func (s *MySqlStore) getCheckinByID(ctx context.Context, id int64) (*types.CheckinItem, error) {
	var r checkinScanRow
	err := s.checkinBaseQuery(ctx).Select(checkinSelect).Where("r.id = ?", id).Take(&r).Error
	if err != nil {
		if gormNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	out := scanRowToCheckin(r)
	return &out, nil
}

// ListCheckinsByMonth 本人某月全部记录（check_date 以 month 为前缀）；check_time 升序。
func (s *MySqlStore) ListCheckinsByMonth(ctx context.Context, companyID, userID int64, month string) ([]types.CheckinItem, error) {
	rows := []checkinScanRow{}
	err := s.checkinBaseQuery(ctx).Select(checkinSelect).
		Where("r.company_id = ?", companyID).
		Where("r.user_id = ?", userID).
		Where("r.check_date LIKE ?", month+"-%").
		Order("r.check_time ASC").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := []types.CheckinItem{}
	for _, r := range rows {
		out = append(out, scanRowToCheckin(r))
	}
	return out, nil
}

// InsertMakeupCheckin 补卡批准后把补卡时间写入 checkins（补卡也算打卡）。
//   - punchTime 为本地时间字符串，兼容 "2006-01-02 15:04" 与 "2006-01-02 15:04:05"；
//   - 按本地时区解析出 check_date（本地日期）与 check_time（转 UTC 存储，与实时打卡一致）；
//   - 幂等：同公司+用户+日期+时刻已存在则跳过（重复审批不会重复插入）。
//
// InsertMakeupCheckin 补卡批准后落库。
//   - checkDate 为调用方给定的「目标日期」（被补那天），跨天补下班（如补 10-27 卡、实际 10-28 02:00 打卡）
//     仍落到目标日期；为空则回退 punchTime 字面日期（独立发起/旧数据）。
//   - checkTime 由 punchTime 解析（实际时刻，转 UTC 存储，与实时打卡一致）；
//   - 幂等：同公司+用户+日期+时刻已存在则跳过（重复审批不会重复插入）。
func (s *MySqlStore) InsertMakeupCheckin(ctx context.Context, companyID, userID int64, checkDate, punchTime, workStart, workEnd string, graceLate, graceEarly int64, slot int64) error {
	// 兼容 ISO("2026-10-25T21:00") 与空格分隔("2026-10-25 21:00")两种 punchTime 格式。
	norm := strings.Replace(punchTime, "T", " ", 1)
	t, err := time.ParseInLocation("2006-01-02 15:04", norm, localTZ)
	if err != nil {
		t, err = time.ParseInLocation("2006-01-02 15:04:05", norm, localTZ)
		if err != nil {
			return err
		}
	}
	// checkDate 归一化为纯 "YYYY-MM-DD"：无论调用方传纯日期还是完整时间戳（如 "2026-10-26T00:00:00Z"）
	// 都能正确落 date 列，避免 Error 1292。
	if checkDate == "" {
		checkDate = t.In(localTZ).Format("2006-01-02")
	} else {
		checkDate = normalizeDate(checkDate)
	}
	checkTime := t.UTC()
	var cnt int64
	if err := s.gorm.WithContext(ctx).Table("checkins").
		Where("company_id = ? AND user_id = ? AND check_date = ? AND check_time = ?",
			companyID, userID, checkDate, checkTime).Count(&cnt).Error; err != nil {
		return err
	}
	if cnt > 0 {
		return nil // 幂等：已存在同时间点补卡
	}
	// slot 为补的卡类型：stateWork=补上班卡 / stateOff=补下班卡。
	if slot != stateWork && slot != stateOff {
		slot = stateWork
	}
	// 防重复补卡：同用户同日同槽位(上班/下班)已补过 → 跳过，一个迟到不能补无数张卡。
	var slotCnt int64
	if err := s.gorm.WithContext(ctx).Table("checkins").
		Where("company_id = ? AND user_id = ? AND check_date = ? AND (state & 8) != 0 AND (state & ?) != 0",
			companyID, userID, checkDate, slot).Count(&slotCnt).Error; err != nil {
		return err
	}
	if slotCnt > 0 {
		return nil // 该日期该槽位已补过卡
	}
	// 补卡带 8 位：补上班卡=1|8=9，补下班卡=2|8=10，从数字即可看出是补卡。
	makeupState := slot | stateMakeup
	row := types.CheckinRow{
		CompanyID:     companyID,
		UserID:        userID,
		CheckDate:     checkDate,
		CheckTime:     checkTime,
		StdStart:      fullDT(checkDate, workStart),
		StdEnd:        fullDT(checkDate, workEnd),
		GraceLateMin:  graceLate,
		GraceEarlyMin: graceEarly,
		State:         makeupState,
		CreatedAt:     time.Now().UTC(),
	}
	return s.gorm.WithContext(ctx).Table("checkins").Create(&row).Error
}

// GetCheckinSettings 公司打卡白名单；未配置返回 (nil, nil)。
func (s *MySqlStore) GetCheckinSettings(ctx context.Context, companyID int64) (*types.CheckinSettings, error) {
	var r types.CompanyCheckinSettingsRow
	err := s.gorm.WithContext(ctx).Table("company_checkin_settings").
		Where("company_id = ?", companyID).Take(&r).Error
	if err != nil {
		if gormNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &types.CheckinSettings{
		ID:             r.ID,
		CompanyID:      r.CompanyID,
		MacEnabled:     r.MacEnabled,
		MacWhitelist:   decodeStrings(r.MacWhitelist),
		IpWhitelist:    decodeStrings(r.IpWhitelist),
		NoPunchUserIds: decodeStrings(r.NoPunchUserIds),
		CreatedAt:      toIso(r.CreatedAt),
		UpdatedAt:      toIso(r.UpdatedAt),
	}, nil
}

// SaveCheckinSettings upsert 公司打卡白名单（一公司一条）。macEnabled 控制是否启用 MAC 限制。
func (s *MySqlStore) SaveCheckinSettings(ctx context.Context, companyID int64, macEnabled bool, macWhitelist, ipWhitelist, noPunchUserIds []string) (*types.CheckinSettings, error) {
	now := time.Now().UTC()
	var existing types.CompanyCheckinSettingsRow
	err := s.gorm.WithContext(ctx).Table("company_checkin_settings").
		Where("company_id = ?", companyID).Take(&existing).Error
	if err != nil && !gormNotFound(err) {
		return nil, err
	}
	if err == nil {
		res := s.gorm.WithContext(ctx).Table("company_checkin_settings").
			Where("id = ?", existing.ID).
			Updates(map[string]any{
				"mac_enabled":       macEnabled,
				"mac_whitelist":     encodeStrings(macWhitelist),
				"ip_whitelist":      encodeStrings(ipWhitelist),
				"no_punch_user_ids": encodeStrings(noPunchUserIds),
				"updated_at":        now,
			})
		if res.Error != nil {
			return nil, res.Error
		}
		return s.GetCheckinSettings(ctx, companyID)
	}
	row := types.CompanyCheckinSettingsRow{
		CompanyID:      companyID,
		MacEnabled:     macEnabled,
		MacWhitelist:   encodeStrings(macWhitelist),
		IpWhitelist:    encodeStrings(ipWhitelist),
		NoPunchUserIds: encodeStrings(noPunchUserIds),
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := s.gorm.WithContext(ctx).Table("company_checkin_settings").Create(&row).Error; err != nil {
		return nil, err
	}
	return s.GetCheckinSettings(ctx, companyID)
}
