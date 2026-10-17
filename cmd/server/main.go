// Package main 是 mochi-oa-server Go 复刻版的服务端入口（1:1 对标源 server/src/index.ts 的启动流程）。
//
// 启动顺序（与 index.ts 逐行对齐）：
//  1. loadConfig → 2. IceCloudflare 启用则覆盖 cfg.IceServers（失败回退并 Warn）
//  3. store.open/init → 4. mailer 初始化（环境主邮件未配置则统计 DB 配置数）
//  5. seedHolidays（失败仅 Error 日志，不中断）→ 6. 初始管理员播种
//  7. HTTP(S) server + socket.io server（path/transports）→ 8. io.use 认证中间件
//  9. 注册全部 socket handler + HTTP 路由模块 → 10. io.on('connection') presence 接线
//
// 11. listen host:port → 12. 优雅退出（socket close → http close → store close）。
//
// 单例纪律：config/db/socket/mailer/presence 全部只初始化一次（db/mailer/config 内部 sync.Once，
// presence 包级注册表，socket 引擎 New 一次后传给全部 Register）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"mochi-oa-server-go/internal/config"
	"mochi-oa-server-go/internal/db"
	"mochi-oa-server-go/internal/handlers/attendance"
	"mochi-oa-server-go/internal/handlers/auth"
	"mochi-oa-server-go/internal/handlers/checkin"
	"mochi-oa-server-go/internal/handlers/conversation"
	"mochi-oa-server-go/internal/handlers/mailholiday"
	"mochi-oa-server-go/internal/handlers/org"
	"mochi-oa-server-go/internal/handlers/pm"
	proxyh "mochi-oa-server-go/internal/handlers/proxy"
	"mochi-oa-server-go/internal/handlers/report"
	"mochi-oa-server-go/internal/handlers/rtcdev"
	"mochi-oa-server-go/internal/handlers/task"
	apphttp "mochi-oa-server-go/internal/http"
	"mochi-oa-server-go/internal/ice"
	"mochi-oa-server-go/internal/logger"
	"mochi-oa-server-go/internal/mailer"
	"mochi-oa-server-go/internal/password"
	"mochi-oa-server-go/internal/presence"
	proxypkg "mochi-oa-server-go/internal/proxy"
	"mochi-oa-server-go/internal/socket"
	"mochi-oa-server-go/internal/socketapi"
	"mochi-oa-server-go/internal/token"
	"mochi-oa-server-go/internal/types"
	"mochi-oa-server-go/internal/util"
)

