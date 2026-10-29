// Package mailholiday 是「邮件配置 / 假期 / 管理后台用户管理」socket 事件处理器，
// 1:1 复刻自源仓库 server/src/handlers/mail.ts、holiday.ts、admin-users.ts。
//
// 本分三个文件：
//   - mail.go：主邮件配置读取/设置（mail:getMainConfig / mail:setMainConfig，库为权威）+ 首启播种；
//     SERVER_ADMIN 判定收敛到唯一入口 admin.IsServerAdminByUsername（见 admin/perm.go）；
//   - holiday.go：假期 CRUD（holiday:list/add/remove/addMany/removeMany/reset，变更广播 holiday:updated）
//   - 导出 SeedHolidays 播种内置法定假期；
//   - admin.go：/view/admin 后台用户/公司/设备管理（admin:listUsers/updateUser/setServerAdmin/listCompanies/listDevices）。
//
// 行为对齐要点：
//   - 事件名、字段名（camelCase）、ack 信封（util.OK/Fail 扁平）与 TS 逐字一致；
//   - SERVER_ADMIN 基于用户名（大小写不敏感）判定，判据 = config.ServerAdmins ∪ server_admins 表，
//     表为权威、重启保留（admin:setServerAdmin 写表，见 admin.go）；
//   - 错误文案与 TS 逐字一致；存储层意外错误必打日志、绝不吞掉；邮件密码绝不进日志、绝不外泄。
//
// 注册方式：RegisterMail / RegisterHoliday / RegisterAdminUsers；store 必须是基础层 db 单例，
// 本包不新建连接。
package mailholiday

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"mochi-oa-server-go/internal/config"
	"mochi-oa-server-go/internal/db"
	"mochi-oa-server-go/internal/handlers/admin"
	"mochi-oa-server-go/internal/logger"
	"mochi-oa-server-go/internal/socketapi"
	"mochi-oa-server-go/internal/types"
	"mochi-oa-server-go/internal/util"
)

// 日志模块前缀（硬性约束：统一 internal/logger）。
const mod = "handlers/mailholiday"

// ==================== SERVER_ADMIN 判定 ====================

// SERVER_ADMIN 判定收敛到唯一入口 admin.IsServerAdminByUsername（需求 8/9/10）：
// 判据 = 环境种子 config.ServerAdmins ∪ server_admins 表，表为权威、重启保留。
// 本包不再维护运行时内存集合（setServerAdmin 写表，见 admin.go）。

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
		isAdmin := admin.IsServerAdminByUsername(auth.Username, cfg, store)

		// 库为权威（需求确认）：先读 DB 默认主配置；读不到才回退 .env 种子（source=env）。
		// 一旦首启播种 .env 进 mail_configs，之后以库为准、后台可改、重启保留。
		var source string
		var view any
		mc, err := store.GetDefaultMailConfig(context.Background(), db.MailConfigScope{})
		if err != nil {
			logger.Error(mod, "mail:getMainConfig 查询主邮件配置失败", "error", err.Error())
			ack(util.Fail(err.Error()))
			return
		}
		if mc != nil {
			source = "db"
			view = viewMailConfig(mc)
		} else if cfg.Mail != nil && cfg.Mail.Enabled {
			source = "env"
			view = envConfigToView(cfg.Mail)
		} else {
			source = "none"
			view = nil
		}
		ack(util.OK(map[string]any{
			"serverAdmin": isAdmin,
			"source":      source, // 'db' | 'env' | 'none'
			// 仅库配置（source=db）时才允许客户端设置；环境种子不可在后台改。
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
		if !admin.IsServerAdminByUsername(auth.Username, cfg, store) {
			ack(util.Fail("需要 SERVER_ADMIN 权限"))
			return
		}

		// 库为权威：SERVER_ADMIN 直接写入 mail_configs 主配置（覆盖 .env 种子，重启保留）。

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
