// Package auth 是 socket 认证域 handler，1:1 移植自 server/src/handlers/auth.ts。
//
// 职责：
//   - 每次连接生成 RSA-2048 密钥对（公钥 SPKI PEM 经 auth:getPublicKey 下发，私钥仅内存随连接销毁），
//     桌面端用公钥加密登录/注册凭据（enc 字段），管理页走明文凭据——双通道都要兼容；
//   - 邮箱验证码（注册/验证码登录）、忘记密码三段流程（发重置码 → 校验码发重置令牌邮件）；
//   - 注册/登录建会话（多端）、auth:me 凭会话恢复、登出、会话列表、踢下线（sessionRoom 推送）；
//   - 修改密码（踢下线其他端）、设置登录有效期、更新个人资料（全员广播 user:profileUpdated）。
//
// 集成约定（socketapi.Conn 未暴露握手细节，集成中间件需在 connection 前写入以下连接态，缺省时行为与 TS 兜底一致）：
//   - "auth"            *types.Auth：认证中间件已写入（对应 socket.data.auth）；
//   - "handshakeHeaders" map[string]any：原始 HTTP 请求头（用于 realIp 解析 XFF 等）；
//   - "remoteAddr"       string：TCP 直连对端地址（realIp 最终兜底）；
//   - "handshakeDevice"  string：握手 auth.device（deviceLabel 第二来源，桌面端恒为 "MochiOA ..."）。
//
// 已知缺口（不在本包解决，见交付报告）：
//   - presence（addPresence/broadcastPresence）尚未移植为 Go 包，register/login 中的调用点留 hook 注释；
//   - mail.ts 的 serverAdminOverrides 运行时管理员集合属 admin handler 范围，本包 isServerAdmin 仅判 config.ServerAdmins。
package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"mochi-oa-server-go/internal/config"
	"mochi-oa-server-go/internal/db"
	"mochi-oa-server-go/internal/logger"
	"mochi-oa-server-go/internal/mailer"
	"mochi-oa-server-go/internal/password"
	"mochi-oa-server-go/internal/resettoken"
	"mochi-oa-server-go/internal/socketapi"
	"mochi-oa-server-go/internal/token"
	"mochi-oa-server-go/internal/types"
	"mochi-oa-server-go/internal/util"
	"mochi-oa-server-go/internal/verify"
)

const logMod = "handlers/auth"

// 连接态约定键（见文件头）。
const (
	keyAuth            = "auth"
	keyHandshakeHeaders = "handshakeHeaders"
	keyRemoteAddr      = "remoteAddr"
	keyHandshakeDevice = "handshakeDevice"
)

// RegisterAuth 注册全部认证事件。ctx 参数对应 TS 的 {io, store, config, mailer}。
// mailer 为 nil 时表示主邮件未配置（与 TS ctx.mailer undefined 等价）。
// 始终返回 nil（注册阶段无外部依赖可失败）；保留 error 返回值供集成阶段统一签名。
func RegisterAuth(s socketapi.Server, st db.Store, cfg *config.ServerConfig, ml *mailer.Mailer) error {
	h := &handler{s: s, store: st, cfg: cfg, mailer: ml}

	s.OnConnection(func(c socketapi.Conn) error {
		// 登录优化：每次连接生成 RSA-2048 密钥对（私钥仅内存、随连接销毁，前向安全）。
		priv, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			logger.Error(logMod, "RSA 密钥对生成失败", "conn", c.ID(), "error", err.Error())
			return nil
		}
		pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
		if err != nil {
			logger.Error(logMod, "RSA 公钥序列化失败", "conn", c.ID(), "error", err.Error())
			return nil
		}
		pubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
		h.keys.set(c.ID(), &rsaPair{priv: priv, pubPEM: pubPEM})
		return nil
	})
	s.OnDisconnect(func(c socketapi.Conn, _ string) {
		// 私钥随连接销毁：断线即删，不长期留存。
		h.keys.del(c.ID())
	})

	s.On("auth:getPublicKey", h.onGetPublicKey)
	s.On("auth:sendEmailCode", h.onSendEmailCode)
	s.On("auth:sendResetCode", h.onSendResetCode)
	s.On("auth:sendResetMail", h.onSendResetMail)
	s.On("auth:register", h.onRegister)
	s.On("auth:login", h.onLogin)
	s.On("auth:me", h.onMe)
	s.On("auth:logout", h.onLogout)
	s.On("auth:sessions", h.onSessions)
	s.On("auth:endSession", h.onEndSession)
	s.On("auth:endAllSessions", h.onEndAllSessions)
	s.On("auth:changePassword", h.onChangePassword)
	s.On("auth:setSessionDays", h.onSetSessionDays)
	s.On("user:updateProfile", h.onUpdateProfile)
	return nil
}

// handler 聚合一次注册所需的依赖（对标 TS registerAuthHandlers 闭包捕获的 ctx）。
type handler struct {
	s      socketapi.Server
	store  db.Store
	cfg    *config.ServerConfig
	mailer *mailer.Mailer
	keys   rsaKeyStore
}

// rsaPair 是单次连接的 RSA 凭据：私钥用于解密客户端 enc，公钥 PEM 下发给客户端加密用。
type rsaPair struct {
	priv   *rsa.PrivateKey
	pubPEM string
}

// rsaKeyStore 是 socketID → 密钥对的进程内表。
// TS 用裸 Map（单线程事件循环天然安全）；Go 并发处理连接，故加锁。
type rsaKeyStore struct {
	mu sync.RWMutex
	m  map[string]*rsaPair
}

