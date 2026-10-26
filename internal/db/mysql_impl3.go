// Package db 的 MySqlStore 剩余方法：邀请、人事、会话、爱发电。
//
// 本文件全部走 GORM（s.gorm），不再出现裸 SQL 字面量；表名一律用 Table("...") 显式指定。
package db

import (
	"context"
	"strings"
	"time"

	"gorm.io/gorm/clause"

	"mochi-oa-server-go/internal/types"
)

// ==================== 公司邀请 ====================

// invitationView 联表组装邀请视图（公司/邀请人/部门名）。
func (s *MySqlStore) invitationView(ctx context.Context, row types.CompanyInvitationRow) (*types.CompanyInvitation, error) {
	inv := &types.CompanyInvitation{
		ID: row.ID, CompanyID: row.CompanyID, UserID: row.UserID,
		DepartmentID: row.DepartmentID, InvitedBy: row.InvitedBy, Code: row.Code, Status: row.Status,
	}
	if row.ExpiresAt != nil {
		t := toIso(*row.ExpiresAt)
		inv.ExpiresAt = &t
	}
	if c, err := s.GetCompanyByID(ctx, row.CompanyID); err == nil && c != nil {
		inv.CompanyName = c.Name
		inv.CompanyCode = c.Code
	}
	if u, err := s.GetUserByID(ctx, row.UserID); err == nil && u != nil {
		inv.Username = u.Username
		nick := u.Nick
		if nick == "" {
			nick = u.Username
		}
		inv.UserNick = nick
	}
	if inviter, err := s.GetUserByID(ctx, row.InvitedBy); err == nil && inviter != nil {
		nick := inviter.Nick
		if nick == "" {
			nick = inviter.Username
		}
		inv.InviterNick = nick
	}
	if row.DepartmentID != nil {
		if d, err := s.GetDepartmentByID(ctx, *row.DepartmentID); err == nil && d != nil {
			name := d.Name
			inv.DepartmentName = &name
		}
	}
	inv.CreatedAt = toIso(row.CreatedAt)
	return inv, nil
}

// CreateCompanyInvitation 创建邀请（同公司同用户已有 pending 则冲突）。
func (s *MySqlStore) CreateCompanyInvitation(ctx context.Context, in CreateInvitationInput) (*types.CompanyInvitation, error) {
	// 原实现用 SELECT ... LIMIT 1 判重：取不到行（ErrNoRows）表示可创建。
	// Pluck 在无行时同样返回 nil 错误 + 空切片，语义一致。
	dupIDs := []int64{}
	if err := s.gorm.WithContext(ctx).Table("company_invitations").
		Where("company_id = ? AND user_id = ? AND status = 'pending'", in.CompanyID, in.UserID).
		Limit(1).Pluck("id", &dupIDs).Error; err != nil {
		return nil, err
	}
	if len(dupIDs) > 0 {
		return nil, &AlreadyExistsError{Msg: "invitation pending"}
	}
	var expires any
	if in.ExpiresAt != nil && *in.ExpiresAt != "" {
		if t, err := parseTimeFlex(*in.ExpiresAt); err == nil {
			expires = t
		}
	}
	// 用 map 插入：列清单与原 SQL 完全一致，status 交给库默认值 'pending'
	// （若改用结构体 Create，GORM 会把零值 "" 写进 enum 列，语义就变了）。
	if err := s.gorm.WithContext(ctx).Table("company_invitations").Create(map[string]any{
		"company_id":    in.CompanyID,
		"user_id":       in.UserID,
		"department_id": in.DepartmentID,
		"invited_by":    in.InvitedBy,
		"code":          in.Code,
		"expires_at":    expires,
		"created_at":    time.Now().UTC(),
	}).Error; err != nil {
		return nil, err
	}
	// code 列有唯一索引，回读自增主键（等价 LastInsertId）。
	var id int64
	if err := s.gorm.WithContext(ctx).Table("company_invitations").
		Where("code = ?", in.Code).Pluck("id", &id).Error; err != nil {
		return nil, err
	}
	return s.GetCompanyInvitationByID(ctx, id)
}

// GetCompanyInvitationByID 按主键取邀请。
func (s *MySqlStore) GetCompanyInvitationByID(ctx context.Context, id int64) (*types.CompanyInvitation, error) {
	var row types.CompanyInvitationRow
	err := s.gorm.WithContext(ctx).Table("company_invitations").Where("id = ?", id).Take(&row).Error
	if gormNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.invitationView(ctx, row)
}

