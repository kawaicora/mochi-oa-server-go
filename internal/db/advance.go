package db

import (
	"context"
	"strconv"
	"time"

	"gorm.io/gorm"

	"mochi-oa-server-go/internal/types"
)

// 本文件实现备用金申请（advance）的 MySQL 访问，风格照 expense.go。

// CreateAdvanceRequestInput 备用金申请创建入参。
type CreateAdvanceRequestInput struct {
	CompanyID   int64
	UserID      int64
	Category    string
	Amount      float64
	Reason      string
	Evidence    []string
	ApproverIDs []int64
	CcIDs       []int64
}

// advanceScanRow 联表行：备用金字段 + 发起人 nick/username。
type advanceScanRow struct {
	ID            int64      `gorm:"column:id"`
	CompanyID     int64      `gorm:"column:company_id"`
	UserID        int64      `gorm:"column:user_id"`
	Category      string     `gorm:"column:category"`
	Amount        float64    `gorm:"column:amount"`
	Reason        string     `gorm:"column:reason"`
	Evidence      string     `gorm:"column:evidence"`
	Status        string     `gorm:"column:status"`
	ApproverID    *int64     `gorm:"column:approver_id"`
	ApprovalNote  *string    `gorm:"column:approval_note"`
	ApprovedAt    *time.Time `gorm:"column:approved_at"`
	ApproverIDs   string     `gorm:"column:approver_ids"`
	CcIDs         string     `gorm:"column:cc_ids"`
	CcViewedIDs   string     `gorm:"column:cc_viewed_ids"`
	NotifyStopped bool       `gorm:"column:notify_stopped"`
	CreatedAt     time.Time  `gorm:"column:created_at"`
	UpdatedAt     time.Time  `gorm:"column:updated_at"`
	Username      string     `gorm:"column:username"`
	Nick          string     `gorm:"column:nick"`
}

func (s *MySqlStore) advanceBaseQuery(ctx context.Context) *gorm.DB {
	return s.gorm.WithContext(ctx).Table("advances r").
		Joins("LEFT JOIN users u ON u.id = r.user_id")
}

func scanRowToAdvance(r advanceScanRow) types.AdvanceRequest {
	return types.AdvanceRequest{
		ID:            r.ID,
		CompanyID:     r.CompanyID,
		UserID:        r.UserID,
		Category:      r.Category,
		Amount:        r.Amount,
		Reason:        r.Reason,
		Evidence:      decodeStringArray(r.Evidence),
		Status:        r.Status,
		ApproverID:    r.ApproverID,
		ApprovalNote:  r.ApprovalNote,
		ApprovedAt:    isoPtr(r.ApprovedAt),
		ApproverIDs:   decodeRecipients(r.ApproverIDs),
		CcIDs:         decodeRecipients(r.CcIDs),
		CcViewedIds:   decodeRecipients(r.CcViewedIDs),
		CreatedAt:     toIso(r.CreatedAt),
		UpdatedAt:     toIso(r.UpdatedAt),
		Nick:          r.Nick,
		Username:      r.Username,
		NotifyStopped: r.NotifyStopped,
	}
}

const advanceSelect = "r.id, r.company_id, r.user_id, r.category, r.amount, r.reason, r.evidence, r.status, r.approver_id, r.approval_note, r.approved_at, r.approver_ids, r.cc_ids, r.cc_viewed_ids, r.notify_stopped, r.created_at, r.updated_at, u.username, u.nick"

