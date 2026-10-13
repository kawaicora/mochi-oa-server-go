package socket

import (
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"mochi-oa-server-go/internal/config"
	"mochi-oa-server-go/internal/logger"
)

// websocketUpgrader 包装 gorilla/websocket.Upgrader。
type websocketUpgrader struct {
	up *websocket.Upgrader
}

func newUpgrader(cfg *config.ServerConfig, path string) websocketUpgrader {
	return websocketUpgrader{
		up: &websocket.Upgrader{
			// 校验 Origin：与 CORS 策略一致（* 或白名单）。
			CheckOrigin: func(r *http.Request) bool {
				// 同源或无 Origin（非浏览器）放行；跨域按 CORS 列表。
				origin := r.Header.Get("Origin")
				if origin == "" {
					return true
				}
				if cfg.CORSStar {
					return true
				}
				for _, o := range cfg.CORSList {
					if equalFold(o, origin) {
						return true
					}
				}
				return false
			},
		},
	}
}

// handleWebsocket 处理 transport=websocket 的 GET 请求（含升级探测与直连）。
func (s *Server) handleWebsocket(w http.ResponseWriter, r *http.Request, sid string) {
	s.cors.writeHeaders(w, r, "GET, POST, OPTIONS")

	conn, err := s.upgrader.up.Upgrade(w, r, nil)
	if err != nil {
		logger.Warn("socket", "websocket upgrade failed", "err", err)
		return
	}

	if sid == "" {
		// 直连 websocket（不经 polling）：新建会话，先回 OPEN。
		c := newConn(newSID(), s, r.Header, r.URL.Query())
		s.addSession(c)
		c.markWebsocketUp(conn)
		upgrades := []string{}
		open := encodeEIOOPEN(c.id, upgrades,
			int(defaultPingInterval/time.Millisecond),
			int(defaultPingTimeout/time.Millisecond),
			maxHttpBufferSize)
		c.writeWS(websocket.TextMessage, open)
		logger.Info("socket", "direct websocket connected", "sid", c.idShort())
		go c.readLoopWS(s)
		return
	}

	// 升级探测：客户端先发 PING("probe")，服务端回 PONG("probe")，再发 UPGRADE。
	c := s.getSession(sid)
	if c == nil {
		_ = conn.Close()
		logger.Warn("socket", "upgrade with invalid sid", "sid", sid[:min(8, len(sid))])
		return
	}

	// 读 probe PING。
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		_ = conn.Close()
		return
	}
	if string(msg) != string(eioPING)+"probe" {
		// 不是 probe，直接当普通报文处理（容错）。
		_ = conn.Close()
		return
	}
	// 回 PONG("probe")。
	_ = conn.WriteMessage(websocket.TextMessage, []byte{byte(eioPONG), 'p', 'r', 'o', 'b', 'e'})

	// 读 UPGRADE 确认帧。
	_, msg, err = conn.ReadMessage()
	if err != nil || string(msg) != string(eioUPGRADE) {
		_ = conn.Close()
		return
	}

	// 升级完成：切换传输。
	c.markWebsocketUp(conn)
	go c.readLoopWS(s)
}

// readLoopWS 是 websocket 连接的读循环：读帧 → 分发 EIO MESSAGE。
func (c *Conn) readLoopWS(s *Server) {
	defer c.shutdown("ws read loop end")
	for {
		mt, msg, err := c.ws.ReadMessage()
		if err != nil {
			if !c.isClosed() {
				logger.Info("socket", "ws read error", "sid", c.idShort(), "err", err)
			}
			return
		}
		if mt == websocket.BinaryMessage {
			// 二进制附件帧：本实现按 1 附件约定，忽略具体内容（应用层不消费）。
			continue
		}
		if len(msg) == 0 {
			continue
		}
		switch msg[0] {
		case eioPING:
			// 服务端应回 PONG（EIO4 心跳由服务端主动发；这里兼容客户端 probe/主动 PING）。
			c.writeWS(websocket.TextMessage, []byte{eioPONG})
		case eioPONG:
			c.signalPong()
		case eioMESSAGE:
			s.dispatchPacket(c, msg[1:])
		case eioCLOSE:
			c.shutdown("client close")
			return
		default:
			logger.Info("socket", "ws unhandled frame", "head", string(msg[0]), "sid", c.idShort())
		}
	}
}

// equalFold 大小写不相等比较（避免 strings 引入冗余）。
func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
