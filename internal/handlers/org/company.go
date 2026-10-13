// Package org 是组织架构域的 socket 事件处理器集合。
//
// 本文件（company.go）1:1 复刻 server/src/handlers/company.ts（557 行）：
//   - 公司 CRUD：创建 / 加入（邀请码）/ 退出 / 解散 / 重命名 / 转让所有者
//   - 公司成员：列表 / 角色设置 / 移除 / 直接添加
//   - 部门：创建 / 列表 / 成员分配 / 移除 / 删除
//   - 入职流程：管理员邀请 → 用户同意/拒绝
//   - 成员人事信息：读取 / 更新
//
// 编码约定：
//   - 事件名、错误文案、广播负载与 TS 逐字一致；
//   - ack 为扁平 {ok:true, ...业务字段}，客户端 normalizeAck 只剥 ok/error；
//   - 所有 Store 调用用 context.Background()（无请求级超时需求）；
//   - 日志前缀 [handlers/org]，敏感值绝不进日志。
package org

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"mochi-oa-server-go/internal/db"
	"mochi-oa-server-go/internal/logger"
	"mochi-oa-server-go/internal/socketapi"
	"mochi-oa-server-go/internal/types"
	"mochi-oa-server-go/internal/util"
)

// 日志模块前缀（硬性约束）。
const mod = "handlers/org"

// ─── 共享校验工具（对标 company.ts 顶部的 isName / isCode / isCompanyRole / rank / idOf） ───

// isName 校验组织名/部门名/群名：非空 trim 后长度 > 0 且总长 ≤ 64。
func isName(v any) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	return len(strings.TrimSpace(s)) > 0 && len(s) <= 64
}

var codeRe = regexp.MustCompile(`^[A-Za-z0-9]{3,32}$`)

// isCode 校验加入码：3-32 位字母数字。
func isCode(v any) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	return codeRe.MatchString(s)
}

// isCompanyRole 校验公司角色字符串。
func isCompanyRole(v any) bool {
	s, ok := v.(string)
	return ok && (s == "owner" || s == "admin" || s == "member")
}

// roleRank 角色比较：owner(3) > admin(2) > member(1)。
func roleRank(role string) int {
	switch role {
	case "owner":
		return 3
	case "admin":
		return 2
	default:
		return 1
	}
}

// toInt64 把 JSON 反序列化后的 any 值转为 int64（对标 Number(v) + Number.isInteger 检查）。
// 第二返回值 false 表示不是合法整数（含 NaN/小数/无法解析的字符串）。
func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		if n != math.Trunc(n) {
			return 0, false
		}
		return int64(n), true
	case string:
		var i int64
		_, err := fmt.Sscanf(strings.TrimSpace(n), "%d", &i)
		if err != nil {
			return 0, false
		}
		return i, true
	default:
		return 0, false
	}
}

// parseData 把 json.RawMessage 解析为 map[string]any；空/null/解析失败返回空 map。
func parseData(data json.RawMessage) map[string]any {
	if len(data) == 0 {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return map[string]any{}
	}
	if m == nil {
		return map[string]any{}
	}
	return m
}

// getAuth 从连接取认证用户（对标 socket.data.auth）。未登录返回 nil。
func getAuth(c socketapi.Conn) *types.Auth {
	v := c.Get("auth")
	if v == nil {
		return nil
	}
	if a, ok := v.(*types.Auth); ok && a != nil {
		return a
	}
	return nil
}

// isOnline 检查用户是否有活跃连接（对标 presence.isOnline）。
// Go 侧无独立 presence 包，直接遍历 srv.Sockets() 判断。
func isOnline(srv socketapi.Server, userID int64) bool {
	for _, c := range srv.Sockets() {
		a := getAuth(c)
		if a != nil && a.ID == userID {
			return true
		}
	}
	return false
}

// joinUserSockets 让某用户的所有在线连接加入指定房间（对标 for (const s of io.sockets.sockets.values())）。
func joinUserSockets(srv socketapi.Server, userID int64, room string) {
	for _, c := range srv.Sockets() {
		a := getAuth(c)
		if a != nil && a.ID == userID {
			_ = c.Join(room)
		}
	}
}

// leaveUserSockets 让某用户的所有在线连接离开指定房间。
func leaveUserSockets(srv socketapi.Server, userID int64, room string) {
	for _, c := range srv.Sockets() {
		a := getAuth(c)
		if a != nil && a.ID == userID {
			_ = c.Leave(room)
		}
	}
}

// ─── 注册入口 ───

