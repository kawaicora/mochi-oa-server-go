// Package db 的 MySqlStore 剩余方法：邀请、人事、会话、爱发电、邮件配置、假期、任务流程、TAPD。
package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

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
	var dupID int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM company_invitations WHERE company_id=? AND user_id=? AND status='pending' LIMIT 1`, in.CompanyID, in.UserID).Scan(&dupID)
	if err == nil {
		return nil, &AlreadyExistsError{Msg: "invitation pending"}
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var expires any
	if in.ExpiresAt != nil && *in.ExpiresAt != "" {
		if t, err := parseTimeFlex(*in.ExpiresAt); err == nil {
			expires = t
		}
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO company_invitations (company_id, user_id, department_id, invited_by, code, expires_at, created_at) VALUES (?,?,?,?,?,?,NOW(3))`,
		in.CompanyID, in.UserID, in.DepartmentID, in.InvitedBy, in.Code, expires)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.GetCompanyInvitationByID(ctx, id)
}

// GetCompanyInvitationByID 按主键取邀请。
func (s *MySqlStore) GetCompanyInvitationByID(ctx context.Context, id int64) (*types.CompanyInvitation, error) {
	var row types.CompanyInvitationRow
	err := s.db.QueryRowContext(ctx, `SELECT id, company_id, user_id, department_id, invited_by, code, expires_at, status, created_at FROM company_invitations WHERE id=?`, id).
		Scan(&row.ID, &row.CompanyID, &row.UserID, &row.DepartmentID, &row.InvitedBy, &row.Code, &row.ExpiresAt, &row.Status, &row.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
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
	err := s.db.QueryRowContext(ctx, `SELECT id, company_id, user_id, department_id, invited_by, code, expires_at, status, created_at FROM company_invitations WHERE code=?`, code).
		Scan(&row.ID, &row.CompanyID, &row.UserID, &row.DepartmentID, &row.InvitedBy, &row.Code, &row.ExpiresAt, &row.Status, &row.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.invitationView(ctx, row)
}

// ListCompanyInvitations 列公司邀请（创建倒序）。
func (s *MySqlStore) ListCompanyInvitations(ctx context.Context, companyID int64) ([]types.CompanyInvitation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, company_id, user_id, department_id, invited_by, code, expires_at, status, created_at FROM company_invitations WHERE company_id=? ORDER BY created_at DESC`, companyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.CompanyInvitation{}
	for rows.Next() {
		var row types.CompanyInvitationRow
		if err := rows.Scan(&row.ID, &row.CompanyID, &row.UserID, &row.DepartmentID, &row.InvitedBy, &row.Code, &row.ExpiresAt, &row.Status, &row.CreatedAt); err != nil {
			return nil, err
		}
		v, err := s.invitationView(ctx, row)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, rows.Err()
}

// ListPendingInvitationsForUser 列用户 pending 邀请。
func (s *MySqlStore) ListPendingInvitationsForUser(ctx context.Context, userID int64) ([]types.CompanyInvitation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, company_id, user_id, department_id, invited_by, code, expires_at, status, created_at FROM company_invitations WHERE user_id=? AND status='pending' ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.CompanyInvitation{}
	for rows.Next() {
		var row types.CompanyInvitationRow
		if err := rows.Scan(&row.ID, &row.CompanyID, &row.UserID, &row.DepartmentID, &row.InvitedBy, &row.Code, &row.ExpiresAt, &row.Status, &row.CreatedAt); err != nil {
			return nil, err
		}
		v, err := s.invitationView(ctx, row)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, rows.Err()
}

// SetCompanyInvitationStatus 更新邀请状态。
func (s *MySqlStore) SetCompanyInvitationStatus(ctx context.Context, id int64, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE company_invitations SET status=? WHERE id=?`, status, id)
	return err
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
	err := s.db.QueryRowContext(ctx, `SELECT company_id, user_id, real_name, id_card, bank_card, resume_url, portfolio_url, updated_at FROM member_profiles WHERE company_id=? AND user_id=?`, companyID, userID).
		Scan(&r.CompanyID, &r.UserID, &r.RealName, &r.IDCard, &r.BankCard, &r.ResumeURL, &r.PortfolioURL, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
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
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO member_profiles (company_id, user_id, real_name, id_card, bank_card, resume_url, portfolio_url, updated_at) VALUES (?,?,?,?,?,?,?,NOW(3))
		 ON DUPLICATE KEY UPDATE real_name=VALUES(real_name), id_card=VALUES(id_card), bank_card=VALUES(bank_card), resume_url=VALUES(resume_url), portfolio_url=VALUES(portfolio_url), updated_at=NOW(3)`,
		companyID, userID, realName, idCard, bankCard, resumeURL, portfolioURL)
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
	res, err := s.db.ExecContext(ctx, `INSERT INTO sessions (user_id, token_hash, device, ip, location, expires_at, created_at, last_active_at) VALUES (?,?,?,?,?,?,NOW(3),NOW(3))`,
		in.UserID, in.TokenHash, in.Device, in.IP, loc, expires)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.getSessionByID(ctx, id)
}

func (s *MySqlStore) getSessionByID(ctx context.Context, id int64) (*types.Session, error) {
	var r types.SessionRow
	err := s.db.QueryRowContext(ctx, `SELECT id, user_id, token_hash, device, ip, location, expires_at, last_active_at, created_at FROM sessions WHERE id=?`, id).
		Scan(&r.ID, &r.UserID, &r.TokenHash, &r.Device, &r.IP, &r.Location, &r.ExpiresAt, &r.LastActiveAt, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
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
	err := s.db.QueryRowContext(ctx, `SELECT id, user_id, token_hash, device, ip, location, expires_at, last_active_at, created_at FROM sessions WHERE token_hash=?`, tokenHash).
		Scan(&r.ID, &r.UserID, &r.TokenHash, &r.Device, &r.IP, &r.Location, &r.ExpiresAt, &r.LastActiveAt, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
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
	rows, err := s.db.QueryContext(ctx, `SELECT id, user_id, token_hash, device, ip, location, expires_at, last_active_at, created_at FROM sessions WHERE user_id=? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.Session{}
	for rows.Next() {
		var r types.SessionRow
		if err := rows.Scan(&r.ID, &r.UserID, &r.TokenHash, &r.Device, &r.IP, &r.Location, &r.ExpiresAt, &r.LastActiveAt, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, rowToSession(&r))
	}
	return out, rows.Err()
}

// GetUserActiveLocation 取用户最近有效会话的归属地。
func (s *MySqlStore) GetUserActiveLocation(ctx context.Context, userID int64) (string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT location, expires_at FROM sessions WHERE user_id=? ORDER BY last_active_at DESC`, userID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var first string
	for rows.Next() {
		var loc string
		var exp sql.NullTime
		if err := rows.Scan(&loc, &exp); err != nil {
			return "", err
		}
		if first == "" {
			first = loc
		}
		if exp.Valid && exp.Time.After(time.Now().UTC()) {
			return loc, nil
		}
	}
	return first, nil
}

// TouchSession 刷新会话活跃时间。
func (s *MySqlStore) TouchSession(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_active_at=NOW(3) WHERE id=?`, id)
	return err
}

// DeleteSession 删除会话。
func (s *MySqlStore) DeleteSession(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id=?`, id)
	return err
}

// DeleteExpiredSessions 清理过期会话，返回删除条数。
func (s *MySqlStore) DeleteExpiredSessions(ctx context.Context, now int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < FROM_UNIXTIME(?)`, now/1000)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// GetSessionDays 取用户登录有效期（天）。
func (s *MySqlStore) GetSessionDays(ctx context.Context, userID int64) (*int64, error) {
	var sd sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT session_days FROM users WHERE id=?`, userID).Scan(&sd)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !sd.Valid {
		return nil, nil
	}
	v := sd.Int64
	return &v, nil
}

// SetSessionDays 设置用户登录有效期（天，nil 清除）。
func (s *MySqlStore) SetSessionDays(ctx context.Context, userID int64, days *int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET session_days=? WHERE id=?`, days, userID)
	return err
}

