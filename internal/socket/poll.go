package socket

import (
	"io"
	"net/http"
	"time"

	"mochi-oa-server-go/internal/logger"
)

// handlePolling 处理 transport=polling 的全部 HTTP 请求（GET 握手/长轮询、POST 上报）。
//
// 线路语义（EIO4）：
//   - GET 无 sid：新会话，回 OPEN 报文后立即结束响应。
//   - GET 有 sid：长轮询，挂起直到有报文可发或超时，把 outbox 全部报文拼成一个 payload 回包。
//   - POST 有 sid：body 是客户端上传的 payload（含若干 EIO MESSAGE），解析后逐条分发，回 "ok"。
func (s *Server) handlePolling(w http.ResponseWriter, r *http.Request, sid string) {
	s.cors.writeHeaders(w, r, "GET, POST, OPTIONS")

	if r.Method == http.MethodPost {
		s.pollPost(w, r, sid)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// ── GET ──
	if sid == "" {
		// 握手：新建会话，回 OPEN。
		s.handshakeGET(w, r)
		return
	}
	c := s.getSession(sid)
	if c == nil {
		http.Error(w, "invalid sid", http.StatusBadRequest)
		return
	}
	s.longPollGET(w, r, c)
}

// handshakeGET 新连接握手：回 OPEN 报文。
func (s *Server) handshakeGET(w http.ResponseWriter, r *http.Request) {
	conn := newConn(newSID(), s, r.Header, r.URL.Query())
	s.addSession(conn)

	upgrades := []string{}
	for _, t := range s.cfg.Transports {
		if t == "websocket" {
			upgrades = append(upgrades, "websocket")
		}
	}
	open := encodeEIOOPEN(conn.id, upgrades,
		int(defaultPingInterval/time.Millisecond),
		int(defaultPingTimeout/time.Millisecond),
		maxHttpBufferSize)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(open)
	logger.Info("socket", "polling handshake", "sid", conn.idShort())
}

// longPollGET 长轮询：立即回已有报文；空则挂起等待。
func (s *Server) longPollGET(w http.ResponseWriter, r *http.Request, c *Conn) {
	// 有积压立即回；否则等待。
	if !c.hasOutbox() {
		c.waitPollBlock()
	}
	if c.isClosed() {
		http.Error(w, "closed", http.StatusBadRequest)
		return
	}
	frames := c.flushPoll()
	if len(frames) == 0 {
		// 空回包：回 NOOP(=6) 结束本轮，客户端会再开新轮询。
		frames = [][]byte{{eioNOOP}}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	for _, f := range frames {
		_, _ = w.Write(f)
	}
}

// pollPost 处理客户端 POST 上传的 payload。
func (s *Server) pollPost(w http.ResponseWriter, r *http.Request, sid string) {
	c := s.getSession(sid)
	if c == nil {
		http.Error(w, "invalid sid", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxHttpBufferSize))
	if err != nil {
		logger.Warn("socket", "read post body failed", "err", err)
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	// 解析 payload：按 EIO packet type 切分。每个报文以一个数字字符开头；
	// base64 二进制包以 'b' 开头。
	dispatchPollPayload(c, body)

	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// dispatchPollPayload 切分 polling payload 并分发其中的 EIO MESSAGE 报文。
//
// payload 是若干 EIO 报文的拼接，每个报文首字符是类型数字（'0'..'6'）或 'b'。
// 我们只关心 MESSAGE(='4')：剥掉 '4' 后交给 socket.io 层分发。
func dispatchPollPayload(c *Conn, body []byte) {
	i := 0
	for i < len(body) {
		typ := body[i]
		switch typ {
		case eioPING:
			// 客户端在 polling 下发 PING？EIO4 是服务端发 PING；这里兼容收到 PING 回 PONG。
			c.sendEIO(eioPONG, nil)
			i++
		case eioPONG:
			c.signalPong()
			i++
		case eioMESSAGE:
			// 找下一个报文起点：下一个出现的数字字符（'0'..'6'）或 'b'。
			j := i + 1
			for j < len(body) && !isEIOFrameStart(body[j]) {
				j++
			}
			pkt := body[i+1 : j]
			s := c.srv
			s.dispatchPacket(c, pkt)
			i = j
		case 'b':
			// base64 二进制包：'b' 后到下一个帧起点。
			j := i + 1
			for j < len(body) && !isEIOFrameStart(body[j]) {
				j++
			}
			i = j
		default:
			// 其他报文（OPEN/CLOSE/UPGRADE/NOOP）在 polling 上行忽略。
			i++
		}
	}
}

// isEIOFrameStart 判断字节是否为 EIO payload 里一个新报文的起始标记。
func isEIOFrameStart(b byte) bool {
	switch b {
	case '0', '1', '2', '3', '4', '5', '6', 'b':
		return true
	}
	return false
}