func main() {
	cfg := config.Load()
	ctx := context.Background()

	// ---- 1. ICE Cloudflare 覆盖（对标 index.ts fetchCloudflareIce 段） ----
	if cfg.IceCloudflare != nil && cfg.IceCloudflare.Enabled {
		if servers := ice.FetchCloudflareIce(cfg); len(servers) > 0 {
			cfg.IceServers = servers
			logger.Info("ice", "已用 Cloudflare TURN 动态凭证覆盖静态 ICE 配置")
		} else {
			logger.Warn("ice", "Cloudflare TURN 获取失败，回退静态 ICE 配置")
		}
	}

	// ---- 2. 打开存储（db.Open 内部已完成 Ping/Init） ----
	store, err := db.Open(ctx, cfg)
	if err != nil {
		logger.Error("mochioa-server", "存储初始化失败", "error", err.Error())
		os.Exit(1)
	}

	// ---- 3. mailer 初始化（对标 createMailer + 主邮件日志） ----
	ml := mailer.NewMailer(cfg, store)
	mailer.SetDefault(ml)
	if cfg.Mail != nil && cfg.Mail.Enabled {
		logger.Info("mail", "环境已配置主邮件，按 .env 发送")
	} else {
		list, err := store.ListMailConfigs(ctx, db.MailConfigScope{})
		if err == nil {
			logger.Info("mail", fmt.Sprintf("环境未配置主邮件，当前 DB 已有 %d 个主邮件配置", len(list)))
		}
	}

	// ---- 4. 假期种子数据（失败只 Error 不中断，对标 seedHolidays().catch(console.error)） ----
	if err := mailholiday.SeedHolidays(store); err != nil {
		logger.Error("mailholiday", "假期种子写入失败（不影响启动）", "error", err.Error())
	}

	// ---- 5. 初始管理员播种（对标 index.ts admin seed 段） ----
	if cfg.Admin.Username != "" && cfg.Admin.Password != "" {
		existing, err := store.GetUserByUsername(ctx, cfg.Admin.Username)
		if err != nil {
			logger.Error("mochioa-server", "查询初始管理员失败", "error", err.Error())
		}
		if existing == nil {
			hash, err := password.HashPassword(cfg.Admin.Password)
			if err != nil {
				logger.Error("mochioa-server", "初始管理员密码哈希失败", "error", err.Error())
				os.Exit(1)
			}
			if _, err := store.CreateUser(ctx, db.CreateUserInput{
				Username:     cfg.Admin.Username,
				PasswordHash: hash,
				Nick:         "管理员",
			}); err != nil {
				logger.Error("mochioa-server", "初始管理员创建失败", "error", err.Error())
				os.Exit(1)
			}
			logger.Info("mochioa-server", "初始管理员已播种", "username", cfg.Admin.Username)
		}
	}

	// ---- 6. socket 引擎 + HTTP mux ----
	srv, err := socket.New(cfg)
	if err != nil {
		logger.Error("mochioa-server", "socket 引擎创建失败", "error", err.Error())
		os.Exit(1)
	}
	mux := http.NewServeMux()
	// 引擎 ServeHTTP 只认 cfg.Path 与 cfg.Path+"/"，显式挂载两条 pattern（对标 io.path）。
	mux.Handle(cfg.Path, srv)
	mux.Handle(cfg.Path+"/", srv)

	// ---- 7. HTTP 路由模块（files / reset-pwd / admin-web / afdian） ----
	apphttp.RegisterUploadRoutes(mux, store, cfg)
	apphttp.RegisterResetPwd(mux, store, srv, cfg)
	apphttp.RegisterAdminWeb(mux, cfg)
	apphttp.RegisterAfdianRoutes(mux, store, cfg)

	// ---- 8. io.use 认证中间件（对标 index.ts io.use 逐行语义） ----
	srv.Use(authMiddleware(store, cfg))

	// ---- 9. 注册全部 socket handler（读各包实际导出签名调用） ----
	if err := auth.RegisterAuth(srv, store, cfg, ml); err != nil {
		logger.Error("mochioa-server", "auth handler 注册失败", "error", err.Error())
		os.Exit(1)
	}
	org.RegisterCompanyHandlers(srv)
	org.RegisterGroupHandlers(srv)
	org.RegisterOrgHandlers(srv)
	org.RegisterFriendHandlers(srv)
	conversation.Register(conversation.Ctx{Srv: srv, Store: store, Config: cfg})
	task.Register(srv, store)
	pm.Register(srv, store)
	rtcdev.RegisterRtc(rtcdev.Ctx{Srv: srv, Store: store, Config: cfg})
	rtcdev.RegisterDev(rtcdev.Ctx{Srv: srv, Store: store, Config: cfg})
	rtcdev.RegisterSysmon(rtcdev.Ctx{Srv: srv, Store: store, Config: cfg})
	mailholiday.RegisterMail(srv, store, cfg)
	mailholiday.RegisterHoliday(srv, store)
	mailholiday.RegisterAdminUsers(srv, store, cfg)
	report.RegisterReportHandlers(srv, store, cfg)
	attendance.RegisterAttendanceHandlers(srv, store)
	checkin.RegisterCheckinHandlers(srv, store)
	proxyh.RegisterProxyHandlers(srv, store)

	// ---- 出网代理（默认关闭；PROXY_ENABLED=true 才起 HTTP+SOCKS5 监听，失败降级打日志继续主服务） ----
	var proxySrv *proxypkg.Server
	if cfg.Proxy.Enabled {
		proxySrv = proxypkg.New(store, cfg.Proxy.HTTPPort, cfg.Proxy.SOCKS5Port)
		proxySrv.Start()
		logger.Info("mochioa-server", "出网代理已启用", "httpPort", cfg.Proxy.HTTPPort, "socks5Port", cfg.Proxy.SOCKS5Port)
	}

	// ---- 10. presence 接线（对标 io.on('connection') / io.on('disconnect')） ----
	srv.OnConnection(onConnection(srv, store))
	srv.OnDisconnect(onDisconnect(srv, store))

	// ---- 11. 监听 ----
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	httpSrv := &http.Server{Addr: addr, Handler: mux}
	scheme := "http"
	listenErr := make(chan error, 1)
	if cfg.HTTPS != nil {
		scheme = "https"
		go func() { listenErr <- httpSrv.ListenAndServeTLS(cfg.HTTPS.Cert, cfg.HTTPS.Key) }()
	} else {
		go func() { listenErr <- httpSrv.ListenAndServe() }()
	}
	logger.Info("mochioa-server", fmt.Sprintf("HTTP(S) server listening on %s://%s", scheme, addr))

	// ---- 12. 优雅退出 SIGINT/SIGTERM（顺序对标 index.ts shutdown） ----
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sigCh:
		logger.Info("mochioa-server", "收到退出信号，开始优雅关闭")
	case err := <-listenErr:
		if err != nil && err != http.ErrServerClosed {
			logger.Error("mochioa-server", "HTTP 服务异常退出", "error", err.Error())
		}
	}
	// 顺序：socket close → http close → store close。
	if err := srv.Close(); err != nil {
		logger.Error("mochioa-server", "socket 关闭失败", "error", err.Error())
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("mochioa-server", "HTTP 关闭失败", "error", err.Error())
	}
	if err := store.Close(context.Background()); err != nil {
		logger.Error("mochioa-server", "存储关闭失败", "error", err.Error())
	}
	logger.Info("mochioa-server", "已退出")
}

