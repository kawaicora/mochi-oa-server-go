// server_admins 表存取：SERVER_ADMIN 持久化（需求 9）。
//
// 判定语义：SERVER_ADMIN = 环境种子 config.ServerAdmins ∪ 本表；本表为权威，
// 后台 admin:setServerAdmin 增删写表，重启保留。用户名小写存储、判定不区分大小写。
package db

import (
	"context"
	"strings"
	"time"

	"mochi-oa-server-go/internal/types"
)

// ListServerAdmins 返回全部 SERVER_ADMIN（按用户名升序）。
func (s *MySqlStore) ListServerAdmins(ctx context.Context) ([]types.ServerAdminRow, error) {
	var rows []types.ServerAdminRow
	err := s.gorm.WithContext(ctx).Order("username ASC").Find(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// UpsertServerAdmin 把某用户设为 SERVER_ADMIN（已存在则仅更新时间）。
// username 统一小写存储；createdBy 记录创建人用户 id（首启播种为 0）。
func (s *MySqlStore) UpsertServerAdmin(ctx context.Context, username string, createdBy int64) error {
	name := strings.ToLower(strings.TrimSpace(username))
	if name == "" {
		return nil
	}
	now := time.Now()
	var row types.ServerAdminRow
	err := s.gorm.WithContext(ctx).Where("username = ?", name).First(&row).Error
	if err == nil {
		// 已存在：更新 updated_at（幂等，不重复建）。
		return s.gorm.WithContext(ctx).Model(&types.ServerAdminRow{}).
			Where("username = ?", name).Update("updated_at", now).Error
	}
	// 不存在：插入。
	return s.gorm.WithContext(ctx).Create(&types.ServerAdminRow{
		Username:  name,
		CreatedBy: createdBy,
		CreatedAt: now,
		UpdatedAt: now,
	}).Error
}

// DeleteServerAdmin 取消某用户的 SERVER_ADMIN（环境种子不在表内，删表即取消）。
func (s *MySqlStore) DeleteServerAdmin(ctx context.Context, username string) error {
	name := strings.ToLower(strings.TrimSpace(username))
	if name == "" {
		return nil
	}
	return s.gorm.WithContext(ctx).Where("username = ?", name).Delete(&types.ServerAdminRow{}).Error
}
