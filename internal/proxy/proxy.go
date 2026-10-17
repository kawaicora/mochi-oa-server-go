// Package proxy 实现无外网员工的出网代理：HTTP 代理（普通转发 + CONNECT 隧道给 HTTPS）
// 与 SOCKS5 代理（RFC 1928 CONNECT；UDP ASSOCIATE/BIND 不支持）。
//
// 鉴权（HTTP Basic 与 SOCKS5 RFC1929 共用）：按 username+secret 查 proxy_accounts，
// 必须 status=active 且 now<=expires_at 才放行；任何失败都打日志（client addr/username/原因），绝不静默。
//
// 到期失效：每次鉴权即校验有效期；另起每小时 sweep goroutine 把已过期 active 账号批量置 disabled。
package proxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"mochi-oa-server-go/internal/db"
	"mochi-oa-server-go/internal/logger"
)

const mod = "proxy"

// Server 聚合 HTTP 与 SOCKS5 两个代理监听。
type Server struct {
	store      db.Store
	httpPort   int
	socks5Port int
	httpSrv    *http.Server
	socksLn    net.Listener
	sweepStop  chan struct{}
}

// New 构造并开始监听（goroutine 内 accept）。失败在调用方打日志后仍继续启动主服务。
func New(store db.Store, httpPort, socks5Port int) *Server {
	return &Server{store: store, httpPort: httpPort, socks5Port: socks5Port}
}

// auth 校验账号：active 且未过期。返回是否放行与拒绝原因（用于日志）。
func (s *Server) auth(username, secret string, clientAddr string) (bool, string) {
	if username == "" || secret == "" {
		logger.Warn(mod, "代理鉴权缺字段", "client", clientAddr, "username", username)
		return false, "missing credentials"
	}
	acc, err := s.store.ProxyAuthLookup(context.Background(), username, secret)
	if err != nil {
		logger.Error(mod, "代理鉴权查询失败", "client", clientAddr, "username", username, "error", err.Error())
		return false, "lookup error"
	}
	if acc == nil {
		logger.Warn(mod, "代理鉴权失败：账号不存在", "client", clientAddr, "username", username)
		return false, "no such account"
	}
	if acc.Status != "active" {
		logger.Warn(mod, "代理鉴权失败：账号已停用", "client", clientAddr, "username", username, "status", acc.Status)
		return false, "account disabled"
	}
	if time.Now().UTC().After(acc.ExpiresAt) {
		logger.Warn(mod, "代理鉴权失败：账号已过期", "client", clientAddr, "username", username, "expiresAt", acc.ExpiresAt.UTC().Format(time.RFC3339))
		return false, "account expired"
	}
	return true, ""
}

// Start 起 HTTP 代理、SOCKS5 监听与 sweep。任一监听失败打日志并继续（降级）。
func (s *Server) Start() {
	// ── HTTP 代理 ──
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.httpHandler)
	s.httpSrv = &http.Server{Addr: ":" + strconv.Itoa(s.httpPort), Handler: mux}
	go func() {
		logger.Info(mod, "HTTP 代理监听", "port", s.httpPort)
		if err := s.httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error(mod, "HTTP 代理监听失败（降级，主服务继续）", "port", s.httpPort, "error", err.Error())
		}
	}()

	// ── SOCKS5 ──
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(s.socks5Port))
	if err != nil {
		logger.Error(mod, "SOCKS5 监听失败（降级，主服务继续）", "port", s.socks5Port, "error", err.Error())
	} else {
		s.socksLn = ln
		logger.Info(mod, "SOCKS5 代理监听", "port", s.socks5Port)
		go s.socksAccept(ln)
	}

	// ── 每小时 sweep 过期账号 ──
	s.sweepStop = make(chan struct{})
	go s.sweepLoop()
}

// Stop 优雅关闭。
func (s *Server) Stop() {
	close(s.sweepStop)
	if s.httpSrv != nil {
		_ = s.httpSrv.Close()
	}
	if s.socksLn != nil {
		_ = s.socksLn.Close()
	}
}