func (rs *rsaKeyStore) set(id string, k *rsaPair) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.m == nil {
		rs.m = map[string]*rsaPair{}
	}
	rs.m[id] = k
}

func (rs *rsaKeyStore) get(id string) *rsaPair {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.m[id]
}

func (rs *rsaKeyStore) del(id string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	delete(rs.m, id)
}

// ==================== 入参解析小工具（对齐 TS 的 typeof 收敛写法） ====================

// dataMap 把事件负载解析为 map；null/空负载给空 map。
func dataMap(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return map[string]any{}
	}
	return m
}

// strField 取字符串字段（TS: typeof m[k] === 'string'）。ok=false 表示不是字符串。
func strField(m map[string]any, k string) (v string, ok bool) {
	v, ok = m[k].(string)
	return v, ok
}

// trimSlice 去空白后截断到 n 字符（JS slice 按 UTF-16；中文按 rune 截足够安全，此处与 TS 截断长度一致）。
func trimSlice(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}

// numField 取数字字段（TS: Number(m[k])；json 里 number 一律 float64）。
func numField(m map[string]any, k string) (float64, bool) {
	v, ok := m[k].(float64)
	return v, ok
}

// ==================== 校验规则（逐字移植 TS 正则/长度） ====================

var reUsername = regexp.MustCompile(`^[A-Za-z0-9_\-.]{3,32}$`)

func isUsername(v string) bool { return reUsername.MatchString(v) }
func isPassword(v string) bool  { return len(v) >= 6 && len(v) <= 128 }
func isNick(v string) bool      { return len(v) <= 32 }
func isEmail(v string) bool     { return len(v) <= 128 }

var reEmailStrict = regexp.MustCompile(`.+@.+\..+`)

// ==================== 认证态 ====================

// authed 取当前连接的认证用户（对标 TS socket.data.auth）。
func authed(c socketapi.Conn) *types.Auth {
	v := c.Get(keyAuth)
	if v == nil {
		return nil
	}
	switch a := v.(type) {
	case *types.Auth:
		return a
	case types.Auth:
		return &a
	}
	return nil
}

// setAuth 写回连接认证态（TS: socket.data.auth = {...}）。
func setAuth(c socketapi.Conn, a *types.Auth) { c.Set(keyAuth, a) }

// isServerAdmin 判定服务器所有者权限：用户名大小写不敏感匹配 config.ServerAdmins。
// （TS 另有 serverAdminOverrides 运行时集合，属 admin handler 移植范围，见文件头缺口说明。）
func isServerAdmin(a *types.Auth, cfg *config.ServerConfig) bool {
	if a == nil || cfg == nil {
		return false
	}
	n := strings.ToLower(a.Username)
	for _, u := range cfg.ServerAdmins {
		if strings.ToLower(u) == n {
			return true
		}
	}
	return false
}

// ==================== RSA 凭据解密 ====================

type creds struct {
	account  string
	username string
	password string
	code     string
}

// decryptCreds 用本连接私钥解密客户端 enc（base64(RSA-PKCS1v15(JSON))）。
// 对应 TS: privateDecrypt({ key, padding: RSA_PKCS1_PADDING }, Buffer.from(enc,'base64'))。
// 任何一步失败返回 nil（TS 也整体 catch 返回 null）。
func (h *handler) decryptCreds(socketID, enc string) *creds {
	k := h.keys.get(socketID)
	if k == nil {
		return nil
	}
	ciphertext, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return nil
	}
	plain, err := rsa.DecryptPKCS1v15(nil, k.priv, ciphertext)
	if err != nil {
		return nil
	}
	var obj struct {
		Account  any `json:"account"`
		Username any `json:"username"`
		Password any `json:"password"`
		Code     any `json:"code"`
	}
	if err := json.Unmarshal(plain, &obj); err != nil {
		return nil
	}
	out := &creds{}
	if s, ok := obj.Account.(string); ok {
		out.account = s
	}
	if s, ok := obj.Username.(string); ok {
		out.username = s
	}
	if s, ok := obj.Password.(string); ok {
		out.password = s
	}
	if s, ok := obj.Code.(string); ok {
		out.code = s
	}
	return out
}

// ==================== 会话/设备/IP 辅助 ====================

// parseDurationMs 把 "30d"/"12h" 这类串解析为毫秒；无法解析兜底 30 天。
// 逐字移植 TS parseDurationMs（支持 s/m/h/d/w 单字母后缀）。
func parseDurationMs(s string) int64 {
	m := reDuration.FindStringSubmatch(strings.ToLower(strings.TrimSpace(s)))
	if m == nil {
		return 30 * 86400000
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 30 * 86400000
	}
	switch m[2] {
	case "s":
		return n * 1000
	case "m":
		return n * 60000
	case "h":
		return n * 3600000
	case "d":
		return n * 86400000
	case "w":
		return n * 7 * 86400000
	}
	return 30 * 86400000
}

var reDuration = regexp.MustCompile(`^(\d+)([smhdw])$`)

// sessionExpiry 算会话过期时刻的 ISO 串：用户自设天数优先，否则服务端默认 jwtExpiresIn。
func (h *handler) sessionExpiry(ctx context.Context, userID int64) string {
	days, err := h.store.GetSessionDays(ctx, userID)
	if err == nil && days != nil && *days > 0 {
		return time.Now().AddDate(0, 0, int(*days)).UTC().Format("2006-01-02T15:04:05.000Z")
	}
	return time.Now().Add(time.Duration(parseDurationMs(h.cfg.JWTExpiresIn)) * time.Millisecond).UTC().Format("2006-01-02T15:04:05.000Z")
}

