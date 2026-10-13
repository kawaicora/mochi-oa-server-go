// Package db 的 MySqlStore 方法：邮件配置、假期。
//
// 本文件全部走 GORM（s.gorm），不再出现裸 SQL 字面量；表名一律用 Table("...") 显式指定。
package db

import (
	"context"
	"database/sql"
	"strconv"
	"time"

	"gorm.io/gorm/clause"

	"mochi-oa-server-go/internal/types"
)

// ==================== 邮件配置 ====================

// ListMailConfigs 列邮件配置（scope.CompanyID nil=主配置，否则公司私有）。
func (s *MySqlStore) ListMailConfigs(ctx context.Context, scope MailConfigScope) ([]types.MailConfig, error) {
	q := s.gorm.WithContext(ctx).Table("mail_configs").
		Select("id, company_id, email, display_name, host, port, secure, user, password, is_default, enabled, priority, created_at")
	if scope.CompanyID == nil {
		q = q.Where("company_id IS NULL")
	} else {
		q = q.Where("company_id=?", *scope.CompanyID)
	}
	rows := []types.MailConfigRow{}
	if err := q.Order("priority ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := []types.MailConfig{}
	for i := range rows {
		out = append(out, rowToMailConfig(&rows[i]))
	}
	return out, nil
}

// GetMailConfig 按主键取。
func (s *MySqlStore) GetMailConfig(ctx context.Context, id int64) (*types.MailConfig, error) {
	var r types.MailConfigRow
	err := s.gorm.WithContext(ctx).Table("mail_configs").
		Select("id, company_id, email, display_name, host, port, secure, user, password, is_default, enabled, priority, created_at").
		Where("id=?", id).
		Take(&r).Error
	if gormNotFound(err) {
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
	q := s.gorm.WithContext(ctx).Table("mail_configs").
		Select("id, company_id, email, display_name, host, port, secure, user, password, is_default, enabled, priority, created_at")
	if scope.CompanyID == nil {
		q = q.Where("company_id IS NULL AND enabled=true")
	} else {
		q = q.Where("company_id=? AND enabled=true", *scope.CompanyID)
	}
	var r types.MailConfigRow
	err := q.Order("is_default DESC, priority ASC, id ASC").Limit(1).Take(&r).Error
	if gormNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c := rowToMailConfig(&r)
	return &c, nil
}

// mailConfigInsertRow 只用于插入：列清单与原 INSERT 一致，且不带任何 default 标签，
// 保证 secure/enabled 等显式 false 不会被 GORM 当成「未设置」而替换为库默认值。
type mailConfigInsertRow struct {
	ID          int64     `gorm:"column:id;primaryKey;autoIncrement"`
	CompanyID   *int64    `gorm:"column:company_id"`
	Email       string    `gorm:"column:email"`
	DisplayName string    `gorm:"column:display_name"`
	Host        string    `gorm:"column:host"`
	Port        int64     `gorm:"column:port"`
	Secure      bool      `gorm:"column:secure"`
	User        string    `gorm:"column:user"`
	Password    string    `gorm:"column:password"`
	IsDefault   bool      `gorm:"column:is_default"`
	Enabled     bool      `gorm:"column:enabled"`
	Priority    int64     `gorm:"column:priority"`
	CreatedAt   time.Time `gorm:"column:created_at"`
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
		// 用不带 default 标签的局部插入结构体 Create：
		// MailConfigRow 上的 `default:...` 是给 AutoMigrate 生成 DDL 用的，而 GORM 在
		// 结构体 Create 时会把「零值」替换成声明的默认值——那会让显式传入的
		// secure=false / enabled=false 被悄悄改成 true。局部结构体没有默认值标签，
		// 所有列都按实参写入，语义与原 SQL 完全一致，同时还能回填自增主键。
		row := mailConfigInsertRow{
			CompanyID:   in.CompanyID,
			Email:       in.Email,
			DisplayName: in.DisplayName,
			Host:        in.Host,
			Port:        port,
			Secure:      secure,
			User:        in.User,
			Password:    in.Password,
			IsDefault:   boolPtr(in.IsDefault, false),
			Enabled:     boolPtr(in.Enabled, true),
			Priority:    priority,
			CreatedAt:   time.Now().UTC(),
		}
		if err := s.gorm.WithContext(ctx).Table("mail_configs").Create(&row).Error; err != nil {
			return nil, err
		}
		return s.GetMailConfig(ctx, row.ID)
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
		// 原实现忽略该清理语句的错误，这里同样忽略。
		if base.CompanyID != nil {
			_ = s.gorm.WithContext(ctx).Table("mail_configs").
				Where("company_id=? AND id<>?", *base.CompanyID, *in.ID).
				Updates(map[string]any{"is_default": false}).Error
		} else {
			_ = s.gorm.WithContext(ctx).Table("mail_configs").
				Where("company_id IS NULL AND id<>?", *in.ID).
				Updates(map[string]any{"is_default": false}).Error
		}
	}
	// 全列部分更新：map 更新不会跳过零值，且不会自动写 updated_at（与原 SQL 一致）。
	err = s.gorm.WithContext(ctx).Table("mail_configs").
		Where("id=?", *in.ID).
		Updates(map[string]any{
			"email":        email,
			"display_name": display,
			"host":         host,
			"port":         port,
			"secure":       secure,
			"user":         user,
			"password":     password,
			"is_default":   isDefault,
			"enabled":      enabled,
			"priority":     priority,
		}).Error
	if err != nil {
		return nil, err
	}
	return s.GetMailConfig(ctx, *in.ID)
}

// DeleteMailConfig 删除配置。
func (s *MySqlStore) DeleteMailConfig(ctx context.Context, id int64) error {
	res := s.gorm.WithContext(ctx).Table("mail_configs").Where("id=?", id).Delete(nil)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
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
	q := s.gorm.WithContext(ctx).Table("holidays").
		Select("id, date, name, type, created_at")
	if year > 0 {
		q = q.Where("date LIKE ?", itoa(year)+"-%")
	}
	rows := []types.HolidayRow{}
	if err := q.Order("date ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := []types.Holiday{}
	for i := range rows {
		r := &rows[i]
		out = append(out, types.Holiday{ID: r.ID, Date: r.Date, Name: r.Name, Type: r.Type, CreatedAt: toIso(r.CreatedAt)})
	}
	return out, nil
}

// SaveHoliday upsert 假期（按 date 唯一）。
func (s *MySqlStore) SaveHoliday(ctx context.Context, in SaveHolidayInput) (*types.Holiday, error) {
	typ := in.Type
	if typ == "" {
		typ = "legal"
	}
	// 对应原 SQL 的 ON DUPLICATE KEY UPDATE name=VALUES(name), type=VALUES(type)：
	// 冲突时只覆盖 name/type，created_at 仅在插入时写入（显式 NOW(3) 等价物）。
	err := s.gorm.WithContext(ctx).Table("holidays").
		Clauses(clause.OnConflict{DoUpdates: clause.AssignmentColumns([]string{"name", "type"})}).
		Create(map[string]any{
			"date":       in.Date,
			"name":       in.Name,
			"type":       typ,
			"created_at": time.Now().UTC(),
		}).Error
	if err != nil {
		return nil, err
	}
	var r types.HolidayRow
	err = s.gorm.WithContext(ctx).Table("holidays").
		Select("id, date, name, type, created_at").
		Where("date=?", in.Date).
		Take(&r).Error
	if err != nil {
		if gormNotFound(err) {
			// 原实现此处由 QueryRowContext 直接返回 database/sql 的 ErrNoRows，保持一致。
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	h := types.Holiday{ID: r.ID, Date: r.Date, Name: r.Name, Type: r.Type, CreatedAt: toIso(r.CreatedAt)}
	return &h, nil
}

// DeleteHoliday 按日期删除。
func (s *MySqlStore) DeleteHoliday(ctx context.Context, date string) error {
	// 原实现不检查影响行数，有错才返回错误。
	return s.gorm.WithContext(ctx).Table("holidays").Where("date=?", date).Delete(nil).Error
}

// itoa 简易 int64 → string。
func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}