// GetCompanyInvitationByCode 按邀请码取。
func (s *MySqlStore) GetCompanyInvitationByCode(ctx context.Context, code string) (*types.CompanyInvitation, error) {
	var row types.CompanyInvitationRow
	err := s.gorm.WithContext(ctx).Table("company_invitations").Where("code = ?", code).Take(&row).Error
	if gormNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.invitationView(ctx, row)
}

// ListCompanyInvitations 列公司邀请（创建倒序）。
func (s *MySqlStore) ListCompanyInvitations(ctx context.Context, companyID int64) ([]types.CompanyInvitation, error) {
	rows := []types.CompanyInvitationRow{}
	if err := s.gorm.WithContext(ctx).Table("company_invitations").
		Where("company_id = ?", companyID).
		Order("created_at DESC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := []types.CompanyInvitation{}
	for _, row := range rows {
		v, err := s.invitationView(ctx, row)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, nil
}

// ListPendingInvitationsForUser 列用户 pending 邀请。
func (s *MySqlStore) ListPendingInvitationsForUser(ctx context.Context, userID int64) ([]types.CompanyInvitation, error) {
	rows := []types.CompanyInvitationRow{}
	if err := s.gorm.WithContext(ctx).Table("company_invitations").
		Where("user_id = ? AND status = 'pending'", userID).
		Order("created_at DESC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := []types.CompanyInvitation{}
	for _, row := range rows {
		v, err := s.invitationView(ctx, row)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, nil
}

// SetCompanyInvitationStatus 更新邀请状态。
func (s *MySqlStore) SetCompanyInvitationStatus(ctx context.Context, id int64, status string) error {
	return s.gorm.WithContext(ctx).Table("company_invitations").
		Where("id = ?", id).
		Updates(map[string]any{"status": status}).Error
}

// GetCompanyMainGroup 找公司全员群（按「{公司名}总群」约定，回退 owner 自建群）。
func (s *MySqlStore) GetCompanyMainGroup(ctx context.Context, companyID int64) (*types.Group, error) {
	groups, err := s.ListGroups(ctx, companyID, "")
	if err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return nil, nil
	}
	company, err := s.GetCompanyByID(ctx, companyID)
	if err != nil {
		return nil, err
	}
	cname := ""
	if company != nil {
		cname = company.Name
	}
	for i := range groups {
		if groups[i].Name == cname+"总群" {
			return &groups[i], nil
		}
	}
	for i := range groups {
		if strings.HasSuffix(groups[i].Name, "总群") {
			return &groups[i], nil
		}
	}
	for i := range groups {
		if company != nil && groups[i].OwnerID == company.OwnerID {
			return &groups[i], nil
		}
	}
	return nil, nil
}

// ==================== 成员人事信息 ====================

// GetMemberProfile 取人事档案。
func (s *MySqlStore) GetMemberProfile(ctx context.Context, companyID, userID int64) (*types.MemberProfile, error) {
	var r types.MemberProfileRow
	err := s.gorm.WithContext(ctx).Table("member_profiles").
		Where("company_id = ? AND user_id = ?", companyID, userID).
		Take(&r).Error
	if gormNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p := types.MemberProfile{
		CompanyID: r.CompanyID, UserID: r.UserID, RealName: r.RealName, IDCard: r.IDCard, BankCard: r.BankCard,
		ResumeURL: r.ResumeURL, PortfolioURL: r.PortfolioURL, UpdatedAt: toIso(r.UpdatedAt),
	}
	return &p, nil
}

// UpsertMemberProfile 更新/插入人事档案。
func (s *MySqlStore) UpsertMemberProfile(ctx context.Context, companyID, userID int64, in MemberProfileInput) (*types.MemberProfile, error) {
	base, _ := s.GetMemberProfile(ctx, companyID, userID)
	realName, idCard, bankCard, resumeURL, portfolioURL := "", "", "", "", ""
	if base != nil {
		realName, idCard, bankCard, resumeURL, portfolioURL = base.RealName, base.IDCard, base.BankCard, base.ResumeURL, base.PortfolioURL
	}
	if in.RealName != nil {
		realName = *in.RealName
	}
	if in.IDCard != nil {
		idCard = *in.IDCard
	}
	if in.BankCard != nil {
		bankCard = *in.BankCard
	}
	if in.ResumeURL != nil {
		resumeURL = *in.ResumeURL
	}
	if in.PortfolioURL != nil {
		portfolioURL = *in.PortfolioURL
	}
	// 对应原 SQL 的 ON DUPLICATE KEY UPDATE ... updated_at=NOW(3)：
	// 冲突时覆盖全部业务列 + updated_at，不覆盖 company_id/user_id（联合主键）。
	err := s.gorm.WithContext(ctx).Table("member_profiles").
		Clauses(clause.OnConflict{DoUpdates: clause.AssignmentColumns([]string{
			"real_name", "id_card", "bank_card", "resume_url", "portfolio_url", "updated_at",
		})}).
		Create(map[string]any{
			"company_id":    companyID,
			"user_id":       userID,
			"real_name":     realName,
			"id_card":       idCard,
			"bank_card":     bankCard,
			"resume_url":    resumeURL,
			"portfolio_url": portfolioURL,
			"updated_at":    time.Now().UTC(),
		}).Error
	if err != nil {
		return nil, err
	}
	return s.GetMemberProfile(ctx, companyID, userID)
}

// ==================== 登录会话 ====================

// CreateSession 创建会话。
func (s *MySqlStore) CreateSession(ctx context.Context, in CreateSessionInput) (*types.Session, error) {
	expires, err := parseTimeFlex(in.ExpiresAt)
	if err != nil {
		return nil, err
	}
	loc := in.Location
	if loc == "" {
		loc = ""
	}
	now := time.Now().UTC()
	row := types.SessionRow{
		UserID: in.UserID, TokenHash: in.TokenHash, Device: in.Device, IP: in.IP,
		Location: loc, ExpiresAt: expires, CreatedAt: now, LastActiveAt: now,
	}
	if err := s.gorm.WithContext(ctx).Table("sessions").Create(&row).Error; err != nil {
		return nil, err
	}
	return s.getSessionByID(ctx, row.ID)
}

func (s *MySqlStore) getSessionByID(ctx context.Context, id int64) (*types.Session, error) {
	var r types.SessionRow
	err := s.gorm.WithContext(ctx).Table("sessions").Where("id = ?", id).Take(&r).Error
	if gormNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ses := rowToSession(&r)
	return &ses, nil
}

// GetSessionByTokenHash 按 token 摘要找会话。
func (s *MySqlStore) GetSessionByTokenHash(ctx context.Context, tokenHash string) (*types.Session, error) {
	var r types.SessionRow
	err := s.gorm.WithContext(ctx).Table("sessions").Where("token_hash = ?", tokenHash).Take(&r).Error
	if gormNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ses := rowToSession(&r)
	return &ses, nil
}

// ListSessionsByUser 列用户会话（新→旧）。
func (s *MySqlStore) ListSessionsByUser(ctx context.Context, userID int64) ([]types.Session, error) {
	rows := []types.SessionRow{}
	if err := s.gorm.WithContext(ctx).Table("sessions").
		Where("user_id = ?", userID).
		Order("created_at DESC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := []types.Session{}
	for i := range rows {
		out = append(out, rowToSession(&rows[i]))
	}
	return out, nil
}

// sessionLocationRow GetUserActiveLocation 的临时行。
type sessionLocationRow struct {
	Location  string     `gorm:"column:location"`
	ExpiresAt *time.Time `gorm:"column:expires_at"`
}

// GetUserActiveLocation 取用户最近有效会话的归属地。
func (s *MySqlStore) GetUserActiveLocation(ctx context.Context, userID int64) (string, error) {
	rows := []sessionLocationRow{}
	if err := s.gorm.WithContext(ctx).Table("sessions").
		Select("location, expires_at").
		Where("user_id = ?", userID).
		Order("last_active_at DESC").
		Scan(&rows).Error; err != nil {
		return "", err
	}
	var first string
	for _, r := range rows {
		if first == "" {
			first = r.Location
		}
		if r.ExpiresAt != nil && r.ExpiresAt.After(time.Now().UTC()) {
			return r.Location, nil
		}
	}
	return first, nil
}

// TouchSession 刷新会话活跃时间。
func (s *MySqlStore) TouchSession(ctx context.Context, id int64) error {
	return s.gorm.WithContext(ctx).Table("sessions").
		Where("id = ?", id).
		Updates(map[string]any{"last_active_at": time.Now().UTC()}).Error
}

// DeleteSession 删除会话。
func (s *MySqlStore) DeleteSession(ctx context.Context, id int64) error {
	return s.gorm.WithContext(ctx).Table("sessions").Where("id = ?", id).Delete(nil).Error
}

// DeleteExpiredSessions 清理过期会话，返回删除条数。
func (s *MySqlStore) DeleteExpiredSessions(ctx context.Context, now int64) (int64, error) {
	// 保留库侧 FROM_UNIXTIME，避免依赖服务器时区与 Go 侧换算的一致性。
	res := s.gorm.WithContext(ctx).Table("sessions").
		Where("expires_at < FROM_UNIXTIME(?)", now/1000).
		Delete(nil)
	if res.Error != nil {
		return 0, res.Error
	}
	return res.RowsAffected, nil
}

// GetSessionDays 取用户登录有效期（天）。
func (s *MySqlStore) GetSessionDays(ctx context.Context, userID int64) (*int64, error) {
	var r struct {
		SessionDays *int64 `gorm:"column:session_days"`
	}
	err := s.gorm.WithContext(ctx).Table("users").
		Select("session_days").
		Where("id = ?", userID).
		Take(&r).Error
	if gormNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if r.SessionDays == nil {
		return nil, nil
	}
	v := *r.SessionDays
	return &v, nil
}

// SetSessionDays 设置用户登录有效期（天，nil 清除）。
func (s *MySqlStore) SetSessionDays(ctx context.Context, userID int64, days *int64) error {
	var val any
	if days != nil {
		val = *days
	}
	return s.gorm.WithContext(ctx).Table("users").
		Where("id = ?", userID).
		Updates(map[string]any{"session_days": val}).Error
}

// ==================== 爱发电订单 ====================

// afdianOrderUpdateCols 冲突时需要覆盖的业务列（不含主键与 created_at）。
var afdianOrderUpdateCols = []string{
	"custom_order_id", "plan_id", "plan_title", "title", "month", "amount", "show_amount",
	"status", "remark", "redeem_id", "product_type", "discount", "buyer_user_id",
	"buyer_name", "buyer_avatar", "create_time", "sku_detail", "raw",
}

// UpsertAfdianOrder 落库/更新订单（按 out_trade_no）。
func (s *MySqlStore) UpsertAfdianOrder(ctx context.Context, in UpsertAfdianOrderInput) (*types.AfdianOrder, error) {
	// 对应原 SQL 的 ON DUPLICATE KEY UPDATE <业务列>=VALUES(<业务列>)：created_at 仅在插入时写入。
	err := s.gorm.WithContext(ctx).Table("afdian_orders").
		Clauses(clause.OnConflict{DoUpdates: clause.AssignmentColumns(afdianOrderUpdateCols)}).
		Create(map[string]any{
			"out_trade_no":    in.OutTradeNo,
			"custom_order_id": orEmpty(in.CustomOrderID),
			"plan_id":         orEmpty(in.PlanID),
			"plan_title":      orEmpty(in.PlanTitle),
			"title":           orEmpty(in.Title),
			"month":           in.Month,
			"amount":          orEmpty(in.Amount),
			"show_amount":     orEmpty(in.ShowAmount),
			"status":          in.Status,
			"remark":          orEmpty(in.Remark),
			"redeem_id":       orEmpty(in.RedeemID),
			"product_type":    in.ProductType,
			"discount":        orEmpty(in.Discount),
			"buyer_user_id":   orEmpty(in.BuyerUserID),
			"buyer_name":      orEmpty(in.BuyerName),
			"buyer_avatar":    orEmpty(in.BuyerAvatar),
			"create_time":     time.Unix(in.CreateTime, 0).UTC(),
			"sku_detail":      orEmpty(in.SkuDetail),
			"raw":             orEmpty(in.Raw),
			"created_at":      time.Now().UTC(),
		}).Error
	if err != nil {
		return nil, err
	}
	return s.GetAfdianOrderByOutTradeNo(ctx, in.OutTradeNo)
}

// GetAfdianOrderByOutTradeNo 按订单号查。
func (s *MySqlStore) GetAfdianOrderByOutTradeNo(ctx context.Context, outTradeNo string) (*types.AfdianOrder, error) {
	var r types.AfdianOrderRow
	err := s.gorm.WithContext(ctx).Table("afdian_orders").
		Where("out_trade_no = ?", outTradeNo).
		Take(&r).Error
	if gormNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	o := rowToAfdian(&r)
	return &o, nil
}

// ListAfdianOrders 列订单（创建倒序）。
func (s *MySqlStore) ListAfdianOrders(ctx context.Context, limit, offset int64) ([]types.AfdianOrder, error) {
	rows := []types.AfdianOrderRow{}
	if err := s.gorm.WithContext(ctx).Table("afdian_orders").
		Order("created_at DESC").
		Limit(int(limit)).
		Offset(int(offset)).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := []types.AfdianOrder{}
	for i := range rows {
		out = append(out, rowToAfdian(&rows[i]))
	}
	return out, nil
}

// orEmpty 空串兜底。
func orEmpty(v string) string {
	if v == "" {
		return ""
	}
	return v
}
