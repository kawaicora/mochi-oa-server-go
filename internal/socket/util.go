package socket

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
)

// base64Encode 把二进制包成 EIO4 polling 用的 base64 文本（不带前缀，前缀由调用方加 'b'）。
func base64Encode(data []byte) []byte {
	out := make([]byte, base64.StdEncoding.EncodedLen(len(data)))
	base64.StdEncoding.Encode(out, data)
	return out
}

// base64Decode 解码 EIO4 polling 的 base64 二进制包。
func base64Decode(data []byte) ([]byte, error) {
	return base64.StdEncoding.DecodeString(string(data))
}

// newSID 生成 engine.io sid（16 字节随机 hex，与 node 端随机字符串量级一致）。
func newSID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