// deviceLabel 设备标识：登录参数 → 握手 auth.device → 兜底「未知设备」。
func (h *handler) deviceLabel(c socketapi.Conn, fromData any) string {
	if s, ok := fromData.(string); ok && strings.TrimSpace(s) != "" {
		return trimSlice(s, 120)
	}
	if v := c.Get(keyHandshakeDevice); v != nil {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			return trimSlice(s, 120)
		}
	}
	return "未知设备"
}

// clientInfo 是客户端随 clientInfo 上报的归属地/设备。
type clientInfo struct {
	ip       string
	location string
	device   string
}

// clientInfoFrom 解析 data.clientInfo（TS 同名函数，字段逐字一致）。
func clientInfoFrom(m map[string]any) clientInfo {
	ci := clientInfo{}
	raw, _ := m["clientInfo"].(map[string]any)
	if raw == nil {
		return ci
	}
	if v, ok := strField(raw, "ip"); ok && strings.TrimSpace(v) != "" {
		ci.ip = trimSlice(v, 64)
	}
	if v, ok := strField(raw, "location"); ok {
		ci.location = trimSlice(v, 120)
	}
	if v, ok := strField(raw, "device"); ok {
		ci.device = trimSlice(v, 120)
	}
	return ci
}

// headerGet 大小写不敏感地读请求头（HTTP 头名本来不区分大小写）。
func headerGet(m map[string]any, name string) string {
	n := strings.ToLower(name)
	for k, v := range m {
		if strings.ToLower(k) != n {
			continue
		}
		switch t := v.(type) {
		case string:
			return strings.TrimSpace(t)
		case []string:
			if len(t) > 0 {
				return strings.TrimSpace(t[0])
			}
		case []any:
			if len(t) > 0 {
				if s, ok := t[0].(string); ok {
					return strings.TrimSpace(s)
				}
			}
		}
	}
	return ""
}

// realIp 解析客户端真实 IP：CF-Connecting-IP → X-Forwarded-For 最左 → X-Real-IP/X-Client-IP → TCP 直连地址。
// 注：无条件信任代理头（与 TS 一致；生产反代场景适用）。
func (h *handler) realIp(c socketapi.Conn) string {
	headers, _ := c.Get(keyHandshakeHeaders).(map[string]any)
	first := func(s string) string {
		if i := strings.Index(s, ","); i >= 0 {
			s = s[:i]
		}
		return trimSlice(s, 64)
	}
	if headers != nil {
		if v := headerGet(headers, "cf-connecting-ip"); v != "" {
			return first(v)
		}
		if v := headerGet(headers, "x-forwarded-for"); v != "" {
			return first(v)
		}
		if v := headerGet(headers, "x-real-ip"); v != "" {
			return first(v)
		} else if v := headerGet(headers, "x-client-ip"); v != "" {
			return first(v)
		}
	}
	if v, ok := c.Get(keyRemoteAddr).(string); ok {
		return trimSlice(v, 64)
	}
	return ""
}

// resolveSessionIp 会话记录 IP：客户端自报 IP 非环回即采用（Docker/NAT 下服务端只见网关地址），否则回退 realIp。
func (h *handler) resolveSessionIp(c socketapi.Conn, clientIP string) string {
	cip := strings.ToLower(strings.TrimSpace(clientIP))
	if cip != "" && cip != "127.0.0.1" && cip != "::1" && cip != "localhost" {
		if len(cip) > 64 {
			cip = cip[:64]
		}
		return cip
	}
	return h.realIp(c)
}

// sessionView 会话对外视图（不含 tokenHash，逐字对齐 TS）。
func sessionView(s *types.Session) map[string]any {
	expired := false
	if t, err := time.Parse("2006-01-02T15:04:05.000Z", s.ExpiresAt); err == nil {
		expired = t.Before(time.Now())
	}
	return map[string]any{
		"id":           s.ID,
		"device":       s.Device,
		"ip":           s.IP,
		"location":     s.Location,
		"createdAt":    s.CreatedAt,
		"expiresAt":    s.ExpiresAt,
		"lastActiveAt": s.LastActiveAt,
		"expired":      expired,
	}
}

// userView 用户对外视图（TS loginPayload/auth:me 里手工挑的字段，不含 sessionDays/createdAt）。
func userView(u *types.User) map[string]any {
	return map[string]any{
		"id":       u.ID,
		"username": u.Username,
		"nick":     u.Nick,
		"avatar":   u.Avatar,
		"email":    u.Email,
		"phone":    u.Phone,
		"extra":    u.Extra,
	}
}

// loginPayload 登录成功载荷：user + companies + session。
func (h *handler) loginPayload(ctx context.Context, u *types.User, s *types.Session) (map[string]any, error) {
	companies, err := h.store.GetUserCompanies(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"user":      userView(u),
		"companies": companies,
		"session":   sessionView(s),
	}, nil
}

// findByAccount 按用户名或邮箱查用户（登录账号=用户名/邮箱；任一查库失败吞掉返回 nil，与 TS .catch(()=>null) 一致）。
func (h *handler) findByAccount(ctx context.Context, account string) (*types.UserWithPassword, error) {
	u, err := h.store.GetUserByUsername(ctx, strings.TrimSpace(account))
	if err == nil && u != nil {
		return u, nil
	}
	if strings.Contains(account, "@") {
		u, err = h.store.GetUserByEmail(ctx, strings.TrimSpace(account))
		if err == nil {
			return u, nil
		}
	}
	return nil, nil
}

