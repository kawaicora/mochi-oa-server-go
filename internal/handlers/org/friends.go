// Package org —— friends.go 1:1 复刻 server/src/handlers/friends.ts（50 行）：
//   - 好友添加 / 删除 / 列表
//
// 好友与公司无关，钉钉无公司也能加。
package org

import (
	"context"
	"encoding/json"
	"errors"

	"mochi-oa-server-go/internal/db"
	"mochi-oa-server-go/internal/logger"
	"mochi-oa-server-go/internal/socketapi"
	"mochi-oa-server-go/internal/util"
)

// RegisterFriendHandlers 注册好友相关 socket 事件。
// 对标 registerFriendHandlers(ctx)。
func RegisterFriendHandlers(srv socketapi.Server) {
	store := db.GetStore()
	ctx := context.Background()

	// ---- 加好友（与公司无关） ----
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
		err := store.AddFriend(ctx, a.ID, uid)
		if err != nil {
			// 互加语义：已是好友视为成功（幂等）
			var ae *db.AlreadyExistsError
			if errors.As(err, &ae) {
				ack(util.OK(nil))
				return
			}
			logger.Error(mod, "添加好友失败", "error", err.Error(), "from", a.ID, "to", uid)
			ack(util.Fail("添加失败"))
			return
		}
		_ = srv.EmitTo(util.UserRoom(a.ID), "friends:updated", map[string]any{})
		_ = srv.EmitTo(util.UserRoom(uid), "friends:updated", map[string]any{})
		ack(util.OK(nil))
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
			// TS: .catch(() => ack(fail('不是好友'))) —— 即使删除失败也继续广播
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
