// Package mailholiday 是「邮件配置 / 假期 / 管理后台用户管理」socket 事件处理器，
// 1:1 复刻自源仓库 server/src/handlers/mail.ts、holiday.ts、admin-users.ts。
//
// 本分三个文件：
//   - mail.go：主邮件配置读取/设置（mail:getMainConfig / mail:setMainConfig）+
//     SERVER_ADMIN 判定与运行时覆盖注册表 serverAdminOverrides；
//   - holiday.go：假期 CRUD（holiday:list/add/remove/addMany/removeMany/reset，变更广播 holiday:updated）
//   - 导出 SeedHolidays 播种内置法定假期；
//   - admin.go：/view/admin 后台用户/公司/设备管理（admin:listUsers/updateUser/setServerAdmin/listCompanies/listDevices）。
//
// 行为对齐要点：
//   - 事件名、字段名（camelCase）、ack 信封（util.OK/Fail 扁平）与 TS 逐字一致；
//   - SERVER_ADMIN 基于用户名（大小写不敏感）判定，名单来自 config.ServerAdmins
//     （空时已由 config 回退播种管理员 ADMIN_USER），叠加运行时覆盖 serverAdminOverrides；
//   - 错误文案与 TS 逐字一致；存储层意外错误必打日志、绝不吞掉；邮件密码绝不进日志、绝不外泄。
//
// 注册方式：RegisterMail / RegisterHoliday / RegisterAdminUsers；store 必须是基础层 db 单例，
// 本包不新建连接；唯一进程内注册表为 serverAdminOverrides（并发安全）。
package mailholiday

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"mochi-oa-server-go/internal/config"
	"mochi-oa-server-go/internal/db"
	"mochi-oa-server-go/internal/logger"
	"mochi-oa-server-go/internal/socketapi"
	"mochi-oa-server-go/internal/types"
	"mochi-oa-server-go/internal/util"
)

// 日志模块前缀（硬性约束：统一 internal/logger）。
const mod = "handlers/mailholiday"

// ==================== SERVER_ADMIN 判定与运行时覆盖（对标 mail.ts） ====================

// serverAdminOverrides 运行时 SERVER_ADMIN 覆盖集合（对标 mail.ts: export const serverAdminOverrides = new Set<string>()）。
// 由 /view/admin 用户管理面板动态增删（admin:setServerAdmin）；重启后回退 config.ServerAdmins。
// key 一律为小写用户名（与 TS 的 set/add(target.username.toLowerCase()) 对齐）。
// 包级并发安全注册表：admin 面板多连接并发操作时用 RWMutex 保护。
var serverAdminOverrides = struct {
	sync.RWMutex
	names map[string]struct{}
}{names: map[string]struct{}{}}

// isServerAdminUsername 判定用户名是否 SERVER_ADMIN（对标 mail.ts: isServerAdmin）。
// 判定规则：config.ServerAdmins 名单（大小写不敏感）命中，或运行时覆盖集合命中，二者其一即可；
// 与公司/群角色无关。config.ServerAdmins 为空的回退语义已由 config 层处理（回退 ADMIN_USER）。
func isServerAdminUsername(username string, cfg *config.ServerConfig) bool {
	n := strings.ToLower(username)
	for _, u := range cfg.ServerAdmins {
		if strings.ToLower(u) == n {
			return true
		}
	}
	serverAdminOverrides.RLock()
	_, ok := serverAdminOverrides.names[n]
	serverAdminOverrides.RUnlock()
	return ok
}

// addOverride 把用户名加入运行时管理员覆盖集合（小写存储）。
func addOverride(username string) {
	serverAdminOverrides.Lock()
	serverAdminOverrides.names[strings.ToLower(username)] = struct{}{}
	serverAdminOverrides.Unlock()
}

// deleteOverride 从运行时覆盖集合移除用户名；config 里的保留不动（对标 TS 注释：取消管理员仅移除运行时覆盖）。
func deleteOverride(username string) {
	serverAdminOverrides.Lock()
	delete(serverAdminOverrides.names, strings.ToLower(username))
	serverAdminOverrides.Unlock()
}

// ==================== 内部小工具 ====================