// joinUserRooms 加入定向房间：userRoom（私信）+ sessionRoom（会话推送）+ 各群房间。
func (h *handler) joinUserRooms(ctx context.Context, c socketapi.Conn, a *types.Auth) {
	_ = c.Join(util.UserRoom(a.ID))
	_ = c.Join(util.SessionRoom(a.SessionID))
	groups, err := h.store.GetUserGroups(ctx, a.ID)
	if err != nil {
		logger.Warn(logMod, "拉取用户群列表失败，跳过群房间", "userID", a.ID, "error", err.Error())
		return
	}
	for _, g := range groups {
		_ = c.Join(util.GroupRoom(g.ID))
	}
}

// notifyOtherSessions 给该用户其他未过期会话推「新设备登录」弹窗（到 sessionRoom）。
func (h *handler) notifyOtherSessions(ctx context.Context, userID int64, newSessionID int64, device, ip, location string) {
	others, err := h.store.ListSessionsByUser(ctx, userID)
	if err != nil {
		logger.Warn(logMod, "拉取会话列表失败，跳过新设备通知", "userID", userID, "error", err.Error())
		return
	}
	at := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	for i := range others {
		o := &others[i]
		if o.ID == newSessionID {
			continue
		}
		if t, err := time.Parse("2006-01-02T15:04:05.000Z", o.ExpiresAt); err == nil && t.Before(time.Now()) {
			continue
		}
		_ = h.s.EmitTo(util.SessionRoom(o.ID), "auth:newDeviceLogin", map[string]any{
			"sessionId": o.ID,
			"device":    device,
			"ip":        ip,
			"location":  location,
			"at":        at,
		})
	}
}

// disconnectRoomSockets 强制断开某房间内全部连接（TS: io.in(room).disconnectSockets(true)）。
// socketapi 无现成 RoomBroadcastDisconnect，用 Sockets()+Rooms() 等价实现。
func (h *handler) disconnectRoomSockets(room string) {
	for _, c := range h.s.Sockets() {
		for _, r := range c.Rooms() {
			if r == room {
				c.Disconnect("session revoked")
				break
			}
		}
	}
}

// revokeSession 踢下线一个会话：删库 + sessionRoom 推 auth:sessionRevoked + 断开在线连接。
func (h *handler) revokeSession(ctx context.Context, sessionID int64, device, reason string) {
	if err := h.store.DeleteSession(ctx, sessionID); err != nil {
		logger.Warn(logMod, "删除会话失败", "sessionID", sessionID, "error", err.Error())
	}
	at := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	_ = h.s.EmitTo(util.SessionRoom(sessionID), "auth:sessionRevoked", map[string]any{
		"sessionId": sessionID,
		"reason":    reason,
		"device":    device,
		"at":        at,
	})
	h.disconnectRoomSockets(util.SessionRoom(sessionID))
}

// ==================== 事件处理 ====================

// onGetPublicKey 下发本连接 RSA 公钥（SPKI PEM，与 TS export({type:'spki',format:'pem'}) 一致）。
func (h *handler) onGetPublicKey(c socketapi.Conn, _ json.RawMessage, ack socketapi.Ack) {
	k := h.keys.get(c.ID())
	if k == nil {
		ack(util.Fail("密钥未就绪"))
		return
	}
	ack(util.OK(map[string]any{"publicKey": k.pubPEM}))
}

// onSendEmailCode 发送通用邮箱验证码（注册/验证码登录）。
func (h *handler) onSendEmailCode(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
	m := dataMap(raw)
	email, _ := strField(m, "email")
	email = strings.TrimSpace(email)
	if !isEmail(email) {
		ack(util.Fail("邮箱不合法"))
		return
	}
	purpose := "register"
	if p, ok := strField(m, "purpose"); ok && p != "" {
		purpose = p
	}
	if purpose != "register" && purpose != "login" {
		ack(util.Fail("不支持的验证码用途"))
		return
	}
	if h.mailer == nil {
		ack(util.Fail("主邮件未配置，无法发送验证码"))
		return
	}
	issued := verify.Default.Issue(purpose, email)
	if issued == nil || issued.Code == "" {
		left := int64(0)
		if issued != nil {
			left = issued.RemainingMs
		}
		secs := int(math.Max(1, math.Ceil(float64(left)/1000)))
		ack(util.Fail("发送太频繁，请 " + itoa(secs) + " 秒后重试"))
		return
	}
	ctx := context.Background()
	html := "<p>您好：</p><p>您的邮箱验证码是 <b style=\"font-size:20px\">" + issued.Code + "</b>，10 分钟内有效。</p><p>若非本人操作请忽略本邮件。</p>"
	if err := h.mailer.Send(ctx, email, "【Mochi OA】邮箱验证码", html); err != nil {
		logger.Error(logMod, "验证码邮件发送失败", "error", err.Error())
		ack(util.Fail("验证码发送失败：" + err.Error()))
		return
	}
	ack(util.OK(nil))
}

