// Package config 是服务端配置加载器，逐项移植自源仓库 server/src/config.ts。
//
// 加载顺序：先读工作目录下的 .env（godotenv，文件不存在则跳过不报错），
// 再用真实环境变量覆盖（os.Getenv 优先，与 dotenv 注入 process.env 后再读的语义一致）。
//
// 全部字段语义、默认值、解析规则与 config.ts 1:1 对齐，包括：
//   - bool 解析：'1'/'true'（不区分大小写）为 true，缺省取默认值；
//   - TRANSPORTS 逗号解析、ICE_SERVERS JSON 或逗号解析；
//   - CORS_ORIGIN '*' 或逗号列表；
//   - SSL_KEY+SSL_CERT 同时存在才启用 HTTPS；
//   - SERVER_ADMIN_USERS 为空回退 ADMIN_USER；
//   - MAIL_HOST 存在即启用环境级主邮件；
//   - AFDIAN_USER_ID 存在即启用爱发电；
//   - FILE_BASE_URL 缺省时拼 http://{HOST}:{PORT} 并去掉尾部斜杠。
package config

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/joho/godotenv"

	"mochi-oa-server-go/internal/logger"
)

// Transport 是 socket.io 传输方式。
type Transport = string

// IceServer 是 WebRTC ICE 服务器（stun/turn），下发给客户端建 P2P 用。
type IceServer struct {
	URLs      any      `json:"urls"`              // string 或 []string
	Username  string   `json:"username,omitempty"` // turn 鉴权用户名
	Credential string `json:"credential,omitempty"` // turn 鉴权凭证
}

// HTTPSConfig 是直接提供 HTTPS 时的证书；nil 表示 HTTP。
type HTTPSConfig struct {
	Key  string
	Cert string
}

// MySQLConfig 是 MySQL 连接配置。
type MySQLConfig struct {
	Host     string
	Port     int
	User     string
	Password string
	Database string
	// AutoCreateDatabase 为 true 时启动阶段自动建库（CREATE DATABASE IF NOT EXISTS），
	// 对应环境变量 AUTO_CREATE_DATABASE，默认开启。
	AutoCreateDatabase bool
	// AutoMigrate 为 true 时启动阶段用 GORM AutoMigrate 建缺失的表/列/索引，
	// 对应环境变量 AUTO_MIGRATE，默认开启。
	AutoMigrate bool
}

// CloudflareTURNConfig 是 Cloudflare TURN 动态取 ICE 配置；nil 表示未启用。
type CloudflareTURNConfig struct {
	Enabled bool
	KeyID   string
	Token   string
	TTL     int // 凭证有效期（秒），默认 86400
}

// FileConfig 是本地文件存储配置。
type FileConfig struct {
	Dir           string
	PublicBase    string // 公开访问源（origin，不带 /files）
	MaxBytes      int64  // 单文件大小上限（字节）
	UseRelativeURL bool  // true=返回 /files/xxx 相对路径
}

// AdminConfig 是初始管理员（首次启动播种）；Username/Password 空串表示未配置。
type AdminConfig struct {
	Username string
	Password string
}

// ServerMailConfig 是服务器级主邮件配置（来自 .env/Docker）。
type ServerMailConfig struct {
	Host      string
	Port      int
	Secure    bool // true=SSL/TLS 直连(465)；false=明文/STARTTLS
	User      string
	Password  string
	FromEmail string // 发信地址，缺省用 User
	FromName  string // 发件显示名
	Enabled   bool
}

// AfdianConfig 是爱发电开放平台配置；UserID 存在即启用。
type AfdianConfig struct {
	UserID    string
	Token     string // 敏感值，绝不进日志
	PublicKey string // webhook 验签公钥 PEM
}

// ServerConfig 是全局配置根结构（对应 config.ts 的 ServerConfig）。
type ServerConfig struct {
	Host           string
	Port           int
	HTTPS          *HTTPSConfig // nil=HTTP
	CORSStar       bool         // true=CORSOrigin 为 '*'；false 时用 CORSList
	CORSList       []string     // 逗号分隔的来源列表（CORSStar=false 时有效）
	Transports     []Transport
	TrustProxy     bool
	Path           string
	Storage        string // "mysql" | "memory"
	MySQL          MySQLConfig
	JWTSecret      string
	JWTExpiresIn   string
	IceServers     []IceServer
	IceCloudflare  *CloudflareTURNConfig // nil=未启用
	File           FileConfig
	Admin          AdminConfig
	ServerAdmins   []string
	Mail           *ServerMailConfig // nil=未配置环境级主邮件
	Afdian         *AfdianConfig     // nil=未启用
}