// sweepLoop 每小时把已过期 active 账号批量置 disabled。
func (s *Server) sweepLoop() {
	t := time.NewTicker(1 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-s.sweepStop:
			return
		case <-t.C:
			n, err := s.store.SweepExpiredProxyAccounts(context.Background())
			if err != nil {
				logger.Error(mod, "代理账号到期 sweep 失败", "error", err.Error())
			} else if n > 0 {
				logger.Info(mod, "代理账号到期 sweep 处置", "disabled", n)
			}
		}
	}
}

// ──────────────── HTTP 代理 ────────────────

// httpHandler 处理两种请求：CONNECT（HTTPS 隧道）与普通 absolute-form 转发。
func (s *Server) httpHandler(w http.ResponseWriter, r *http.Request) {
	clientAddr := r.RemoteAddr
	user, pass, ok := proxyBasic(r)
	if !ok {
		logger.Warn(mod, "HTTP 代理缺 Proxy-Authorization", "client", clientAddr)
		http.Error(w, "Proxy Authentication Required", http.StatusProxyAuthRequired)
		return
	}
	if allow, reason := s.auth(user, pass, clientAddr); !allow {
		logger.Warn(mod, "HTTP 代理鉴权拒绝", "client", clientAddr, "username", user, "reason", reason)
		http.Error(w, "Proxy Authentication Required", http.StatusProxyAuthRequired)
		return
	}

	if r.Method == http.MethodConnect {
		s.handleConnect(w, r, user, clientAddr)
		return
	}
	// 普通请求：r.URL 是 absolute-form（代理模式），直接转发。
	if !r.URL.IsAbs() {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	logger.Info(mod, "HTTP 代理转发", "client", clientAddr, "username", user, "method", r.Method, "url", r.URL.String())
	r.RequestURI = ""
	transport := http.DefaultTransport
	resp, err := transport.RoundTrip(r)
	if err != nil {
		logger.Error(mod, "HTTP 代理转发失败", "client", clientAddr, "url", r.URL.String(), "error", err.Error())
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request, user, clientAddr string) {
	logger.Info(mod, "HTTP 代理 CONNECT 隧道", "client", clientAddr, "username", user, "target", r.Host)
	dst, err := net.DialTimeout("tcp", r.Host, 10*time.Second)
	if err != nil {
		logger.Error(mod, "HTTP 代理 CONNECT 拨号失败", "client", clientAddr, "target", r.Host, "error", err.Error())
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		dst.Close()
		http.Error(w, "Not Implemented", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		dst.Close()
		logger.Error(mod, "HTTP 代理 hijack 失败", "client", clientAddr, "error", err.Error())
		return
	}
	io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
	go pipe(client, dst)
	go pipe(dst, client)
}

// proxyBasic 解析 Proxy-Authorization: Basic。
func proxyBasic(r *http.Request) (user, pass string, ok bool) {
	h := r.Header.Get("Proxy-Authorization")
	if h == "" {
		return "", "", false
	}
	const p = "Basic "
	if !strings.HasPrefix(h, p) {
		return "", "", false
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(h[len(p):]))
	if err != nil {
		return "", "", false
	}
	parts := strings.SplitN(string(b), ":", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// ──────────────── SOCKS5 ────────────────

func (s *Server) socksAccept(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			logger.Warn(mod, "SOCKS5 accept 失败", "error", err.Error())
			return
		}
		go s.socksHandle(conn)
	}
}

// socksHandle 完成 RFC1928 握手 + RFC1929 用户名密码认证 + CONNECT。
func (s *Server) socksHandle(conn net.Conn) {
	defer conn.Close()
	clientAddr := conn.RemoteAddr().String()
	br := bufio.NewReader(conn)

	// 握手：VER=0x05, NMETHODS, METHODS。
	ver, err := br.ReadByte()
	if err != nil || ver != 0x05 {
		logger.Warn(mod, "SOCKS5 非 0x05 版本", "client", clientAddr)
		return
	}
	nmethods, err := br.ReadByte()
	if err != nil {
		return
	}
	if _, err := io.ReadFull(br, make([]byte, int(nmethods))); err != nil {
		return
	}
	// 选择用户名/密码认证（0x02）。
	conn.Write([]byte{0x05, 0x02})

	// RFC1929 认证子协商。
	av, err := br.ReadByte()
	if err != nil || av != 0x01 {
		return
	}
	ulen, err := br.ReadByte()
	if err != nil {
		return
	}
	ubuf := make([]byte, int(ulen))
	if _, err := io.ReadFull(br, ubuf); err != nil {
		return
	}
	plen, err := br.ReadByte()
	if err != nil {
		return
	}
	pbuf := make([]byte, int(plen))
	if _, err := io.ReadFull(br, pbuf); err != nil {
		return
	}
	user, pass := string(ubuf), string(pbuf)
	if allow, reason := s.auth(user, pass, clientAddr); !allow {
		conn.Write([]byte{0x01, 0x01}) // 失败
		logger.Warn(mod, "SOCKS5 鉴权拒绝", "client", clientAddr, "username", user, "reason", reason)
		return
	}
	conn.Write([]byte{0x01, 0x00}) // 成功

	// 请求：VER, CMD, RSV, ATYP, DST.ADDR, DST.PORT。
	head := make([]byte, 4)
	if _, err := io.ReadFull(br, head); err != nil {
		return
	}
	if head[0] != 0x05 {
		return
	}
	if head[1] != 0x01 {
		// 仅支持 CONNECT；UDP ASSOCIATE(0x03)/BIND(0x02) 不支持。
		logger.Warn(mod, "SOCKS5 不支持的命令", "client", clientAddr, "cmd", head[1])
		conn.Write([]byte{0x05, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	host, err := readSOCKSAddr(br, head[3])
	if err != nil {
		return
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(br, port); err != nil {
		return
	}
	target := net.JoinHostPort(host, strconv.Itoa(int(port[0])<<8|int(port[1])))

	logger.Info(mod, "SOCKS5 CONNECT", "client", clientAddr, "username", user, "target", target)
	dst, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		logger.Error(mod, "SOCKS5 拨号失败", "client", clientAddr, "target", target, "error", err.Error())
		conn.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer dst.Close()
	// 成功响应（BND.ADDR 全 0 占位）。
	conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	done := make(chan struct{}, 2)
	go func() { pipe(dst, conn); done <- struct{}{} }()
	go func() { pipe(conn, dst); done <- struct{}{} }()
	<-done
}

// readSOCKSAddr 按 ATYP 读 DST.ADDR。
func readSOCKSAddr(br *bufio.Reader, atyp byte) (string, error) {
	switch atyp {
	case 0x01: // IPv4
		b := make([]byte, 4)
		if _, err := io.ReadFull(br, b); err != nil {
			return "", err
		}
		return net.IP(b).String(), nil
	case 0x03: // 域名
		l, err := br.ReadByte()
		if err != nil {
			return "", err
		}
		b := make([]byte, int(l))
		if _, err := io.ReadFull(br, b); err != nil {
			return "", err
		}
		return string(b), nil
	case 0x04: // IPv6
		b := make([]byte, 16)
		if _, err := io.ReadFull(br, b); err != nil {
			return "", err
		}
		return net.IP(b).String(), nil
	}
	return "", errSOCKSAddr
}

var errSOCKSAddr = errors.New("unsupported atyp")

// ──────────────── 小工具 ────────────────

func pipe(dst io.Writer, src io.Reader) {
	io.Copy(dst, src)
	if c, ok := dst.(interface{ CloseWrite() error }); ok {
		c.CloseWrite()
	}
}

func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}