// onSendResetCode 忘记密码第一步：向绑定邮箱发重置验证码（purpose=reset）。
func (h *handler) onSendResetCode(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
	m := dataMap(raw)
	email, _ := strField(m, "email")
	email = strings.TrimSpace(email)
	if !isEmail(email) || !reEmailStrict.MatchString(email) {
		ack(util.Fail("邮箱格式不合法"))
		return
	}
	ctx := context.Background()
	user, err := h.store.GetUserByEmail(ctx, email)
	if err != nil || user == nil || user.Email == "" {
		ack(util.Fail("该邮箱未注册"))
		return
	}
	if h.mailer == nil {
		ack(util.Fail("主邮件未配置，无法发送验证码"))
		return
	}
	issued := verify.Default.Issue("reset", email)
	if issued == nil || issued.Code == "" {
		left := int64(0)
		if issued != nil {
			left = issued.RemainingMs
		}
		secs := int(math.Max(1, math.Ceil(float64(left)/1000)))
		ack(util.Fail("发送太频繁，请 " + itoa(secs) + " 秒后重试"))
		return
	}
	html := "<p>您好：</p><p>您的重置密码验证码是 <b style=\"font-size:20px\">" + issued.Code + "</b>，10 分钟内有效。</p><p>若非本人操作请忽略本邮件。</p>"
	if err := h.mailer.Send(ctx, email, "【Mochi OA】重置密码验证码", html); err != nil {
		logger.Error(logMod, "重置验证码邮件发送失败", "error", err.Error())
		ack(util.Fail("验证码发送失败：" + err.Error()))
		return
	}
	ack(util.OK(nil))
}

// onSendResetMail 忘记密码第二步：验证码通过 → 发一次性重置令牌邮件（含 /view/reset-pwd 链接）。
func (h *handler) onSendResetMail(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
	m := dataMap(raw)
	email, _ := strField(m, "email")
	email = strings.TrimSpace(email)
	code, _ := strField(m, "code")
	code = strings.TrimSpace(code)
	baseURL, _ := strField(m, "baseUrl")
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if !isEmail(email) || !reEmailStrict.MatchString(email) {
		ack(util.Fail("邮箱格式不合法"))
		return
	}
	if code == "" {
		ack(util.Fail("请输入验证码"))
		return
	}
	ctx := context.Background()
	user, err := h.store.GetUserByEmail(ctx, email)
	if err != nil || user == nil || user.Email == "" {
		ack(util.Fail("该邮箱未注册"))
		return
	}
	if h.mailer == nil {
		ack(util.Fail("主邮件未配置，无法发送重置邮件"))
		return
	}
	v := verify.Default.Verify("reset", email, code)
	if !v.OK {
		ack(util.Fail(v.Error))
		return
	}
	// baseUrl 非法（非 http(s)://host[:port]）退回相对路径。
	if !reBaseURL.MatchString(baseURL) {
		baseURL = ""
	}
	rt := resettoken.Issue(user.ID, user.Email, 0) // 0=默认 30 分钟，与 TS resetTokens.issue 一致
	link := "/view/reset-pwd?token=" + rt
	if baseURL != "" {
		link = baseURL + link
	}
	html := "<p>您好：</p><p>请点击下方链接重置密码（30 分钟内有效）：</p><p><a href=\"" + link + "\">" + link + "</a></p><p>若非本人操作请忽略本邮件。</p>"
	if err := h.mailer.Send(ctx, email, "【Mochi OA】重置密码", html); err != nil {
		logger.Error(logMod, "重置密码邮件发送失败", "error", err.Error())
		ack(util.Fail("重置邮件发送失败：" + err.Error()))
		return
	}
	ack(util.OK(nil))
}

var reBaseURL = regexp.MustCompile(`^https?://[\w.-]+(:\d+)?$`)

// onRegister 注册：校验 → 建用户 → 建会话 → 回 token。
func (h *handler) onRegister(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
	m := dataMap(raw)
	// 双通道：enc 存在则 RSA 解密覆盖 username/password。
	if enc, ok := strField(m, "enc"); ok && enc != "" {
		cre := h.decryptCreds(c.ID(), enc)
		if cre == nil || cre.username == "" || cre.password == "" {
			ack(util.Fail("注册凭据解密失败"))
			return
		}
		m["username"] = cre.username
		m["password"] = cre.password
	}
	username, _ := strField(m, "username")
	username = strings.TrimSpace(username)
	passwordStr, _ := strField(m, "password")
	if !isUsername(username) {
		ack(util.Fail("用户名不合法（3-32 位字母数字_-.）"))
		return
	}
	if !isPassword(passwordStr) {
		ack(util.Fail("密码长度需 6-128"))
		return
	}
	nick, hasNick := strField(m, "nick")
	if hasNick && !isNick(nick) {
		ack(util.Fail("昵称过长"))
		return
	}
	// 邮箱可选：填了必须通过 register 验证码。
	var email string
	if e, ok := strField(m, "email"); ok && strings.TrimSpace(e) != "" {
		email = strings.TrimSpace(e)
		if !isEmail(email) {
			ack(util.Fail("邮箱格式不合法"))
			return
		}
		code, _ := strField(m, "code")
		code = strings.TrimSpace(code)
		if code == "" {
			ack(util.Fail("请输入邮箱验证码"))
			return
		}
		v := verify.Default.Verify("register", email, code)
		if !v.OK {
			ack(util.Fail(v.Error))
			return
		}
	}
	avatar := ""
	if v, ok := strField(m, "avatar"); ok {
		avatar = trimSlice(v, 255)
	}
	phone := ""
	if v, ok := strField(m, "phone"); ok {
		phone = trimSlice(v, 32)
	}
	ctx := context.Background()
	hash, err := password.HashPassword(passwordStr)
	if err != nil {
		logger.Error(logMod, "密码散列失败", "error", err.Error())
		ack(util.Fail("注册失败"))
		return
	}
	nickVal := nick
	user, err := h.store.CreateUser(ctx, db.CreateUserInput{
		Username:     username,
		PasswordHash: hash,
		Nick:         nickVal,
		Email:        email,
		Avatar:       avatar,
		Phone:        phone,
	})
	if err != nil {
		var ae *db.AlreadyExistsError
		if errors.As(err, &ae) && ae.Msg == "username taken" {
			ack(util.Fail("用户名已被占用"))
			return
		}
		logger.Error(logMod, "创建用户失败", "error", err.Error())
		ack(util.Fail("注册失败"))
		return
	}
	// 注册也建一个会话（客户端随后登出会删掉它）。
	sessToken := token.NewSessionToken()
	ci := clientInfoFrom(m)
	device := h.deviceLabel(c, firstNonEmpty(ci.device, fieldAny(m, "device")))
	session, err := h.store.CreateSession(ctx, db.CreateSessionInput{
		UserID:    user.ID,
		TokenHash: token.HashToken(sessToken),
		Device:    device,
		IP:        h.resolveSessionIp(c, ci.ip),
		Location:  ci.location,
		ExpiresAt: h.sessionExpiry(ctx, user.ID),
	})
	if err != nil {
		logger.Error(logMod, "注册建会话失败", "userID", user.ID, "error", err.Error())
		ack(util.Fail("注册失败"))
		return
	}
	nickOut := user.Nick
	if nickOut == "" {
		nickOut = user.Username
	}
	setAuth(c, &types.Auth{ID: user.ID, Username: user.Username, Nick: nickOut, SessionID: session.ID, SessionDevice: session.Device})
	a := authed(c)
	h.joinUserRooms(ctx, c, a)
	// TODO(presence): addPresence(user.id, socket.id) + broadcastPresence(...) 待 presence 包移植后接入。

	payload, err := h.loginPayload(ctx, user, session)
	if err != nil {
		logger.Error(logMod, "注册组装载荷失败", "error", err.Error())
		ack(util.Fail("注册失败"))
		return
	}
	payload["token"] = sessToken
	ack(util.OK(payload))
}

