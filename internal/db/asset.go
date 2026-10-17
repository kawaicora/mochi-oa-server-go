package db

import (
	"context"
	"fmt"
	"time"

	"mochi-oa-server-go/internal/types"
)

// 本文件实现资产管理（assets + asset_flows）的 MySQL 访问。
// code 规则：AS{YYYYMMDD}-{3位序号}，公司内唯一，冲突重试。

func assetToItem(r types.AssetRow) types.AssetItem {
	return types.AssetItem{
		ID:        r.ID,
		CompanyID: r.CompanyID,
		Code:      r.Code,
		Name:      r.Name,
		Price:     r.Price,
		AssetType: r.AssetType,
		Status:    r.Status,
		CreatedAt: toIso(r.CreatedAt),
		UpdatedAt: toIso(r.UpdatedAt),
	}
}

// nextAssetCode 生成当日下一个编号 AS{date}-{seq:03d}；按已有同日前缀计数。
func (s *MySqlStore) nextAssetCode(ctx context.Context, companyID int64) (string, error) {
	prefix := "AS" + time.Now().Format("20060102")
	var maxSeq int64
	err := s.gorm.WithContext(ctx).Table("assets").
		Where("company_id = ?", companyID).
		Where("code LIKE ?", prefix+"-%").
		// MySQL SUBSTRING 1 起：code="AS20261017-001"，prefix 长 10，序号从第 12 位起（跳过 "-"）。
		Select("COALESCE(MAX(CAST(SUBSTRING(code, ?) AS UNSIGNED)), 0)", len(prefix)+2).
		Scan(&maxSeq).Error
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%03d", prefix, maxSeq+1), nil
}

// CreateAsset 新建资产（自动生成 code）。
func (s *MySqlStore) CreateAsset(ctx context.Context, in CreateAssetInput) (*types.AssetItem, error) {
	var row types.AssetRow
	for i := 0; i < 5; i++ {
		code, err := s.nextAssetCode(ctx, in.CompanyID)
		if err != nil {
			return nil, err
		}
		now := time.Now().UTC()
		row = types.AssetRow{
			CompanyID: in.CompanyID,
			Code:      code,
			Name:      in.Name,
			Price:     in.Price,
			AssetType: in.AssetType,
			Status:    "in_stock",
			CreatedAt: now,
			UpdatedAt: now,
		}
		if err := s.gorm.WithContext(ctx).Table("assets").Create(&row).Error; err != nil {
			if isUniqueErr(err) {
				continue
			}
			return nil, err
		}
		it := assetToItem(row)
		return &it, nil
	}
	return nil, &AlreadyExistsError{Msg: "asset code conflict"}
}

// GetAssetRowByID 取资产行（handler 校验用）。
func (s *MySqlStore) getAssetRowByID(ctx context.Context, id int64) (*types.AssetRow, error) {
	var r types.AssetRow
	err := s.gorm.WithContext(ctx).Table("assets").Where("id = ?", id).Take(&r).Error
	if err != nil {
		if gormNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &r, nil
}

// GetAssetByID 按 id 取视图。
func (s *MySqlStore) GetAssetByID(ctx context.Context, id int64) (*types.AssetItem, error) {
	r, err := s.getAssetRowByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, nil
	}
	it := assetToItem(*r)
	return &it, nil
}