// handshakeHolder 是引擎握手信息接口（实际由 *socket.Conn 实现；
// 任务约定取这三个方法——引擎未暴露 RemoteAddr，见 remoteHolder 的可选降级）。
type handshakeHolder interface {
	HandshakeAuth() json.RawMessage
	Headers() http.Header
	Query() url.Values
}

// 编译期断言：握手中间件靠类型断言拿到握手信息，*socket.Conn 必须实现 handshakeHolder。
//
// 这条断言专治一个曾经真实发生的静默故障：socket.Conn 的 Query() 曾返回别名
// map[string][]string，而本接口要求 url.Values（定义类型）——Go 要求方法签名完全一致，
// 于是 c.(handshakeHolder) 永远失败、hh 恒为 nil，中间件直接 return next()，
// 「用握手 token 自动登录」整条链路被静默跳过（独立终端/远程窗口报「需要 SERVER_ADMIN 权限」，
// 且客户端在 token 有效期内也无法自动登录）。有了这行，同类问题会在编译期报错而不是运行时静默。
var _ handshakeHolder = (*socket.Conn)(nil)

// remoteHolder 是可选扩展接口：若引擎将来暴露 RemoteAddr() 即可取到；当前版本降级为空串
// （auth.go realIp 有 headers 链兜底，行为与 TS 缺省一致）。
type remoteHolder interface {
	RemoteAddr() string
}

