// Package db 的单例入口：根据 config.Storage 打开 MySQL 或 Memory 存储。
// 全局只有一份（sync.Once），绝不重复连接/重复初始化。
package db

import (
	"context"
	"sync"

	"mochi-oa-server-go/internal/config"
	"mochi-oa-server-go/internal/logger"
)

var (
	once sync.Once
	store Store
	err  error
)

// Open 打开并初始化全局 Store 单例。重复调用返回首次结果。
func Open(ctx context.Context, cfg *config.ServerConfig) (Store, error) {
	once.Do(func() {
		if cfg.Storage == "memory" {
			logger.Info("db", "使用 MemoryStore（开发/测试）")
			store = NewMemoryStore()
			err = store.Init(ctx)
			return
		}
		logger.Info("db", "使用 MySqlStore")
		var ms *MySqlStore
		ms, err = NewMySqlStore(cfg.MySQL)
		if err != nil {
			logger.Error("db", "MySQL 连接池创建失败", "error", err.Error())
			return
		}
		store = ms
		err = store.Init(ctx)
	})
	return store, err
}

// GetStore 返回已打开的全局 Store（须先 Open）。
func GetStore() Store {
	return store
}
