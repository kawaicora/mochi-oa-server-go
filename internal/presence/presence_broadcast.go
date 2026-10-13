package presence

import (
	"context"

	"mochi-oa-server-go/internal/db"
	"mochi-oa-server-go/internal/logger"
	"mochi-oa-server-go/internal/socketapi"
	"mochi-oa-server-go/internal/types"
	"mochi-oa-server-go/internal/util"
)

// Broadcast 向该用户所属的所有群房间广播在线状态变更（1:1 移植 presence-broadcast.ts）。
//
// 语义逐字对齐 TS：
//   - 谁触发：连接建立后（online=true）/ 断开且该用户无其他在线连接时（online=false）；
//   - 发给谁：该用户加入的每一个群房间 group:{groupId}（store.getUserGroups）；
//   - 事件名：'presence:update'；
//   - 负载：{ userId, nick, online }——nick 取连接态 auth.nick（resolveNick 已补齐）；
//   - 未登录连接（无 auth）直接忽略；查询出错仅记日志、不阻断主流程（TS .catch(()=>{})）。
func Broadcast(srv socketapi.Server, store db.Store, c socketapi.Conn, online bool) {
	v := c.Get("auth")
	a, ok := v.(*types.Auth)
	if !ok || a == nil {
		return
	}
	groups, err := store.GetUserGroups(context.Background(), a.ID)
	if err != nil {
		logger.Warn("presence", "broadcast 查询用户群失败", "userId", a.ID, "error", err.Error())
		return
	}
	for _, g := range groups {
		_ = srv.EmitTo(util.GroupRoom(g.ID), "presence:update", map[string]any{
			"userId": a.ID,
			"nick":   a.Nick,
			"online": online,
		})
	}
}
