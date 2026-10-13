// Package org —— group.go 1:1 复刻 server/src/handlers/group.ts（330 行）：
//   - 群创建 / 加入（邀请码）/ 退出 / 解散
//   - 群成员 / 角色（管理员设置/取消）/ 所有者转让
//   - 禁言（mutedUntil）
//   - 群邀请（内存态，per-connection）
//   - 群列表 / 搜索
//
// 注意：TS 中 group:create 允许 companyId=0（无公司群），校验为 companyId < 0 即非法。
package org

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"mochi-oa-server-go/internal/db"
	"mochi-oa-server-go/internal/logger"
	"mochi-oa-server-go/internal/socketapi"
	"mochi-oa-server-go/internal/util"
)

// groupInvite 内存态群邀请（对标 group.ts 顶部 invites Map 的 value 结构）。
type groupInvite struct {
	GroupID   int64
	GroupName string
	FromUser  int64
	FromNick  string
}

// RegisterGroupHandlers 注册群相关全部 socket 事件。
// 对标 registerGroupHandlers(ctx)。
func RegisterGroupHandlers(srv socketapi.Server) {
	store := db.GetStore()
	ctx := context.Background()

	// canManage 群主/管理员可管理。
	canManage := func(role string) bool {
		return role == "owner" || role == "admin"
	}
	// broadcastMembers 群成员变更后广播。
	broadcastMembers := func(groupID int64) {
		_ = srv.EmitTo(util.GroupRoom(groupID), "group:members-updated", map[string]any{"groupId": groupID})
	}
	// joinGroupSockets 让某用户的所有在线连接加入群房间。
	joinGroupSockets := func(userID, groupID int64) {
		joinUserSockets(srv, userID, util.GroupRoom(groupID))
	}

	srv.OnConnection(func(c socketapi.Conn) error {
		// 每条连接维护自己的 groupInvites map（对标 TS 的 per-connection Map）。
		c.Set("groupInvites", &sync.Map{})
		return nil
	})

	// ---- 创建群：公司成员即可；创建人为群主 ----
	srv.On("group:create", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		// TS: companyId < 0 即非法（=0 允许，表示无公司群）
		cid, ok := toInt64(d["companyId"])
		if !ok || cid < 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		if !isName(d["name"]) {
			ack(util.Fail("群名不合法"))
			return
		}
		name := strings.TrimSpace(d["name"].(string))
		if existing, _ := store.GetGroupByName(ctx, cid, name); existing != nil {
			ack(util.Fail("已存在同名群"))
			return
		}
		// 有公司则须为公司成员
		if cid > 0 {
			myRole, _ := store.GetMemberRole(ctx, cid, a.ID)
			if myRole == "" {
				ack(util.Fail("不在公司中"))
				return
			}
		}

		// 可选部门
		var deptID *int64
		if didVal, hasDID := d["departmentId"]; hasDID && didVal != nil {
			if n, ok := toInt64(didVal); ok && n > 0 {
				dept, _ := store.GetDepartmentByID(ctx, n)
				if dept == nil || dept.CompanyID != cid {
					ack(util.Fail("部门不属于该公司"))
					return
				}
				deptID = &n
			} else {
				ack(util.Fail("部门不合法"))
				return
			}
		}

		for attempt := 0; attempt < 5; attempt++ {
			gcode := util.MakeJoinCode()
			group, err := store.CreateGroup(ctx, db.CreateGroupInput{
				CompanyID:    cid,
				DepartmentID: deptID,
				Name:         name,
				Code:         gcode,
				OwnerID:      a.ID,
			})
			if err != nil {
				var ae *db.AlreadyExistsError
				if errors.As(err, &ae) {
					continue
				}
				logger.Error(mod, "创建群失败", "error", err.Error())
				ack(util.Fail("创建失败"))
				return
			}
			_, _ = store.GetOrCreateGroupConversation(ctx, group.ID, a.ID)
			_ = c.Join(util.GroupRoom(group.ID))
			ack(util.OK(map[string]any{"group": group}))
			return
		}
		ack(util.Fail("群码生成冲突，请重试"))
	})

	// ---- 我的群列表 ----
	srv.On("group:list", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		groups, err := store.GetUserGroups(ctx, a.ID)
		if err != nil {
			logger.Error(mod, "获取群列表失败", "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		ack(util.OK(map[string]any{"groups": groups}))
	})

	// ---- 某公司下的群搜索 ----
	srv.On("group:search", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		var cid int64
		cidVal, hasCID := d["companyId"]
		if hasCID && cidVal != nil {
			n, ok := toInt64(cidVal)
			if !ok || n <= 0 {
				ack(util.Fail("参数不合法"))
				return
			}
			cid = n
			myRole, _ := store.GetMemberRole(ctx, cid, a.ID)
			if myRole == "" {
				ack(util.Fail("不在公司中"))
				return
			}
		}
		keyword := strVal(d, "keyword")
		groups, err := store.ListGroups(ctx, cid, keyword)
		if err != nil {
			logger.Error(mod, "搜索群失败", "error", err.Error())
			ack(util.Fail("搜索失败"))
			return
		}
		ack(util.OK(map[string]any{"groups": groups}))
	})

	// ---- 凭群码加入群 ----
	srv.On("group:join", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		codeV, hasCode := d["code"]
		if !hasCode || !isCode(codeV) {
			ack(util.Fail("群码不合法"))
			return
		}
		code := codeV.(string)
		group, err := store.GetGroupByCode(ctx, code)
		if err != nil {
			logger.Error(mod, "按群码查群失败", "error", err.Error())
			ack(util.Fail("加入失败"))
			return
		}
		if group == nil {
			ack(util.Fail("群不存在或群码错误"))
			return
		}
		// 有公司群须为公司成员
		if group.CompanyID > 0 {
			myRole, _ := store.GetMemberRole(ctx, group.CompanyID, a.ID)
			if myRole == "" {
				ack(util.Fail("不在该公司，无法加入该群"))
				return
			}
		}
		err = store.AddGroupMember(ctx, group.ID, a.ID, "member")
		if err != nil {
			var ae *db.AlreadyExistsError
			if errors.As(err, &ae) {
				ack(util.Fail("已在群中"))
				return
			}
			logger.Error(mod, "加入群失败", "error", err.Error())
			ack(util.Fail("加入失败"))
			return
		}
		if conv, err := store.GetOrCreateGroupConversation(ctx, group.ID, 0); err == nil {
			_ = store.AddConversationMember(ctx, conv.ID, a.ID)
		}
		_ = c.Join(util.GroupRoom(group.ID))
		broadcastMembers(group.ID)
		ack(util.OK(map[string]any{"group": group}))
	})

	// ---- 退出群 ----
	srv.On("group:leave", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		gid, ok := toInt64(d["groupId"])
		if !ok || gid <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		role, _ := store.GetGroupMemberRole(ctx, gid, a.ID)
		if role == "" {
			ack(util.Fail("不在群中"))
			return
		}
		if role == "owner" {
			ack(util.Fail("群主不能退出群"))
			return
		}
		if err := store.RemoveGroupMember(ctx, gid, a.ID); err != nil {
			logger.Error(mod, "退出群失败", "error", err.Error())
			ack(util.Fail("操作失败"))
			return
		}
		if conv, err := store.GetOrCreateGroupConversation(ctx, gid, 0); err == nil {
			_ = store.RemoveConversationMember(ctx, conv.ID, a.ID)
		}
		_ = c.Leave(util.GroupRoom(gid))
		broadcastMembers(gid)
		_ = srv.EmitTo(util.UserRoom(a.ID), "group:removed", map[string]any{"groupId": gid})
		ack(util.OK(nil))
	})

	// ---- 群成员列表 ----
	srv.On("group:members", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		gid, ok := toInt64(d["groupId"])
		if !ok || gid <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		myRole, _ := store.GetGroupMemberRole(ctx, gid, a.ID)
		if myRole == "" {
			ack(util.Fail("不在群中"))
			return
		}
		members, err := store.GetGroupMembers(ctx, gid)
		if err != nil {
			logger.Error(mod, "获取群成员失败", "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		for i := range members {
			uid := members[i].UserID
			if u, _ := store.GetUserByID(ctx, uid); u != nil {
				avatar := u.Avatar
				members[i].Avatar = &avatar
			}
			online := isOnline(srv, uid)
			members[i].Online = &online
		}
		ack(util.OK(map[string]any{"members": members}))
	})

	// ---- 群主/管理员移除成员 ----
	srv.On("group:kick", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		gid, ok1 := toInt64(d["groupId"])
		uid, ok2 := toInt64(d["userId"])
		if !ok1 || gid <= 0 || !ok2 || uid <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		myRole, _, _ := store.GetGroupMember(ctx, gid, a.ID)
		if !canManage(myRole) {
			ack(util.Fail("仅群主/管理员可移除成员"))
			return
		}
		targetRole, _, _ := store.GetGroupMember(ctx, gid, uid)
		if targetRole == "" {
			ack(util.Fail("目标不在群中"))
			return
		}
		if targetRole == "owner" {
			ack(util.Fail("不能移除群主"))
			return
		}
		if myRole == "admin" && targetRole == "admin" {
			ack(util.Fail("管理员不能移除其他管理员"))
			return
		}
		if err := store.RemoveGroupMember(ctx, gid, uid); err != nil {
			logger.Error(mod, "移除群成员失败", "error", err.Error())
			ack(util.Fail("操作失败"))
			return
		}
		if conv, err := store.GetOrCreateGroupConversation(ctx, gid, 0); err == nil {
			_ = store.RemoveConversationMember(ctx, conv.ID, uid)
		}
		leaveUserSockets(srv, uid, util.GroupRoom(gid))
		broadcastMembers(gid)
		_ = srv.EmitTo(util.UserRoom(uid), "group:removed", map[string]any{"groupId": gid})
		ack(util.OK(nil))
	})

	// ---- 群主设为管理员 ----
	srv.On("group:setAdmin", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		gid, ok1 := toInt64(d["groupId"])
		uid, ok2 := toInt64(d["userId"])
		if !ok1 || gid <= 0 || !ok2 || uid <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		myRole, _, _ := store.GetGroupMember(ctx, gid, a.ID)
		if myRole != "owner" {
			ack(util.Fail("仅群主可设置管理员"))
			return
		}
		targetRole, _, _ := store.GetGroupMember(ctx, gid, uid)
		if targetRole == "" {
			ack(util.Fail("目标不在群中"))
			return
		}
		if targetRole != "member" {
			ack(util.Fail("已是群主/管理员"))
			return
		}
		_ = store.SetGroupMemberRole(ctx, gid, uid, "admin")
		broadcastMembers(gid)
		ack(util.OK(nil))
	})

	// ---- 群主取消管理员 ----
	srv.On("group:unsetAdmin", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		gid, ok1 := toInt64(d["groupId"])
		uid, ok2 := toInt64(d["userId"])
		if !ok1 || gid <= 0 || !ok2 || uid <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		myRole, _, _ := store.GetGroupMember(ctx, gid, a.ID)
		if myRole != "owner" {
			ack(util.Fail("仅群主可取消管理员"))
			return
		}
		targetRole, _, _ := store.GetGroupMember(ctx, gid, uid)
		if targetRole == "" {
			ack(util.Fail("目标不在群中"))
			return
		}
		if targetRole != "admin" {
			ack(util.Fail("目标不是管理员"))
			return
		}
		_ = store.SetGroupMemberRole(ctx, gid, uid, "member")
		broadcastMembers(gid)
		ack(util.OK(nil))
	})

	// ---- 群主/管理员禁言（seconds>0 禁言；0/缺省 解禁） ----
	srv.On("group:mute", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		gid, ok1 := toInt64(d["groupId"])
		uid, ok2 := toInt64(d["userId"])
		if !ok1 || gid <= 0 || !ok2 || uid <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		// seconds: TS 默认 0；Number(d.seconds) —— 接受数字（含浮点）
		seconds := float64(0)
		if sv, hasS := d["seconds"]; hasS && sv != nil {
			switch n := sv.(type) {
			case float64:
				seconds = n
			case string:
				fmt.Sscanf(n, "%f", &seconds)
			}
		}
		myRole, _, _ := store.GetGroupMember(ctx, gid, a.ID)
		if !canManage(myRole) {
			ack(util.Fail("仅群主/管理员可禁言"))
			return
		}
		targetRole, _, _ := store.GetGroupMember(ctx, gid, uid)
		if targetRole == "" {
			ack(util.Fail("目标不在群中"))
			return
		}
		if targetRole == "owner" {
			ack(util.Fail("不能禁言群主"))
			return
		}
		if myRole == "admin" && targetRole == "admin" {
			ack(util.Fail("管理员不能禁言其他管理员"))
			return
		}
		var mutedUntil *string
		if seconds > 0 {
			t := time.Now().Add(time.Duration(seconds * float64(time.Second))).UTC().Format(time.RFC3339Nano)
			mutedUntil = &t
		}
		_ = store.SetGroupMemberMuted(ctx, gid, uid, mutedUntil)
		broadcastMembers(gid)
		ack(util.OK(map[string]any{"mutedUntil": mutedUntil}))
	})

	// ---- 群主/管理员直接加人（公司成员） ----
	srv.On("group:add", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		gid, ok1 := toInt64(d["groupId"])
		uid, ok2 := toInt64(d["userId"])
		if !ok1 || gid <= 0 || !ok2 || uid <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		group, _ := store.GetGroupByID(ctx, gid)
		if group == nil {
			ack(util.Fail("群不存在"))
			return
		}
		myRole, _, _ := store.GetGroupMember(ctx, gid, a.ID)
		if !canManage(myRole) {
			ack(util.Fail("仅群主/管理员可添加成员"))
			return
		}
		if group.CompanyID > 0 {
			targetCRole, _ := store.GetMemberRole(ctx, group.CompanyID, uid)
			if targetCRole == "" {
				ack(util.Fail("目标不在该公司"))
				return
			}
		}
		targetGRole, _, _ := store.GetGroupMember(ctx, gid, uid)
		if targetGRole != "" {
			ack(util.Fail("已在群中"))
			return
		}
		err := store.AddGroupMember(ctx, gid, uid, "member")
		if err != nil {
			var ae *db.AlreadyExistsError
			if errors.As(err, &ae) {
				ack(util.Fail("已在群中"))
				return
			}
			logger.Error(mod, "添加群成员失败", "error", err.Error())
			ack(util.Fail("添加失败"))
			return
		}
		if conv, err := store.GetOrCreateGroupConversation(ctx, gid, 0); err == nil {
			_ = store.AddConversationMember(ctx, conv.ID, uid)
		}
		joinGroupSockets(uid, gid)
		broadcastMembers(gid)
		_ = srv.EmitTo(util.UserRoom(uid), "group:added", map[string]any{"groupId": gid})
		ack(util.OK(nil))
	})

	// ---- 群友邀请（普通成员/管理员/群主均可） ----
	srv.On("group:invite", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		gid, ok1 := toInt64(d["groupId"])
		uid, ok2 := toInt64(d["userId"])
		if !ok1 || gid <= 0 || !ok2 || uid <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		group, _ := store.GetGroupByID(ctx, gid)
		if group == nil {
			ack(util.Fail("群不存在"))
			return
		}
		myRole, _, _ := store.GetGroupMember(ctx, gid, a.ID)
		if myRole == "" {
			ack(util.Fail("不在群中"))
			return
		}
		if group.CompanyID > 0 {
			targetC, _ := store.GetMemberRole(ctx, group.CompanyID, uid)
			if targetC == "" {
				ack(util.Fail("目标不在该公司"))
				return
			}
		}
		targetGRole, _, _ := store.GetGroupMember(ctx, gid, uid)
		if targetGRole != "" {
			ack(util.Fail("对方已在群中"))
			return
		}
		if u, _ := store.GetUserByID(ctx, uid); u == nil {
			ack(util.Fail("对方不存在"))
			return
		}
		// 存入本连接的 invites map
		invites, _ := c.Get("groupInvites").(*sync.Map)
		nick := a.Nick
		if nick == "" {
			nick = a.Username
		}
		invites.Store(fmt.Sprintf("%d:%d", gid, uid), &groupInvite{
			GroupID:   gid,
			GroupName: group.Name,
			FromUser:  a.ID,
			FromNick:  nick,
		})
		_ = srv.EmitTo(util.UserRoom(uid), "group:invite", map[string]any{
			"groupId":   gid,
			"groupName": group.Name,
			"from": map[string]any{
				"userId": a.ID,
				"nick":   nick,
			},
			"ts": time.Now().UnixMilli(),
		})
		ack(util.OK(nil))
	})

	// ---- 接受邀请入群 ----
	srv.On("group:acceptInvite", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		gid, ok := toInt64(d["groupId"])
		if !ok || gid <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		invites, _ := c.Get("groupInvites").(*sync.Map)
		raw, _ := invites.Load(fmt.Sprintf("%d:%d", gid, a.ID))
		inv, _ := raw.(*groupInvite)
		if inv == nil {
			ack(util.Fail("没有待处理的邀请"))
			return
		}
		group, _ := store.GetGroupByID(ctx, gid)
		if group == nil {
			invites.Delete(fmt.Sprintf("%d:%d", gid, a.ID))
			ack(util.Fail("群不存在"))
			return
		}
		myRole, _, _ := store.GetGroupMember(ctx, gid, a.ID)
		if myRole != "" {
			invites.Delete(fmt.Sprintf("%d:%d", gid, a.ID))
			ack(util.Fail("已在群中"))
			return
		}
		if group.CompanyID > 0 {
			cRole, _ := store.GetMemberRole(ctx, group.CompanyID, a.ID)
			if cRole == "" {
				invites.Delete(fmt.Sprintf("%d:%d", gid, a.ID))
				ack(util.Fail("不在该公司，无法入群"))
				return
			}
		}
		_ = store.AddGroupMember(ctx, gid, a.ID, "member")
		if conv, err := store.GetOrCreateGroupConversation(ctx, gid, 0); err == nil {
			_ = store.AddConversationMember(ctx, conv.ID, a.ID)
		}
		_ = c.Join(util.GroupRoom(gid))
		invites.Delete(fmt.Sprintf("%d:%d", gid, a.ID))
		broadcastMembers(gid)
		ack(util.OK(map[string]any{"group": group}))
	})

	// ---- 拒绝邀请 ----
	srv.On("group:declineInvite", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		gid, ok := toInt64(d["groupId"])
		if !ok || gid <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		invites, _ := c.Get("groupInvites").(*sync.Map)
		invites.Delete(fmt.Sprintf("%d:%d", gid, a.ID))
		ack(util.OK(nil))
	})

	// ---- 转让群主（仅群主） ----
	srv.On("group:transfer", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		gid, ok1 := toInt64(d["groupId"])
		uid, ok2 := toInt64(d["userId"])
		if !ok1 || gid <= 0 || !ok2 || uid <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		myRole, _, _ := store.GetGroupMember(ctx, gid, a.ID)
		if myRole != "owner" {
			ack(util.Fail("仅群主可转让"))
			return
		}
		targetRole, _, _ := store.GetGroupMember(ctx, gid, uid)
		if targetRole == "" {
			ack(util.Fail("目标不在群中"))
			return
		}
		if uid == a.ID {
			ack(util.Fail("不能转让给自己"))
			return
		}
		_ = store.SetGroupMemberRole(ctx, gid, uid, "owner")
		_ = store.SetGroupMemberRole(ctx, gid, a.ID, "admin")
		_ = store.SetGroupOwner(ctx, gid, uid)
		broadcastMembers(gid)
		ack(util.OK(nil))
	})

	// ---- 解散群（仅群主） ----
	srv.On("group:dissolve", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		gid, ok := toInt64(d["groupId"])
		if !ok || gid <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		myRole, _, _ := store.GetGroupMember(ctx, gid, a.ID)
		if myRole != "owner" {
			ack(util.Fail("仅群主可解散群"))
			return
		}
		if err := store.DeleteGroup(ctx, gid); err != nil {
			logger.Error(mod, "解散群失败", "error", err.Error())
			ack(util.Fail("操作失败"))
			return
		}
		_ = srv.EmitTo(util.GroupRoom(gid), "group:dissolved", map[string]any{"groupId": gid})
		ack(util.OK(nil))
	})

}
