// Package logger 是全项目唯一的日志出口。
//
// 设计要点（对标源码 [mochioa-server] 风格）：
//   - 包级单例（sync.Once 初始化），全局只有一份，不允许重复初始化；
//   - 输出格式：2026-10-13T12:00:00.000Z [模块] msg key=value；
//   - 全项目禁止散落 fmt.Println，一律走本包；
//   - 报错必须打日志、错误向上返回，绝不吞错误；
//   - 敏感值（密码/token/app_secret/会话哈希）一律打码或省略，绝不落日志。
package logger

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// 日志级别常量（用于模块内对齐，避免散落魔法字符串）。
const (
	levelInfo  = "INFO"
	levelWarn  = "WARN"
	levelError = "ERROR"
)

// RedactKeys 是约定俗成的敏感键名集合。
// 调用方在打 kv 日志时，若键命中这里（大小写不敏感），值会被自动打码。
// 注意：本集合只覆盖「按 kv 传入」的场景；直接拼进 msg 的敏感串需调用方自行脱敏。
var redactKeys = map[string]struct{}{
	"password":    {},
	"passwordhash": {},
	"token":       {},
	"tokensecret": {},
	"jwtsecret":   {},
	"appsecret":    {},
	"app_secret":  {},
	"secret":      {},
	"sessionhash": {},
	"hash":        {},
	"credential":  {},
	"privatekey":  {},
}

// Redact 是脱敏约定：调用方遇到敏感键时，应显式传 Redact(k, v)，
// 使日志输出为 key=***（而不是原值）。本函数本身只返回占位串，便于在非 kv 场景复用。
func Redact(k, v string) string {
	_ = k // 键名仅用于调用方自文档化；值统一打码
	return "***"
}

// entry 是内部日志器实例（目前只写 stdout，预留后续接文件/远端）。
var (
	once     sync.Once
	instance *logger
)

type logger struct {
	mu sync.Mutex // 保证多行日志不交错
}

// initLogger 懒初始化单例。
func initLogger() {
	once.Do(func() {
		instance = &logger{}
	})
}

// formatKV 把可变 kv 列表拍平成 "k1=v1 k2=v2"。
// kv 必须成对出现（key, value, key, value...）；落单的键会被忽略。
// 命中敏感键的值一律打码为 ***。
func formatKV(kv []any) string {
	out := ""
	for i := 0; i+1 < len(kv); i += 2 {
		key, ok := kv[i].(string)
		if !ok {
			continue
		}
		val := kv[i+1]
		if isRedacted(key) {
			val = "***"
		}
		if out != "" {
			out += " "
		}
		out += fmt.Sprintf("%s=%v", key, val)
	}
	return out
}

// isRedacted 判断键是否命中敏感集合（大小写不敏感，去掉下划线/连字符后比较）。
func isRedacted(key string) bool {
	norm := make([]rune, 0, len(key))
	for _, r := range key {
		switch {
		case r >= 'A' && r <= 'Z':
			norm = append(norm, r+('a'-'A'))
		case r == '_' || r == '-':
			// 忽略分隔符
		default:
			norm = append(norm, r)
		}
	}
	_, hit := redactKeys[string(norm)]
	return hit
}

// emit 是唯一的底层输出函数：拼时间戳+级别+模块前缀+消息+kv。
func emit(level, module, msg string, kv []any) {
	initLogger()
	ts := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	line := fmt.Sprintf("%s [%s] [%s] %s", ts, level, module, msg)
	if extra := formatKV(kv); extra != "" {
		line += " " + extra
	}
	instance.mu.Lock()
	defer instance.mu.Unlock()
	// Error 走 stderr，其余走 stdout，便于重定向区分。
	if level == levelError {
		fmt.Fprintln(os.Stderr, line)
	} else {
		fmt.Fprintln(os.Stdout, line)
	}
}

// Info 打一条 INFO。module 如 "config"/"db"，kv 成对（key,value...）。
func Info(module, msg string, kv ...any) {
	emit(levelInfo, module, msg, kv)
}

// Warn 打一条 WARN。
func Warn(module, msg string, kv ...any) {
	emit(levelWarn, module, msg, kv)
}

// Error 打一条 ERROR。错误必须打日志、再由调用方返回，绝不吞掉。
func Error(module, msg string, kv ...any) {
	emit(levelError, module, msg, kv)
}
