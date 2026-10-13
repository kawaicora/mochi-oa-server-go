// Package util 提供跨模块的小工具：加入码生成、房间名约定、统一应答构造器。
// 移植自源仓库 server/src/util.ts，保持 handler 侧调用方式 1:1。
package util

import (
	"math/rand"
	"fmt"
)

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