// onLogin 登录（多会话：每次登录建新会话并通知其他会话）。
func (h *handler) onLogin(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
	m := dataMap(raw)
	account := ""
	if v, ok := strField(m, "account"); ok {
		account = strings.TrimSpace(v)
	} else if v, ok := strField(m, "username"); ok {
		account = strings.TrimSpace(v)
	}
	passwordStr, hasPassword := strField(m, "password")
	if !hasPassword {
		passwordStr = ""
	}
	code := ""
	if v, ok := strField(m, "code"); ok {
		code = strings.TrimSpace(v)
	}
	if enc, ok := strField(m, "enc"); ok && enc != "" {
		cre := h.decryptCreds(c.ID(), enc)
		if cre == nil || (cre.account == "" && cre.username == "") {
			ack(util.Fail("登录凭据解密失败"))
			return
		}
		account = strings.TrimSpace(firstNonEmpty(cre.account, cre.username))
		passwordStr = cre.password
		code = cre.code
	}
	if account == "" {
		ack(util.Fail("参数缺失"))
		return
	}
	ctx := context.Background()
	user, err := h.findByAccount(ctx, account)
	if err != nil {
		logger.Error(logMod, "查询用户失败", "error", err.Error())
		ack(util.Fail("用户名或密码错误"))
		return
	}
	if user == nil {
		ack(util.Fail("用户名或密码错误"))
		return
	}
	if code != "" {
		// 验证码登录：验证码发往该账号绑定邮箱。
		if user.Email == "" {
			ack(util.Fail("该账号未绑定邮箱，无法使用验证码登录"))
			return
		}
		v := verify.Default.Verify("login", user.Email, code)
		if !v.OK {
			ack(util.Fail(v.Error))
			return
		}
	} else {
		if passwordStr == "" || !password.VerifyPassword(passwordStr, user.PasswordHash) {
			ack(util.Fail("用户名或密码错误"))
			return
		}
	}
	sessToken := token.NewSessionToken()
	ci := clientInfoFrom(m)
	device := h.deviceLabel(c, firstNonEmpty(ci.device, fieldAny(m, "device")))
	ip := h.resolveSessionIp(c, ci.ip)
	session, err := h.store.CreateSession(ctx, db.CreateSessionInput{
		UserID:    user.ID,
		TokenHash: token.HashToken(sessToken),
		Device:    device,
		IP:        ip,
		Location:  ci.location,
		ExpiresAt: h.sessionExpiry(ctx, user.ID),
	})
	if err != nil {
		logger.Error(logMod, "登录建会话失败", "userID", user.ID, "error", err.Error())
		ack(util.Fail("登录失败"))
		return
	}
	nickOut := user.Nick
	if nickOut == "" {
		nickOut = user.Username
	}
	auth := &types.Auth{ID: user.ID, Username: user.Username, Nick: nickOut, SessionID: session.ID, SessionDevice: device}
	setAuth(c, auth)
	h.joinUserRooms(ctx, c, auth)
	// TODO(presence): addPresence + broadcastPresence 待 presence 包移植后接入。
	h.notifyOtherSessions(ctx, user.ID, session.ID, device, ip, ci.location)

	payload, err := h.loginPayload(ctx, &user.User, session)
	if err != nil {
		logger.Error(logMod, "登录组装载荷失败", "error", err.Error())
		ack(util.Fail("登录失败"))
		return
	}
	payload["token"] = sessToken
	payload["serverAdmin"] = isServerAdmin(auth, h.cfg)
	ack(util.OK(payload))
}

