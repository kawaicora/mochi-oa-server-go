package db

import (
	"context"
	"encoding/json"
	"time"

	"gorm.io/gorm"

	"mochi-oa-server-go/internal/types"
)

// 本文件实现打卡记录与公司打卡白名单配置的 MySQL 访问。
// JSON 列（mac/ip_whitelist）一律 gorm:"type:json" 可空无 default，写入侧空数组落 "[]"，
// 读取侧 decodeStrings 对 NULL/空/损坏兜底 []。

// checkinScanRow 联表行：打卡记录字段 + 发起人 nick/username。
type checkinScanRow struct {
	ID        int64     `gorm:"column:id"`
	CompanyID int64     `gorm:"column:company_id"`
	UserID    int64     `gorm:"column:user_id"`
	CheckDate string    `gorm:"column:check_date"`
	CheckTime time.Time `gorm:"column:check_time"`
	IP        *string   `gorm:"column:ip"`
	MAC       *string   `gorm:"column:mac"`
	CreatedAt time.Time `gorm:"column:created_at"`
	Username  string    `gorm:"column:username"`
	Nick      string    `gorm:"column:nick"`
}

func (s *MySqlStore) checkinBaseQuery(ctx context.Context) *gorm.DB {
	return s.gorm.WithContext(ctx).Table("checkins r").
		Joins("LEFT JOIN users u ON u.id = r.user_id")
}

const checkinSelect = "r.id, r.company_id, r.user_id, r.check_date, r.check_time, r.ip, r.mac, r.created_at, u.username, u.nick"

func scanRowToCheckin(r checkinScanRow) types.CheckinItem {
	return types.CheckinItem{
		ID:        r.ID,
		CompanyID: r.CompanyID,
		UserID:    r.UserID,
		CheckDate: r.CheckDate,
		CheckTime: toIso(r.CheckTime),
		IP:        r.IP,
		MAC:       r.MAC,
		CreatedAt: toIso(r.CreatedAt),
		Nick:      r.Nick,
		Username:  r.Username,
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

// CreateCheckin 写入一条打卡记录。
func (s *MySqlStore) CreateCheckin(ctx context.Context, companyID, userID int64, checkDate, ip string, mac *string) (*types.CheckinItem, error) {
	now := time.Now().UTC()
	row := types.CheckinRow{
		CompanyID: companyID,
		UserID:    userID,
		CheckDate: checkDate,
		CheckTime: now,
		IP:        &ip,
		MAC:       mac,
		CreatedAt: now,
	}
	if err := s.gorm.WithContext(ctx).Table("checkins").Create(&row).Error; err != nil {
		return nil, err
	}
	return s.getCheckinByID(ctx, row.ID)
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
		ID:           r.ID,
		CompanyID:    r.CompanyID,
		MacWhitelist: decodeStrings(r.MacWhitelist),
		IpWhitelist:  decodeStrings(r.IpWhitelist),
		CreatedAt:    toIso(r.CreatedAt),
		UpdatedAt:    toIso(r.UpdatedAt),
	}, nil
}

// SaveCheckinSettings upsert 公司打卡白名单（一公司一条）。
func (s *MySqlStore) SaveCheckinSettings(ctx context.Context, companyID int64, macWhitelist, ipWhitelist []string) (*types.CheckinSettings, error) {
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
				"mac_whitelist": encodeStrings(macWhitelist),
				"ip_whitelist":  encodeStrings(ipWhitelist),
				"updated_at":    now,
			})
		if res.Error != nil {
			return nil, res.Error
		}
		return s.GetCheckinSettings(ctx, companyID)
	}
	row := types.CompanyCheckinSettingsRow{
		CompanyID:    companyID,
		MacWhitelist: encodeStrings(macWhitelist),
		IpWhitelist:  encodeStrings(ipWhitelist),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := s.gorm.WithContext(ctx).Table("company_checkin_settings").Create(&row).Error; err != nil {
		return nil, err
	}
	return s.GetCheckinSettings(ctx, companyID)
}
