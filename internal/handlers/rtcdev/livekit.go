package rtcdev

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"mochi-oa-server-go/internal/config"
	"mochi-oa-server-go/internal/logger"
)

// livekit.go —— LiveKit SFU 访问 token 签发。
//
// 设计约束（服务端统一签发、客户端不持有密钥）：
//   - LIVEKIT_URL / LIVEKIT_API_SECRET 同时配置才启用（见 config.liveKitConfig）。
//   - apiKey/apiSecret 只存在于服务端 .env，用于按 LiveKit JWT 规范签发 HS256 access token。
//   - token 必须包含 canPublishData:true，否则 LiveKit data 消息（远程键鼠控制）不可用。
//
// 依赖全部来自标准库，不引入外部 LiveKit SDK（签发协议是公开的 JWT 结构）。

// liveKitTokenTTL 是签发的 token 有效期（秒）。进入/查看按需签发，2 小时足够一次会话。
const liveKitTokenTTL = int64(2 * 60 * 60)

// liveKitVideoGrants 是 token 内嵌的 video.grants（对房间权限的授予）。
type liveKitVideoGrants struct {
	Room           string `json:"room"`
	RoomJoin       bool   `json:"roomJoin"`
	CanPublish     bool   `json:"canPublish"`
	CanSubscribe   bool   `json:"canSubscribe"`
	CanPublishData bool   `json:"canPublishData"`
}

// b64urlText 把字符串按 base64url（无 padding）编码，JWT 段用。
func b64urlText(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

// b64urlBytes 把字节按 base64url（无 padding）编码，签名段用。
func b64urlBytes(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// signLiveKitToken 按 LiveKit JWT 规范（HS256）签发访问 token。
// identity 为该参与者在房间内的身份（当前用 userId 字符串）；room 为 LiveKit 房间名（已带前缀）。
// lk 为 nil 或密钥缺失时返回错误。
func signLiveKitToken(lk *config.LiveKitConfig, identity, room string) (string, error) {
	if lk == nil || lk.APIKey == "" || lk.APISecret == "" {
		return "", fmt.Errorf("LiveKit 未配置（缺少 LIVEKIT_API_KEY/API_SECRET）")
	}
	now := time.Now().Unix()
	header := b64urlText(`{"alg":"HS256","typ":"JWT"}`)
	payload := liveKitTokenTTLPayload(lk, identity, room, now)
	payloadEnc := b64urlText(payload)
	data := header + "." + payloadEnc

	// HMAC-SHA256 签名
	mac := hmac.New(sha256.New, []byte(lk.APISecret))
	mac.Write([]byte(data))
	sig := b64urlBytes(mac.Sum(nil))
	return data + "." + sig, nil
}

// liveKitTokenTTLPayload 构造 token 的 payload JSON（结构对齐客户端旧实现的 HS256 JWT）。
func liveKitTokenTTLPayload(lk *config.LiveKitConfig, identity, room string, now int64) string {
	video := liveKitVideoGrants{
		Room:           room,
		RoomJoin:       true,
		CanPublish:     true,
		CanSubscribe:   true,
		CanPublishData: true,
	}
	// 用 map 便于 JSON 序列化且键序无关紧要（签名按序列化后字节计算）
	obj := map[string]any{
		"iss": lk.APIKey,
		"sub": identity,
		"nbf": now - 10,
		"exp": now + liveKitTokenTTL,
		"video": map[string]any{
			"room":           video.Room,
			"roomJoin":       video.RoomJoin,
			"canPublish":     video.CanPublish,
			"canSubscribe":   video.CanSubscribe,
			"canPublishData": video.CanPublishData,
		},
	}
	b, err := json.Marshal(obj)
	if err != nil {
		// 该结构可序列化，走到这里属于异常；返回空串由调用方当作失败处理
		return ""
	}
	return string(b)
}

// liveKitRoomName 业务房间标识 → LiveKit 房间名（带前缀，避免与其他业务房间重名）。
// 服务端与客户端统一使用同一前缀逻辑。
func liveKitRoomName(lk *config.LiveKitConfig, roomID string) string {
	prefix := "oa"
	if lk != nil && lk.RoomPrefix != "" {
		prefix = lk.RoomPrefix
	}
	return prefix + "-" + roomID
}

// liveKitEnabled 判定 SFU 可用（token 签发前置条件）。
func liveKitEnabled(lk *config.LiveKitConfig) bool {
	return lk != nil && lk.URL != "" && lk.APIKey != "" && lk.APISecret != ""
}

// joinSfuFields 为某用户在指定房间签发 SFU 连接信息（url/token/roomName）。
// LiveKit 未启用或签发失败时返回空串（客户端视为 SFU 不可用，走旧逻辑）；成功时调用方追加到 ack 响应。
func joinSfuFields(cfg *config.ServerConfig, roomID string, userID int64) (url, token, roomName string) {
	lk := cfg.LiveKit
	if !liveKitEnabled(lk) {
		return "", "", ""
	}
	roomName = liveKitRoomName(lk, roomID)
	tok, err := signLiveKitToken(lk, strconv.FormatInt(userID, 10), roomName)
	if err != nil {
		logger.Warn(module, "签发 LiveKit token 失败", "user", userID, "room", roomID, "error", err)
		return "", "", ""
	}
	return lk.URL, tok, roomName
}
