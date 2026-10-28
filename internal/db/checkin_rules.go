package db

import (
	"context"
	"strings"
	"time"

	"mochi-oa-server-go/internal/types"
)

// 本文件实现公司打卡排班规则（company_checkin_rules，一公司一行）的读写。
// week_rules/alt_rule 以 TEXT 存 JSON 字符串，校验与反序列化在 handler 侧完成。

// GetCheckinRules 取公司规则；未配置返回 (nil, nil)。
func (s *MySqlStore) GetCheckinRules(ctx context.Context, companyID int64) (*types.CompanyCheckinRulesRow, error) {
	var r types.CompanyCheckinRulesRow
	err := s.gorm.WithContext(ctx).Table("company_checkin_rules").
		Where("company_id = ?", companyID).Take(&r).Error
	if err != nil {
		if gormNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &r, nil
}

// SaveCheckinRules upsert 公司排班规则。
func (s *MySqlStore) SaveCheckinRules(ctx context.Context, companyID int64, mode string, graceLate, graceEarly int64, weekRulesJSON string, altRuleJSON *string, offworkDeadline string) error {
	now := time.Now().UTC()
	// offwork_deadline 已弃用（下班窗口固定为「下班 +8 小时」）；列仍存于表（TIME not null），
	// 空值统一落 "00:00:00"，避免 TIME 列拒绝空串。
	if strings.TrimSpace(offworkDeadline) == "" {
		offworkDeadline = "00:00:00"
	}
	existing, err := s.GetCheckinRules(ctx, companyID)
	if err != nil {
		return err
	}
	if existing != nil {
		res := s.gorm.WithContext(ctx).Table("company_checkin_rules").Where("id = ?", existing.ID).
			Updates(map[string]any{
				"mode": mode, "grace_late_min": graceLate, "grace_early_min": graceEarly,
				"week_rules": weekRulesJSON, "alt_rule": altRuleJSON,
				"offwork_deadline": offworkDeadline, "updated_at": now,
			})
		return res.Error
	}
	row := types.CompanyCheckinRulesRow{
		CompanyID: companyID, Mode: mode,
		GraceLateMin: graceLate, GraceEarlyMin: graceEarly,
		WeekRules: weekRulesJSON, AltRule: altRuleJSON,
		OffworkDeadline: offworkDeadline,
		CreatedAt:       now, UpdatedAt: now,
	}
	return s.gorm.WithContext(ctx).Table("company_checkin_rules").Create(&row).Error
}
