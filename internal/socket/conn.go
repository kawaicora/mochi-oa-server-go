package socket

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"mochi-oa-server-go/internal/logger"
	"mochi-oa-server-go/internal/socketapi"
)

// 心跳与缓冲默认值（对齐 engine.io ~6.6.0 与源服务端）。
const (
	defaultPingInterval = 25000 * time.Millisecond
	defaultPingTimeout  = 20000 * time.Millisecond
	// maxHttpBufferSize 与源服务端 maxHttpBufferSize=1e6 对齐。
	maxHttpBufferSize = 1 << 20 // 1 MiB
)

// Conn 实现 socketapi.Conn，代表一条 engine.io session（= 默认命名空间 "/" 上的一个 socket）。
//
// 并发模型：
//   - Conn 的状态字段（data/rooms/transport/ws/outbox）全部在 mu 锁内读写；
//   - 写网络用独立 writeMu 串行化（websocket 不允许并发写）；
//   - 出站报文统一走 pumpOut，由 transport 当前形态决定"直写 ws"还是"入 outbox 等长轮询取走"。
type Conn struct {
	id    string // engine.io sid（连接 id）
	srv   *Server
	created time.Time

	mu       sync.Mutex
	data     map[string]any
	rooms    map[string]struct{}
	transport string // "polling" | "websocket"
	closed   bool
	closeReason string

	// websocket 连接（仅当 transport=websocket 时非 nil）。
	ws   *websocket.Conn
	wsMu sync.Mutex

	// 握手期 HTTP 上下文（供集成中间件取 headers/query/auth raw）。
	handshakeAuth json.RawMessage // CONNECT 报文 body（auth）
	headers       http.Header
	handshakeHost string // 握手请求的 Host（req.Host，含端口），用于推断服务器对外地址
	query         urlQuery

	// 出站：等待被长轮询 GET 取走的报文队列。
	outMu  sync.Mutex
	outbox [][]byte
	// long-poll 取走通知：有报文时往这里发信号。
	pollNotify chan struct{}

	// 心跳（EIO4：服务端主动发 PING）。
	pingInterval time.Duration
	pingTimeout  time.Duration
	pingStop     chan struct{}
	pingDone     chan struct{}
	pongNotify   chan struct{} // 收到客户端 PONG 时触发一次

	once sync.Once
}

type urlQuery = map[string][]string

// 编译期断言：*Conn 实现 socketapi.Conn。
var _ socketapi.Conn = (*Conn)(nil)

// newConn 创建一条连接（尚未握手完成）。
func newConn(id string, srv *Server, h http.Header, q urlQuery, host string) *Conn {
	return &Conn{
		id:          id,
		srv:         srv,
		created:     time.Now(),
		data:        map[string]any{},
		rooms:       map[string]struct{}{},
		transport:   "polling",
		headers:     h,
		handshakeHost: host,
		query:       q,
		pollNotify:  make(chan struct{}, 1),
		pingInterval: defaultPingInterval,
		pingTimeout:  defaultPingTimeout,
		pingStop:    make(chan struct{}),
		pingDone:    make(chan struct{}),
		pongNotify:  make(chan struct{}, 1),
	}
}

// ID 返回连接 sid（对标 socket.id）。
func (c *Conn) ID() string { return c.id }

// Set 写入连接级状态（对标 socket.data.x）。认证用户由中间件写入 key="auth"。
func (c *Conn) Set(key string, val any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data[key] = val
}

// Get 读取连接级状态。
func (c *Conn) Get(key string) any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.data[key]
}

// Rooms 返回当前连接所在房间列表（对标 socket.rooms，去掉 sid 自身）。
func (c *Conn) Rooms() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.rooms))
	for r := range c.rooms {
		out = append(out, r)
	}
	return out
}

// Join 加入房间（同时在 Server 的房间表登记）。
func (c *Conn) Join(room string) error {
	c.mu.Lock()
	c.rooms[room] = struct{}{}
	c.mu.Unlock()
	c.srv.roomJoin(room, c)
	return nil
}

// Leave 离开房间。
func (c *Conn) Leave(room string) error {
	c.mu.Lock()
	delete(c.rooms, room)
	c.mu.Unlock()
	c.srv.roomLeave(room, c)
	return nil
}

// Emit 向本连接发一个 JSON 事件（socket.io EVENT）。
func (c *Conn) Emit(event string, data any) error {
	payload, err := encodeSIOEvent(event, data)
	if err != nil {
		return err
	}
	c.sendEIO(eioMESSAGE, payload)
	return nil
}

