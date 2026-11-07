// Package org —— friends.go：好友添加 / 删除 / 列表 / 申请-同意-拒绝。
//
// 好友与公司无关，钉钉无公司也能加。
// server_admin 加好友直接成功，无需对方同意。
package org

import (
	"context"
	"encoding/json"
	"errors"

	"mochi-oa-server-go/internal/config"
	"mochi-oa-server-go/internal/db"
	"mochi-oa-server-go/internal/handlers/admin"
	"mochi-oa-server-go/internal/logger"
	"mochi-oa-server-go/internal/socketapi"
	"mochi-oa-server-go/internal/types"
	"mochi-oa-server-go/internal/util"
)

// isServerAdmin 判断当前连接用户是否为 SERVER_ADMIN。
func isServerAdmin(a *types.Auth) bool {
	return admin.IsServerAdmin(a, config.Get(), db.GetStore())
}

// RegisterFriendHandlers 注册好友相关 socket 事件。
func RegisterFriendHandlers(srv socketapi.Server) {
	store := db.GetStore()
	ctx := context.Background()

	// ---- 加好友（发申请，server_admin 直接成功） ----
	srv.On("friend:add", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		uid, ok := toInt64(d["userId"])
		if !ok || uid <= 0 || uid == a.ID {
			ack(util.Fail("参数不合法"))
			return
		}
		if u, _ := store.GetUserByID(ctx, uid); u == nil {
			ack(util.Fail("对方不存在"))
			return
		}

		// server_admin 直接加好友，无需同意
		if isServerAdmin(a) {
			err := store.AddFriend(ctx, a.ID, uid)
			if err != nil {
				var ae *db.AlreadyExistsError
				if !errors.As(err, &ae) {
					logger.Error(mod, "server_admin 加好友失败", "error", err.Error(), "from", a.ID, "to", uid)
					ack(util.Fail("添加失败"))
					return
				}
			}
			_ = srv.EmitTo(util.UserRoom(a.ID), "friends:updated", map[string]any{})
			_ = srv.EmitTo(util.UserRoom(uid), "friends:updated", map[string]any{})
			ack(util.OK(map[string]any{"direct": true}))
			return
		}

		// 普通用户：检查是否已是好友
		if pend, _ := store.HasPendingRequest(ctx, uid, a.ID); pend {
			// 对方已向我发过申请，直接同意加好友
			_ = store.AddFriend(ctx, a.ID, uid)
			// 把对方的 pending 申请标记为 accepted
			if reqs, _ := store.ListPendingFriendRequests(ctx, a.ID); len(reqs) > 0 {
				for _, r := range reqs {
					if r.FromID == uid {
						_ = store.RespondFriendRequest(ctx, r.ID, a.ID, true)
					}
				}
			}
			_ = srv.EmitTo(util.UserRoom(a.ID), "friends:updated", map[string]any{})
			_ = srv.EmitTo(util.UserRoom(uid), "friends:updated", map[string]any{})
			ack(util.OK(map[string]any{"direct": true}))
			return
		}

		// 创建好友申请
		req, err := store.CreateFriendRequest(ctx, a.ID, uid)
		if err != nil {
			logger.Error(mod, "发送好友申请失败", "error", err.Error(), "from", a.ID, "to", uid)
			ack(util.Fail("发送失败"))
			return
		}
		// 通知对方有新申请
		_ = srv.EmitTo(util.UserRoom(uid), "friend:request", map[string]any{
			"requestId": req.ID,
			"fromId":    a.ID,
		})
		ack(util.OK(map[string]any{"requestSent": true, "requestId": req.ID}))
	})

	// ---- 待处理好友申请列表 ----
	srv.On("friend:requests", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		reqs, err := store.ListPendingFriendRequests(ctx, a.ID)
		if err != nil {
			logger.Error(mod, "获取好友申请列表失败", "error", err.Error(), "userId", a.ID)
			ack(util.Fail("获取失败"))
			return
		}
		ack(util.OK(map[string]any{"requests": reqs}))
	})

	// ---- 同意/拒绝好友申请 ----
	srv.On("friend:respond", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		reqID, ok := toInt64(d["requestId"])
		if !ok || reqID <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		accept := d["accept"] == true
		err := store.RespondFriendRequest(ctx, reqID, a.ID, accept)
		if err != nil {
			var nf *db.NotFoundError
			var ae *db.AlreadyExistsError
			if errors.As(err, &nf) {
				ack(util.Fail("申请不存在或已处理"))
				return
			}
			if errors.As(err, &ae) {
				ack(util.Fail("申请已处理"))
				return
			}
			logger.Error(mod, "处理好友申请失败", "error", err.Error(), "userId", a.ID)
			ack(util.Fail("处理失败"))
			return
		}
		// 广播好友更新
		_ = srv.EmitTo(util.UserRoom(a.ID), "friends:updated", map[string]any{})
		// 通知对方
		_ = srv.EmitTo(util.UserRoom(a.ID), "friend:request", map[string]any{"refresh": true})
		ack(util.OK(map[string]any{"accepted": accept}))
	})

	// ---- 移除好友 ----
	srv.On("friend:remove", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		uid, ok := toInt64(d["userId"])
		if !ok || uid <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		if err := store.RemoveFriend(ctx, a.ID, uid); err != nil {
			logger.Warn(mod, "移除好友失败（可能不是好友）", "error", err.Error(), "from", a.ID, "to", uid)
		}
		_ = srv.EmitTo(util.UserRoom(a.ID), "friends:updated", map[string]any{})
		_ = srv.EmitTo(util.UserRoom(uid), "friends:updated", map[string]any{})
		ack(util.OK(nil))
	})

	// ---- 好友列表 ----
	srv.On("friend:list", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		friends, err := store.ListFriends(ctx, a.ID)
		if err != nil {
			logger.Error(mod, "获取好友列表失败", "error", err.Error(), "userId", a.ID)
			ack(util.Fail("获取失败"))
			return
		}
		ack(util.OK(map[string]any{"friends": friends}))
	})
}