// ==================== 爱发电订单 ====================

// UpsertAfdianOrder 落库/更新订单（按 out_trade_no）。
func (s *MySqlStore) UpsertAfdianOrder(ctx context.Context, in UpsertAfdianOrderInput) (*types.AfdianOrder, error) {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO afdian_orders (out_trade_no, custom_order_id, plan_id, plan_title, title, month, amount, show_amount, status, remark, redeem_id, product_type, discount, buyer_user_id, buyer_name, buyer_avatar, create_time, sku_detail, raw, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,NOW(3))
		 ON DUPLICATE KEY UPDATE custom_order_id=VALUES(custom_order_id), plan_id=VALUES(plan_id), plan_title=VALUES(plan_title), title=VALUES(title), month=VALUES(month), amount=VALUES(amount), show_amount=VALUES(show_amount), status=VALUES(status), remark=VALUES(remark), redeem_id=VALUES(redeem_id), product_type=VALUES(product_type), discount=VALUES(discount), buyer_user_id=VALUES(buyer_user_id), buyer_name=VALUES(buyer_name), buyer_avatar=VALUES(buyer_avatar), create_time=VALUES(create_time), sku_detail=VALUES(sku_detail), raw=VALUES(raw)`,
		in.OutTradeNo, orEmpty(in.CustomOrderID), orEmpty(in.PlanID), orEmpty(in.PlanTitle), orEmpty(in.Title), in.Month, orEmpty(in.Amount), orEmpty(in.ShowAmount), in.Status, orEmpty(in.Remark), orEmpty(in.RedeemID), in.ProductType, orEmpty(in.Discount), orEmpty(in.BuyerUserID), orEmpty(in.BuyerName), orEmpty(in.BuyerAvatar), in.CreateTime, orEmpty(in.SkuDetail), orEmpty(in.Raw))
	if err != nil {
		return nil, err
	}
	return s.GetAfdianOrderByOutTradeNo(ctx, in.OutTradeNo)
}

// GetAfdianOrderByOutTradeNo 按订单号查。
func (s *MySqlStore) GetAfdianOrderByOutTradeNo(ctx context.Context, outTradeNo string) (*types.AfdianOrder, error) {
	var r types.AfdianOrderRow
	err := s.db.QueryRowContext(ctx, `SELECT id, out_trade_no, custom_order_id, plan_id, plan_title, title, month, amount, show_amount, status, remark, redeem_id, product_type, discount, buyer_user_id, buyer_name, buyer_avatar, create_time, sku_detail, raw, created_at FROM afdian_orders WHERE out_trade_no=?`, outTradeNo).
		Scan(&r.ID, &r.OutTradeNo, &r.CustomOrderID, &r.PlanID, &r.PlanTitle, &r.Title, &r.Month, &r.Amount, &r.ShowAmount, &r.Status, &r.Remark, &r.RedeemID, &r.ProductType, &r.Discount, &r.BuyerUserID, &r.BuyerName, &r.BuyerAvatar, &r.CreateTime, &r.SkuDetail, &r.Raw, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
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
	rows, err := s.db.QueryContext(ctx, `SELECT id, out_trade_no, custom_order_id, plan_id, plan_title, title, month, amount, show_amount, status, remark, redeem_id, product_type, discount, buyer_user_id, buyer_name, buyer_avatar, create_time, sku_detail, raw, created_at FROM afdian_orders ORDER BY created_at DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.AfdianOrder{}
	for rows.Next() {
		var r types.AfdianOrderRow
		if err := rows.Scan(&r.ID, &r.OutTradeNo, &r.CustomOrderID, &r.PlanID, &r.PlanTitle, &r.Title, &r.Month, &r.Amount, &r.ShowAmount, &r.Status, &r.Remark, &r.RedeemID, &r.ProductType, &r.Discount, &r.BuyerUserID, &r.BuyerName, &r.BuyerAvatar, &r.CreateTime, &r.SkuDetail, &r.Raw, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, rowToAfdian(&r))
	}
	return out, rows.Err()
}

// orEmpty 空串兜底。
func orEmpty(v string) string {
	if v == "" {
		return ""
	}
	return v
}
