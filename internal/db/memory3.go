// Package db 的 MemoryStore 方法（邀请、人事、会话、爱发电、邮件、假期、项目/任务、TAPD、仪表盘）。
package db

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"mochi-oa-server-go/internal/types"
)

// ==================== 公司邀请 ====================

func (m *MemoryStore) invView(ctx context.Context, row types.CompanyInvitationRow) (*types.CompanyInvitation, error) {
	inv := &types.CompanyInvitation{ID: row.ID, CompanyID: row.CompanyID, UserID: row.UserID, DepartmentID: row.DepartmentID, InvitedBy: row.InvitedBy, Code: row.Code, Status: row.Status, CreatedAt: toIso(row.CreatedAt)}
	if row.ExpiresAt != nil {
		s := toIso(*row.ExpiresAt)
		inv.ExpiresAt = &s
	}
	if c, _ := m.GetCompanyByID(ctx, row.CompanyID); c != nil {
		inv.CompanyName, inv.CompanyCode = c.Name, c.Code
	}
	if u, _ := m.GetUserByID(ctx, row.UserID); u != nil {
		inv.Username = u.Username
		nk := u.Nick
		if nk == "" {
			nk = u.Username
		}
		inv.UserNick = nk
	}
	if invU, _ := m.GetUserByID(ctx, row.InvitedBy); invU != nil {
		nk := invU.Nick
		if nk == "" {
			nk = invU.Username
		}
		inv.InviterNick = nk
	}
	if row.DepartmentID != nil {
		if d, _ := m.GetDepartmentByID(ctx, *row.DepartmentID); d != nil {
			name := d.Name
			inv.DepartmentName = &name
		}
	}
	return inv, nil
}

func (m *MemoryStore) CreateCompanyInvitation(ctx context.Context, in CreateInvitationInput) (*types.CompanyInvitation, error) {
	for _, r := range m.invitations {
		if r.CompanyID == in.CompanyID && r.UserID == in.UserID && r.Status == "pending" {
			return nil, &AlreadyExistsError{Msg: "invitation pending"}
		}
	}
	id := m.nextId()
	var exp *time.Time
	if in.ExpiresAt != nil && *in.ExpiresAt != "" {
		if t, err := parseTimeFlex(*in.ExpiresAt); err == nil {
			exp = &t
		}
	}
	m.invitations[id] = &types.CompanyInvitationRow{ID: id, CompanyID: in.CompanyID, UserID: in.UserID, DepartmentID: in.DepartmentID, InvitedBy: in.InvitedBy, Code: in.Code, ExpiresAt: exp, Status: "pending", CreatedAt: time.Now().UTC()}
	return m.invView(ctx, *m.invitations[id])
}

func (m *MemoryStore) GetCompanyInvitationByID(_ context.Context, id int64) (*types.CompanyInvitation, error) {
	if r, ok := m.invitations[id]; ok {
		return m.invView(context.Background(), *r)
	}
	return nil, nil
}

func (m *MemoryStore) GetCompanyInvitationByCode(_ context.Context, code string) (*types.CompanyInvitation, error) {
	for _, r := range m.invitations {
		if r.Code == code {
			return m.invView(context.Background(), *r)
		}
	}
	return nil, nil
}

func (m *MemoryStore) ListCompanyInvitations(_ context.Context, companyID int64) ([]types.CompanyInvitation, error) {
	out := []types.CompanyInvitation{}
	for _, r := range m.invitations {
		if r.CompanyID == companyID {
			v, _ := m.invView(context.Background(), *r)
			out = append(out, *v)
		}
	}
	return out, nil
}

func (m *MemoryStore) ListPendingInvitationsForUser(_ context.Context, userID int64) ([]types.CompanyInvitation, error) {
	out := []types.CompanyInvitation{}
	for _, r := range m.invitations {
		if r.UserID == userID && r.Status == "pending" {
			v, _ := m.invView(context.Background(), *r)
			out = append(out, *v)
		}
	}
	return out, nil
}

func (m *MemoryStore) SetCompanyInvitationStatus(_ context.Context, id int64, status string) error {
	if r, ok := m.invitations[id]; ok {
		r.Status = status
	}
	return nil
}

