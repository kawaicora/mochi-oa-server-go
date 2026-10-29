// SERVER_ADMIN 唯一判定入口（需求 8/9/10）。
//
// 全仓库只有这一个判定函数（grep isServerAdmin 无重复实现）：判据 =
// 环境种子 config.ServerAdmins（SERVER_ADMIN_USERS，空回退 ADMIN_USER）∪ server_admins 表，
// 命中其一即管理员；表为权威（后台 admin:setServerAdmin 写表、重启保留）；
// 与公司/群角色无关；用户名大小写不敏感。
package admin

import (
	"context"
	"strings"

	"mochi-oa-server-go/internal/config"
	"mochi-oa-server-go/internal/db"
	"mochi-oa-server-go/internal/types"
)

// IsServerAdminByUsername 判定用户名是否为 SERVER_ADMIN（唯一入口）。
// store 允许为 nil：nil 时只判环境种子（不查表，用于尚未持 store 的极早期场景）。
func IsServerAdminByUsername(username string, cfg *config.ServerConfig, store db.Store) bool {
	n := strings.ToLower(strings.TrimSpace(username))
	if n == "" {
		return false
	}
	// 1) 环境种子（config.ServerAdmins；大小写不敏感）
	if cfg != nil {
		for _, u := range cfg.ServerAdmins {
			if strings.ToLower(u) == n {
				return true
			}
		}
	}
	// 2) server_admins 表（持久化，重启保留）
	if store != nil {
		if rows, err := store.ListServerAdmins(context.Background()); err == nil {
			for _, r := range rows {
				if r.Username == n {
					return true
				}
			}
		}
	}
	return false
}

// IsServerAdmin 包装 Auth 身份走 IsServerAdminByUsername。
func IsServerAdmin(a *types.Auth, cfg *config.ServerConfig, store db.Store) bool {
	if a == nil {
		return false
	}
	return IsServerAdminByUsername(a.Username, cfg, store)
}