// onMe 凭已建会话恢复当前用户（重连幂等）。
func (h *handler) onMe(c socketapi.Conn, _ json.RawMessage, ack socketapi.Ack) {
	a := authed(c)
	if a == nil {
		ack(util.Fail("未登录"))
		return
	}
	ctx := context.Background()
	user, err := h.store.GetUserByID(ctx, a.ID)
	if err != nil || user == nil {
		ack(util.Fail("用户不存在"))
		return
	}
	if err := h.store.TouchSession(ctx, a.SessionID); err != nil {
		logger.Warn(logMod, "touchSession 失败", "sessionID", a.SessionID, "error", err.Error())
	}
	sessions, err := h.store.ListSessionsByUser(ctx, a.ID)
	if err != nil {
		logger.Error(logMod, "拉取会话列表失败", "userID", a.ID, "error", err.Error())
		ack(util.Fail("用户不存在"))
		return
	}
	var cur *map[string]any
	for i := range sessions {
		if sessions[i].ID == a.SessionID {
			sv := sessionView(&sessions[i])
			cur = &sv
			break
		}
	}
	companies, err := h.store.GetUserCompanies(ctx, a.ID)
	if err != nil {
		logger.Error(logMod, "拉取公司列表失败", "userID", a.ID, "error", err.Error())
		ack(util.Fail("用户不存在"))
		return
	}
	var sessionOut any
	if cur != nil {
		sessionOut = *cur
	} else {
		sessionOut = nil
	}
	ack(util.OK(map[string]any{
		"user":       userView(user),
		"companies":  companies,
		"session":    sessionOut,
		"serverAdmin": isServerAdmin(a, h.cfg),
	}))
}

// onLogout 删除当前会话（token 随即失效）。
func (h *handler) onLogout(c socketapi.Conn, _ json.RawMessage, ack socketapi.Ack) {
	a := authed(c)
	if a != nil && a.SessionID != 0 {
		if err := h.store.DeleteSession(context.Background(), a.SessionID); err != nil {
			logger.Warn(logMod, "登出删会话失败", "sessionID", a.SessionID, "error", err.Error())
		}
	}
	setAuth(c, nil)
	ack(util.OK(nil))
}

// onSessions 我的登录会话列表 + 登录有效期。
func (h *handler) onSessions(c socketapi.Conn, _ json.RawMessage, ack socketapi.Ack) {
	a := authed(c)
	if a == nil {
		ack(util.Fail("未登录"))
		return
	}
	ctx := context.Background()
	sessions, err := h.store.ListSessionsByUser(ctx, a.ID)
	if err != nil {
		logger.Error(logMod, "拉取会话列表失败", "userID", a.ID, "error", err.Error())
		ack(util.Fail("未登录"))
		return
	}
	views := make([]map[string]any, 0, len(sessions))
	for i := range sessions {
		views = append(views, sessionView(&sessions[i]))
	}
	days, _ := h.store.GetSessionDays(ctx, a.ID)
	ack(util.OK(map[string]any{
		"sessions":         views,
		"currentSessionId": a.SessionID,
		"sessionDays":      days,
	}))
}

// onEndSession 踢下线某个会话（不能踢当前会话）。
func (h *handler) onEndSession(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
	a := authed(c)
	if a == nil {
		ack(util.Fail("未登录"))
		return
	}
	m := dataMap(raw)
	n, ok := numField(m, "sessionId")
	if !ok || n != math.Trunc(n) || n <= 0 {
		ack(util.Fail("参数不合法"))
		return
	}
	sessionID := int64(n)
	ctx := context.Background()
	mine, err := h.store.ListSessionsByUser(ctx, a.ID)
	if err != nil {
		logger.Error(logMod, "拉取会话列表失败", "userID", a.ID, "error", err.Error())
		ack(util.Fail("会话不存在"))
		return
	}
	var target *types.Session
	for i := range mine {
		if mine[i].ID == sessionID {
			target = &mine[i]
			break
		}
	}
	if target == nil {
		ack(util.Fail("会话不存在"))
		return
	}
	if target.ID == a.SessionID {
		ack(util.Fail("不能退出当前会话"))
		return
	}
	h.revokeSession(ctx, sessionID, target.Device, "ended")
	ack(util.OK(nil))
}

// onEndAllSessions 退出所有其他设备（保留当前）。
func (h *handler) onEndAllSessions(c socketapi.Conn, _ json.RawMessage, ack socketapi.Ack) {
	a := authed(c)
	if a == nil {
		ack(util.Fail("未登录"))
		return
	}
	ctx := context.Background()
	mine, err := h.store.ListSessionsByUser(ctx, a.ID)
	if err != nil {
		logger.Error(logMod, "拉取会话列表失败", "userID", a.ID, "error", err.Error())
		ack(util.Fail("未登录"))
		return
	}
	n := 0
	for i := range mine {
		if mine[i].ID == a.SessionID {
			continue
		}
		h.revokeSession(ctx, mine[i].ID, mine[i].Device, "ended")
		n++
	}
	ack(util.OK(map[string]any{"ended": n}))
}