// authMiddleware 对标 index.ts io.use 中间件：从 handshake.auth.token 或 query.token 取 token，
// 校验会话有效后把 auth 放进连接态；任何失败都放行（next）——未登录连接只拿不到受保护事件。
func authMiddleware(store db.Store, cfg *config.ServerConfig) socketapi.MiddlewareFunc {
	return func(c socketapi.Conn, next func() error) error {
		// ---- 先补齐握手状态（auth handler 约定的三个 key） ----
		hh, okHH := c.(handshakeHolder)
		if !okHH {
			// 正常不该发生（上面有编译期断言）；真发生就明确报出来，
			// 不要再像以前那样静默跳过认证，否则表现为「谁都登录不上/自动登录失效」。
			logger.Error("auth", "连接未实现 handshakeHolder，握手 token 认证被跳过", "conn", c.ID())
		}
		handshakeDevice := ""
		headers := map[string]any{}
		if okHH {
			var authObj struct {
				Token  string `json:"token"`
				Device string `json:"device"`
			}
			_ = json.Unmarshal(hh.HandshakeAuth(), &authObj)
			handshakeDevice = authObj.Device
			// auth.go headerGet 支持 string / []string，http.Header 直接传 []string 值。
			for k, v := range hh.Headers() {
				headers[k] = v
			}
		}
		c.Set("handshakeHeaders", headers)
		c.Set("handshakeDevice", handshakeDevice)
		remoteAddr := ""
		if rh, ok := c.(remoteHolder); ok {
			remoteAddr = rh.RemoteAddr()
		}
		c.Set("remoteAddr", remoteAddr)

		// ---- token 认证 ----
		tokenStr := ""
		if okHH {
			var authObj struct {
				Token string `json:"token"`
			}
			_ = json.Unmarshal(hh.HandshakeAuth(), &authObj)
			tokenStr = authObj.Token
			if tokenStr == "" {
				tokenStr = hh.Query().Get("token")
			}
		}
		if tokenStr == "" {
			return next()
		}
		sess, err := store.GetSessionByTokenHash(context.Background(), token.HashToken(tokenStr))
		if err != nil {
			// TS: 整个 promise 链 .catch(() => next())——出错放行，但绝不吞错误（打日志）。
			logger.Error("auth", "握手会话查询异常", "error", err.Error())
			return next()
		}
		if sess == nil {
			// 带了 token 却查不到会话：多为登出/换设备后本地仍留着旧 token。
			// 打一行日志，便于区分「token 失效」与「压根没带 token」。
			logger.Warn("auth", "握手 token 未匹配到会话（已登出或 token 过期后被清理）")
			return next()
		}
		if !sessionValid(sess.ExpiresAt) {
			logger.Warn("auth", "握手会话已过期，删除并放行为未登录", "userID", sess.UserID, "expiresAt", sess.ExpiresAt)
			_ = store.DeleteSession(context.Background(), sess.ID)
			return next()
		}
		user, err := store.GetUserByID(context.Background(), sess.UserID)
		if err != nil {
			logger.Error("auth", "握手查用户异常", "error", err.Error())
			return next()
		}
		if user == nil {
			return next()
		}
		_ = store.TouchSession(context.Background(), sess.ID)
		nick := user.Nick
		if nick == "" {
			nick = user.Username
		}
		c.Set("auth", &types.Auth{
			ID:            user.ID,
			Username:      user.Username,
			Nick:          nick,
			SessionID:     sess.ID,
			SessionDevice: sess.Device,
		})
		return next()
	}
}

// sessionValid 会话未过期（ExpiresAt 是 ISO 字符串；与 auth.go sessionView 同格式解析）。
func sessionValid(expiresAt string) bool {
	t, err := time.Parse("2006-01-02T15:04:05.000Z", expiresAt)
	if err != nil {
		return false
	}
	return t.After(time.Now())
}

// onConnection 对标 io.on('connection') 的 presence 段：加在线表 + 进用户/会话房间 + resolveNick + 广播上线。
func onConnection(srv socketapi.Server, store db.Store) func(socketapi.Conn) error {
	return func(c socketapi.Conn) error {
		v := c.Get("auth")
		a, ok := v.(*types.Auth)
		if !ok || a == nil {
			return nil
		}
		presence.Add(a.ID, c.ID())
		_ = c.Join(util.UserRoom(a.ID))
		_ = c.Join(util.SessionRoom(a.SessionID))
		// resolveNick：连接态 auth.nick 可能滞后于资料修改，重连时以 DB 为准。
		if user, err := store.GetUserByID(context.Background(), a.ID); err == nil && user != nil && user.Nick != "" {
			a.Nick = user.Nick
		}
		presence.Broadcast(srv, store, c, true)
		return nil
	}
}

// onDisconnect 对标 io.on('disconnect')：摘在线表；用户无其它在线连接时广播下线。
func onDisconnect(srv socketapi.Server, store db.Store) func(socketapi.Conn, string) {
	return func(c socketapi.Conn, _ string) {
		v := c.Get("auth")
		a, ok := v.(*types.Auth)
		if !ok || a == nil {
			logger.Warn("main", "客户端断线，但取不到用户认证信息")
			return
		}
		// 注意 logger 的 kv 是「键值成对」，不是 printf：写成 ("...:%s %s", u, n) 会变成
		// 「%s %s」原样输出 + 把 n 当成 u 的值（之前那行日志就是这么打歪的）。
		logger.Info("main", "客户端断线", "username", a.Username, "nick", a.Nick, "sessionID", a.SessionID)
		stillOnline := presence.Remove(a.ID, c.ID())
		if !stillOnline {
			presence.Broadcast(srv, store, c, false)
		}
	}
}
