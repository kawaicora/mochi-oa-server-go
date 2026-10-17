package db

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"mochi-oa-server-go/internal/types"
)

// 本文件实现出网代理账号与上网申请审批的 MySQL 访问。
// JSON 列（approver_ids/cc_ids）type:json 可空无 default；写入侧空数组落 "[]"，读取侧 decodeRecipients 兜底 []。

// genUsername 生成唯一用户名：co{companyId}-{8位随机小写十六进制}。
func genUsername(companyID int64) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("co%d-%s", companyID, hex.EncodeToString(b))
}

// genSecret 生成 24 字符随机密钥（12 字节 → 24 位十六进制）。
func genSecret() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// CreateProxyAccount 新建账号；username 冲突则重试若干次（rand 碰撞概率极低）。
func (s *MySqlStore) CreateProxyAccount(ctx context.Context, companyID, createdBy int64, expiresAt time.Time, note *string) (*types.ProxyAccountRow, error) {
	var lastErr error
	for i := 0; i < 5; i++ {
		row := types.ProxyAccountRow{
			CompanyID: companyID,
			Username:  genUsername(companyID),
			Secret:    genSecret(),
			ExpiresAt: expiresAt.UTC(),
			Status:    "active",
			Note:      note,
			CreatedBy: createdBy,
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
		}
		if err := s.gorm.WithContext(ctx).Table("proxy_accounts").Create(&row).Error; err != nil {
			if isUniqueErr(err) {
				lastErr = err
				continue
			}
			return nil, err
		}
		return &row, nil
	}
	return nil, lastErr
}

// ProxyAuthLookup 按 username+secret 鉴权（HTTP Basic / SOCKS5 RFC1929 共用）。
func (s *MySqlStore) ProxyAuthLookup(ctx context.Context, username, secret string) (*types.ProxyAccountRow, error) {
	var r types.ProxyAccountRow
	err := s.gorm.WithContext(ctx).Table("proxy_accounts").
		Where("username = ?", username).
		Where("secret = ?", secret).
		Take(&r).Error
	if err != nil {
		if gormNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &r, nil
}

// GetProxyAccountByID 取账号行（服务端内部用，含 secret）。
func (s *MySqlStore) GetProxyAccountByID(ctx context.Context, id int64) (*types.ProxyAccountRow, error) {
	var r types.ProxyAccountRow
	err := s.gorm.WithContext(ctx).Table("proxy_accounts").Where("id = ?", id).Take(&r).Error
	if err != nil {
		if gormNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &r, nil
}

// ListProxyAccounts 公司账号分页（视图不含 secret）。
func (s *MySqlStore) ListProxyAccounts(ctx context.Context, companyID int64, page, pageSize int64) ([]types.ProxyAccountItem, int64, error) {
	q := s.gorm.WithContext(ctx).Table("proxy_accounts").Where("company_id = ?", companyID)
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
	rows := []types.ProxyAccountRow{}
	if err := q.Order("id DESC").Limit(int(pageSize)).Offset(int((page - 1) * pageSize)).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	out := []types.ProxyAccountItem{}
	for _, r := range rows {
		out = append(out, types.ProxyAccountItem{
			ID:        r.ID,
			CompanyID: r.CompanyID,
			Username:  r.Username,
			ExpiresAt: toIso(r.ExpiresAt),
			Status:    r.Status,
			Note:      r.Note,
			CreatedBy: r.CreatedBy,
			CreatedAt: toIso(r.CreatedAt),
			UpdatedAt: toIso(r.UpdatedAt),
		})
	}
	return out, total, nil
}

// DisableProxyAccount 置 disabled。
func (s *MySqlStore) DisableProxyAccount(ctx context.Context, id int64) error {
	res := s.gorm.WithContext(ctx).Table("proxy_accounts").
		Where("id = ?", id).
		Updates(map[string]any{"status": "disabled", "updated_at": time.Now().UTC()})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return &NotFoundError{Msg: "proxy account not found"}
	}
	return nil
}

// DeleteProxyAccount 物理删除。
func (s *MySqlStore) DeleteProxyAccount(ctx context.Context, id int64) error {
	res := s.gorm.WithContext(ctx).Table("proxy_accounts").Where("id = ?", id).Delete(nil)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return &NotFoundError{Msg: "proxy account not found"}
	}
	return nil
}