// CreateAdvanceRequest 发起备用金申请；审批人/抄送人由调用方按快照传入。
func (s *MySqlStore) CreateAdvanceRequest(ctx context.Context, in CreateAdvanceRequestInput) (*types.AdvanceRequest, error) {
	now := time.Now().UTC()
	row := types.AdvanceRequestRow{
		CompanyID:   in.CompanyID,
		UserID:      in.UserID,
		Category:    in.Category,
		Amount:      in.Amount,
		Reason:      in.Reason,
		Evidence:    encodeStringArray(in.Evidence),
		ApproverIDs: encodeRecipients(in.ApproverIDs),
		CcIDs:       encodeRecipients(in.CcIDs),
		CcViewedIDs: "[]",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.gorm.WithContext(ctx).Table("advances").Create(&row).Error; err != nil {
		return nil, err
	}
	return s.GetAdvanceByID(ctx, row.ID)
}

func (s *MySqlStore) GetAdvanceByID(ctx context.Context, id int64) (*types.AdvanceRequest, error) {
	var r advanceScanRow
	err := s.advanceBaseQuery(ctx).Select(advanceSelect).Where("r.id = ?", id).Take(&r).Error
	if err != nil {
		if gormNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	out := scanRowToAdvance(r)
	return &out, nil
}

// ListAdvanceRequestsByUser 只看本人历史；category 空串=不限；createdAt 倒序分页。
func (s *MySqlStore) ListAdvanceRequestsByUser(ctx context.Context, companyID, userID int64, category string, page, pageSize int64) ([]types.AdvanceRequest, int64, error) {
	q := s.gorm.WithContext(ctx).Table("advances r").
		Where("r.company_id = ?", companyID).
		Where("r.user_id = ?", userID)
	if category != "" {
		q = q.Where("r.category = ?", category)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 10
	}
	if pageSize > 100 {
		pageSize = 100
	}
	listQ := s.advanceBaseQuery(ctx).Select(advanceSelect).
		Where("r.company_id = ?", companyID).
		Where("r.user_id = ?", userID)
	if category != "" {
		listQ = listQ.Where("r.category = ?", category)
	}
	rows := []advanceScanRow{}
	if err := listQ.Order("r.created_at DESC, r.id DESC").
		Limit(int(pageSize)).
		Offset(int((page-1)*pageSize)).
		Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	out := []types.AdvanceRequest{}
	for _, r := range rows {
		out = append(out, scanRowToAdvance(r))
	}
	return out, total, nil
}

func (s *MySqlStore) ApproveAdvanceRequest(ctx context.Context, id, approverID int64) (*types.AdvanceRequest, error) {
	now := time.Now().UTC()
	res := s.gorm.WithContext(ctx).Table("advances").
		Where("id = ?", id).
		Updates(map[string]any{
			"status":      "approved",
			"approver_id": approverID,
			"approved_at": now,
			"updated_at":  now,
		})
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, &NotFoundError{Msg: "advance request not found"}
	}
	return s.GetAdvanceByID(ctx, id)
}

func (s *MySqlStore) RejectAdvanceRequest(ctx context.Context, id int64, note string) (*types.AdvanceRequest, error) {
	now := time.Now().UTC()
	res := s.gorm.WithContext(ctx).Table("advances").
		Where("id = ?", id).
		Updates(map[string]any{
			"status":        "rejected",
			"approval_note": note,
			"updated_at":    now,
		})
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, &NotFoundError{Msg: "advance request not found"}
	}
	return s.GetAdvanceByID(ctx, id)
}

// ListReceivedAdvance 收到审批：本人提交 OR 本人被列为审批人（含自审批）。
func (s *MySqlStore) ListReceivedAdvance(ctx context.Context, companyID, userID int64) ([]types.AdvanceRequest, error) {
	approverJSON := strconv.FormatInt(userID, 10)
	rows := []advanceScanRow{}
	err := s.advanceBaseQuery(ctx).Select(advanceSelect).
		Where("r.company_id = ?", companyID).
		Where("(r.user_id = ? OR JSON_CONTAINS(r.approver_ids, ?))", userID, approverJSON).
		Order("r.created_at DESC, r.id DESC").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := []types.AdvanceRequest{}
	for _, r := range rows {
		out = append(out, scanRowToAdvance(r))
	}
	return out, nil
}

// ListReceivedAdvanceCC 收到抄送：本人提交 OR 本人被列为抄送人。
func (s *MySqlStore) ListReceivedAdvanceCC(ctx context.Context, companyID, userID int64) ([]types.AdvanceRequest, error) {
	ccJSON := strconv.FormatInt(userID, 10)
	rows := []advanceScanRow{}
	err := s.advanceBaseQuery(ctx).Select(advanceSelect).
		Where("r.company_id = ?", companyID).
		Where("(r.user_id = ? OR JSON_CONTAINS(r.cc_ids, ?))", userID, ccJSON).
		Order("r.created_at DESC, r.id DESC").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := []types.AdvanceRequest{}
	for _, r := range rows {
		out = append(out, scanRowToAdvance(r))
	}
	return out, nil
}

// MarkAdvanceCCViewed 把 userID 加入该备用金申请的 cc_viewed_ids（幂等）。
func (s *MySqlStore) MarkAdvanceCCViewed(ctx context.Context, requestID, userID int64) error {
	var row struct {
		CcViewedIDs string `gorm:"column:cc_viewed_ids"`
	}
	err := s.gorm.WithContext(ctx).Table("advances").
		Where("id = ?", requestID).
		Select("IFNULL(cc_viewed_ids,'[]') AS cc_viewed_ids").
		Take(&row).Error
	if err != nil {
		return err
	}
	existing := decodeRecipients(row.CcViewedIDs)
	for _, v := range existing {
		if v == userID {
			return nil
		}
	}
	existing = append(existing, userID)
	return s.gorm.WithContext(ctx).Table("advances").
		Where("id = ?", requestID).
		Updates(map[string]any{
			"cc_viewed_ids": encodeRecipients(existing),
			"updated_at":    time.Now().UTC(),
		}).Error
}

// StopAdvanceNotification 幂等置 notify_stopped=1（停止循环提醒）。
func (s *MySqlStore) StopAdvanceNotification(ctx context.Context, requestID int64, companyID int64) error {
	return s.gorm.WithContext(ctx).
		Table("advances").
		Where("id = ? AND company_id = ?", requestID, companyID).
		Update("notify_stopped", 1).Error
}