// RegisterCompanyHandlers 注册公司/部门/邀请/人事相关全部 socket 事件。
// 对标 registerCompanyHandlers(ctx)。
func RegisterCompanyHandlers(srv socketapi.Server) {
	store := db.GetStore()
	ctx := context.Background()

	srv.OnConnection(func(c socketapi.Conn) error {
		// 连接级初始化：无额外状态需要存（TS 侧 invites map 在 group.go）。
		return nil
	})

	// ---- 创建公司：创建人自动成为 owner ----
	srv.On("company:create", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		if !isName(d["name"]) {
			ack(util.Fail("组织名不合法"))
			return
		}
		if codeV, hasCode := d["code"]; hasCode && codeV != nil && !isCode(codeV) {
			ack(util.Fail("加入码需 3-32 位字母数字"))
			return
		}
		name := strings.TrimSpace(d["name"].(string))
		if existing, _ := store.GetCompanyByName(ctx, name); existing != nil {
			ack(util.Fail("已存在同名企业/团队"))
			return
		}

		providedCode, _ := d["code"].(string)
		codeProvided := providedCode != ""

		for attempt := 0; attempt < 5; attempt++ {
			code := providedCode
			if !codeProvided {
				code = util.MakeJoinCode()
			}
			company, err := store.CreateCompany(ctx, db.CreateCompanyInput{
				Name:    name,
				Code:    code,
				OwnerID: a.ID,
			})
			if err != nil {
				var ae *db.AlreadyExistsError
				if errors.As(err, &ae) {
					if !codeProvided {
						continue // 生成码冲突，换一个
					}
					ack(util.Fail("加入码已被占用"))
					return
				}
				logger.Error(mod, "创建公司失败", "error", err.Error(), "userId", a.ID)
				ack(util.Fail("创建失败"))
				return
			}
			_ = c.Join(util.CompanyRoom(company.ID))
			// 自动创建「全员群（总群）」：钉钉式，入职/入司自动加入
			for g := 0; g < 5; g++ {
				gcode := util.MakeJoinCode()
				group, gerr := store.CreateGroup(ctx, db.CreateGroupInput{
					CompanyID: company.ID,
					Name:      company.Name + "总群",
					Code:      gcode,
					OwnerID:   a.ID,
				})
				if gerr != nil {
					var gae *db.AlreadyExistsError
					if errors.As(gerr, &gae) {
						continue
					}
					break
				}
				_, _ = store.GetOrCreateGroupConversation(ctx, group.ID, a.ID)
				break
			}
			ack(util.OK(map[string]any{"company": company}))
			return
		}
		ack(util.Fail("加入码生成冲突，请重试"))
	})

	// ---- 我的公司列表 ----
	srv.On("company:list", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		raw, err := store.GetUserCompanies(ctx, a.ID)
		if err != nil {
			logger.Error(mod, "获取公司列表失败", "error", err.Error(), "userId", a.ID)
			ack(util.Fail("获取失败"))
			return
		}
		// 非管理员看不到加入码（code 置空）
		companies := make([]map[string]any, len(raw))
		for i, m := range raw {
			comp := m.Company
			isAdmin := m.Role == "owner" || m.Role == "admin"
			if !isAdmin {
				comp.Code = ""
			}
			companies[i] = map[string]any{
				"company": comp,
				"role":    m.Role,
			}
		}
		ack(util.OK(map[string]any{"companies": companies}))
	})

	// ---- 搜索用户（按用户名/昵称/ID，用于管理员添加成员候选） ----
	srv.On("user:search", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		kw := strings.TrimSpace(strVal(d, "keyword"))
		if kw == "" {
			ack(util.OK(map[string]any{"users": []any{}}))
			return
		}
		idMatch := int64(0)
		if n, ok := parseIntStrict(kw); ok && n > 0 {
			idMatch = n
		}
		all, err := store.ListUsers(ctx)
		if err != nil {
			logger.Error(mod, "列出用户失败", "error", err.Error())
			ack(util.Fail("搜索失败"))
			return
		}
		k := strings.ToLower(kw)
		type userItem struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
			Nick     string `json:"nick"`
			Avatar   string `json:"avatar"`
			Phone    string `json:"phone"`
		}
		users := []userItem{}
		for _, u := range all {
			uname := strings.ToLower(u.Username)
			nick := strings.ToLower(u.Nick)
			if strings.Contains(uname, k) || (u.Nick != "" && strings.Contains(nick, k)) || (idMatch != 0 && u.ID == idMatch) {
				users = append(users, userItem{ID: u.ID, Username: u.Username, Nick: u.Nick, Avatar: u.Avatar, Phone: u.Phone})
			}
			if len(users) >= 30 {
				break
			}
		}
		ack(util.OK(map[string]any{"users": users}))
	})

	// ---- 搜索公司（用于加入） ----
	srv.On("company:search", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		keyword := strVal(d, "keyword")
		companies, err := store.ListCompanies(ctx, keyword)
		if err != nil {
			logger.Error(mod, "搜索公司失败", "error", err.Error())
			ack(util.Fail("搜索失败"))
			return
		}
		ack(util.OK(map[string]any{"companies": companies}))
	})

	// ---- 凭加入码加入公司 ----
	srv.On("company:join", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		codeV, hasCode := d["code"]
		if !hasCode || !isCode(codeV) {
			ack(util.Fail("加入码不合法"))
			return
		}
		code := codeV.(string)
		company, err := store.GetCompanyByCode(ctx, code)
		if err != nil {
			logger.Error(mod, "按码查公司失败", "error", err.Error())
			ack(util.Fail("加入失败"))
			return
		}
		if company == nil {
			ack(util.Fail("公司不存在或加入码错误"))
			return
		}
		err = store.AddMember(ctx, company.ID, a.ID, "member")
		if err != nil {
			var ae *db.AlreadyExistsError
			if errors.As(err, &ae) {
				ack(util.Fail("已在公司中"))
				return
			}
			logger.Error(mod, "加入公司失败", "error", err.Error(), "companyId", company.ID, "userId", a.ID)
			ack(util.Fail("加入失败"))
			return
		}
		_ = c.Join(util.CompanyRoom(company.ID))
		_ = srv.EmitTo(util.CompanyRoom(company.ID), "company:updated", map[string]any{"companyId": company.ID})
		ack(util.OK(map[string]any{"company": company}))
	})

	// ---- 退出公司 ----
	srv.On("company:leave", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		cid, ok := toInt64(d["companyId"])
		if !ok || cid <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		role, err := store.GetMemberRole(ctx, cid, a.ID)
		if err != nil {
			logger.Error(mod, "查角色失败", "error", err.Error())
			ack(util.Fail("操作失败"))
			return
		}
		if role == "" {
			ack(util.Fail("不在公司中"))
			return
		}
		if role == "owner" {
			ack(util.Fail("创建人不能退出公司"))
			return
		}
		if err := store.RemoveMember(ctx, cid, a.ID); err != nil {
			logger.Error(mod, "退出公司失败", "error", err.Error(), "companyId", cid, "userId", a.ID)
			ack(util.Fail("操作失败"))
			return
		}
		_ = c.Leave(util.CompanyRoom(cid))
		_ = srv.EmitTo(util.CompanyRoom(cid), "company:updated", map[string]any{"companyId": cid})
		_ = srv.EmitTo(util.UserRoom(a.ID), "company:removed", map[string]any{"companyId": cid})
		ack(util.OK(nil))
	})

	// ---- 公司成员列表 ----
	srv.On("company:members", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		cid, ok := toInt64(d["companyId"])
		if !ok || cid <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		myRole, _ := store.GetMemberRole(ctx, cid, a.ID)
		if myRole == "" {
			ack(util.Fail("不在公司中"))
			return
		}
		members, err := store.GetMembers(ctx, cid)
		if err != nil {
			logger.Error(mod, "获取成员列表失败", "error", err.Error(), "companyId", cid)
			ack(util.Fail("获取失败"))
			return
		}
		// 填充用户信息 + 登录归属地 + 在线状态
		for i := range members {
			uid := members[i].UserID
			if u, _ := store.GetUserByID(ctx, uid); u != nil {
				avatar := u.Avatar
				phone := u.Phone
				extra := u.Extra
				members[i].Avatar = &avatar
				members[i].Phone = &phone
				members[i].Extra = &extra
			}
			loc, _ := store.GetUserActiveLocation(ctx, uid)
			if loc != "" {
				members[i].Location = &loc
			}
			online := isOnline(srv, uid)
			members[i].Online = &online
		}
		ack(util.OK(map[string]any{"members": members}))
	})

	// ---- 设置公司角色（owner/admin 可） ----
	srv.On("company:setRole", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		cid, ok1 := toInt64(d["companyId"])
		uid, ok2 := toInt64(d["userId"])
		if !ok1 || cid <= 0 || !ok2 || uid <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		if !isCompanyRole(d["role"]) {
			ack(util.Fail("角色不合法"))
			return
		}
		newRole := d["role"].(string)
		myRole, _ := store.GetMemberRole(ctx, cid, a.ID)
		if myRole == "" {
			ack(util.Fail("不在公司中"))
			return
		}
		if roleRank(myRole) < roleRank("admin") {
			ack(util.Fail("无权限"))
			return
		}
		targetRole, _ := store.GetMemberRole(ctx, cid, uid)
		if targetRole == "" {
			ack(util.Fail("目标不在公司中"))
			return
		}
		if roleRank(myRole) < roleRank(newRole) {
			ack(util.Fail("无权限授予该角色"))
			return
		}
		if newRole == "owner" && myRole != "owner" {
			ack(util.Fail("仅创建人可转移创建权"))
			return
		}
		if targetRole == "owner" && myRole != "owner" {
			ack(util.Fail("不能修改创建人角色"))
			return
		}
		if err := store.SetRole(ctx, cid, uid, newRole); err != nil {
			logger.Error(mod, "设置角色失败", "error", err.Error())
			ack(util.Fail("操作失败"))
			return
		}
		_ = srv.EmitTo(util.CompanyRoom(cid), "company:updated", map[string]any{"companyId": cid})
		ack(util.OK(nil))
	})

	// ---- 移除公司成员（owner/admin 可；不可移除创建人） ----
	srv.On("company:kick", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		cid, ok1 := toInt64(d["companyId"])
		uid, ok2 := toInt64(d["userId"])
		if !ok1 || cid <= 0 || !ok2 || uid <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		myRole, _ := store.GetMemberRole(ctx, cid, a.ID)
		if myRole == "" {
			ack(util.Fail("不在公司中"))
			return
		}
		if roleRank(myRole) < roleRank("admin") {
			ack(util.Fail("无权限"))
			return
		}
		targetRole, _ := store.GetMemberRole(ctx, cid, uid)
		if targetRole == "" {
			ack(util.Fail("目标不在公司中"))
			return
		}
		if targetRole == "owner" {
			ack(util.Fail("不能移除创建人"))
			return
		}
		if targetRole == "admin" && myRole != "owner" {
			ack(util.Fail("仅创建人可移除管理员"))
			return
		}
		if err := store.RemoveMember(ctx, cid, uid); err != nil {
			logger.Error(mod, "移除成员失败", "error", err.Error())
			ack(util.Fail("操作失败"))
			return
		}
		leaveUserSockets(srv, uid, util.CompanyRoom(cid))
		_ = srv.EmitTo(util.CompanyRoom(cid), "company:updated", map[string]any{"companyId": cid})
		_ = srv.EmitTo(util.UserRoom(uid), "company:removed", map[string]any{"companyId": cid})
		ack(util.OK(nil))
	})

	// ---- 直接添加成员到公司（管理员；可选分配到部门） ----
	srv.On("company:addMember", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		cid, ok := toInt64(d["companyId"])
		if !ok || cid <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		myRole, _ := store.GetMemberRole(ctx, cid, a.ID)
		if myRole == "" {
			ack(util.Fail("不在公司中"))
			return
		}
		if roleRank(myRole) < roleRank("admin") {
			ack(util.Fail("仅管理员可添加成员"))
			return
		}

		// 目标用户：优先 userId，否则按 username
		var targetID int64
		uidVal, hasUID := d["userId"]
		if hasUID && uidVal != nil {
			if n, ok := toInt64(uidVal); ok && n > 0 {
				if u, _ := store.GetUserByID(ctx, n); u != nil {
					targetID = u.ID
				}
			}
		}
		if targetID == 0 {
			uname := strings.TrimSpace(strVal(d, "username"))
			if uname != "" {
				if u, _ := store.GetUserByUsername(ctx, uname); u != nil {
					targetID = u.ID
				}
			}
		}
		if targetID == 0 {
			ack(util.Fail("用户不存在"))
			return
		}
		if targetID == a.ID {
			ack(util.Fail("不能添加自己"))
			return
		}
		curRole, _ := store.GetMemberRole(ctx, cid, targetID)
		if curRole != "" {
			ack(util.Fail("对方已在公司中"))
			return
		}

		// 校验部门属于本公司
		var deptID *int64
		didVal, hasDID := d["departmentId"]
		if hasDID && didVal != nil {
			if n, ok := toInt64(didVal); ok && n != 0 {
				if dept, _ := store.GetDepartmentByID(ctx, n); dept != nil && dept.CompanyID == cid {
					deptID = &n
				} else {
					ack(util.Fail("部门不属于该公司"))
					return
				}
			}
		}

		if err := store.AddMember(ctx, cid, targetID, "member"); err != nil {
			logger.Error(mod, "添加成员失败", "error", err.Error())
			ack(util.Fail("添加失败"))
			return
		}
		if deptID != nil {
			_ = store.AssignDepartment(ctx, *deptID, targetID)
		}
		_ = srv.EmitTo(util.CompanyRoom(cid), "company:updated", map[string]any{"companyId": cid})
		joinUserSockets(srv, targetID, util.CompanyRoom(cid))
		_ = srv.EmitTo(util.UserRoom(targetID), "company:added", map[string]any{"companyId": cid})
		ack(util.OK(nil))
	})

	// ---- 创建部门：仅公司 owner/admin ----
	srv.On("company:createDepartment", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		cid, ok := toInt64(d["companyId"])
		if !ok || cid <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		if !isName(d["name"]) {
			ack(util.Fail("部门名不合法"))
			return
		}
		myRole, _ := store.GetMemberRole(ctx, cid, a.ID)
		if myRole == "" {
			ack(util.Fail("不在公司中"))
			return
		}
		if roleRank(myRole) < roleRank("admin") {
			ack(util.Fail("仅管理员可创建部门"))
			return
		}
		var parentID *int64
		if pidVal, hasPID := d["parentId"]; hasPID && pidVal != nil {
			if n, ok := toInt64(pidVal); ok && n > 0 {
				parentID = &n
			} else {
				ack(util.Fail("上级部门不合法"))
				return
			}
		}
		dept, err := store.CreateDepartment(ctx, db.CreateDepartmentInput{
			CompanyID: cid,
			Name:      strings.TrimSpace(d["name"].(string)),
			ParentID:  parentID,
		})
		if err != nil {
			logger.Error(mod, "创建部门失败", "error", err.Error())
			ack(util.Fail("创建失败"))
			return
		}
		_ = srv.EmitTo(util.CompanyRoom(cid), "company:updated", map[string]any{"companyId": cid})
		ack(util.OK(map[string]any{"department": dept}))
	})

	// ---- 公司部门列表 ----
	srv.On("company:listDepartments", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		cid, ok := toInt64(d["companyId"])
		if !ok || cid <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		myRole, _ := store.GetMemberRole(ctx, cid, a.ID)
		if myRole == "" {
			ack(util.Fail("不在公司中"))
			return
		}
		depts, err := store.ListDepartments(ctx, cid)
		if err != nil {
			logger.Error(mod, "列出部门失败", "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		ack(util.OK(map[string]any{"departments": depts}))
	})

	// ---- 分配成员到部门（owner/admin 可） ----
	srv.On("department:assign", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		did, ok1 := toInt64(d["departmentId"])
		uid, ok2 := toInt64(d["userId"])
		if !ok1 || did <= 0 || !ok2 || uid <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		dept, _ := store.GetDepartmentByID(ctx, did)
		if dept == nil {
			ack(util.Fail("部门不存在"))
			return
		}
		myRole, _ := store.GetMemberRole(ctx, dept.CompanyID, a.ID)
		if myRole == "" {
			ack(util.Fail("不在公司中"))
			return
		}
		if roleRank(myRole) < roleRank("admin") {
			ack(util.Fail("仅管理员可分配成员"))
			return
		}
		targetRole, _ := store.GetMemberRole(ctx, dept.CompanyID, uid)
		if targetRole == "" {
			ack(util.Fail("目标不在公司中"))
			return
		}
		err := store.AssignDepartment(ctx, did, uid)
		if err != nil {
			var ae *db.AlreadyExistsError
			if errors.As(err, &ae) {
				ack(util.Fail("已在部门中"))
				return
			}
			logger.Error(mod, "分配部门失败", "error", err.Error())
			ack(util.Fail("分配失败"))
			return
		}
		_ = srv.EmitTo(util.CompanyRoom(dept.CompanyID), "company:updated", map[string]any{"companyId": dept.CompanyID})
		ack(util.OK(nil))
	})

	// ---- 移出部门（owner/admin 可） ----
	srv.On("department:removeMember", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		did, ok1 := toInt64(d["departmentId"])
		uid, ok2 := toInt64(d["userId"])
		if !ok1 || did <= 0 || !ok2 || uid <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		dept, _ := store.GetDepartmentByID(ctx, did)
		if dept == nil {
			ack(util.Fail("部门不存在"))
			return
		}
		myRole, _ := store.GetMemberRole(ctx, dept.CompanyID, a.ID)
		if myRole == "" {
			ack(util.Fail("不在公司中"))
			return
		}
		if roleRank(myRole) < roleRank("admin") {
			ack(util.Fail("仅管理员可操作"))
			return
		}
		_ = store.RemoveDepartmentMember(ctx, did, uid)
		_ = srv.EmitTo(util.CompanyRoom(dept.CompanyID), "company:updated", map[string]any{"companyId": dept.CompanyID})
		ack(util.OK(nil))
	})

	// ---- 部门成员列表 ----
	srv.On("department:members", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		did, ok := toInt64(d["departmentId"])
		if !ok || did <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		dept, _ := store.GetDepartmentByID(ctx, did)
		if dept == nil {
			ack(util.Fail("部门不存在"))
			return
		}
		myRole, _ := store.GetMemberRole(ctx, dept.CompanyID, a.ID)
		if myRole == "" {
			ack(util.Fail("不在公司中"))
			return
		}
		members, err := store.GetDepartmentMembers(ctx, did)
		if err != nil {
			logger.Error(mod, "获取部门成员失败", "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		for i := range members {
			uid := members[i].UserID
			if u, _ := store.GetUserByID(ctx, uid); u != nil {
				avatar := u.Avatar
				phone := u.Phone
				extra := u.Extra
				members[i].Avatar = &avatar
				members[i].Phone = &phone
				members[i].Extra = &extra
			}
			loc, _ := store.GetUserActiveLocation(ctx, uid)
			if loc != "" {
				members[i].Location = &loc
			}
			online := isOnline(srv, uid)
			members[i].Online = &online
		}
		ack(util.OK(map[string]any{"members": members}))
	})

	// ---- 重命名公司（owner/admin） ----
	srv.On("company:rename", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		cid, ok := toInt64(d["companyId"])
		if !ok || cid <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		if !isName(d["name"]) {
			ack(util.Fail("组织名不合法"))
			return
		}
		myRole, _ := store.GetMemberRole(ctx, cid, a.ID)
		if myRole == "" {
			ack(util.Fail("不在公司中"))
			return
		}
		if roleRank(myRole) < roleRank("admin") {
			ack(util.Fail("仅管理员可重命名公司"))
			return
		}
		newName := strings.TrimSpace(d["name"].(string))
		dup, _ := store.GetCompanyByName(ctx, newName)
		if dup != nil && dup.ID != cid {
			ack(util.Fail("已存在同名企业/团队"))
			return
		}
		if err := store.RenameCompany(ctx, cid, newName); err != nil {
			logger.Error(mod, "重命名公司失败", "error", err.Error())
			ack(util.Fail("操作失败"))
			return
		}
		company, _ := store.GetCompanyByID(ctx, cid)
		_ = srv.EmitTo(util.CompanyRoom(cid), "company:updated", map[string]any{"company": company})
		ack(util.OK(map[string]any{"company": company}))
	})

	// ---- 删除部门（owner/admin；级联移除成员归属） ----
	srv.On("company:deleteDepartment", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		did, ok := toInt64(d["departmentId"])
		if !ok || did <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		dept, _ := store.GetDepartmentByID(ctx, did)
		if dept == nil {
			ack(util.Fail("部门不存在"))
			return
		}
		myRole, _ := store.GetMemberRole(ctx, dept.CompanyID, a.ID)
		if myRole == "" {
			ack(util.Fail("不在公司中"))
			return
		}
		if roleRank(myRole) < roleRank("admin") {
			ack(util.Fail("仅管理员可删除部门"))
			return
		}
		if err := store.DeleteDepartment(ctx, did); err != nil {
			logger.Error(mod, "删除部门失败", "error", err.Error())
			ack(util.Fail("操作失败"))
			return
		}
		_ = srv.EmitTo(util.CompanyRoom(dept.CompanyID), "company:updated", map[string]any{"companyId": dept.CompanyID})
		ack(util.OK(nil))
	})

	// ---- 转让公司（仅 owner） ----
	srv.On("company:transfer", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		cid, ok1 := toInt64(d["companyId"])
		uid, ok2 := toInt64(d["userId"])
		if !ok1 || cid <= 0 || !ok2 || uid <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		company, _ := store.GetCompanyByID(ctx, cid)
		if company == nil {
			ack(util.Fail("公司不存在"))
			return
		}
		if company.OwnerID != a.ID {
			ack(util.Fail("仅创建者可转让"))
			return
		}
		targetRole, _ := store.GetMemberRole(ctx, cid, uid)
		if targetRole == "" {
			ack(util.Fail("目标不在公司中"))
			return
		}
		if uid == a.ID {
			ack(util.Fail("不能转让给自己"))
			return
		}
		_ = store.SetRole(ctx, cid, uid, "owner")
		_ = store.SetRole(ctx, cid, a.ID, "admin")
		_ = store.SetCompanyOwner(ctx, cid, uid)
		_ = srv.EmitTo(util.CompanyRoom(cid), "company:updated", map[string]any{"company": company})
		ack(util.OK(nil))
	})

	// ---- 解散公司（仅 owner） ----
	srv.On("company:dissolve", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		cid, ok := toInt64(d["companyId"])
		if !ok || cid <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		company, _ := store.GetCompanyByID(ctx, cid)
		if company == nil {
			ack(util.Fail("公司不存在"))
			return
		}
		if company.OwnerID != a.ID {
			ack(util.Fail("仅创建者可解散"))
			return
		}
		// 通知所有群已解散
		groups, _ := store.ListGroups(ctx, cid, "")
		for _, g := range groups {
			_ = srv.EmitTo(util.GroupRoom(g.ID), "group:dissolved", map[string]any{"groupId": g.ID})
		}
		if err := store.DeleteCompany(ctx, cid); err != nil {
			logger.Error(mod, "解散公司失败", "error", err.Error())
			ack(util.Fail("操作失败"))
			return
		}
		_ = srv.EmitTo(util.CompanyRoom(cid), "company:dissolved", map[string]any{"companyId": cid})
		ack(util.OK(nil))
	})

	// ---- 管理员邀请用户加入公司 ----
	srv.On("company:invite", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		cid, ok := toInt64(d["companyId"])
		if !ok || cid <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		username := strings.TrimSpace(strVal(d, "username"))
		if username == "" {
			ack(util.Fail("请输入对方账号"))
			return
		}
		myRole, _ := store.GetMemberRole(ctx, cid, a.ID)
		if myRole == "" {
			ack(util.Fail("不在公司中"))
			return
		}
		if roleRank(myRole) < roleRank("admin") {
			ack(util.Fail("仅管理员可邀请"))
			return
		}
		target, err := store.GetUserByUsername(ctx, username)
		if err != nil {
			logger.Error(mod, "按名查用户失败", "error", err.Error())
			ack(util.Fail("邀请失败"))
			return
		}
		if target == nil {
			ack(util.Fail("账号不存在"))
			return
		}
		curRole, _ := store.GetMemberRole(ctx, cid, target.ID)
		if curRole != "" {
			ack(util.Fail("对方已在公司中"))
			return
		}

		// 校验部门属于本公司
		var deptID *int64
		if didVal, hasDID := d["departmentId"]; hasDID && didVal != nil {
			if n, ok := toInt64(didVal); ok && n != 0 {
				if dept, _ := store.GetDepartmentByID(ctx, n); dept != nil && dept.CompanyID == cid {
					deptID = &n
				} else {
					ack(util.Fail("部门不属于该公司"))
					return
				}
			}
		}

		// 可选过期时间
		var expiresAt *string
		if eaVal, hasEA := d["expiresAt"]; hasEA && eaVal != nil {
			eaStr := strings.TrimSpace(fmt.Sprintf("%v", eaVal))
			if eaStr != "" && eaStr != "<nil>" {
				t, err := time.Parse(time.RFC3339, eaStr)
				if err != nil {
					// 尝试宽松解析
					t2, err2 := time.Parse("2006-01-02T15:04:05", eaStr)
					if err2 != nil {
						ack(util.Fail("过期时间不合法"))
						return
					}
					t = t2
				}
				s := t.UTC().Format(time.RFC3339)
				expiresAt = &s
			}
		}

		// 8 位邀请码 = makeJoinCode() + makeJoinCode().slice(0,2) = 10 位
		code := util.MakeJoinCode() + util.MakeJoinCode()[:2]
		inv, err := store.CreateCompanyInvitation(ctx, db.CreateInvitationInput{
			CompanyID:    cid,
			UserID:       target.ID,
			DepartmentID: deptID,
			InvitedBy:    a.ID,
			Code:         code,
			ExpiresAt:    expiresAt,
		})
		if err != nil {
			var ae *db.AlreadyExistsError
			if errors.As(err, &ae) {
				ack(util.Fail("已邀请过对方，等待其同意"))
				return
			}
			logger.Error(mod, "创建邀请失败", "error", err.Error())
			ack(util.Fail("邀请失败"))
			return
		}
		_ = srv.EmitTo(util.UserRoom(target.ID), "company:inviteReceived", map[string]any{"invitation": inv})
		ack(util.OK(map[string]any{"invitation": inv}))
	})

	// ---- 用户：我的待处理邀请 ----
	srv.On("company:myInvites", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		invites, err := store.ListPendingInvitationsForUser(ctx, a.ID)
		if err != nil {
			logger.Error(mod, "列出待处理邀请失败", "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		ack(util.OK(map[string]any{"invites": invites}))
	})

	// ---- 管理员：公司发出的邀请 ----
	srv.On("company:listInvitations", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		cid, ok := toInt64(d["companyId"])
		if !ok || cid <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		myRole, _ := store.GetMemberRole(ctx, cid, a.ID)
		if myRole != "owner" && myRole != "admin" {
			ack(util.Fail("仅管理员可查看"))
			return
		}
		invites, err := store.ListCompanyInvitations(ctx, cid)
		if err != nil {
			logger.Error(mod, "列出邀请失败", "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		ack(util.OK(map[string]any{"invites": invites}))
	})

	// ---- 用户：同意邀请 ----
	srv.On("company:acceptInvite", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		invID, ok := toInt64(d["invitationId"])
		if !ok || invID <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		inv, err := store.GetCompanyInvitationByID(ctx, invID)
		if err != nil {
			logger.Error(mod, "查邀请失败", "error", err.Error())
			ack(util.Fail("邀请不存在"))
			return
		}
		if inv == nil {
			ack(util.Fail("邀请不存在"))
			return
		}
		if inv.UserID != a.ID {
			ack(util.Fail("无权处理该邀请"))
			return
		}
		if inv.Status != "pending" {
			ack(util.Fail("邀请已失效"))
			return
		}
		// 过期检查
		if inv.ExpiresAt != nil {
			if t, err := time.Parse(time.RFC3339, *inv.ExpiresAt); err == nil && t.Before(time.Now()) {
				_ = store.SetCompanyInvitationStatus(ctx, inv.ID, "expired")
				ack(util.Fail("邀请已过期"))
				return
			}
		}
		// 加入公司
		err = store.AddMember(ctx, inv.CompanyID, a.ID, "member")
		if err != nil {
			var ae *db.AlreadyExistsError
			if errors.As(err, &ae) {
				ack(util.Fail("你已在公司中"))
				return
			}
			logger.Error(mod, "接受邀请加入公司失败", "error", err.Error())
			ack(util.Fail("加入失败"))
			return
		}
		// 分配部门
		if inv.DepartmentID != nil {
			_ = store.AssignDepartment(ctx, *inv.DepartmentID, a.ID)
		}
		// 全员群（缺失则自动补建）
		mainGroup, _ := store.GetCompanyMainGroup(ctx, inv.CompanyID)
		if mainGroup == nil {
			company, _ := store.GetCompanyByID(ctx, inv.CompanyID)
			companyName := "公司"
			if company != nil {
				companyName = company.Name
			}
			for g := 0; g < 5; g++ {
				ng, gerr := store.CreateGroup(ctx, db.CreateGroupInput{
					CompanyID: inv.CompanyID,
					Name:      companyName + "总群",
					Code:      util.MakeJoinCode(),
					OwnerID:   inv.InvitedBy,
				})
				if gerr != nil {
					var gae *db.AlreadyExistsError
					if errors.As(gerr, &gae) {
						continue
					}
					break
				}
				mainGroup = ng
				break
			}
		}
		if mainGroup != nil {
			_ = store.AddGroupMember(ctx, mainGroup.ID, a.ID, "member")
			if conv, err := store.GetOrCreateGroupConversation(ctx, mainGroup.ID, 0); err == nil {
				_ = store.AddConversationMember(ctx, conv.ID, a.ID)
			}
			_ = srv.EmitTo(util.GroupRoom(mainGroup.ID), "group:members-updated", map[string]any{"groupId": mainGroup.ID})
		}
		// 邀请标记已使用
		_ = store.SetCompanyInvitationStatus(ctx, inv.ID, "accepted")
		inv.Status = "accepted"
		_ = c.Join(util.CompanyRoom(inv.CompanyID))
		_ = srv.EmitTo(util.CompanyRoom(inv.CompanyID), "company:updated", map[string]any{"companyId": inv.CompanyID})
		_ = srv.EmitTo(util.UserRoom(a.ID), "company:added", map[string]any{"companyId": inv.CompanyID})
		company, _ := store.GetCompanyByID(ctx, inv.CompanyID)
		_ = srv.EmitTo(util.UserRoom(inv.InvitedBy), "company:inviteResolved", map[string]any{
			"invitation": inv,
			"company":    company,
		})
		ack(util.OK(map[string]any{"company": company}))
	})

	// ---- 用户：拒绝邀请 ----
	srv.On("company:declineInvite", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		invID, ok := toInt64(d["invitationId"])
		if !ok || invID <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		inv, _ := store.GetCompanyInvitationByID(ctx, invID)
		if inv == nil {
			ack(util.Fail("邀请不存在"))
			return
		}
		if inv.UserID != a.ID {
			ack(util.Fail("无权处理该邀请"))
			return
		}
		if inv.Status != "pending" {
			ack(util.Fail("邀请已失效"))
			return
		}
		_ = store.SetCompanyInvitationStatus(ctx, inv.ID, "declined")
		ack(util.OK(nil))
	})

	// ---- 读取我的某公司人事信息 ----
	srv.On("member:getProfile", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		cid, ok := toInt64(d["companyId"])
		if !ok || cid <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		myRole, _ := store.GetMemberRole(ctx, cid, a.ID)
		if myRole == "" {
			ack(util.Fail("不在公司中"))
			return
		}
		profile, err := store.GetMemberProfile(ctx, cid, a.ID)
		if err != nil {
			logger.Error(mod, "获取人事信息失败", "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		ack(util.OK(map[string]any{"profile": profile}))
	})

	// ---- 填写/更新我的某公司人事信息 ----
	srv.On("member:updateProfile", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		cid, ok := toInt64(d["companyId"])
		if !ok || cid <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		myRole, _ := store.GetMemberRole(ctx, cid, a.ID)
		if myRole == "" {
			ack(util.Fail("不在公司中"))
			return
		}
		// 对标 TS 的 str(v) = typeof v === 'string' ? v.trim() : ''
		strField := func(key string) string {
			if v, ok := d[key].(string); ok {
				return strings.TrimSpace(v)
			}
			return ""
		}
		rn := strField("realName")
		idc := strField("idCard")
		bc := strField("bankCard")
		ru := strField("resumeUrl")
		pu := strField("portfolioUrl")
		profile, err := store.UpsertMemberProfile(ctx, cid, a.ID, db.MemberProfileInput{
			RealName:     &rn,
			IDCard:       &idc,
			BankCard:     &bc,
			ResumeURL:    &ru,
			PortfolioURL: &pu,
		})
		if err != nil {
			logger.Error(mod, "更新人事信息失败", "error", err.Error())
			ack(util.Fail("保存失败"))
			return
		}
		_ = srv.EmitTo(util.CompanyRoom(cid), "company:updated", map[string]any{"companyId": cid})
		ack(util.OK(map[string]any{"profile": profile}))
	})
}

// strVal 从 map 取字符串值；不存在或非字符串返回空串。
func strVal(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// parseIntStrict 严格解析正整数字符串（用于 user:search 的 ID 匹配）。
func parseIntStrict(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	var n int64
	_, err := fmt.Sscanf(s, "%d", &n)
	if err != nil || n <= 0 {
		return 0, false
	}
	// 确保整个字符串都是数字
	cleaned := strings.TrimLeft(s, "0")
	if cleaned == "" {
		cleaned = "0"
	}
	return n, fmt.Sprint(n) == cleaned || (n > 0 && s == fmt.Sprintf("%d", n))
}
