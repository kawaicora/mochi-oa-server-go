// Package db 的 MySqlStore 方法：邮件配置、假期。
package db

import (
	"context"
	"database/sql"
	"errors"
	"strconv"

	"mochi-oa-server-go/internal/types"
)

// ==================== 邮件配置 ====================

// ListMailConfigs 列邮件配置（scope.CompanyID nil=主配置，否则公司私有）。
func (s *MySqlStore) ListMailConfigs(ctx context.Context, scope MailConfigScope) ([]types.MailConfig, error) {
	var rows *sql.Rows
	var err error
	if scope.CompanyID == nil {
		rows, err = s.db.QueryContext(ctx, `SELECT id, company_id, email, display_name, host, port, secure, user, password, is_default, enabled, priority, created_at FROM mail_configs WHERE company_id IS NULL ORDER BY priority ASC, id ASC`)
	} else {
		rows, err = s.db.QueryContext(ctx, `SELECT id, company_id, email, display_name, host, port, secure, user, password, is_default, enabled, priority, created_at FROM mail_configs WHERE company_id=? ORDER BY priority ASC, id ASC`, *scope.CompanyID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.MailConfig{}
	for rows.Next() {
		var r types.MailConfigRow
		if err := rows.Scan(&r.ID, &r.CompanyID, &r.Email, &r.DisplayName, &r.Host, &r.Port, &r.Secure, &r.User, &r.Password, &r.IsDefault, &r.Enabled, &r.Priority, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, rowToMailConfig(&r))
	}
	return out, rows.Err()
}

// GetMailConfig 按主键取。
func (s *MySqlStore) GetMailConfig(ctx context.Context, id int64) (*types.MailConfig, error) {
	var r types.MailConfigRow
	err := s.db.QueryRowContext(ctx, `SELECT id, company_id, email, display_name, host, port, secure, user, password, is_default, enabled, priority, created_at FROM mail_configs WHERE id=?`, id).
		Scan(&r.ID, &r.CompanyID, &r.Email, &r.DisplayName, &r.Host, &r.Port, &r.Secure, &r.User, &r.Password, &r.IsDefault, &r.Enabled, &r.Priority, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c := rowToMailConfig(&r)
	return &c, nil
}

// GetDefaultMailConfig 取默认配置（先按 is_default，回退同作用域 priority 最低）。
func (s *MySqlStore) GetDefaultMailConfig(ctx context.Context, scope MailConfigScope) (*types.MailConfig, error) {
	var rows *sql.Rows
	var err error
	if scope.CompanyID == nil {
		rows, err = s.db.QueryContext(ctx, `SELECT id, company_id, email, display_name, host, port, secure, user, password, is_default, enabled, priority, created_at FROM mail_configs WHERE company_id IS NULL AND enabled=true ORDER BY is_default DESC, priority ASC, id ASC LIMIT 1`)
	} else {
		rows, err = s.db.QueryContext(ctx, `SELECT id, company_id, email, display_name, host, port, secure, user, password, is_default, enabled, priority, created_at FROM mail_configs WHERE company_id=? AND enabled=true ORDER BY is_default DESC, priority ASC, id ASC LIMIT 1`, *scope.CompanyID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, nil
	}
	var r types.MailConfigRow
	if err := rows.Scan(&r.ID, &r.CompanyID, &r.Email, &r.DisplayName, &r.Host, &r.Port, &r.Secure, &r.User, &r.Password, &r.IsDefault, &r.Enabled, &r.Priority, &r.CreatedAt); err != nil {
		return nil, err
	}
	c := rowToMailConfig(&r)
	return &c, nil
}

// SaveMailConfig 新增（id=0）或更新邮件配置；更新时清空同作用域其他默认。
func (s *MySqlStore) SaveMailConfig(ctx context.Context, in SaveMailConfigInput) (*types.MailConfig, error) {
	if in.ID == nil || *in.ID == 0 {
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
		res, err := s.db.ExecContext(ctx,
			`INSERT INTO mail_configs (company_id, email, display_name, host, port, secure, user, password, is_default, enabled, priority, created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,NOW(3))`,
			in.CompanyID, in.Email, in.DisplayName, in.Host, port, secure, in.User, in.Password, boolPtr(in.IsDefault, false), boolPtr(in.Enabled, true), priority)
		if err != nil {
			return nil, err
		}
		id, _ := res.LastInsertId()
		return s.GetMailConfig(ctx, id)
	}
	base, err := s.GetMailConfig(ctx, *in.ID)
	if err != nil {
		return nil, err
	}
	if base == nil {
		return nil, &NotFoundError{Msg: "mail config not found"}
	}
	email := orStrPtr(in.Email, base.Email)
	display := orStrPtr(in.DisplayName, base.DisplayName)
	host := orStrPtr(in.Host, base.Host)
	port := base.Port
	if in.Port != nil {
		port = *in.Port
	}
	secure := base.Secure
	if in.Secure != nil {
		secure = *in.Secure
	}
	user := orStrPtr(in.User, base.User)
	password := orStrPtr(in.Password, base.Password)
	isDefault := base.IsDefault
	if in.IsDefault != nil {
		isDefault = *in.IsDefault
	}
	enabled := base.Enabled
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	priority := base.Priority
	if in.Priority != nil {
		priority = *in.Priority
	}
	if isDefault {
		if base.CompanyID != nil {
			_, _ = s.db.ExecContext(ctx, `UPDATE mail_configs SET is_default=false WHERE company_id=? AND id<>?`, *base.CompanyID, *in.ID)
		} else {
			_, _ = s.db.ExecContext(ctx, `UPDATE mail_configs SET is_default=false WHERE company_id IS NULL AND id<>?`, *in.ID)
		}
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE mail_configs SET email=?, display_name=?, host=?, port=?, secure=?, user=?, password=?, is_default=?, enabled=?, priority=? WHERE id=?`,
		email, display, host, port, secure, user, password, isDefault, enabled, priority, *in.ID)
	if err != nil {
		return nil, err
	}
	return s.GetMailConfig(ctx, *in.ID)
}

// DeleteMailConfig 删除配置。
func (s *MySqlStore) DeleteMailConfig(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM mail_configs WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{Msg: "mail config not found"}
	}
	return nil
}

// boolPtr 取可选 bool 的值（默认 dft）。
func boolPtr(p *bool, dft bool) bool {
	if p == nil {
		return dft
	}
	return *p
}

// orStrPtr 空串回退旧值。
func orStrPtr(v, dft string) string {
	if v == "" {
		return dft
	}
	return v
}

// ==================== 假期 ====================

// ListHolidays 列假期（year=0 全部；否则按 date LIKE 'YYYY-%'）。
func (s *MySqlStore) ListHolidays(ctx context.Context, year int64) ([]types.Holiday, error) {
	q := `SELECT id, date, name, type, created_at FROM holidays`
	args := []any{}
	if year > 0 {
		q += ` WHERE date LIKE ?`
		args = append(args, itoa(year)+"-%")
	}
	q += ` ORDER BY date ASC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.Holiday{}
	for rows.Next() {
		var r types.HolidayRow
		if err := rows.Scan(&r.ID, &r.Date, &r.Name, &r.Type, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, types.Holiday{ID: r.ID, Date: r.Date, Name: r.Name, Type: r.Type, CreatedAt: toIso(r.CreatedAt)})
	}
	return out, rows.Err()
}

// SaveHoliday upsert 假期（按 date 唯一）。
func (s *MySqlStore) SaveHoliday(ctx context.Context, in SaveHolidayInput) (*types.Holiday, error) {
	typ := in.Type
	if typ == "" {
		typ = "legal"
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO holidays (date, name, type, created_at) VALUES (?,?,?,NOW(3))
		 ON DUPLICATE KEY UPDATE name=VALUES(name), type=VALUES(type)`, in.Date, in.Name, typ)
	if err != nil {
		return nil, err
	}
	var r types.HolidayRow
	err = s.db.QueryRowContext(ctx, `SELECT id, date, name, type, created_at FROM holidays WHERE date=?`, in.Date).
		Scan(&r.ID, &r.Date, &r.Name, &r.Type, &r.CreatedAt)
	if err != nil {
		return nil, err
	}
	h := types.Holiday{ID: r.ID, Date: r.Date, Name: r.Name, Type: r.Type, CreatedAt: toIso(r.CreatedAt)}
	return &h, nil
}

// DeleteHoliday 按日期删除。
func (s *MySqlStore) DeleteHoliday(ctx context.Context, date string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM holidays WHERE date=?`, date)
	return err
}

// itoa 简易 int64 → string。
func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}