// authedUser 取连接上的完整登录身份（对标 TS: socket.data.auth）。
// 认证中间件以 *types.Auth / types.Auth 写入 conn.Set("auth", ...)。
func authedUser(c socketapi.Conn) (*types.Auth, bool) {
	v := c.Get("auth")
	switch a := v.(type) {
	case *types.Auth:
		if a != nil {
			return a, true
		}
	case types.Auth:
		cp := a
		return &cp, true
	}
	return nil, false
}

// parseData 把事件负载 JSON 字节解析为 map；null/空/非法一律给空 map（对标 TS: data ?? {}）。
func parseData(raw json.RawMessage) map[string]any {
	m := map[string]any{}
	if len(raw) == 0 {
		return m
	}
	_ = json.Unmarshal(raw, &m)
	return m
}

// asBool 对标 TS Boolean(v)：true / 非零数字 / 非空字符串 为真。
func asBool(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case float64:
		return b != 0
	case string:
		return b != ""
	}
	return false
}

// emailRe 发信邮箱格式（与 mail.ts: /^[^@\s]+@[^@\s]+\.[^@\s]+$/ 逐字一致）。
var emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// dateRe 日期格式（与 holiday.ts: /^\d{4}-\d{2}-\d{2}$/ 逐字一致）。
var dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// holidayTypes 合法假期类型白名单（与 holiday.ts: ['legal','workday','custom'] 一致）。
var holidayTypes = map[string]struct{}{"legal": {}, "workday": {}, "custom": {}}

// viewMailConfig 对外展示的配置视图（对标 mail.ts: viewMailConfig，隐藏 SMTP 密码明文，字段集合逐字一致）。
func viewMailConfig(c *types.MailConfig) map[string]any {
	return map[string]any{
		"email":       c.Email,
		"displayName": c.DisplayName,
		"host":        c.Host,
		"port":        c.Port,
		"secure":      c.Secure,
		"user":        c.User,
		"enabled":     c.Enabled,
		"isDefault":   c.IsDefault,
		"password":    "••••••••",
	}
}

// envConfigToMail 把环境级主邮件配置映射为对外视图（对标 mailer.ts: envConfigToMail，字段一致）。
// 注意：这是对外展示结构，Password 绝不参与（viewMailConfig 固定打码）。
func envConfigToView(c *config.ServerMailConfig) map[string]any {
	from := c.FromEmail
	if from == "" {
		from = c.User
	}
	return map[string]any{
		"email":       from,
		"displayName": c.FromName,
		"host":        c.Host,
		"port":        int64(c.Port),
		"secure":      c.Secure,
		"user":        c.User,
		"enabled":     true,
		"isDefault":   true,
		"password":    "••••••••",
	}
}

// ==================== 邮件配置（对标 mail.ts: registerMailHandlers） ====================