var (
	once sync.Once
	cfg  *ServerConfig
)

// Load 返回全局配置单例（sync.Once，只加载一次）。
// 先读 .env（不存在不报错），再叠加真实环境变量。
func Load() *ServerConfig {
	once.Do(func() {
		// godotenv 默认不覆盖已有环境变量；文件不存在时吞掉错误（与 dotenv 静默语义一致）。
		_ = godotenv.Load()
		cfg = build()
		logger.Info("config", "配置加载完成",
			"host", cfg.Host, "port", cfg.Port, "storage", cfg.Storage,
			"cors_star", cfg.CORSStar, "https", cfg.HTTPS != nil,
			"mail", cfg.Mail != nil, "afdian", cfg.Afdian != nil)
	})
	return cfg
}

// Get 是 Load 的别名，便于已加载场景直接取单例。
func Get() *ServerConfig { return Load() }

// boolEnv 解析布尔环境变量：未设置取默认值；否则 '1' 或 'true'（不区分大小写）为 true。
func boolEnv(key string, dft bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return dft
	}
	if v == "1" {
		return true
	}
	return strings.ToLower(v) == "true"
}

// intEnv 解析整数环境变量：未设置/解析失败取默认值（对齐 Number(undefined)||默认）。
func intEnv(key string, dft int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return dft
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return dft
	}
	return n
}

// strEnv 读取字符串环境变量，未设置取默认值。
func strEnv(key, dft string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return dft
}

// transports 解析 TRANSPORTS：默认 websocket,polling；过滤非法值；空则回退默认。
func transports() []Transport {
	raw := strEnv("TRANSPORTS", "websocket,polling")
	parts := strings.Split(raw, ",")
	out := make([]Transport, 0, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t == "websocket" || t == "polling" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return []Transport{"websocket", "polling"}
	}
	return out
}

// iceServers 解析 ICE_SERVERS：JSON 数组 或 逗号分隔 stun/turn URL；默认公共 STUN。
func iceServers() []IceServer {
	raw := strings.TrimSpace(os.Getenv("ICE_SERVERS"))
	dft := []IceServer{{URLs: "stun:stun.l.google.com:19302"}}
	if raw == "" {
		return dft
	}
	if strings.HasPrefix(raw, "[") {
		var arr []IceServer
		if err := json.Unmarshal([]byte(raw), &arr); err != nil || len(arr) == 0 {
			return dft
		}
		return arr
	}
	parts := strings.Split(raw, ",")
	out := make([]IceServer, 0, len(parts))
	for _, u := range parts {
		u = strings.TrimSpace(u)
		if u != "" {
			out = append(out, IceServer{URLs: u})
		}
	}
	if len(out) == 0 {
		return dft
	}
	return out
}

// serverAdmins 取服务器管理员用户名列表：SERVER_ADMIN_USERS 优先，为空回退 ADMIN_USER。
func serverAdmins() []string {
	raw := strings.TrimSpace(os.Getenv("SERVER_ADMIN_USERS"))
	var list []string
	if raw != "" {
		for _, u := range strings.Split(raw, ",") {
			if u = strings.TrimSpace(u); u != "" {
				list = append(list, u)
			}
		}
	} else if admin := strings.TrimSpace(os.Getenv("ADMIN_USER")); admin != "" {
		list = []string{admin}
	}
	// 去重（对齐 new Set(list)）
	seen := map[string]struct{}{}
	out := make([]string, 0, len(list))
	for _, u := range list {
		if _, dup := seen[u]; !dup {
			seen[u] = struct{}{}
			out = append(out, u)
		}
	}
	return out
}

// serverMail 构造环境级主邮件：MAIL_HOST 存在即启用。
func serverMail() *ServerMailConfig {
	host := strings.TrimSpace(os.Getenv("MAIL_HOST"))
	if host == "" {
		return nil
	}
	user := strings.TrimSpace(os.Getenv("MAIL_USER"))
	return &ServerMailConfig{
		Host:      host,
		Port:      intEnv("MAIL_PORT", 465),
		Secure:    boolEnv("MAIL_SECURE", true),
		User:      user,
		Password:  os.Getenv("MAIL_PASSWORD"),
		FromEmail: orDefault(strings.TrimSpace(os.Getenv("MAIL_FROM")), user),
		FromName:  strings.TrimSpace(os.Getenv("MAIL_FROM_NAME")),
		Enabled:   boolEnv("MAIL_ENABLED", true),
	}
}

