// Package socket 实现与 socket.io v4（EIO4）1:1 兼容的服务端，供 Go 版 OA 使用。
//
// 本包是对冻结接口 internal/socketapi 的唯一实现。协议细节见 packet.go 顶部注释。
package socket

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"mochi-oa-server-go/internal/config"
	"mochi-oa-server-go/internal/logger"
	"mochi-oa-server-go/internal/socketapi"
)

// Server 是 socket.io v4 服务端（默认命名空间 "/"）。完整实现 socketapi.Server。
type Server struct {
	cfg   *config.ServerConfig
	path  string // config.Path，默认 /socket.io
	cors  corsPolicy
	upgrader websocketUpgrader

	mu         sync.RWMutex
	handlers   map[string]socketapi.HandlerFunc
	binHandlers map[string]socketapi.BinaryHandlerFunc
	middlewares []socketapi.MiddlewareFunc
	onConnect  func(socketapi.Conn) error
	onDisconnect func(socketapi.Conn, string)

	sessions map[string]*Conn            // sid -> conn
	rooms    map[string]map[string]*Conn  // room -> sid -> conn
}

// 编译期断言：*Server 实现 socketapi.Server。
var _ socketapi.Server = (*Server)(nil)

// New 创建 socket 服务端。cfg 为全局配置；读取 Path/Transports/CORS/TrustProxy。
// 返回的 *Server 同时是一个 http.Handler，由集成层挂到自己的 mux 上。
func New(cfg *config.ServerConfig) (*Server, error) {
	path := cfg.Path
	if path == "" {
		path = "/socket.io"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	s := &Server{
		cfg:      cfg,
		path:     path,
		cors:     newCORSPolicy(cfg.CORSStar, cfg.CORSList),
		handlers: map[string]socketapi.HandlerFunc{},
		binHandlers: map[string]socketapi.BinaryHandlerFunc{},
		sessions: map[string]*Conn{},
		rooms:    map[string]map[string]*Conn{},
	}
	s.upgrader = newUpgrader(cfg, path)
	logger.Info("socket", "server created", "path", path,
		"transports", strings.Join(cfg.Transports, ","), "cors_star", cfg.CORSStar)
	return s, nil
}

// ─── socketapi.Server：注册接口 ──────────────────────────────────────────────

// Use 注册连接级中间件（对标 io.use）。在握手 CONNECT 后、OnConnection 前按序执行。
func (s *Server) Use(mw socketapi.MiddlewareFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.middlewares = append(s.middlewares, mw)
}

// On 注册一个 JSON 事件处理器。
func (s *Server) On(event string, h socketapi.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[event] = h
}

// OnBinary 注册一个二进制事件处理器。
func (s *Server) OnBinary(event string, h socketapi.BinaryHandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.binHandlers[event] = h
}

// OnConnection 注册连接建立钩子（认证放行后调用）。
func (s *Server) OnConnection(h func(socketapi.Conn) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onConnect = h
}

// OnDisconnect 注册断开钩子。
func (s *Server) OnDisconnect(h func(socketapi.Conn, string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onDisconnect = h
}

// ─── socketapi.Server：广播接口 ──────────────────────────────────────────────

// EmitTo 向房间内所有连接发事件（io.to(room).emit）。
func (s *Server) EmitTo(room, event string, data any) error {
	s.mu.RLock()
	conns := snapshotRoom(s.rooms[room])
	s.mu.RUnlock()
	for _, c := range conns {
		if err := c.Emit(event, data); err != nil {
			logger.Warn("socket", "emit to room failed", "room", room, "err", err)
		}
	}
	return nil
}

// EmitToSkip 向房间内发事件但跳过某个连接（socket.to(room).emit，即"广播给房间里其他人"）。
func (s *Server) EmitToSkip(room, event string, data any, skipConnID string) error {
	s.mu.RLock()
	conns := snapshotRoom(s.rooms[room])
	s.mu.RUnlock()
	for _, c := range conns {
		if c.id == skipConnID {
			continue
		}
		if err := c.Emit(event, data); err != nil {
			logger.Warn("socket", "emit to room(skip) failed", "room", room, "err", err)
		}
	}
	return nil
}

// Broadcast 向所有连接发事件（io.emit）。
func (s *Server) Broadcast(event string, data any) error {
	s.mu.RLock()
	all := make([]*Conn, 0, len(s.sessions))
	for _, c := range s.sessions {
		all = append(all, c)
	}
	s.mu.RUnlock()
	for _, c := range all {
		if err := c.Emit(event, data); err != nil {
			logger.Warn("socket", "broadcast failed", "err", err)
		}
	}
	return nil
}

// Sockets 返回当前所有连接。
func (s *Server) Sockets() []socketapi.Conn {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]socketapi.Conn, 0, len(s.sessions))
	for _, c := range s.sessions {
		out = append(out, c)
	}
	return out
}

