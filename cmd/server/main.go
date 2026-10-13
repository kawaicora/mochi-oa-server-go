// Package main 是服务端入口占位。
//
// 基础层阶段：仅加载 config、打开 store 单例、打一条占位日志，正常退出。
// 真实的 HTTP/socket 接线由后续集成子任务完成（本文件不做实际监听）。
package main

import (
	"context"

	"mochi-oa-server-go/internal/config"
	"mochi-oa-server-go/internal/db"
	"mochi-oa-server-go/internal/logger"
)

func main() {
	ctx := context.Background()

	// 1. 加载配置（单例）
	cfg := config.Load()

	// 2. 打开存储（单例；memory 不依赖 MySQL，mysql 仅 Ping 不建表）
	if _, err := db.Open(ctx, cfg); err != nil {
		logger.Error("main", "存储初始化失败", "error", err.Error())
	}

	logger.Info("mochioa-server", "基础层占位完成")
}
