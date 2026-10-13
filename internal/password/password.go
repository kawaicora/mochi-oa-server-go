// Package password 负责密码散列与校验，移植自源仓库 server/src/password.ts。
//
// 算法：scrypt（Go 标准库无内置，用 golang.org/x/crypto/scrypt）。
// 存储格式："scrypt:{saltHex}:{hashHex}"，盐 16 字节随机，派生密钥 64 字节，
// 与 Node 的 scryptSync(password, salt, 64) 参数对齐（N=16384,r=8,p=1）。
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"strings"

	"golang.org/x/crypto/scrypt"
)

const keyLen = 64

// HashPassword 生成密码散列：随机 16 字节盐，scrypt 派生 64 字节，返回 "scrypt:{salt}:{hash}"。
func HashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash, err := scrypt.Key([]byte(pw), salt, 16384, 8, 1, keyLen)
	if err != nil {
		return "", err
	}
	return "scrypt:" + hex.EncodeToString(salt) + ":" + hex.EncodeToString(hash), nil
}

// VerifyPassword 校验密码是否匹配已存储的散列；常量时间比较防时序攻击。
func VerifyPassword(pw, stored string) bool {
	parts := strings.Split(stored, ":")
	if len(parts) != 3 || parts[0] != "scrypt" {
		return false
	}
	salt, err := hex.DecodeString(parts[1])
	if err != nil {
		return false
	}
	expected, err := hex.DecodeString(parts[2])
	if err != nil {
		return false
	}
	candidate, err := scrypt.Key([]byte(pw), salt, 16384, 8, 1, keyLen)
	if err != nil {
		return false
	}
	if len(candidate) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare(candidate, expected) == 1
}