// GetAssetRowByCode 按 code 取行（二维码页用）。
func (s *MySqlStore) GetAssetRowByCode(ctx context.Context, code string) (*types.AssetRow, error) {
	var r types.AssetRow
	err := s.gorm.WithContext(ctx).Table("assets").Where("code = ?", code).Take(&r).Error
	if err != nil {
		if gormNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &r, nil
}

// ListAssets 关键词匹配 name/code，createdAt 倒序分页。
func (s *MySqlStore) ListAssets(ctx context.Context, companyID int64, keyword string, page, pageSize int64) ([]types.AssetItem, int64, error) {
	q := s.gorm.WithContext(ctx).Table("assets").Where("company_id = ?", companyID)
	if keyword != "" {
		like := "%" + keyword + "%"
		q = q.Where("name LIKE ? OR code LIKE ?", like, like)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	rows := []types.AssetRow{}
	if err := q.Order("id DESC").Limit(int(pageSize)).Offset(int((page - 1) * pageSize)).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	out := []types.AssetItem{}
	for _, r := range rows {
		out = append(out, assetToItem(r))
	}
	return out, total, nil
}

// UpdateAsset 更新资产。
func (s *MySqlStore) UpdateAsset(ctx context.Context, id int64, in UpdateAssetInput) (*types.AssetItem, error) {
	updates := map[string]any{"updated_at": time.Now().UTC()}
	if in.Name != nil {
		updates["name"] = *in.Name
	}
	if in.Price != nil {
		updates["price"] = *in.Price
	}
	if in.AssetType != nil {
		updates["asset_type"] = *in.AssetType
	}
	if in.Status != nil {
		updates["status"] = *in.Status
	}
	res := s.gorm.WithContext(ctx).Table("assets").Where("id = ?", id).Updates(updates)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, &NotFoundError{Msg: "asset not found"}
	}
	return s.GetAssetByID(ctx, id)
}

// DeleteAsset 删资产及其全部流转记录。
func (s *MySqlStore) DeleteAsset(ctx context.Context, id int64) error {
	tx := s.gorm.WithContext(ctx).Begin()
	if err := tx.Where("asset_id = ?", id).Delete(&types.AssetFlowRow{}).Error; err != nil {
		tx.Rollback()
		return err
	}
	res := tx.Where("id = ?", id).Delete(&types.AssetRow{})
	if res.Error != nil {
		tx.Rollback()
		return res.Error
	}
	if res.RowsAffected == 0 {
		tx.Rollback()
		return &NotFoundError{Msg: "asset not found"}
	}
	return tx.Commit().Error
}

// assetFlowScanRow 联表行：流转 + 资产 code/name + 经办人昵称。
type assetFlowScanRow struct {
	ID           int64     `gorm:"column:id"`
	CompanyID    int64     `gorm:"column:company_id"`
	AssetID      int64     `gorm:"column:asset_id"`
	FlowType     string    `gorm:"column:flow_type"`
	Direction    string    `gorm:"column:direction"`
	OperatorID   int64     `gorm:"column:operator_id"`
	RelatedTo    *string   `gorm:"column:related_to"`
	Note         *string   `gorm:"column:note"`
	CreatedAt    time.Time `gorm:"column:created_at"`
	AssetCode    string    `gorm:"column:asset_code"`
	AssetName    string    `gorm:"column:asset_name"`
	OperatorNick string    `gorm:"column:operator_nick"`
}

func scanFlow(r assetFlowScanRow) types.FlowItem {
	return types.FlowItem{
		ID:           r.ID,
		CompanyID:    r.CompanyID,
		AssetID:      r.AssetID,
		AssetCode:    r.AssetCode,
		AssetName:    r.AssetName,
		FlowType:     r.FlowType,
		Direction:    r.Direction,
		OperatorID:   r.OperatorID,
		OperatorNick: r.OperatorNick,
		RelatedTo:    r.RelatedTo,
		Note:         r.Note,
		CreatedAt:    toIso(r.CreatedAt),
	}
}

const flowSelect = "f.id, f.company_id, f.asset_id, f.flow_type, f.direction, f.operator_id, f.related_to, f.note, f.created_at, a.code AS asset_code, a.name AS asset_name, COALESCE(u.nick, u.username) AS operator_nick"

// CreateAssetFlow 建流转记录，并按最新方向更新资产 status。
func (s *MySqlStore) CreateAssetFlow(ctx context.Context, in CreateAssetFlowInput) (*types.FlowItem, *types.AssetItem, error) {
	now := time.Now().UTC()
	row := types.AssetFlowRow{
		CompanyID:  in.CompanyID,
		AssetID:    in.AssetID,
		FlowType:   in.FlowType,
		Direction:  in.Direction,
		OperatorID: in.OperatorID,
		RelatedTo:  in.RelatedTo,
		Note:       in.Note,
		CreatedAt:  now,
	}
	if err := s.gorm.WithContext(ctx).Table("asset_flows").Create(&row).Error; err != nil {
		return nil, nil, err
	}
	// 按方向更新资产 status：最后一条流转为 out → out；否则 in_stock。
	newStatus := "in_stock"
	if in.Direction == "out" {
		newStatus = "out"
	}
	if err := s.gorm.WithContext(ctx).Table("assets").Where("id = ?", in.AssetID).
		Updates(map[string]any{"status": newStatus, "updated_at": now}).Error; err != nil {
		return nil, nil, err
	}
	// 回查联表行。
	var r assetFlowScanRow
	err := s.gorm.WithContext(ctx).Table("asset_flows f").
		Joins("LEFT JOIN assets a ON a.id = f.asset_id").
		Joins("LEFT JOIN users u ON u.id = f.operator_id").
		Select(flowSelect).Where("f.id = ?", row.ID).Take(&r).Error
	if err != nil {
		return nil, nil, err
	}
	asset, _ := s.GetAssetByID(ctx, in.AssetID)
	it := scanFlow(r)
	return &it, asset, nil
}

// ListAssetFlows 公司流转记录，assetId 缺省=公司全部，倒序分页。
func (s *MySqlStore) ListAssetFlows(ctx context.Context, companyID, assetID int64, page, pageSize int64) ([]types.FlowItem, int64, error) {
	q := s.gorm.WithContext(ctx).Table("asset_flows f").Where("f.company_id = ?", companyID)
	if assetID > 0 {
		q = q.Where("f.asset_id = ?", assetID)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	rows := []assetFlowScanRow{}
	listQ := s.gorm.WithContext(ctx).Table("asset_flows f").
		Joins("LEFT JOIN assets a ON a.id = f.asset_id").
		Joins("LEFT JOIN users u ON u.id = f.operator_id").
		Select(flowSelect).Where("f.company_id = ?", companyID)
	if assetID > 0 {
		listQ = listQ.Where("f.asset_id = ?", assetID)
	}
	if err := listQ.Order("f.id DESC").Limit(int(pageSize)).Offset(int((page - 1) * pageSize)).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	out := []types.FlowItem{}
	for _, r := range rows {
		out = append(out, scanFlow(r))
	}
	return out, total, nil
}