// Close 关闭服务端与所有连接。
func (s *Server) Close() error {
	s.mu.RLock()
	all := make([]*Conn, 0, len(s.sessions))
	for _, c := range s.sessions {
		all = append(all, c)
	}
	s.mu.RUnlock()
	for _, c := range all {
		c.shutdown("server close")
	}
	logger.Info("socket", "server closed")
	return nil
}

// ─── 房间与会话表内部操作 ─────────────────────────────────────────────────────

func (s *Server) roomJoin(room string, c *Conn) {
	s.mu.Lock()
	if s.rooms[room] == nil {
		s.rooms[room] = map[string]*Conn{}
	}
	s.rooms[room][c.id] = c
	s.mu.Unlock()
}

func (s *Server) roomLeave(room string, c *Conn) {
	s.mu.Lock()
	if m, ok := s.rooms[room]; ok {
		delete(m, c.id)
		if len(m) == 0 {
			delete(s.rooms, room)
		}
	}
	s.mu.Unlock()
}

func (s *Server) addSession(c *Conn) {
	s.mu.Lock()
	s.sessions[c.id] = c
	s.mu.Unlock()
}

func (s *Server) removeSession(c *Conn) {
	s.mu.Lock()
	delete(s.sessions, c.id)
	s.mu.Unlock()
}

func (s *Server) getSession(sid string) *Conn {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessions[sid]
}

func snapshotRoom(m map[string]*Conn) []*Conn {
	out := make([]*Conn, 0, len(m))
	for _, c := range m {
		out = append(out, c)
	}
	return out
}

// firstArg 从 EVENT 的参数数组（如 [{"msg":"hi"}]）取出首元素作为 handler 的 data。
// 无参数时返回 null（对齐 TS handler 收到 undefined/{} 的容错）。
func firstArg(args json.RawMessage) json.RawMessage {
	if len(args) == 0 || string(args) == "null" {
		return json.RawMessage("null")
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(args, &arr); err != nil || len(arr) == 0 {
		return json.RawMessage("null")
	}
	return arr[0]
}

// fireDisconnect 触发 OnDisconnect 钩子。
func (s *Server) fireDisconnect(c *Conn, reason string) {
	s.mu.RLock()
	h := s.onDisconnect
	s.mu.RUnlock()
	if h != nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					logger.Error("socket", "onDisconnect panic recovered", "err", r)
				}
			}()
			h(c, reason)
		}()
	}
}

// fireConnect 触发 OnConnection 钩子。
func (s *Server) fireConnect(c *Conn) {
	s.mu.RLock()
	h := s.onConnect
	s.mu.RUnlock()
	if h != nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					logger.Error("socket", "onConnect panic recovered", "err", r)
				}
			}()
			if err := h(c); err != nil {
				logger.Warn("socket", "onConnect returned error, disconnecting", "err", err)
				c.shutdown("onConnect error")
			}
		}()
	}
}

// ServeHTTP 是 socket.io 的统一 HTTP 入口。
//
// 集成方式：把 *Server 挂到自己 mux 的 s.path 前缀上，例如
//
//	mux.Handle(s.path+"/", srv)
//	mux.Handle(s.path, srv)
//
// 非本路径的请求由调用方 mux 自行处理；本函数只响应 config.Path（默认 /socket.io）下的
// EIO 请求（GET 握手/轮询、POST 上报、websocket 升级）。路径不匹配时回 404。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// CORS 预检统一拦截。
	if s.cors.handlePreflight(w, r) {
		return
	}

	// 路径匹配：/socket.io 或 /socket.io/。
	p := r.URL.Path
	if p != s.path && p != s.path+"/" {
		http.NotFound(w, r)
		return
	}

	q := r.URL.Query()
	transport := q.Get("transport")
	sid := q.Get("sid")

	// 仅允许配置里启用的传输。
	if !transportEnabled(s.cfg.Transports, transport) {
		http.Error(w, "transport unavailable", http.StatusBadRequest)
		return
	}

	switch transport {
	case "websocket":
		s.handleWebsocket(w, r, sid)
	default:
		s.handlePolling(w, r, sid)
	}
}