// afdianConfig 构造爱发电配置：AFDIAN_USER_ID 存在即启用。
func afdianConfig() *AfdianConfig {
	uid := strings.TrimSpace(os.Getenv("AFDIAN_USER_ID"))
	if uid == "" {
		return nil
	}
	return &AfdianConfig{
		UserID:    uid,
		Token:     os.Getenv("AFDIAN_TOKEN"),
		PublicKey: strings.TrimSpace(os.Getenv("AFDIAN_WEBHOOK_PUBLIC_KEY")),
	}
}

// orDefault 空串回退到备用值。
func orDefault(v, dft string) string {
	if v == "" {
		return dft
	}
	return v
}

// build 组装完整配置（对应 loadConfig）。
func build() *ServerConfig {
	host := strEnv("HOST", "0.0.0.0")
	// 端口默认 3001（用户要求默认端口；源码硬编码回退为 3000，这里按需求改为 3001）。
	port := intEnv("PORT", 3001)

	// CORS：未设置 或 '*' → 通配；否则逗号列表。
	corsRaw, corsSet := os.LookupEnv("CORS_ORIGIN")
	corsStar := true
	var corsList []string
	if corsSet && corsRaw != "*" && corsRaw != "" {
		corsStar = false
		for _, s := range strings.Split(corsRaw, ",") {
			if s = strings.TrimSpace(s); s != "" {
				corsList = append(corsList, s)
			}
		}
	}

	// HTTPS：SSL_KEY 与 SSL_CERT 同时存在才启用。
	var httpsCfg *HTTPSConfig
	if k, c := os.Getenv("SSL_KEY"), os.Getenv("SSL_CERT"); k != "" && c != "" {
		httpsCfg = &HTTPSConfig{Key: k, Cert: c}
	}

	// Cloudflare TURN：ICE_USE_CLOUDFLARE 开启 且 token 存在才启用。
	var iceCF *CloudflareTURNConfig
	if boolEnv("ICE_USE_CLOUDFLARE", false) && os.Getenv("CLOUDFLARE_TURN_TOKEN") != "" {
		iceCF = &CloudflareTURNConfig{
			Enabled: true,
			KeyID:   strings.TrimSpace(os.Getenv("CLOUDFLARE_TURN_KEY_ID")),
			Token:   os.Getenv("CLOUDFLARE_TURN_TOKEN"),
			TTL:     intEnv("ICE_SERVERS_TOKEN_SECOND", 86400),
		}
	}

	// FILE_BASE_URL 缺省拼 http://{HOST}:{PORT}，并去掉尾部斜杠。
	publicBase := strEnv("FILE_BASE_URL", "")
	if publicBase == "" {
		publicBase = "http://" + host + ":" + strconv.Itoa(port)
	}
	publicBase = strings.TrimRight(publicBase, "/")

	// 只有 MySQL 一种存储实现（memory 模式已移除）
	storage := "mysql"

	return &ServerConfig{
		Host:          host,
		Port:          port,
		HTTPS:         httpsCfg,
		CORSStar:      corsStar,
		CORSList:      corsList,
		Transports:    transports(),
		TrustProxy:    boolEnv("TRUST_PROXY", false),
		Path:          strEnv("SOCKET_PATH", "/socket.io"),
		Storage:       storage,
		MySQL: MySQLConfig{
			Host:               strEnv("MYSQL_HOST", "127.0.0.1"),
			Port:               intEnv("MYSQL_PORT", 3306),
			User:               strEnv("MYSQL_USER", "root"),
			Password:           os.Getenv("MYSQL_PASSWORD"),
			Database:           strEnv("MYSQL_DATABASE", "mochioa"),
			AutoCreateDatabase: boolEnv("AUTO_CREATE_DATABASE", true),
			AutoMigrate:        boolEnv("AUTO_MIGRATE", true),
		},
		JWTSecret:    strEnv("JWT_SECRET", "change-me-in-production"),
		JWTExpiresIn: strEnv("JWT_EXPIRES_IN", "30d"),
		IceServers:   iceServers(),
		IceCloudflare: iceCF,
		File: FileConfig{
			Dir:            strEnv("FILE_DIR", "./data"),
			PublicBase:     publicBase,
			MaxBytes:       int64(intEnv("FILE_MAX_BYTES", 20*1024*1024)),
			UseRelativeURL: boolEnv("USE_RELATIVE_FILE_URL", true),
		},
		Admin: AdminConfig{
			Username: os.Getenv("ADMIN_USER"),
			Password: os.Getenv("ADMIN_PASSWORD"),
		},
		ServerAdmins: serverAdmins(),
		Mail:         serverMail(),
		Afdian:       afdianConfig(),
	}
}
