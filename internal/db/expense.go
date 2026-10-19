package db

import (
	"context"
	"strconv"
	"time"

	"gorm.io/gorm"

	"mochi-oa-server-go/internal/types"
)

// 本文件实现报销申请（expense）的 MySQL 访问。风格照 attendance.go：
// s.gorm.WithContext(ctx).Table("显式表名")，联表用自定义 scan struct + gorm:"column:xxx"。

// CreateExpenseRequestInput 报销申请创建入参。
type CreateExpenseRequestInput struct {
	CompanyID   int64
	UserID      int64
	Category    string
	Amount      float64
	Reason      string
	Evidence    []string
	ApproverIDs []int64
	CcIDs       []int64
}

// expenseScanRow 联表行：报销字段 + 发起人 nick/username。
type expenseScanRow struct {
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

func (s *MySqlStore) expenseBaseQuery(ctx context.Context) *gorm.DB {
	return s.gorm.WithContext(ctx).Table("expenses r").
		Joins("LEFT JOIN users u ON u.id = r.user_id")
}

func scanRowToExpense(r expenseScanRow) types.ExpenseRequest {
	return types.ExpenseRequest{
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

const expenseSelect = "r.id, r.company_id, r.user_id, r.category, r.amount, r.reason, r.evidence, r.status, r.approver_id, r.approval_note, r.approved_at, r.approver_ids, r.cc_ids, r.cc_viewed_ids, r.notify_stopped, r.created_at, r.updated_at, u.username, u.nick"

// CreateExpenseRequest 发起报销申请；审批人/抄送人由调用方按快照传入。
func (s *MySqlStore) CreateExpenseRequest(ctx context.Context, in CreateExpenseRequestInput) (*types.ExpenseRequest, error) {
	now := time.Now().UTC()
	row := types.ExpenseRequestRow{
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
	if err := s.gorm.WithContext(ctx).Table("expenses").Create(&row).Error; err != nil {
		return nil, err
	}
	return s.GetExpenseByID(ctx, row.ID)
}

func (s *MySqlStore) GetExpenseByID(ctx context.Context, id int64) (*types.ExpenseRequest, error) {
	var r expenseScanRow
	err := s.expenseBaseQuery(ctx).Select(expenseSelect).Where("r.id = ?", id).Take(&r).Error
	if err != nil {
		if gormNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	out := scanRowToExpense(r)
	return &out, nil
}

// ListExpenseRequestsByUser 只看本人历史；category 空串=不限；createdAt 倒序分页。
func (s *MySqlStore) ListExpenseRequestsByUser(ctx context.Context, companyID, userID int64, category string, page, pageSize int64) ([]types.ExpenseRequest, int64, error) {
	q := s.gorm.WithContext(ctx).Table("expenses r").
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
	listQ := s.expenseBaseQuery(ctx).Select(expenseSelect).
		Where("r.company_id = ?", companyID).
		Where("r.user_id = ?", userID)
	if category != "" {
		listQ = listQ.Where("r.category = ?", category)
	}
	rows := []expenseScanRow{}
	if err := listQ.Order("r.created_at DESC, r.id DESC").
		Limit(int(pageSize)).
		Offset(int((page-1)*pageSize)).
		Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	out := []types.ExpenseRequest{}
	for _, r := range rows {
		out = append(out, scanRowToExpense(r))
	}
	return out, total, nil
}

func (s *MySqlStore) ApproveExpenseRequest(ctx context.Context, id, approverID int64) (*types.ExpenseRequest, error) {
	now := time.Now().UTC()
	res := s.gorm.WithContext(ctx).Table("expenses").
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
		return nil, &NotFoundError{Msg: "expense request not found"}
	}
	return s.GetExpenseByID(ctx, id)
}

func (s *MySqlStore) RejectExpenseRequest(ctx context.Context, id int64, note string) (*types.ExpenseRequest, error) {
	now := time.Now().UTC()
	res := s.gorm.WithContext(ctx).Table("expenses").
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
		return nil, &NotFoundError{Msg: "expense request not found"}
	}
	return s.GetExpenseByID(ctx, id)
}

// ListReceivedExpense 收到审批：本人提交 OR 本人被列为审批人（含自审批）。
func (s *MySqlStore) ListReceivedExpense(ctx context.Context, companyID, userID int64) ([]types.ExpenseRequest, error) {
	approverJSON := strconv.FormatInt(userID, 10)
	rows := []expenseScanRow{}
	err := s.expenseBaseQuery(ctx).Select(expenseSelect).
		Where("r.company_id = ?", companyID).
		Where("(r.user_id = ? OR JSON_CONTAINS(r.approver_ids, ?))", userID, approverJSON).
		Order("r.created_at DESC, r.id DESC").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := []types.ExpenseRequest{}
	for _, r := range rows {
		out = append(out, scanRowToExpense(r))
	}
	return out, nil
}

// ListReceivedExpenseCC 收到抄送：本人提交 OR 本人被列为抄送人。
func (s *MySqlStore) ListReceivedExpenseCC(ctx context.Context, companyID, userID int64) ([]types.ExpenseRequest, error) {
	ccJSON := strconv.FormatInt(userID, 10)
	rows := []expenseScanRow{}
	err := s.expenseBaseQuery(ctx).Select(expenseSelect).
		Where("r.company_id = ?", companyID).
		Where("(r.user_id = ? OR JSON_CONTAINS(r.cc_ids, ?))", userID, ccJSON).
		Order("r.created_at DESC, r.id DESC").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := []types.ExpenseRequest{}
	for _, r := range rows {
		out = append(out, scanRowToExpense(r))
	}
	return out, nil
}

// MarkExpenseCCViewed 把 userID 加入该报销申请的 cc_viewed_ids（幂等）。
func (s *MySqlStore) MarkExpenseCCViewed(ctx context.Context, requestID, userID int64) error {
	var row struct {
		CcViewedIDs string `gorm:"column:cc_viewed_ids"`
	}
	err := s.gorm.WithContext(ctx).Table("expenses").
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
	return s.gorm.WithContext(ctx).Table("expenses").
		Where("id = ?", requestID).
		Updates(map[string]any{
			"cc_viewed_ids": encodeRecipients(existing),
			"updated_at":    time.Now().UTC(),
		}).Error
}

// StopExpenseNotification 幂等置 notify_stopped=1（停止循环提醒）。
func (s *MySqlStore) StopExpenseNotification(ctx context.Context, requestID int64, companyID int64) error {
	return s.gorm.WithContext(ctx).
		Table("expenses").
		Where("id = ? AND company_id = ?", requestID, companyID).
		Update("notify_stopped", 1).Error
}