// EmitBinary 向本连接发一个二进制事件（BINARY_EVENT，1 个附件）。
// 线路：先发文本帧 "51-[\"event\",{\"_placeholder\":true,\"num\":0}]"，再发二进制附件帧。
// 为什么这样：EIO4 约定二进制事件用 type=5 + 附件数前缀 + JSON 占位符，
// 附件作为后续独立的 EIO MESSAGE 二进制帧顺序下发。
func (c *Conn) EmitBinary(event string, data []byte) error {
	body, _ := json.Marshal([]any{event, map[string]any{"_placeholder": true, "num": 0}})
	frame := make([]byte, 0, 4+len(body))
	frame = append(frame, '5', '1', '-')
	frame = append(frame, body...)
	c.sendEIO(eioMESSAGE, frame)
	// 二进制附件：websocket 下用 binary frame；polling 下走 base64（poll.go 处理）。
	c.sendEIOBinary(data)
	return nil
}

// Disconnect 主动断开本连接（服务端踢人，对标 socket.disconnect()）。
func (c *Conn) Disconnect(reason string) {
	c.shutdown(reason)
}

// ─── 出站分发 ────────────────────────────────────────────────────────────────

// sendEIO 发一个 EIO MESSAGE（字符串型）报文。payload 为 socket.io 层报文本体。
func (c *Conn) sendEIO(typ byte, payload []byte) {
	if c.isClosed() {
		return
	}
	frame := make([]byte, 0, 1+len(payload))
	frame = append(frame, typ)
	frame = append(frame, payload...)
	c.enqueue(frame)
}

// sendEIOBinary 发一个 EIO MESSAGE 二进制帧（仅 websocket 直传；polling 由 base64 包装）。
func (c *Conn) sendEIOBinary(data []byte) {
	if c.isClosed() {
		return
	}
	c.enqueueBinary(data)
}

// enqueue 把一个完整文本帧放入出站队列，并唤醒等待中的长轮询。
func (c *Conn) enqueue(frame []byte) {
	c.outMu.Lock()
	if c.closed {
		c.outMu.Unlock()
		return
	}
	// websocket 直写，不入队。
	if c.transport == "websocket" && c.ws != nil {
		c.outMu.Unlock()
		c.writeWS(websocket.TextMessage, frame)
		return
	}
	c.outbox = append(c.outbox, frame)
	c.outMu.Unlock()
	c.wakePoll()
}

// enqueueBinary 二进制帧：websocket 直写 binary frame；polling 暂以 base64 文本帧追加。
func (c *Conn) enqueueBinary(data []byte) {
	c.outMu.Lock()
	defer c.outMu.Unlock()
	if c.closed {
		return
	}
	if c.transport == "websocket" && c.ws != nil {
		c.writeWS(websocket.BinaryMessage, data)
		return
	}
	// polling：EIO4 规定含二进制的 payload 一律 base64，前缀 'b'。
	c.outbox = append(c.outbox, append([]byte{'b'}, base64Encode(data)...))
	c.wakePollLocked()
}

// wakePoll 唤醒一个被挂起的长轮询 GET。
func (c *Conn) wakePoll() {
	c.outMu.Lock()
	c.wakePollLocked()
	c.outMu.Unlock()
}

func (c *Conn) wakePollLocked() {
	select {
	case c.pollNotify <- struct{}{}:
	default:
	}
}

// flushPoll 取走当前所有待发文本帧（供长轮询 GET 一次性回包）。
func (c *Conn) flushPoll() [][]byte {
	c.outMu.Lock()
	defer c.outMu.Unlock()
	out := c.outbox
	c.outbox = nil
	return out
}

// hasOutbox 是否有待发报文。
func (c *Conn) hasOutbox() bool {
	c.outMu.Lock()
	defer c.outMu.Unlock()
	return len(c.outbox) > 0
}

// waitPollBlock 阻塞等待直到有报文或关闭（供长轮询 GET 在空队列时挂起）。
func (c *Conn) waitPollBlock() {
	if c.hasOutbox() {
		return
	}
	select {
	case <-c.pollNotify:
	case <-c.pingStop:
	case <-time.After(defaultPingInterval + defaultPingTimeout):
		// 兜底超时：到点返回，由 GET 回 NOOP 结束本轮。
	}
}

// ─── websocket 写 ───────────────────────────────────────────────────────────

// writeWS 串行写一个 ws 帧。
func (c *Conn) writeWS(msgType int, data []byte) {
	c.wsMu.Lock()
	defer c.wsMu.Unlock()
	if c.ws == nil {
		return
	}
	_ = c.ws.WriteMessage(msgType, data)
}

// markWebsocketUp 把会话切换到 websocket 传输（升级完成后调用）。
func (c *Conn) markWebsocketUp(ws *websocket.Conn) {
	c.outMu.Lock()
	c.transport = "websocket"
	c.ws = ws
	pending := c.outbox
	c.outbox = nil
	c.outMu.Unlock()
	// 把升级前积攒的轮询报文冲刷到 ws。
	for _, f := range pending {
		c.writeWS(websocket.TextMessage, f)
	}
	logger.Info("socket", "transport upgraded to websocket", "sid", c.idShort())
}