// RegisterMail 注册 mail:* 事件处理器（对标 TS registerMailHandlers(ctx)，ctx.io/store/config 由参数注入）。
// 说明：TS 的 ctx.mailer 仅用于 getMainMail()，本包直接用 config+store 复刻该逻辑（见 mail:getMainConfig），
// 故不接收 mailer 参数；发信能力由 internal/mailer 单例提供给业务模块，不在本 handler 范围内。
func RegisterMail(srv socketapi.Server, store db.Store, cfg *config.ServerConfig) {
	// ---- 读取主邮件配置（含当前用户是否 SERVER_ADMIN） ----
	srv.On("mail:getMainConfig", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		auth, ok := authedUser(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		isAdmin := isServerAdminUsername(auth.Username, cfg)

		// 对标 mailer.ts: getMainMail()：环境级主邮件（.env/Docker）第一优先级；否则读 DB 默认配置。
		var source string
		var view any
		if cfg.Mail != nil && cfg.Mail.Enabled {
			source = "env"
			view = envConfigToView(cfg.Mail)
		} else {
			source = "db"
			mc, err := store.GetDefaultMailConfig(context.Background(), db.MailConfigScope{})
			if err != nil {
				logger.Error(mod, "mail:getMainConfig 查询主邮件配置失败", "error", err.Error())
				ack(util.Fail(err.Error()))
				return
			}
			if mc != nil {
				view = viewMailConfig(mc)
			} else {
				view = nil
			}
		}
		ack(util.OK(map[string]any{
			"serverAdmin": isAdmin,
			"source":      source, // 'env' | 'db'
			// 仅环境未配置（source=db）时才允许客户端设置
			"canEdit": isAdmin && source == "db",
			"config":  view,
		}))
	})

	// ---- 设置主邮件配置（需 SERVER_ADMIN；仅当环境未配置主邮件时生效） ----
	srv.On("mail:setMainConfig", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		auth, ok := authedUser(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		if !isServerAdminUsername(auth.Username, cfg) {
			ack(util.Fail("需要 SERVER_ADMIN 权限"))
			return
		}

		// 环境已配置主邮件：客户端设置不生效（第一优先级是环境配置）。
		if cfg.Mail != nil && cfg.Mail.Enabled {
			ack(util.Fail("主邮件已由服务器环境配置，请在 .env / Docker 中修改"))
			return
		}

		d := parseData(raw)
		email := strings.TrimSpace(str(d["email"]))
		host := strings.TrimSpace(str(d["host"]))
		if email == "" || host == "" {
			ack(util.Fail("发信邮箱与 SMTP 主机必填"))
			return
		}
		if !emailRe.MatchString(email) {
			ack(util.Fail("发信邮箱不合法"))
			return
		}
		// 对标 TS: Number(d.port ?? 465)，必须是 1-65535 的整数。
		port, portOK := parsePort(d["port"])
		if !portOK {
			ack(util.Fail("SMTP 端口不合法"))
			return
		}
		user := strings.TrimSpace(str(d["user"]))
		if user == "" {
			user = email
		}
		password := str(d["password"])
		// 对标 TS: d.secure === undefined ? true : Boolean(d.secure)（缺省 true=SSL 直连 465）。
		secure := true
		if v, present := d["secure"]; present {
			secure = asBool(v)
		}
		displayName := strings.TrimSpace(str(d["displayName"]))
		if len(displayName) > 128 {
			displayName = displayName[:128]
		}

		// 幂等：更新同邮箱主配置，或新增（对标 TS: listMailConfigs({companyId:null}).find(c => c.email === email)）。
		existingList, err := store.ListMailConfigs(context.Background(), db.MailConfigScope{})
		if err != nil {
			logger.Error(mod, "mail:setMainConfig 查询现有主配置失败", "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		var existingID *int64
		var existingPassword string
		for _, c := range existingList {
			if c.Email == email {
				existingID = &c.ID
				existingPassword = c.Password
				break
			}
		}
		// 更新已有配置且密码留空 → 保留原 SMTP 密码。
		finalPassword := password
		if finalPassword == "" {
			finalPassword = existingPassword
		}
		if finalPassword == "" {
			ack(util.Fail("SMTP 密码必填（首次配置需填写）"))
			return
		}
		isDefault := true
		enabled := true
		priority := int64(10)
		_, err = store.SaveMailConfig(context.Background(), db.SaveMailConfigInput{
			ID:          existingID,
			CompanyID:   nil, // nil=主配置（全局）
			Email:       email,
			DisplayName: displayName,
			Host:        host,
			Port:        &port,
			Secure:      &secure,
			User:        user,
			Password:    finalPassword,
			IsDefault:   &isDefault,
			Enabled:     &enabled,
			Priority:    &priority,
		})
		if err != nil {
			logger.Error(mod, "mail:setMainConfig 保存主配置失败", "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		ack(util.OK(nil))
	})
}

// str 对标 TS: typeof v === 'string' ? v : ”（注意：不 trim，调用方按需 TrimSpace）。
func str(v any) string {
	s, _ := v.(string)
	return s
}

// strTrim 对标 TS: typeof v === 'string' ? v.trim() : ”。
func strTrim(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// strCut 对标 TS: String(v ?? ”).trim().slice(0, max)（admin:updateUser 的 patch 字段用）。
func strCut(v any, max int) string {
	var s string
	if v != nil {
		s = fmt.Sprint(v)
	}
	s = strings.TrimSpace(s)
	if len(s) > max {
		s = s[:max]
	}
	return s
}

// parsePort 对标 TS: Number(d.port ?? 465) 且 Number.isInteger(port) && 1<=port<=65535。
// 缺省（nil）→ 465；非有限整数或越界 → (0, false)。
func parsePort(v any) (int64, bool) {
	if v == nil {
		return 465, true
	}
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case float32:
		f = float64(n)
	case int:
		f = float64(n)
	case int64:
		f = float64(n)
	case string:
		x, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		if err != nil || math.IsNaN(x) || math.IsInf(x, 0) {
			return 0, false
		}
		f = x
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) || f < 1 || f > 65535 {
		return 0, false
	}
	return int64(f), true
}
