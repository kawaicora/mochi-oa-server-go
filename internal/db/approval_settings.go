package db

import (
	"context"
	"time"

	"gorm.io/gorm"

	"mochi-oa-server-go/internal/types"
)

// 本文件实现「公司默认审批抄送人」配置的 MySQL 访问（表 company_approval_cc）。
// 语义：发起审批时客户端读取默认抄送人预填 ccIds（可在审批页替换/清空，非强制）；
// 仅公司 owner/admin 可增删改。

// GetApprovalCc 取某公司的默认审批抄送人 userId 列表（按 id 升序）。
func (s *MySqlStore) GetApprovalCc(ctx context.Context, companyID int64) ([]int64, error) {
	var rows []types.CompanyApprovalCcRow
	if err := s.gorm.WithContext(ctx).Table("company_approval_cc").
		Where("company_id = ?", companyID).Order("id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.UserID)
	}
	return ids, nil
}

// SetApprovalCc 整表替换某公司的默认审批抄送人（删旧插新，事务内完成）。
// userIDs 为空 = 清空该公司默认抄送。
func (s *MySqlStore) SetApprovalCc(ctx context.Context, companyID int64, userIDs []int64) error {
	now := time.Now().UTC()
	return s.gorm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Table("company_approval_cc").Where("company_id = ?", companyID).Delete(&types.CompanyApprovalCcRow{}).Error; err != nil {
			return err
		}
		for _, uid := range userIDs {
			row := types.CompanyApprovalCcRow{
				CompanyID: companyID,
				UserID:    uid,
				CreatedAt: now,
			}
			if err := tx.Table("company_approval_cc").Create(&row).Error; err != nil {
				return err
			}
		}
		return nil
	})
}