// transportEnabled 判断请求的 transport 是否在配置允许列表里。
// 空 transport（异常）按 polling 处理；websocket 未启用则拒绝。
func transportEnabled(list []string, t string) bool {
	if t == "" {
		t = "polling"
	}
	for _, v := range list {
		if v == t {
			return true
		}
	}
	return false
}

// ─── 入站 socket.io 报文分发 ──────────────────────────────────────────────────

// dispatchPacket 处理一条来自客户端的 socket.io 层报文（已剥掉 EIO MESSAGE '4'）。
func (s *Server) dispatchPacket(c *Conn, raw []byte) {
	p, err := parseSIOPacket(raw)
	if err != nil {
		logger.Warn("socket", "malformed sio packet", "sid", c.idShort(), "err", err)
		return
	}

	switch p.typ {
	case sioCONNECT:
		s.handleConnect(c, p)
	case sioDISCONNECT:
		c.shutdown("client disconnect")
	case sioEVENT, sioBinaryEvent:
		s.handleEvent(c, p)
	case sioACK:
		// 客户端回的 ACK（服务端发起的带 ack 事件）——本应用服务端不发起带 ack 的 emit，忽略。
	default:
		logger.Warn("socket", "unhandled sio packet type", "type", p.typ, "sid", c.idShort())
	}
}

// handleConnect 处理客户端 CONNECT：跑中间件链 → 回 CONNECT → 触发 OnConnection。
func (s *Server) handleConnect(c *Conn, p sioPacket) {
	c.setHandshakeAuth(p.args)

	// 按注册顺序执行中间件（对标 io.use 链式）。
	s.mu.RLock()
	mws := make([]socketapi.MiddlewareFunc, len(s.middlewares))
	copy(mws, s.middlewares)
	s.mu.RUnlock()

	var nextErr error
	next := func() error { return nil }
	// 从后往前包，形成链式 next。
	for i := len(mws) - 1; i >= 0; i-- {
		mw := mws[i]
		prev := next
		next = func() error {
			return mw(c, prev)
		}
	}
	if nextErr = next(); nextErr != nil {
		logger.Warn("socket", "middleware rejected connection", "sid", c.idShort(), "err", nextErr)
		c.sendEIO(eioMESSAGE, encodeSIOConnectError(nextErr.Error()))
		c.shutdown("middleware rejected")
		return
	}

	// 回 CONNECT 成功（带 socket id）。
	c.sendEIO(eioMESSAGE, encodeSIOConnect(c.id))
	// 启动服务端心跳。
	c.startPingLoop()
	logger.Info("socket", "socket.io connected", "sid", c.idShort())
	s.fireConnect(c)
}

// handleEvent 分发 EVENT/BINARY_EVENT 到注册的处理器。
func (s *Server) handleEvent(c *Conn, p sioPacket) {
	event, args, err := p.eventNameAndArgs()
	if err != nil {
		logger.Warn("socket", "bad event body", "sid", c.idShort(), "err", err)
		return
	}

	// ack 回调：客户端带了 ack id，则构造一个把结果编码成 ACK 包的回调。
	var ack socketapi.Ack
	if p.hasAck {
		ack = func(result any) {
			frame, err := encodeSIOAck(p.ackID, result)
			if err != nil {
				logger.Error("socket", "encode ack failed", "err", err)
				return
			}
			c.sendEIO(eioMESSAGE, frame)
		}
	} else {
		ack = func(any) {} // no-op
	}

	s.mu.RLock()
	h, ok := s.handlers[event]
	binH, binOK := s.binHandlers[event]
	s.mu.RUnlock()

	// 对齐 TS handler：socket.emit('event', obj) 的 server handler 直接拿到 obj，
	// 而非整个 args 数组。这里取 args 数组首元素作为 data；无参则为 null。
	data := firstArg(args)

	switch {
	case ok:
		func() {
			defer func() {
				if r := recover(); r != nil {
					logger.Error("socket", "event handler panic", "event", event, "err", r)
				}
			}()
			h(c, data, ack)
		}()
	case binOK:
		func() {
			defer func() {
				if r := recover(); r != nil {
					logger.Error("socket", "binary handler panic", "event", event, "err", r)
				}
			}()
			// 二进制事件的 data 取首个附件（本实现按 1 附件约定）。
			binH(c, nil)
		}()
	default:
		logger.Info("socket", "no handler for event", "event", event, "sid", c.idShort())
	}
}