func (m *MemoryStore) GetCompanyMainGroup(ctx context.Context, companyID int64) (*types.Group, error) {
	groups, _ := m.ListGroups(ctx, companyID, "")
	if len(groups) == 0 {
		return nil, nil
	}
	cname := ""
	if c, _ := m.GetCompanyByID(ctx, companyID); c != nil {
		cname = c.Name
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
	return nil, nil
}

// ==================== 人事 ====================

func (m *MemoryStore) profileKey(companyID, userID int64) string { return fmt.Sprintf("%d:%d", companyID, userID) }

func (m *MemoryStore) GetMemberProfile(_ context.Context, companyID, userID int64) (*types.MemberProfile, error) {
	if r, ok := m.profiles[m.profileKey(companyID, userID)]; ok {
		p := types.MemberProfile{CompanyID: r.CompanyID, UserID: r.UserID, RealName: r.RealName, IDCard: r.IDCard, BankCard: r.BankCard, ResumeURL: r.ResumeURL, PortfolioURL: r.PortfolioURL, UpdatedAt: toIso(r.UpdatedAt)}
		return &p, nil
	}
	return nil, nil
}

func (m *MemoryStore) UpsertMemberProfile(_ context.Context, companyID, userID int64, in MemberProfileInput) (*types.MemberProfile, error) {
	k := m.profileKey(companyID, userID)
	base := m.profiles[k]
	r := &types.MemberProfileRow{CompanyID: companyID, UserID: userID, UpdatedAt: time.Now().UTC()}
	if base != nil {
		r.RealName, r.IDCard, r.BankCard, r.ResumeURL, r.PortfolioURL = base.RealName, base.IDCard, base.BankCard, base.ResumeURL, base.PortfolioURL
	}
	if in.RealName != nil {
		r.RealName = *in.RealName
	}
	if in.IDCard != nil {
		r.IDCard = *in.IDCard
	}
	if in.BankCard != nil {
		r.BankCard = *in.BankCard
	}
	if in.ResumeURL != nil {
		r.ResumeURL = *in.ResumeURL
	}
	if in.PortfolioURL != nil {
		r.PortfolioURL = *in.PortfolioURL
	}
	m.profiles[k] = r
	return m.GetMemberProfile(context.Background(), companyID, userID)
}

// ==================== 会话 ====================

func (m *MemoryStore) CreateSession(_ context.Context, in CreateSessionInput) (*types.Session, error) {
	exp, _ := parseTimeFlex(in.ExpiresAt)
	id := m.nextId()
	now := time.Now().UTC()
	m.sessions[id] = &types.SessionRow{ID: id, UserID: in.UserID, TokenHash: in.TokenHash, Device: in.Device, IP: in.IP, Location: in.Location, ExpiresAt: exp, LastActiveAt: now, CreatedAt: now}
	r := m.sessions[id]
	ses := rowToSession(r)
	return &ses, nil
}

func (m *MemoryStore) GetSessionByTokenHash(_ context.Context, tokenHash string) (*types.Session, error) {
	for _, r := range m.sessions {
		if r.TokenHash == tokenHash {
			ses := rowToSession(r)
			return &ses, nil
		}
	}
	return nil, nil
}

func (m *MemoryStore) ListSessionsByUser(_ context.Context, userID int64) ([]types.Session, error) {
	out := []types.Session{}
	for _, r := range m.sessions {
		if r.UserID == userID {
			out = append(out, rowToSession(r))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out, nil
}

func (m *MemoryStore) GetUserActiveLocation(_ context.Context, userID int64) (string, error) {
	var first string
	for _, r := range m.sessions {
		if r.UserID == userID {
			if first == "" {
				first = r.Location
			}
			if r.ExpiresAt.After(time.Now().UTC()) {
				return r.Location, nil
			}
		}
	}
	return first, nil
}

func (m *MemoryStore) TouchSession(_ context.Context, id int64) error {
	if r, ok := m.sessions[id]; ok {
		r.LastActiveAt = time.Now().UTC()
	}
	return nil
}

func (m *MemoryStore) DeleteSession(_ context.Context, id int64) error {
	delete(m.sessions, id)
	return nil
}

func (m *MemoryStore) DeleteExpiredSessions(_ context.Context, now int64) (int64, error) {
	cut := time.UnixMilli(now).UTC()
	n := int64(0)
	for id, r := range m.sessions {
		if r.ExpiresAt.Before(cut) {
			delete(m.sessions, id)
			n++
		}
	}
	return n, nil
}

func (m *MemoryStore) GetSessionDays(_ context.Context, userID int64) (*int64, error) {
	if r, ok := m.users[userID]; ok {
		return r.SessionDays, nil
	}
	return nil, nil
}

func (m *MemoryStore) SetSessionDays(_ context.Context, userID int64, days *int64) error {
	if r, ok := m.users[userID]; ok {
		r.SessionDays = days
	}
	return nil
}

// ==================== 爱发电 ====================

func (m *MemoryStore) UpsertAfdianOrder(_ context.Context, in UpsertAfdianOrderInput) (*types.AfdianOrder, error) {
	for _, r := range m.afdianOrders {
		if r.OutTradeNo == in.OutTradeNo {
			r.CustomOrderID = in.CustomOrderID
			r.PlanID = in.PlanID
			r.PlanTitle = in.PlanTitle
			r.Title = in.Title
			r.Month = in.Month
			r.Amount = in.Amount
			r.ShowAmount = in.ShowAmount
			r.Status = in.Status
			r.Remark = in.Remark
			r.RedeemID = in.RedeemID
			r.ProductType = in.ProductType
			r.Discount = in.Discount
			r.BuyerUserID = in.BuyerUserID
			r.BuyerName = in.BuyerName
			r.BuyerAvatar = in.BuyerAvatar
			r.CreateTime = in.CreateTime
			r.SkuDetail = in.SkuDetail
			r.Raw = in.Raw
			o := rowToAfdian(r)
			return &o, nil
		}
	}
	id := m.nextId()
	m.afdianOrders[id] = &types.AfdianOrderRow{ID: id, OutTradeNo: in.OutTradeNo, CustomOrderID: in.CustomOrderID, PlanID: in.PlanID, PlanTitle: in.PlanTitle, Title: in.Title, Month: in.Month, Amount: in.Amount, ShowAmount: in.ShowAmount, Status: in.Status, Remark: in.Remark, RedeemID: in.RedeemID, ProductType: in.ProductType, Discount: in.Discount, BuyerUserID: in.BuyerUserID, BuyerName: in.BuyerName, BuyerAvatar: in.BuyerAvatar, CreateTime: in.CreateTime, SkuDetail: in.SkuDetail, Raw: in.Raw, CreatedAt: time.Now().UTC()}
	o := rowToAfdian(m.afdianOrders[id])
	return &o, nil
}

func (m *MemoryStore) GetAfdianOrderByOutTradeNo(_ context.Context, outTradeNo string) (*types.AfdianOrder, error) {
	for _, r := range m.afdianOrders {
		if r.OutTradeNo == outTradeNo {
			o := rowToAfdian(r)
			return &o, nil
		}
	}
	return nil, nil
}

func (m *MemoryStore) ListAfdianOrders(_ context.Context, limit, offset int64) ([]types.AfdianOrder, error) {
	ids := []int64{}
	for id := range m.afdianOrders {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] > ids[j] })
	out := []types.AfdianOrder{}
	for i, id := range ids {
		if int64(i) < offset {
			continue
		}
		if int64(len(out)) >= limit {
			break
		}
		out = append(out, rowToAfdian(m.afdianOrders[id]))
	}
	return out, nil
}

// ==================== 邮件配置 ====================

func (m *MemoryStore) ListMailConfigs(_ context.Context, scope MailConfigScope) ([]types.MailConfig, error) {
	out := []types.MailConfig{}
	for _, r := range m.mailCfgs {
		if scope.CompanyID == nil {
			if r.CompanyID != nil {
				continue
			}
		} else if r.CompanyID == nil || *r.CompanyID != *scope.CompanyID {
			continue
		}
		out = append(out, rowToMailConfig(r))
	}
	return out, nil
}

func (m *MemoryStore) GetMailConfig(_ context.Context, id int64) (*types.MailConfig, error) {
	if r, ok := m.mailCfgs[id]; ok {
		c := rowToMailConfig(r)
		return &c, nil
	}
	return nil, nil
}

func (m *MemoryStore) GetDefaultMailConfig(_ context.Context, scope MailConfigScope) (*types.MailConfig, error) {
	list, _ := m.ListMailConfigs(context.Background(), scope)
	var best *types.MailConfig
	for i := range list {
		if !list[i].Enabled {
			continue
		}
		if best == nil || list[i].IsDefault || list[i].Priority < best.Priority {
			cp := list[i]
			best = &cp
		}
	}
	return best, nil
}

func (m *MemoryStore) SaveMailConfig(_ context.Context, in SaveMailConfigInput) (*types.MailConfig, error) {
	if in.ID == nil || *in.ID == 0 {
		id := m.nextId()
		port := int64(465)
		if in.Port != nil {
			port = *in.Port
		}
		secure := true
		if in.Secure != nil {
			secure = *in.Secure
		}
		priority := int64(100)
		if in.Priority != nil {
			priority = *in.Priority
		}
		isDefault := false
		if in.IsDefault != nil {
			isDefault = *in.IsDefault
		}
		enabled := true
		if in.Enabled != nil {
			enabled = *in.Enabled
		}
		m.mailCfgs[id] = &types.MailConfigRow{ID: id, CompanyID: in.CompanyID, Email: in.Email, DisplayName: in.DisplayName, Host: in.Host, Port: port, Secure: secure, User: in.User, Password: in.Password, IsDefault: isDefault, Enabled: enabled, Priority: priority, CreatedAt: time.Now().UTC()}
		return m.GetMailConfig(context.Background(), id)
	}
	base, _ := m.GetMailConfig(context.Background(), *in.ID)
	if base == nil {
		return nil, &NotFoundError{Msg: "mail config not found"}
	}
	_ = base
	id := *in.ID
	r := m.mailCfgs[id]
	if in.Email != "" {
		r.Email = in.Email
	}
	if in.DisplayName != "" {
		r.DisplayName = in.DisplayName
	}
	if in.Host != "" {
		r.Host = in.Host
	}
	if in.Port != nil {
		r.Port = *in.Port
	}
	if in.Secure != nil {
		r.Secure = *in.Secure
	}
	if in.User != "" {
		r.User = in.User
	}
	if in.Password != "" {
		r.Password = in.Password
	}
	if in.IsDefault != nil {
		if *in.IsDefault {
			for _, o := range m.mailCfgs {
				if o.ID != id {
					same := (o.CompanyID == nil && r.CompanyID == nil) || (o.CompanyID != nil && r.CompanyID != nil && *o.CompanyID == *r.CompanyID)
					if same {
						o.IsDefault = false
					}
				}
			}
		}
		r.IsDefault = *in.IsDefault
	}
	if in.Enabled != nil {
		r.Enabled = *in.Enabled
	}
	if in.Priority != nil {
		r.Priority = *in.Priority
	}
	return m.GetMailConfig(context.Background(), id)
}

func (m *MemoryStore) DeleteMailConfig(_ context.Context, id int64) error {
	if _, ok := m.mailCfgs[id]; ok {
		delete(m.mailCfgs, id)
		return nil
	}
	return &NotFoundError{Msg: "mail config not found"}
}

// ==================== 假期 ====================

func (m *MemoryStore) ListHolidays(_ context.Context, year int64) ([]types.Holiday, error) {
	out := []types.Holiday{}
	for _, r := range m.holidays {
		if year > 0 && !strings.HasPrefix(r.Date, fmt.Sprintf("%d-", year)) {
			continue
		}
		out = append(out, types.Holiday{ID: r.ID, Date: r.Date, Name: r.Name, Type: r.Type, CreatedAt: toIso(r.CreatedAt)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	return out, nil
}

func (m *MemoryStore) SaveHoliday(_ context.Context, in SaveHolidayInput) (*types.Holiday, error) {
	typ := in.Type
	if typ == "" {
		typ = "legal"
	}
	if r, ok := m.holidays[in.Date]; ok {
		r.Name = in.Name
		r.Type = typ
		h := types.Holiday{ID: r.ID, Date: r.Date, Name: r.Name, Type: r.Type, CreatedAt: toIso(r.CreatedAt)}
		return &h, nil
	}
	id := m.nextId()
	m.holidays[in.Date] = &types.HolidayRow{ID: id, Date: in.Date, Name: in.Name, Type: typ, CreatedAt: time.Now().UTC()}
	r := m.holidays[in.Date]
	h := types.Holiday{ID: r.ID, Date: r.Date, Name: r.Name, Type: r.Type, CreatedAt: toIso(r.CreatedAt)}
	return &h, nil
}

func (m *MemoryStore) DeleteHoliday(_ context.Context, date string) error {
	delete(m.holidays, date)
	return nil
}