// onChangePassword 修改密码：校验旧密码 → 更新散列 → 踢下线其他所有会话。
func (h *handler) onChangePassword(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
	a := authed(c)
	if a == nil {
		ack(util.Fail("未登录"))
		return
	}
	m := dataMap(raw)
	oldPwd, hasOld := strField(m, "oldPassword")
	newPwd, hasNew := strField(m, "newPassword")
	if !hasOld || !hasNew {
		ack(util.Fail("参数缺失"))
		return
	}
	if !isPassword(newPwd) {
		ack(util.Fail("新密码长度需 6-128"))
		return
	}
	if oldPwd == newPwd {
		ack(util.Fail("新密码不能与旧密码相同"))
		return
	}
	ctx := context.Background()
	user, err := h.store.GetUserByUsername(ctx, a.Username)
	if err != nil || user == nil || !password.VerifyPassword(oldPwd, user.PasswordHash) {
		ack(util.Fail("旧密码错误"))
		return
	}
	hash, err := password.HashPassword(newPwd)
	if err != nil {
		logger.Error(logMod, "新密码散列失败", "error", err.Error())
		ack(util.Fail("新密码长度需 6-128"))
		return
	}
	if err := h.store.UpdateUserPassword(ctx, a.ID, hash); err != nil {
		logger.Error(logMod, "更新密码失败", "userID", a.ID, "error", err.Error())
		ack(util.Fail("旧密码错误"))
		return
	}
	// 其他会话全部失效（要求重新登录）；当前会话保留。
	mine, err := h.store.ListSessionsByUser(ctx, a.ID)
	if err != nil {
		logger.Error(logMod, "拉取会话列表失败", "userID", a.ID, "error", err.Error())
		ack(util.OK(nil))
		return
	}
	for i := range mine {
		if mine[i].ID == a.SessionID {
			continue
		}
		h.revokeSession(ctx, mine[i].ID, mine[i].Device, "password-changed")
	}
	ack(util.OK(nil))
}

// onSetSessionDays 设置我的登录有效期（天；0/空=用服务端默认）。
func (h *handler) onSetSessionDays(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
	a := authed(c)
	if a == nil {
		ack(util.Fail("未登录"))
		return
	}
	m := dataMap(raw)
	var days *int64
	if rawVal, present := m["days"]; present && rawVal != nil {
		if n, ok := rawVal.(float64); ok && n != 0 {
			if n != math.Trunc(n) || n < 1 || n > 365 {
				ack(util.Fail("登录有效期需在 1-365 天之间"))
				return
			}
			d := int64(n)
			days = &d
		}
	}
	if err := h.store.SetSessionDays(context.Background(), a.ID, days); err != nil {
		logger.Error(logMod, "设置登录有效期失败", "userID", a.ID, "error", err.Error())
	}
	ack(util.OK(map[string]any{"sessionDays": days}))
}

// onUpdateProfile 更新我的资料（昵称/头像/电话/紧急联系人），并全员广播 user:profileUpdated。
func (h *handler) onUpdateProfile(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
	a := authed(c)
	if a == nil {
		ack(util.Fail("未登录"))
		return
	}
	m := dataMap(raw)
	var nick, avatar, phone *string
	if v, ok := strField(m, "nick"); ok {
		s := trimSlice(v, 32)
		nick = &s
	}
	if v, ok := strField(m, "avatar"); ok {
		s := trimSlice(v, 255)
		avatar = &s
	}
	if v, ok := strField(m, "phone"); ok {
		s := trimSlice(v, 32)
		phone = &s
	}
	// 拓展信息：紧急联系人（最多 3 组，序列化为 JSON 存储）。
	var extra *string
	if rawExtra, present := m["extra"]; present && rawExtra != nil {
		if obj, ok := rawExtra.(map[string]any); ok {
			rawList, _ := obj["emergency"].([]any)
			if len(rawList) > 3 {
				rawList = rawList[:3]
			}
			emergency := make([]map[string]string, 0, len(rawList))
			for _, e := range rawList {
				it, _ := e.(map[string]any)
				name, _ := it["name"].(string)
				tel, _ := it["phone"].(string)
				if len(name) > 32 {
					name = name[:32]
				}
				if len(tel) > 32 {
					tel = tel[:32]
				}
				emergency = append(emergency, map[string]string{"name": name, "phone": tel})
			}
			b, _ := json.Marshal(map[string]any{"emergency": emergency})
			s := string(b)
			extra = &s
		}
	}
	if nick == nil && avatar == nil && phone == nil && extra == nil {
		ack(util.Fail("没有可更新的字段"))
		return
	}
	ctx := context.Background()
	if err := h.store.UpdateUserProfile(ctx, a.ID, db.UserProfilePatch{
		Nick: nick, Avatar: avatar, Phone: phone, Extra: extra,
	}); err != nil {
		logger.Error(logMod, "更新资料失败", "userID", a.ID, "error", err.Error())
		ack(util.Fail("没有可更新的字段"))
		return
	}
	user, err := h.store.GetUserByID(ctx, a.ID)
	if err != nil || user == nil {
		ack(util.Fail("用户不存在"))
		return
	}
	// 同步当前 socket 显示名。
	nickOut := user.Nick
	if nickOut == "" {
		nickOut = user.Username
	}
	a.Nick = nickOut
	setAuth(c, a)
	// 广播给所有在线端（好友/公司成员/会话显示同步刷新）。
	_ = h.s.Broadcast("user:profileUpdated", map[string]any{"user": userView(user)})
	ack(util.OK(map[string]any{"user": userView(user)}))
}

// ==================== 小工具 ====================

// fieldAny 取任意类型字段（用于 device 这类容忍 any 的入参）。
func fieldAny(m map[string]any, k string) any {
	return m[k]
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// itoa 整数字符串（避免 strconv 在错误路径多包一层 import 噪音）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	buf := [20]byte{}
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// itoa 整数字符串（避免 strconv 在错误路径多包一层 import 噪音）。
