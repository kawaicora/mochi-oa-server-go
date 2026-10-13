// Package verify 是通用验证码管理器，移植自源仓库 server/src/verify-code.ts。
//
// 只负责验证码的「生成、存储、校验、冷却、过期」，不绑定发送通道（邮件/短信等）。
// 存储为进程内 Map（单实例足够）；多实例部署可换 Redis/DB，接口不变。
package verify

import (
	"math/rand"
	"strings"
	"sync"
	"time"
)

// Options 是验证码管理器配置（对应 VerifyCodeOptions）。
type Options struct {
	TTLMs        int64 // 有效期毫秒，默认 10 分钟
	CooldownMs   int64 // 重发冷却毫秒，默认 60 秒
	MaxAttempts  int   // 最大尝试次数，超过作废，默认 5
	CodeLen      int   // 验证码长度，默认 6
	Digits       bool  // 是否纯数字，默认 true
}

// IssueResult 是 issue 的返回。
type IssueResult struct {
	Code        string `json:"code"`
	RemainingMs int64  `json:"remainingMs"`
}

// VerifyResult 是 verify 的返回。
type VerifyResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

type entry struct {
	code      string
	expiresAt int64
	attempts  int
}

const codeChars = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // 去 I/O/0/1

// Manager 是验证码管理器。
type Manager struct {
	ttlMs       int64
	cooldownMs  int64
	maxAttempts int
	codeLen     int
	digits      bool
	mu          sync.Mutex
	entries     map[string]*entry
	cooldowns   map[string]int64
}

// NewManager 构造管理器；opts 为 nil 时用默认值。
func NewManager(opts *Options) *Manager {
	o := Options{}
	if opts != nil {
		o = *opts
	}
	dft := func(v, d int64) int64 {
		if v == 0 {
			return d
		}
		return v
	}
	if o.MaxAttempts == 0 {
		o.MaxAttempts = 5
	}
	if o.CodeLen == 0 {
		o.CodeLen = 6
	}
	return &Manager{
		ttlMs:       dft(o.TTLMs, 10*60*1000),
		cooldownMs:  dft(o.CooldownMs, 60*1000),
		maxAttempts: o.MaxAttempts,
		codeLen:     o.CodeLen,
		digits:      o.Digits,
		entries:     map[string]*entry{},
		cooldowns:   map[string]int64{},
	}
}

// key 拼验证码存储键（purpose:to，to 归一化小写去空格）。
func key(purpose, to string) string {
	return purpose + ":" + strings.ToLower(strings.TrimSpace(to))
}

// makeCode 生成验证码。
func (m *Manager) makeCode() string {
	if m.digits {
		max := 1
		for i := 0; i < m.codeLen; i++ {
			max *= 10
		}
		n := rand.Intn(max)
		return padLeftZero(n, m.codeLen)
	}
	b := make([]byte, m.codeLen)
	for i := range b {
		b[i] = codeChars[rand.Intn(len(codeChars))]
	}
	return string(b)
}

// padLeftZero 把数字补零到定长。
func padLeftZero(n, width int) string {
	s := ""
	for i := 0; i < width; i++ {
		s = "0" + s
	}
	out := []rune(s)
	digits := []rune{}
	for n > 0 {
		digits = append([]rune{rune('0' + n%10)}, digits...)
		n /= 10
	}
	copy(out[len(out)-len(digits):], digits)
	return string(out)
}

// Issue 生成验证码并缓存；冷却期内返回空 code 与剩余冷却毫秒。
func (m *Manager) Issue(purpose, to string) *IssueResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := key(purpose, to)
	now := time.Now().UnixMilli()
	if last, ok := m.cooldowns[k]; ok && now < last+m.cooldownMs {
		return &IssueResult{Code: "", RemainingMs: last + m.cooldownMs - now}
	}
	code := m.makeCode()
	m.entries[k] = &entry{code: code, expiresAt: now + m.ttlMs, attempts: 0}
	m.cooldowns[k] = now
	m.sweepLocked()
	return &IssueResult{Code: code, RemainingMs: 0}
}

// Verify 校验并消费（一次性）。成功即删除；失败累计次数，超限作废。
func (m *Manager) Verify(purpose, to, input string) VerifyResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := key(purpose, to)
	e, ok := m.entries[k]
	if !ok {
		return VerifyResult{OK: false, Error: "验证码不存在或已过期"}
	}
	now := time.Now().UnixMilli()
	if now > e.expiresAt {
		delete(m.entries, k)
		return VerifyResult{OK: false, Error: "验证码已过期"}
	}
	value := strings.TrimSpace(input)
	if e.attempts >= m.maxAttempts {
		delete(m.entries, k)
		return VerifyResult{OK: false, Error: "尝试次数过多，请重新获取验证码"}
	}
	if e.code != value {
		e.attempts++
		if e.attempts >= m.maxAttempts {
			delete(m.entries, k)
			return VerifyResult{OK: false, Error: "尝试次数过多，请重新获取验证码"}
		}
		return VerifyResult{OK: false, Error: "验证码错误"}
	}
	delete(m.entries, k)
	return VerifyResult{OK: true}
}

// Revoke 主动作废（如邮箱绑定成功后）。
func (m *Manager) Revoke(purpose, to string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, key(purpose, to))
}

// Sweep 清理过期记录（防内存膨胀）。
func (m *Manager) Sweep() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked()
}

func (m *Manager) sweepLocked() {
	now := time.Now().UnixMilli()
	for k, e := range m.entries {
		if now > e.expiresAt {
			delete(m.entries, k)
		}
	}
	for k, t := range m.cooldowns {
		if now > t+m.cooldownMs {
			delete(m.cooldowns, k)
		}
	}
}

// Default 是默认单例（对应源码 export const verifyCodes = new VerifyCodeManager()）。
var Default = NewManager(nil)
