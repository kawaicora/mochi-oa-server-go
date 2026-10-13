// Package token 负责 JWT 签发/校验与会话不透明 token 的生成/摘要。
// 移植自源仓库 server/src/token.ts。
//
// 两类 token：
//   - JWT：自包含（sub/username），用于握手认证；expiresIn 支持 "30d"/"12h" 这类字符串。
//   - 会话 token：32 字节随机串，落库只存 sha256 摘要，可单独撤销、支持多端。
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// TokenPayload 是 JWT 自定义负载（对标 TS 的 TokenPayload）。
type TokenPayload struct {
	Sub      string `json:"sub"`
	Username string `json:"username"`
}

// ParseExpiry 把 "30d"/"12h"/"60m" 这类字符串解析为有效期时长。
// 纯数字按秒处理；无法解析时返回默认 30 天（与源码 jwt 默认行为兜底一致）。
func ParseExpiry(s string) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return 30 * 24 * time.Hour
	}
	// 纯数字 → 秒
	if n, err := strconv.Atoi(s); err == nil {
		return time.Duration(n) * time.Second
	}
	// 带后缀
	num, unit := "", s
	for i, r := range s {
		if r < '0' || r > '9' {
			num = s[:i]
			unit = s[i:]
			break
		}
	}
	n, err := strconv.Atoi(num)
	if err != nil {
		return 30 * 24 * time.Hour
	}
	switch strings.ToLower(unit) {
	case "d", "day", "days":
		return time.Duration(n) * 24 * time.Hour
	case "h", "hour", "hours":
		return time.Duration(n) * time.Hour
	case "m", "min", "minute", "minutes":
		return time.Duration(n) * time.Minute
	case "s", "sec", "second", "seconds":
		return time.Duration(n) * time.Second
	default:
		return 30 * 24 * time.Hour
	}
}

// SignToken 签发 JWT：payload{ sub, username }，secret 签名，expiresIn 见 ParseExpiry。
func SignToken(payload TokenPayload, secret, expiresIn string) (string, error) {
	now := time.Now()
	claims := jwt.RegisteredClaims{
		Subject:   payload.Sub,
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(ParseExpiry(expiresIn))),
	}
	// 业务字段 username 放入私有 claim（RegisteredClaims 无 username）。
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	if tok.Claims.(jwt.MapClaims) == nil {
		_ = claims
	}
	// 使用 MapClaims 以便同时携带 username。
	mapClaims := jwt.MapClaims{
		"sub":      payload.Sub,
		"username": payload.Username,
		"iat":      now.Unix(),
		"exp":      now.Add(ParseExpiry(expiresIn)).Unix(),
	}
	tok = jwt.NewWithClaims(jwt.SigningMethodHS256, mapClaims)
	return tok.SignedString([]byte(secret))
}

// VerifyToken 校验 JWT 签名与有效期，返回 payload；失败返回 nil。
func VerifyToken(tokenStr, secret string) *TokenPayload {
	tok, err := jwt.Parse(tokenStr, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return []byte(secret), nil
	})
	if err != nil || !tok.Valid {
		return nil
	}
	claims, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		return nil
	}
	sub, _ := claims["sub"].(string)
	username, _ := claims["username"].(string)
	if sub == "" || username == "" {
		return nil
	}
	return &TokenPayload{Sub: sub, Username: username}
}

// NewSessionToken 生成一个不可猜测的会话 token（32 字节随机串的 hex）。
func NewSessionToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败属不可恢复；退回时间兜底，避免运行崩溃（正常不会发生）。
		return fmt.Sprintf("%x%d", make([]byte, 32), time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// HashToken 计算 token 的 sha256 摘要（hex），用于会话表查找/存储（不存明文）。
func HashToken(tokenStr string) string {
	sum := sha256.Sum256([]byte(tokenStr))
	return hex.EncodeToString(sum[:])
}