// markPolling 回退到 polling（升级失败时）。
func (c *Conn) markPolling() {
	c.mu.Lock()
	c.transport = "polling"
	c.ws = nil
	c.mu.Unlock()
}

// ─── 心跳（EIO4：服务端主动 PING）─────────────────────────────────────────────

// startPingLoop 启动服务端心跳：每 pingInterval 发一次 PING，等客户端 PONG；
// pingTimeout 内没等到 PONG 即判死链关闭连接。
//
// 为什么必须服务端发：EIO4 相对 v3 的关键反转——v3 是客户端发 PING，v4 改为
// 服务端发 PING。若沿用客户端发 PING 的实现，v4 客户端会一直等服务端 PING 而超时断开。
func (c *Conn) startPingLoop() {
	go func() {
		defer close(c.pingDone)
		t := time.NewTicker(c.pingInterval)
		defer t.Stop()
		for {
			select {
			case <-c.pingStop:
				return
			case <-t.C:
				c.sendEIO(eioPING, nil)
				// 等待 PONG：由 readLoop 在收到 eioPONG 时复位 deadline。
				if !c.waitPong() {
					logger.Warn("socket", "ping timeout, closing dead connection", "sid", c.idShort())
					c.shutdown("ping timeout")
					return
				}
			}
		}
	}()
}

// pongCh 是每连接的 PONG 通知通道（收到客户端 PONG 时触发一次）。
// 用指针字段占位，实际在 conn 结构里通过 pingStop/pingDone 与 readLoop 协作。
// 这里用 channel 字段简化。

// waitPong 在 pingTimeout 内等待一次 PONG 信号。
func (c *Conn) waitPong() bool {
	timer := time.NewTimer(c.pingTimeout)
	defer timer.Stop()
	select {
	case <-c.pongChan():
		return true
	case <-timer.C:
		return false
	case <-c.pingStop:
		return false
	}
}

// pongCh 返回 PONG 信号通道（已在 newConn 初始化）。
func (c *Conn) pongChan() <-chan struct{} {
	return c.pongNotify
}

// signalPong 收到客户端 PONG 时调用，唤醒 waitPong。
func (c *Conn) signalPong() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pongNotify != nil {
		select {
		case c.pongNotify <- struct{}{}:
		default:
		}
	}
}

// ─── 关闭 ────────────────────────────────────────────────────────────────────

func (c *Conn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// shutdown 关闭连接、退出房间、触发 OnDisconnect。幂等。
func (c *Conn) shutdown(reason string) {
	c.once.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.closeReason = reason
		c.mu.Unlock()

		// 停止心跳
		close(c.pingStop)

		// 离开所有房间
		for _, r := range c.Rooms() {
			_ = c.Leave(r)
		}

		// 关闭 ws
		c.wsMu.Lock()
		if c.ws != nil {
			_ = c.ws.Close()
			c.ws = nil
		}
		c.wsMu.Unlock()

		// 从服务端会话表移除
		c.srv.removeSession(c)

		logger.Info("socket", "connection closed", "sid", c.idShort(), "reason", reason)
		c.srv.fireDisconnect(c, reason)
	})
}

// idShort 返回脱敏的短 id（日志用，避免长 sid 刷屏）。
func (c *Conn) idShort() string {
	if len(c.id) > 8 {
		return c.id[:8]
	}
	return c.id
}

// ─── 握手上下文访问器（供集成中间件类型断言使用）──────────────────────────────

// HandshakeAuth 返回客户端 CONNECT 报文里的 auth JSON（可能为 nil）。
// 集成中间件：authRaw := conn.(*socket.Conn).HandshakeAuth()
func (c *Conn) HandshakeAuth() json.RawMessage { return c.handshakeAuth }

// Headers 返回握手请求的 HTTP 头。
func (c *Conn) Headers() http.Header { return c.headers }

// Host 返回握手请求的 Host（req.Host，可能含端口）。
func (c *Conn) Host() string { return c.handshakeHost }

// Query 返回握手请求的 query 参数。
//
// 返回值必须是 url.Values（定义类型），不能写成别名 urlQuery = map[string][]string：
// cmd/server 的握手中间件用的接口签名是 Query() url.Values，而 Go 要求方法签名完全一致
// （定义类型之间不会因为底层类型相同而互相满足），一旦返回别名，conn.(handshakeHolder)
// 就会永远断言失败 → 握手 token 认证被静默跳过（表现为自动登录失效、独立窗口提示
//「需要 SERVER_ADMIN 权限」）。cmd/server/main.go 里加了编译期断言来兜住这一点。
func (c *Conn) Query() url.Values { return url.Values(c.query) }

// setHandshakeAuth 记录 CONNECT body。
func (c *Conn) setHandshakeAuth(raw json.RawMessage) { c.handshakeAuth = raw }
