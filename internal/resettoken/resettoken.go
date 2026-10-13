// Package resettoken 是一次性密码重置令牌，移植自源仓库 server/src/reset-tokens.ts。
//
// 只负责令牌的「生成、存储、校验、过期、一次性消费」，不绑定发送通道。
// 忘记密码流程：邮箱验证码通过后 issue 一个令牌，把含 /view/reset-pwd?token= 的链接发到邮箱；
// GET 渲染表单时 peek（不消费），POST 提交新密码时 consume（一次性）。
// 进程内 Map（单实例足够）；重启失效，需持久化时换 Redis/DB。
package resettoken

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

const defaultTTL = 30 * 60 * 1000 // 30 分钟

type entry struct {
	userID    int64
	email     string
	expiresAt int64
}

// TokenInfo 是 peek/consume 解析出的令牌归属。
type TokenInfo struct {
	UserID int64  `json:"userId"`
	Email  string `json:"email"`
}

var (
	mu    sync.Mutex
	store = map[string]*entry{}
)

// Issue 生成一次性重置令牌并缓存，返回令牌（拼进链接发给用户）。
// ttlMs<=0 时用默认 30 分钟。
func Issue(userID int64, email string, ttlMs int64) string {
	if ttlMs <= 0 {
		ttlMs = defaultTTL
	}
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	mu.Lock()
	store[tok] = &entry{userID: userID, email: email, expiresAt: time.Now().UnixMilli() + ttlMs}
	sweepLocked()
	mu.Unlock()
	return tok
}

// Peek 仅校验令牌有效性（渲染表单页用，不消费，可重复调用直到提交）。
func Peek(tok string) *TokenInfo {
	mu.Lock()
	defer mu.Unlock()
	e, ok := store[tok]
	if !ok {
		return nil
	}
	if time.Now().UnixMilli() > e.expiresAt {
		delete(store, tok)
		return nil
	}
	return &TokenInfo{UserID: e.userID, Email: e.email}
}

// Consume 校验并消费（一次性，POST 改密时调用，成功即删除）。
func Consume(tok string) *TokenInfo {
	mu.Lock()
	defer mu.Unlock()
	e, ok := store[tok]
	if !ok {
		return nil
	}
	if time.Now().UnixMilli() > e.expiresAt {
		delete(store, tok)
		return nil
	}
	delete(store, tok)
	return &TokenInfo{UserID: e.userID, Email: e.email}
}

// Sweep 清理过期令牌（issue 时也会触发）。
func Sweep() {
	mu.Lock()
	defer mu.Unlock()
	sweepLocked()
}

func sweepLocked() {
	now := time.Now().UnixMilli()
	for k, e := range store {
		if now > e.expiresAt {
			delete(store, k)
		}
	}
}
