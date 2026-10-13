// admin.go —— /view/admin 管理后台用户/公司/设备管理 handler，
// 1:1 复刻自 server/src/handlers/admin-users.ts。
//
// 全部事件要求 SERVER_ADMIN（服务端兜底判定，与 TS 一致）：
//   - admin:listUsers      全部用户（含所属公司 + serverAdmin 标记）
//   - admin:updateUser     修改用户资料（昵称/头像/手机/邮箱），邮箱查重
//   - admin:setServerAdmin 动态设置/取消某用户的 SERVER_ADMIN（运行时覆盖 serverAdminOverrides，重启回退 config）
//   - admin:listCompanies 全部公司（含成员与角色）
//   - admin:listDevices    已登录设备列表（本机 + 在线远程客户端；远程设备注册表 dev.ts 未移植，见文末说明）
package mailholiday

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"runtime"
	"sort"
	"strings"

	"mochi-oa-server-go/internal/config"
	"mochi-oa-server-go/internal/db"
	"mochi-oa-server-go/internal/handlers/rtcdev"
	"mochi-oa-server-go/internal/logger"
	"mochi-oa-server-go/internal/socketapi"
	"mochi-oa-server-go/internal/util"
)

// RegisterAdminUsers 注册 admin:* 事件处理器（对标 TS registerAdminUsersHandlers(ctx)，ctx.io/store/config 由参数注入）。
func RegisterAdminUsers(srv socketapi.Server, store db.Store, cfg *config.ServerConfig) {
	// ---- 全部用户列表 ----
	srv.On("admin:listUsers", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		auth, ok := authedUser(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		if !isServerAdminUsername(auth.Username, cfg) {
			ack(util.Fail("需要 SERVER_ADMIN 权限"))
			return
		}
		users, err := store.ListUsers(context.Background())
		if err != nil {
			logger.Error(mod, "admin:listUsers 查询用户失败", "error", err.Error())
			ack(util.Fail("查询用户失败：" + err.Error()))
			return
		}
		enriched := make([]any, 0, len(users))
		for _, usr := range users {
			// 对标 TS: getUserCompanies 失败 → companies=[]。
			companies := []any{}
			if mems, err := store.GetUserCompanies(context.Background(), usr.ID); err == nil {
				for _, m := range mems {
					companies = append(companies, map[string]any{
						"id":   m.Company.ID,
						"name": m.Company.Name,
						"role": m.Role,
					})
				}
			}
			enriched = append(enriched, map[string]any{
				"id":          usr.ID,
				"username":    usr.Username,
				"nick":        usr.Nick,
				"avatar":      usr.Avatar,
				"phone":       usr.Phone,
				"email":       usr.Email,
				"extra":       usr.Extra,
				"createdAt":   usr.CreatedAt,
				"serverAdmin": isServerAdminUsername(usr.Username, cfg),
				"companies":   companies,
			})
		}
		ack(util.OK(map[string]any{"users": enriched}))
	})

	// ---- 修改用户资料（昵称/头像/手机/邮箱），邮箱查重 ----
	srv.On("admin:updateUser", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		auth, ok := authedUser(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		if !isServerAdminUsername(auth.Username, cfg) {
			ack(util.Fail("需要 SERVER_ADMIN 权限"))
			return
		}
		d := parseData(raw)
		// 对标 TS: Number(d.userId) 且必须为正整数。
		userId, uidOK := asIntStrict(d["userId"])
		if !uidOK {
			ack(util.Fail("参数不合法"))
			return
		}
		// 对标 TS: d.patch && typeof d.patch === 'object' ? d.patch : {}。
		p, _ := d["patch"].(map[string]any)
		patch := db.UserProfilePatch{}
		hasAny := false
		if _, present := p["nick"]; present {
			s := strCut(p["nick"], 32)
			patch.Nick = &s
			hasAny = true
		}
		if _, present := p["avatar"]; present {
			s := strCut(p["avatar"], 500)
			patch.Avatar = &s
			hasAny = true
		}
		if _, present := p["phone"]; present {
			s := strCut(p["phone"], 32)
			patch.Phone = &s
			hasAny = true
		}
		if _, present := p["email"]; present {
			s := strCut(p["email"], 120)
			patch.Email = &s
			hasAny = true
		}
		if !hasAny {
			ack(util.Fail("没有可更新的字段"))
			return
		}
		// 邮箱查重：仅当新邮箱非空且与目标用户当前邮箱不同时检查。
		if patch.Email != nil && *patch.Email != "" {
			target, err := store.GetUserByID(context.Background(), userId)
			if err != nil {
				logger.Error(mod, "admin:updateUser 查询目标用户失败", "userId", userId, "error", err.Error())
				ack(util.Fail("更新失败：" + err.Error()))
				return
			}
			if target != nil && target.Email != *patch.Email {
				byEmail, err := store.GetUserByEmail(context.Background(), *patch.Email)
				// 对标 TS: getUserByEmail(...).catch(() => null)——查不到/出错都视为未占用。
				if err == nil && byEmail != nil && byEmail.ID != userId {
					ack(util.Fail("该邮箱已被其他账号绑定"))
					return
				}
			}
		}
		if err := store.UpdateUserProfile(context.Background(), userId, patch); err != nil {
			logger.Error(mod, "admin:updateUser 更新失败", "userId", userId, "error", err.Error())
			ack(util.Fail("更新失败：" + err.Error()))
			return
		}
		ack(util.OK(nil))
	})

	// ---- 动态设置/取消 SERVER_ADMIN（运行时覆盖，重启回退 config） ----
	srv.On("admin:setServerAdmin", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		auth, ok := authedUser(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		if !isServerAdminUsername(auth.Username, cfg) {
			ack(util.Fail("需要 SERVER_ADMIN 权限"))
			return
		}
		d := parseData(raw)
		username := strings.TrimSpace(fmt.Sprint(d["username"]))
		if username == "" {
			ack(util.Fail("缺少用户名"))
			return
		}
		admin := asBool(d["admin"])
		target, err := store.GetUserByUsername(context.Background(), username)
		// 对标 TS: getUserByUsername(...).catch(() => null)。
		if err != nil || target == nil {
			ack(util.Fail("用户不存在：" + username))
			return
		}
		if admin {
			// 已是 config 名单成员则无需加入覆盖集合（幂等）。
			if !isServerAdminUsername(target.Username, cfg) {
				addOverride(target.Username)
			}
		} else {
			// 取消管理员：仅移除运行时覆盖（config 里的保留）。
			deleteOverride(target.Username)
		}
		ack(util.OK(map[string]any{"admin": admin}))
	})

	// ---- 全部公司（含成员与角色） ----
	srv.On("admin:listCompanies", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		auth, ok := authedUser(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		if !isServerAdminUsername(auth.Username, cfg) {
			ack(util.Fail("需要 SERVER_ADMIN 权限"))
			return
		}
		users, err := store.ListUsers(context.Background())
		if err != nil {
			logger.Error(mod, "admin:listCompanies 查询用户失败", "error", err.Error())
			ack(util.Fail("查询公司失败：" + err.Error()))
			return
		}
		// byCompany: companyID → {id,name,members}。单个用户出错跳过（对标 TS try/catch）。
		type companyAgg struct {
			id      int64
			name    string
			members []any
		}
		byCompany := map[int64]*companyAgg{}
		for _, usr := range users {
			mems, err := store.GetUserCompanies(context.Background(), usr.ID)
			if err != nil {
				continue
			}
			for _, m := range mems {
				co, ok := byCompany[m.Company.ID]
				if !ok {
					co = &companyAgg{id: m.Company.ID, name: m.Company.Name, members: []any{}}
					byCompany[m.Company.ID] = co
				}
				co.members = append(co.members, map[string]any{
					"userId":   usr.ID,
					"username": usr.Username,
					"nick":     usr.Nick,
					"avatar":   usr.Avatar,
					"role":     m.Role,
				})
			}
		}
		// 按 companyID 排序输出（对标 TS Map 插入序；Go map 无序，排序保证稳定可复现）。
		ids := make([]int, 0, len(byCompany))
		for id := range byCompany {
			ids = append(ids, int(id))
		}
		sort.Ints(ids)
		companies := make([]any, 0, len(ids))
		for _, id := range ids {
			co := byCompany[int64(id)]
			companies = append(companies, map[string]any{
				"id":      co.id,
				"name":    co.name,
				"members": co.members,
			})
		}
		ack(util.OK(map[string]any{"companies": companies}))
	})

	// ---- 已登录设备列表（电脑控制九宫格：服务器本机 + 在线远程客户端） ----
	srv.On("admin:listDevices", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		auth, ok := authedUser(c)
		if !ok {
			ack(util.Fail("未登录"))
			return
		}
		if !isServerAdminUsername(auth.Username, cfg) {
			ack(util.Fail("需要 SERVER_ADMIN 权限"))
			return
		}
		// 本机 IPv4 非回退地址（对标 TS: os.networkInterfaces() 收集首个非 internal IPv4）。
		ips := []string{}
		if addrs, err := net.InterfaceAddrs(); err == nil {
			for _, a := range addrs {
				if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
					if v4 := ipnet.IP.To4(); v4 != nil {
						ips = append(ips, v4.String())
					}
				}
			}
		}
		ipStr := strings.Join(ips, "，")
		if ipStr == "" {
			ipStr = "127.0.0.1"
		}
		// 对标 TS: os.type() + ' ' + os.release()；Go 无直接等价 API，用 GOOS+架构 best-effort（见报告差异说明）。
		osStr := runtime.GOOS + " " + runtime.GOARCH
		devices := []any{
			map[string]any{
				"id":       "local",
				"name":     "本机（服务器）",
				"username": "SERVER_ADMIN",
				"os":       osStr,
				"ip":       ipStr,
				"online":   true,
				"remote":   false,
			},
		}
		// 在线远程客户端：合并 rtcdev 注册表快照（dev:register 上报的被控端，30s 心跳视为在线；
		// 字段 id/name/username/os/ip/online/remote 与 TS admin-users.ts 逐条对齐）。
		for _, d := range rtcdev.ListDevices() {
			devices = append(devices, map[string]any{
				"id":       d.DeviceID,
				"name":     d.Name,
				"username": d.Username,
				"os":       d.OS,
				"ip":       d.IP,
				"online":   d.Online,
				"remote":   true,
			})
		}
		ack(util.OK(map[string]any{"devices": devices}))
		logger.Info(mod, "admin:listDevices 返回",
			"devices", fmt.Sprintf("%v", devices))
	})
}

// asIntStrict 对标 TS: Number.isInteger(v) && v > 0 的严格正整数判定。
// JSON 数字经 encoding/json 解析为 float64；非有限/非整数/<=0 一律 (0,false)。
func asIntStrict(v any) (int64, bool) {
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
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) || f <= 0 {
		return 0, false
	}
	return int64(f), true
}