// SweepExpiredProxyAccounts 把已过期的 active 账号批量置 disabled，返回条数。
func (s *MySqlStore) SweepExpiredProxyAccounts(ctx context.Context) (int64, error) {
	res := s.gorm.WithContext(ctx).Table("proxy_accounts").
		Where("status = ?", "active").
		Where("expires_at < ?", time.Now().UTC()).
		Updates(map[string]any{"status": "disabled", "updated_at": time.Now().UTC()})
	if res.Error != nil {
		return 0, res.Error
	}
	return res.RowsAffected, nil
}

// ReactivateProxyAccount 续期：status=active 且 expires_at=新时间。
func (s *MySqlStore) ReactivateProxyAccount(ctx context.Context, id int64, expiresAt time.Time) error {
	res := s.gorm.WithContext(ctx).Table("proxy_accounts").
		Where("id = ?", id).
		Updates(map[string]any{
			"status":     "active",
			"expires_at": expiresAt.UTC(),
			"updated_at": time.Now().UTC(),
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return &NotFoundError{Msg: "proxy account not found"}
	}
	return nil
}

// ─── 上网申请 ───

// proxyReqScanRow 联表行：申请字段 + 申请人 nick/username。
type proxyReqScanRow struct {
	ID           int64      `gorm:"column:id"`
	CompanyID    int64      `gorm:"column:company_id"`
	UserID       int64      `gorm:"column:user_id"`
	Status       string     `gorm:"column:status"`
	Note         *string    `gorm:"column:note"`
	AccountID    *int64     `gorm:"column:account_id"`
	ApproverIDs  string     `gorm:"column:approver_ids"`
	CcIDs        string     `gorm:"column:cc_ids"`
	ApproverID   *int64     `gorm:"column:approver_id"`
	ApprovalNote *string    `gorm:"column:approval_note"`
	ApprovedAt   *time.Time `gorm:"column:approved_at"`
	ExpiresAt    *time.Time `gorm:"column:expires_at"`
	CreatedAt    time.Time  `gorm:"column:created_at"`
	UpdatedAt    time.Time  `gorm:"column:updated_at"`
	Username     string     `gorm:"column:username"`
	Nick         string     `gorm:"column:nick"`
}

func scanRowToProxyReq(r proxyReqScanRow) types.ProxyRequestItem {
	return types.ProxyRequestItem{
		ID:           r.ID,
		CompanyID:    r.CompanyID,
		UserID:       r.UserID,
		Nick:         r.Nick,
		Username:     r.Username,
		Status:       r.Status,
		Note:         r.Note,
		AccountID:    r.AccountID,
		ApproverIDs:  decodeRecipients(r.ApproverIDs),
		CcIDs:        decodeRecipients(r.CcIDs),
		ApproverID:   r.ApproverID,
		ApprovalNote: r.ApprovalNote,
		ApprovedAt:   isoPtr(r.ApprovedAt),
		ExpiresAt:    isoPtr(r.ExpiresAt),
		CreatedAt:    toIso(r.CreatedAt),
		UpdatedAt:    toIso(r.UpdatedAt),
	}
}

const proxyReqSelect = "r.id, r.company_id, r.user_id, r.status, r.note, r.account_id, r.approver_ids, r.cc_ids, r.approver_id, r.approval_note, r.approved_at, r.expires_at, r.created_at, r.updated_at, u.username, u.nick"

// CreateProxyAccessRequest 新建上网申请。
func (s *MySqlStore) CreateProxyAccessRequest(ctx context.Context, in CreateProxyAccessRequestInput) (*types.ProxyRequestItem, error) {
	now := time.Now().UTC()
	row := types.ProxyAccessRequestRow{
		CompanyID:   in.CompanyID,
		UserID:      in.UserID,
		Status:      "pending",
		Note:        in.Note,
		ApproverIDs: encodeRecipients(in.ApproverIDs),
		CcIDs:       encodeRecipients(in.CcIDs),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.gorm.WithContext(ctx).Table("proxy_access_requests").Create(&row).Error; err != nil {
		return nil, err
	}
	return s.GetProxyAccessRequestByID(ctx, row.ID)
}

// GetProxyAccessRequestByID 按 id 取（联表补昵称）。
func (s *MySqlStore) GetProxyAccessRequestByID(ctx context.Context, id int64) (*types.ProxyRequestItem, error) {
	var r proxyReqScanRow
	err := s.gorm.WithContext(ctx).Table("proxy_access_requests r").
		Joins("LEFT JOIN users u ON u.id = r.user_id").
		Select(proxyReqSelect).
		Where("r.id = ?", id).Take(&r).Error
	if err != nil {
		if gormNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	out := scanRowToProxyReq(r)
	return &out, nil
}

// ListProxyAccessRequests 公司申请列表；all=true 看全部（admin），否则只看本人。
func (s *MySqlStore) ListProxyAccessRequests(ctx context.Context, companyID, userID int64, all bool, status string, page, pageSize int64) ([]types.ProxyRequestItem, int64, error) {
	q := s.gorm.WithContext(ctx).Table("proxy_access_requests r").Where("r.company_id = ?", companyID)
	if !all {
		q = q.Where("r.user_id = ?", userID)
	}
	if status != "" {
		q = q.Where("r.status = ?", status)
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
	rows := []proxyReqScanRow{}
	listQ := s.gorm.WithContext(ctx).Table("proxy_access_requests r").
		Joins("LEFT JOIN users u ON u.id = r.user_id").
		Select(proxyReqSelect).
		Where("r.company_id = ?", companyID)
	if !all {
		listQ = listQ.Where("r.user_id = ?", userID)
	}
	if status != "" {
		listQ = listQ.Where("r.status = ?", status)
	}
	if err := listQ.Order("r.created_at DESC, r.id DESC").
		Limit(int(pageSize)).Offset(int((page - 1) * pageSize)).
		Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	out := []types.ProxyRequestItem{}
	for _, r := range rows {
		out = append(out, scanRowToProxyReq(r))
	}
	return out, total, nil
}

// UpdateProxyAccessRequest 更新申请字段。
func (s *MySqlStore) UpdateProxyAccessRequest(ctx context.Context, id int64, in UpdateProxyAccessRequestInput) (*types.ProxyRequestItem, error) {
	updates := map[string]any{"updated_at": time.Now().UTC()}
	if in.Status != nil {
		updates["status"] = *in.Status
	}
	if in.AccountID != nil {
		updates["account_id"] = *in.AccountID
	}
	if in.ApproverID != nil {
		updates["approver_id"] = *in.ApproverID
	}
	if in.ApprovalNote != nil {
		updates["approval_note"] = *in.ApprovalNote
	}
	if in.ApprovedAt != nil {
		updates["approved_at"] = in.ApprovedAt.UTC()
	}
	if in.ExpiresAt != nil {
		updates["expires_at"] = in.ExpiresAt.UTC()
	}
	if in.ApproverIDs != nil {
		updates["approver_ids"] = encodeRecipients(in.ApproverIDs)
	}
	if in.CcIDs != nil {
		updates["cc_ids"] = encodeRecipients(in.CcIDs)
	}
	res := s.gorm.WithContext(ctx).Table("proxy_access_requests").Where("id = ?", id).Updates(updates)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, &NotFoundError{Msg: "proxy access request not found"}
	}
	return s.GetProxyAccessRequestByID(ctx, id)
}
