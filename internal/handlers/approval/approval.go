// Package approval 实现「公司默认审批抄送人」配置的 socket 事件。
//
// 事件：
//   - approval:settingsGet：读公司默认抄送人 userId 列表（登录 + 公司成员即可，
//     供审批发起页预填 ccIds，可在审批页替换/清空，非强制）。
//   - approval:settingsSet：整表替换公司默认抄送人（仅 owner/admin）。
//
// 表：company_approval_cc（见 types.CompanyApprovalCcRow）。
package approval

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"mochi-oa-server-go/internal/db"
	"mochi-oa-server-go/internal/logger"
	"mochi-oa-server-go/internal/socketapi"
	"mochi-oa-server-go/internal/types"
	"mochi-oa-server-go/internal/util"
)

const mod = "handlers/approval"

// RegisterApprovalHandlers 注册公司默认审批抄送人配置事件。
func RegisterApprovalHandlers(srv socketapi.Server, store db.Store) {
	ctx := context.Background()

	// ---- approval:settingsGet：读公司默认抄送人 ----
	srv.On("approval:settingsGet", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		_, cid, _, ok := requireMember(c, parseData(data), store, ack)
		if !ok {
			return
		}
		ids, err := store.GetApprovalCc(ctx, cid)
		if err != nil {
			logger.Error(mod, "approval:settingsGet 读取失败", "companyId", cid, "error", err.Error())
			ack(util.Fail("读取默认抄送失败"))
			return
		}
		ack(util.OK(map[string]any{"userIds": ids}))
	})

	// ---- approval:settingsSet：整表替换公司默认抄送人（owner/admin） ----
	srv.On("approval:settingsSet", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		_, cid, _, ok := requireAdmin(c, d, store, ack)
		if !ok {
			return
		}
		ids := int64Slice(d["userIds"])
		if err := store.SetApprovalCc(ctx, cid, ids); err != nil {
			logger.Error(mod, "approval:settingsSet 保存失败", "companyId", cid, "error", err.Error())
			ack(util.Fail("保存默认抄送失败"))
			return
		}
		ack(util.OK(map[string]any{"userIds": ids}))
	})
}

// ---- 小工具（与 asset/org 等各 handler 包同款，独立小包内自足） ----

func parseData(data json.RawMessage) map[string]any {
	m := map[string]any{}
	if len(data) == 0 {
		return m
	}
	_ = json.Unmarshal(data, &m)
	return m
}

// int64Slice 把 userIds 数组（JSON 数字）解析为 []int64；非法项跳过。
func int64Slice(v any) []int64 {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]int64, 0, len(arr))
	for _, e := range arr {
		if n, ok := toInt64(e); ok {
			out = append(out, n)
		}
	}
	return out
}

func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case string:
		i, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
		if err != nil {
			return 0, false
		}
		return i, true
	}
	return 0, false
}

func getAuth(c socketapi.Conn) *types.Auth {
	a, _ := c.Get("auth").(*types.Auth)
	return a
}

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

// requireMember 校验登录 + 是该公司成员，返回 auth/companyId/role。
func requireMember(c socketapi.Conn, d map[string]any, st db.Store, ack socketapi.Ack) (*types.Auth, int64, string, bool) {
	a := getAuth(c)
	if a == nil {
		ack(util.Fail("未登录"))
		return nil, 0, "", false
	}
	cid, ok := toInt64(d["companyId"])
	if !ok || cid <= 0 {
		ack(util.Fail("参数不合法"))
		return nil, 0, "", false
	}
	role, err := st.GetMemberRole(context.Background(), cid, a.ID)
	if err != nil {
		logger.Error(mod, "查询成员角色失败", "companyId", cid, "userId", a.ID, "error", err.Error())
		ack(util.Fail("操作失败"))
		return nil, 0, "", false
	}
	if role == "" {
		ack(util.Fail("未加入该公司"))
		return nil, 0, "", false
	}
	return a, cid, role, true
}

// requireAdmin 在 requireMember 基础上限定 owner/admin。
func requireAdmin(c socketapi.Conn, d map[string]any, st db.Store, ack socketapi.Ack) (*types.Auth, int64, string, bool) {
	a, cid, role, ok := requireMember(c, d, st, ack)
	if !ok {
		return nil, 0, "", false
	}
	if roleRank(role) < roleRank("admin") {
		ack(util.Fail("仅管理员可操作"))
		return nil, 0, "", false
	}
	return a, cid, role, true
}
