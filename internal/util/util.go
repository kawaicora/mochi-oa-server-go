// Package util 提供跨模块的小工具：加入码生成、房间名约定、统一应答构造器。
// 移植自源仓库 server/src/util.ts，保持 handler 侧调用方式 1:1。
package util

import (
	"fmt"
	"math/rand"
	"os"
	"time"
)

// LocalTZ 对外返回时间的业务本地时区：统一输出带偏移格式、无 Z 后缀，避免客户端按 UTC 显示。
// 可用 TIMEZONE 环境变量覆盖，缺省 Asia/Shanghai。存储层仍为 UTC，此处仅展示层转换。
var LocalTZ = mustLocalTZ()

func mustLocalTZ() *time.Location {
	name := os.Getenv("TIMEZONE")
	if name == "" {
		name = "Asia/Shanghai"
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		loc = time.Local
	}
	return loc
}

// LocalTimeFormat 把 time.Time 转为业务本地时区完整时间字符串（空格分隔、无毫秒、无时区）。
// 全局统一输出格式：2026-10-28 11:17:23。零值返回空串。
func LocalTimeFormat(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(LocalTZ).Format("2006-01-02 15:04:05")
}

// ParseLocalTime 解析完整时间字符串，兼容服务端输出的空格格式与客户端旧 ISO(RFC3339Nano/RFC3339) 及分钟级格式。
// 无时区后缀的按业务本地时区解析；非法返回 (zero, false)。
func ParseLocalTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02T15:04",    // DateTimeInput(time) 输出：2026-10-28T21:00
		"2006-01-02T15:04:05", // T 分隔带秒
	} {
		if t, err := time.ParseInLocation(layout, s, LocalTZ); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// joinCodeChars 是加入码字符集（去掉易混淆的 I/O/0/1，对齐源码）。
const joinCodeChars = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// MakeJoinCode 生成 8 位加入码（大写字母数字，去易混淆字符）。
func MakeJoinCode() string {
	b := make([]byte, 8)
	for i := range b {
		b[i] = joinCodeChars[rand.Intn(len(joinCodeChars))]
	}
	return string(b)
}

// CompanyRoom 公司房间名。
func CompanyRoom(companyID int64) string { return fmt.Sprintf("company:%d", companyID) }

// GroupRoom 群房间名。
func GroupRoom(groupID int64) string { return fmt.Sprintf("group:%d", groupID) }

// UserRoom 用户房间名（在线时加入，私信/离线广播定向送达用）。
func UserRoom(userID int64) string { return fmt.Sprintf("user:%d", userID) }

// SessionRoom 会话房间名（一个登录会话对应一个房间，便于踢下线/新设备登录定向推送）。
func SessionRoom(sessionID int64) string { return fmt.Sprintf("session:%d", sessionID) }

// OK 构造成功应答：{ ok: true, ...data }。data 可为空。
func OK(data map[string]any) map[string]any {
	out := map[string]any{"ok": true}
	for k, v := range data {
		out[k] = v
	}
	return out
}

// Fail 构造失败应答：{ ok: false, error }。
func Fail(errMsg string) map[string]any {
	return map[string]any{"ok": false, "error": errMsg}
}
