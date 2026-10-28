// Package proxy 实现出网代理账号管理与上网申请审批的 socket 事件。
//
// 账号管理（admin/owner）：proxy:account:create / list / revoke / delete。
// 上网申请（成员发起，审批人=申请 approverIds 内或 admin/owner）：
//
//	proxy:request:create / list / get / approve / reject / renew。
//
// 状态机：pending(申请中) → approved(已开通,计时) → 到期账号 disabled → renew_pending(续期审批) → approved(续期)。
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"mochi-oa-server-go/internal/db"
	"mochi-oa-server-go/internal/logger"
	"mochi-oa-server-go/internal/socketapi"
	"mochi-oa-server-go/internal/types"
	"mochi-oa-server-go/internal/util"
)

const mod = "handlers/proxy"

// 续期/开通默认时长。
const accessTTL = 30 * 24 * time.Hour

// resolveProxyHost 取下发给客户端的代理服务器 host。
// 取值链：PROXY_HOST env 显式配置 → 否则取客户端握手 Host 头的 host 部分（即客户端连服务器用的地址，
// 正式环境自动为 oa.kawaimoe.org，无需手工配置）→ 最后回退 127.0.0.1。
func resolveProxyHost(c socketapi.Conn) string {
	if h := strings.TrimSpace(os.Getenv("PROXY_HOST")); h != "" {
		return h
	}
	if c != nil {
		if headers, _ := c.Get("handshakeHeaders").(map[string]any); headers != nil {
			if v := headerGetLower(headers, "host"); v != "" {
				if h, _, err := net.SplitHostPort(v); err == nil && h != "" {
					v = h
				}
				// 去掉可能的 [v6] 包裹
				v = strings.TrimPrefix(strings.TrimSuffix(v, "]"), "[")
				if v != "" {
					return v
				}
			}
		}
	}
	return "127.0.0.1"
}

// proxyPort 读 PROXY_HTTP_PORT，缺省 1081。
func proxyPort() int {
	port := 1081
	if p, err := strconv.Atoi(os.Getenv("PROXY_HTTP_PORT")); err == nil && p > 0 {
		port = p
	}
	return port
}

// headerGetLower 从握手 headers map（key 已可能小写）取值，大小写不敏感。
func headerGetLower(headers map[string]any, key string) string {
	for k, v := range headers {
		if strings.EqualFold(k, key) {
			switch t := v.(type) {
			case string:
				return t
			case []string:
				if len(t) > 0 {
					return t[0]
				}
			}
		}
	}
	return ""
}

// buildAccountView 把 DB 账号转成连接视图；withPassword=true 时带 secret（仅 get 本人）。
func buildAccountView(c socketapi.Conn, acc *types.ProxyAccountRow, withPassword bool) *types.ProxyAccountView {
	if acc == nil {
		return nil
	}
	host := resolveProxyHost(c)
	port := proxyPort()
	exp := util.LocalTimeFormat(acc.ExpiresAt)
	v := &types.ProxyAccountView{
		ID:       acc.ID,
		Host:     host,
		Port:     port,
		Username: acc.Username,
		ExpiresAt: &exp,
	}
	if withPassword {
		pw := acc.Secret
		v.Password = &pw
	}
	return v
}

// ─── 私有工具 ───

func getAuth(c socketapi.Conn) *types.Auth {
	v := c.Get("auth")
	if a, ok := v.(*types.Auth); ok && a != nil {
		return a
	}
	return nil
}

func parseData(data json.RawMessage) map[string]any {
	m := map[string]any{}
	if len(data) == 0 {
		return m
	}
	_ = json.Unmarshal(data, &m)
	return m
}

func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		if n != math.Trunc(n) {
			return 0, false
		}
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

func strField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func strPtr(m map[string]any, key string) *string {
	v, present := m[key]
	if !present || v == nil {
		return nil
	}
	s, _ := v.(string)
	return &s
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

func requireAdmin(c socketapi.Conn, d map[string]any, st db.Store, ack socketapi.Ack) (*types.Auth, int64, bool) {
	a, cid, role, ok := requireMember(c, d, st, ack)
	if !ok {
		return nil, 0, false
	}
	if roleRank(role) < roleRank("admin") {
		ack(util.Fail("仅管理员可操作"))
		return nil, 0, false
	}
	return a, cid, true
}

// canApproveRequest：admin/owner 恒可；否则须在该申请 approverIds 快照内。
func canApproveRequest(req *types.ProxyRequestItem, role string, uid int64) bool {
	if roleRank(role) >= roleRank("admin") {
		return true
	}
	for _, id := range req.ApproverIDs {
		if id == uid {
			return true
		}
	}
	return false
}

// ─── 注册入口 ───

// RegisterProxyHandlers 注册 proxy:account:* 与 proxy:request:* 事件。
func RegisterProxyHandlers(srv socketapi.Server, store db.Store) {
	ctx := context.Background()

	// ====== 账号管理（admin/owner） ======

	// ---- proxy:account:create：生成账号，secret 仅本次返回 ----
	srv.On("proxy:account:create", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		a, cid, ok := requireAdmin(c, d, store, ack)
		if !ok {
			return
		}
		// 有效期：缺省 30 天后；可传 YYYY-MM-DD 或 ISO datetime。
		expires := time.Now().UTC().Add(accessTTL)
		if es := strField(d, "expiresAt"); es != "" {
			if t, err := parseFlexTime(es); err == nil {
				expires = t
			} else {
				ack(util.Fail("有效期格式不合法"))
				return
			}
		}
		note := strPtr(d, "note")
		row, err := store.CreateProxyAccount(ctx, cid, a.ID, expires, note)
		if err != nil {
			logger.Error(mod, "proxy:account:create 失败", "companyId", cid, "error", err.Error())
			ack(util.Fail("创建失败"))
			return
		}
		// secret 仅在此响应返回一次；列表/视图永不带出。
		ack(util.OK(map[string]any{
			"account": map[string]any{
				"id":        row.ID,
				"companyId": row.CompanyID,
				"username":  row.Username,
				"secret":    row.Secret,
				"expiresAt": toIso(row.ExpiresAt),
				"status":    row.Status,
				"note":      row.Note,
				"createdBy": row.CreatedBy,
				"createdAt": toIso(row.CreatedAt),
			},
		}))
	})

	// ---- proxy:account:list ----
	srv.On("proxy:account:list", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		_, cid, ok := requireAdmin(c, d, store, ack)
		if !ok {
			return
		}
		page, pageSize := pageArgs(d, 20)
		rows, total, err := store.ListProxyAccounts(ctx, cid, page, pageSize)
		if err != nil {
			logger.Error(mod, "proxy:account:list 失败", "companyId", cid, "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		if rows == nil {
			rows = []types.ProxyAccountItem{}
		}
		ack(util.OK(map[string]any{"accounts": rows, "total": total}))
	})

	// ---- proxy:account:revoke：立即停用（仅本公司账号） ----
	srv.On("proxy:account:revoke", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		_, cid, ok := requireAdmin(c, d, store, ack)
		if !ok {
			return
		}
		id, ok := toInt64(d["id"])
		if !ok || id <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		// 跨公司隔离：账号必须属于本公司，否则视为不存在。
		acc, err := store.GetProxyAccountByID(ctx, id)
		if err != nil {
			logger.Error(mod, "proxy:account:revoke 查询失败", "id", id, "error", err.Error())
			ack(util.Fail("操作失败"))
			return
		}
		if acc == nil || acc.CompanyID != cid {
			logger.Warn(mod, "proxy:account:revoke 越权访问", "id", id, "companyId", cid)
			ack(util.Fail("账号不存在"))
			return
		}
		if err := store.DisableProxyAccount(ctx, cid, id); err != nil {
			logger.Error(mod, "proxy:account:revoke 失败", "id", id, "error", err.Error())
			ack(util.Fail("操作失败"))
			return
		}
		ack(util.OK(map[string]any{}))
	})

	// ---- proxy:account:delete：物理删除（仅本公司账号） ----
	srv.On("proxy:account:delete", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		_, cid, ok := requireAdmin(c, d, store, ack)
		if !ok {
			return
		}
		id, ok := toInt64(d["id"])
		if !ok || id <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		// 跨公司隔离：账号必须属于本公司，否则视为不存在。
		acc, err := store.GetProxyAccountByID(ctx, id)
		if err != nil {
			logger.Error(mod, "proxy:account:delete 查询失败", "id", id, "error", err.Error())
			ack(util.Fail("操作失败"))
			return
		}
		if acc == nil || acc.CompanyID != cid {
			logger.Warn(mod, "proxy:account:delete 越权访问", "id", id, "companyId", cid)
			ack(util.Fail("账号不存在"))
			return
		}
		if err := store.DeleteProxyAccount(ctx, cid, id); err != nil {
			logger.Error(mod, "proxy:account:delete 失败", "id", id, "error", err.Error())
			ack(util.Fail("操作失败"))
			return
		}
		ack(util.OK(map[string]any{}))
	})

	// ====== 上网申请（成员发起） ======

	// ---- proxy:request:create ----
	srv.On("proxy:request:create", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		a, cid, _, ok := requireMember(c, d, store, ack)
		if !ok {
			return
		}
		// 每人同时只能有一条 pending/renew_pending/approved（未完结）申请。
		existing, _, err := store.ListProxyAccessRequests(ctx, cid, a.ID, false, "", 1, 50)
		if err != nil {
			logger.Error(mod, "proxy:request:create 查重失败", "companyId", cid, "userId", a.ID, "error", err.Error())
			ack(util.Fail("操作失败"))
			return
		}
		for _, r := range existing {
			if r.Status == "pending" || r.Status == "renew_pending" || r.Status == "approved" {
				ack(util.Fail("已有待审批或生效中的上网申请"))
				return
			}
		}
		// 审批人/抄送人由申请人动态选择（不再读公司级预设审批流）。
		// 逐 id 校验本公司成员；不传则 []（此时由 admin/owner 兜底审批）。
		approverIDs, ok := idList(d, "approverIds")
		if !ok {
			logger.Error(mod, "proxy:request:create 审批人 id 非法", "companyId", cid)
			ack(util.Fail("审批人不在该公司"))
			return
		}
		if len(approverIDs) == 0 {
			logger.Error(mod, "proxy:request:create 未选审批人", "companyId", cid)
			ack(util.Fail("请选择审批人"))
			return
		}
		for _, uid := range approverIDs {
			role, err := store.GetMemberRole(context.Background(), cid, uid)
			if err != nil || role == "" {
				logger.Error(mod, "proxy:request:create 审批人不在该公司", "companyId", cid, "userId", uid, "error", errMsg(err))
				ack(util.Fail("审批人不在该公司"))
				return
			}
		}
		ccIDs, ok := idList(d, "ccIds")
		if !ok {
			logger.Error(mod, "proxy:request:create 抄送人 id 非法", "companyId", cid)
			ack(util.Fail("抄送人不在该公司"))
			return
		}
		for _, uid := range ccIDs {
			role, err := store.GetMemberRole(context.Background(), cid, uid)
			if err != nil || role == "" {
				logger.Error(mod, "proxy:request:create 抄送人不在该公司", "companyId", cid, "userId", uid, "error", errMsg(err))
				ack(util.Fail("抄送人不在该公司"))
				return
			}
		}
		req, err := store.CreateProxyAccessRequest(ctx, db.CreateProxyAccessRequestInput{
			CompanyID:   cid,
			UserID:      a.ID,
			Note:        strPtr(d, "note"),
			ApproverIDs: approverIDs,
			CcIDs:       ccIDs,
		})
		if err != nil {
			logger.Error(mod, "proxy:request:create 失败", "companyId", cid, "userId", a.ID, "error", err.Error())
			ack(util.Fail("创建失败"))
			return
		}
		// 推送通知：申请成功 → 审批人全量 + 抄送人（公司通知类，NotifKind=company）。
		noteSum := ""
		if req.Note != nil {
			noteSum = *req.Note
		}
		notifyUsers := append([]int64{}, approverIDs...)
		notifyUsers = append(notifyUsers, ccIDs...)
		for _, uid := range notifyUsers {
			_ = srv.EmitTo(util.UserRoom(uid), "proxy:request:notification", map[string]any{
				"kind": "company", "type": "proxy_request_created", "companyId": cid,
				"requestId": req.ID, "title": "上网申请", "body": "您有一条待审批的上网申请",
				"byUserId": a.ID, "byNick": req.Nick, "note": noteSum,
			})
		}
		ack(util.OK(map[string]any{"request": req}))
	})

	// ---- proxy:request:list：成员只看自己；admin 看全部 ----
	srv.On("proxy:request:list", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		a, cid, role, ok := requireMember(c, d, store, ack)
		if !ok {
			return
		}
		status := strField(d, "status")
		all := roleRank(role) >= roleRank("admin")
		page, pageSize := pageArgs(d, 10)
		rows, total, err := store.ListProxyAccessRequests(ctx, cid, a.ID, all, status, page, pageSize)
		if err != nil {
			logger.Error(mod, "proxy:request:list 失败", "companyId", cid, "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		if rows == nil {
			rows = []types.ProxyRequestItem{}
		}
		// approved 且有账号时，为每条请求嵌套账号连接信息（list 不带 password）。
		for i := range rows {
			if rows[i].Status == "approved" && rows[i].AccountID != nil {
				if acc, gerr := store.GetProxyAccountByID(ctx, *rows[i].AccountID); gerr == nil && acc != nil {
					rows[i].Account = buildAccountView(c, acc, false)
				}
			}
		}
		ack(util.OK(map[string]any{"requests": rows, "total": total}))
	})

	// ---- proxy:request:get：本人/审批人/admin 可看；仅本人且 approved 返回 secret ----
	srv.On("proxy:request:get", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		a, cid, role, ok := requireMember(c, d, store, ack)
		if !ok {
			return
		}
		id, ok := toInt64(d["id"])
		if !ok || id <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		req, err := store.GetProxyAccessRequestByID(ctx, id)
		if err != nil {
			logger.Error(mod, "proxy:request:get 查申请失败", "id", id, "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		if req == nil || req.CompanyID != cid {
			ack(util.Fail("申请不存在"))
			return
		}
		isSelf := req.UserID == a.ID
		if !isSelf && !canApproveRequest(req, role, a.ID) {
			ack(util.Fail("无权查看该申请"))
			return
		}
		out := map[string]any{"request": req}
		// approved 且有账号时嵌套账号视图；本人 approved 才带 password(secret)。
		if req.Status == "approved" && req.AccountID != nil {
			if acc, gerr := store.GetProxyAccountByID(ctx, *req.AccountID); gerr == nil && acc != nil {
				req.Account = buildAccountView(c, acc, isSelf)
				// 兼容：顶层 secret 仍保留（本人 approved）。
				if isSelf {
					out["secret"] = acc.Secret
				}
			}
		}
		ack(util.OK(out))
	})

	// ---- proxy:request:notification:stop：申请人或审批人主动停止循环提醒（幂等）----
	srv.On("proxy:request:notification:stop", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		a, cid, role, ok := requireMember(c, d, store, ack)
		if !ok {
			return
		}
		id, ok := toInt64(d["id"])
		if !ok || id <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		req, err := store.GetProxyAccessRequestByID(ctx, id)
		if err != nil || req == nil || req.CompanyID != cid {
			logger.Warn(mod, "proxy:request:notification:stop 申请不存在", "id", id, "error", err)
			ack(util.Fail("申请不存在"))
			return
		}
		isSelf := req.UserID == a.ID
		if !isSelf && !canApproveRequest(req, role, a.ID) {
			ack(util.Fail("无权操作该申请"))
			return
		}
		if err := store.StopProxyNotification(ctx, id, cid); err != nil {
			logger.Error(mod, "proxy:request:notification:stop 失败", "id", id, "error", err.Error())
			ack(util.Fail("停止提醒失败"))
			return
		}
		ack(util.OK(map[string]any{"ok": true, "notifyStopped": true}))
	})

	// ---- proxy:request:approve ----
	srv.On("proxy:request:approve", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		a, cid, role, ok := requireMember(c, d, store, ack)
		if !ok {
			return
		}
		id, ok := toInt64(d["id"])
		if !ok || id <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		req, err := store.GetProxyAccessRequestByID(ctx, id)
		if err != nil {
			logger.Error(mod, "proxy:request:approve 查申请失败", "id", id, "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		if req == nil || req.CompanyID != cid {
			ack(util.Fail("申请不存在"))
			return
		}
		if req.Status == "approved" {
			ack(util.Fail("该申请已审批"))
			return
		}
		if req.Status != "pending" && req.Status != "renew_pending" {
			ack(util.Fail("当前状态不可审批"))
			return
		}
		if !canApproveRequest(req, role, a.ID) {
			logger.Error(mod, "proxy:request:approve 无权审批", "companyId", cid, "userId", a.ID, "requestId", id)
			ack(util.Fail("无权审批该申请"))
			return
		}
		now := time.Now().UTC()
		newExpires := now.Add(accessTTL)
		if req.Status == "pending" {
			// 首批：新建账号，回填 account_id。
			acc, cerr := store.CreateProxyAccount(ctx, cid, req.UserID, newExpires, nil)
			if cerr != nil {
				logger.Error(mod, "proxy:request:approve 建账号失败", "id", id, "error", cerr.Error())
				ack(util.Fail("审批失败"))
				return
			}
			accID := acc.ID
			updated, uerr := store.UpdateProxyAccessRequest(ctx, id, db.UpdateProxyAccessRequestInput{
				Status:     strPtrStr("approved"),
				AccountID:  &accID,
				ApproverID: &a.ID,
				ApprovedAt: &now,
				ExpiresAt:  &newExpires,
			})
			if uerr != nil {
				logger.Error(mod, "proxy:request:approve 更新失败", "id", id, "error", uerr.Error())
				ack(util.Fail("审批失败"))
				return
			}
			// 处理时自动停止循环提醒（幂等）。
			if err := store.StopProxyNotification(ctx, id, cid); err != nil {
				logger.Error(mod, "proxy:request:approve 置 notify_stopped 失败", "id", id, "error", err.Error())
			}
			updated.NotifyStopped = true
			_ = srv.EmitTo(util.UserRoom(req.UserID), "proxy:request:notification", map[string]any{
				"kind": "company", "type": "proxy_request_approved", "companyId": cid,
				"requestId": id, "title": "上网申请已通过",
				"body": "您的上网申请已通过，可在详情查看账号",
				"account": map[string]any{"id": acc.ID, "username": acc.Username, "expiresAt": toIso(acc.ExpiresAt)},
			})
			ack(util.OK(map[string]any{
				"request": updated,
				"account": map[string]any{
					"id":        acc.ID,
					"username":  acc.Username,
					"secret":    acc.Secret,
					"expiresAt": toIso(acc.ExpiresAt),
				},
			}))
			return
		}
		// renew_pending：复用原账号，重新开通并顺延 30 天。
		if req.AccountID == nil {
			ack(util.Fail("申请缺少账号，无法续期"))
			return
		}
		if rerr := store.ReactivateProxyAccount(ctx, *req.AccountID, newExpires); rerr != nil {
			logger.Error(mod, "proxy:request:approve 续期失败", "id", id, "error", rerr.Error())
			ack(util.Fail("审批失败"))
			return
		}
		updated, uerr := store.UpdateProxyAccessRequest(ctx, id, db.UpdateProxyAccessRequestInput{
			Status:     strPtrStr("approved"),
			ApproverID: &a.ID,
			ApprovedAt: &now,
			ExpiresAt:  &newExpires,
		})
		if uerr != nil {
			logger.Error(mod, "proxy:request:approve 更新失败", "id", id, "error", uerr.Error())
			ack(util.Fail("审批失败"))
			return
		}
		// 处理时自动停止循环提醒（幂等）。
		if err := store.StopProxyNotification(ctx, id, cid); err != nil {
			logger.Error(mod, "proxy:request:approve(renew) 置 notify_stopped 失败", "id", id, "error", err.Error())
		}
		updated.NotifyStopped = true
		_ = srv.EmitTo(util.UserRoom(req.UserID), "proxy:request:notification", map[string]any{
			"kind": "company", "type": "proxy_request_approved", "companyId": cid,
			"requestId": id, "title": "上网申请已通过",
			"body": "您的上网申请已通过，可在详情查看账号",
		})
		ack(util.OK(map[string]any{"request": updated}))
	})

	// ---- proxy:request:reject ----
	srv.On("proxy:request:reject", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		a, cid, role, ok := requireMember(c, d, store, ack)
		if !ok {
			return
		}
		id, ok := toInt64(d["id"])
		if !ok || id <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		req, err := store.GetProxyAccessRequestByID(ctx, id)
		if err != nil {
			logger.Error(mod, "proxy:request:reject 查申请失败", "id", id, "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		if req == nil || req.CompanyID != cid {
			ack(util.Fail("申请不存在"))
			return
		}
		if req.Status == "rejected" {
			ack(util.Fail("该申请已审批"))
			return
		}
		if req.Status != "pending" && req.Status != "renew_pending" {
			ack(util.Fail("当前状态不可驳回"))
			return
		}
		if !canApproveRequest(req, role, a.ID) {
			ack(util.Fail("无权审批该申请"))
			return
		}
		note := strField(d, "note")
		updated, uerr := store.UpdateProxyAccessRequest(ctx, id, db.UpdateProxyAccessRequestInput{
			Status:       strPtrStr("rejected"),
			ApprovalNote: &note,
		})
		if uerr != nil {
			logger.Error(mod, "proxy:request:reject 失败", "id", id, "error", uerr.Error())
			ack(util.Fail("操作失败"))
			return
		}
		// 处理时自动停止循环提醒（幂等）。
		if err := store.StopProxyNotification(ctx, id, cid); err != nil {
			logger.Error(mod, "proxy:request:reject 置 notify_stopped 失败", "id", id, "error", err.Error())
		}
		updated.NotifyStopped = true
		_ = srv.EmitTo(util.UserRoom(req.UserID), "proxy:request:notification", map[string]any{
			"kind": "company", "type": "proxy_request_rejected", "companyId": cid,
			"requestId": id, "title": "上网申请被拒绝",
			"body": "您的上网申请被拒绝", "note": note,
		})
		ack(util.OK(map[string]any{"request": updated}))
	})

	// ---- proxy:request:renew：本人；原 approved 且账号已到期停用才可发起 ----
	srv.On("proxy:request:renew", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		a, cid, _, ok := requireMember(c, d, store, ack)
		if !ok {
			return
		}
		id, ok := toInt64(d["id"])
		if !ok || id <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		req, err := store.GetProxyAccessRequestByID(ctx, id)
		if err != nil {
			logger.Error(mod, "proxy:request:renew 查申请失败", "id", id, "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		if req == nil || req.CompanyID != cid {
			ack(util.Fail("申请不存在"))
			return
		}
		if req.UserID != a.ID {
			ack(util.Fail("无权操作该申请"))
			return
		}
		if req.Status != "approved" || req.AccountID == nil {
			ack(util.Fail("仅已开通的申请可续期"))
			return
		}
		acc, gerr := store.GetProxyAccountByID(ctx, *req.AccountID)
		if gerr != nil {
			logger.Error(mod, "proxy:request:renew 查账号失败", "id", id, "error", gerr.Error())
			ack(util.Fail("操作失败"))
			return
		}
		// 账号须已停用/到期才允许续期。
		if acc != nil && acc.Status == "active" && time.Now().UTC().Before(acc.ExpiresAt) {
			ack(util.Fail("账号未到期，无需延期"))
			return
		}
		// 续期时由申请人重新动态选择审批人/抄送人（不传则沿用空，admin 兜底）。
		approverIDs, ok := idList(d, "approverIds")
		if !ok {
			logger.Error(mod, "proxy:request:renew 审批人 id 非法", "companyId", cid)
			ack(util.Fail("审批人不在该公司"))
			return
		}
		if len(approverIDs) == 0 {
			logger.Error(mod, "proxy:request:renew 未选审批人", "companyId", cid)
			ack(util.Fail("请选择审批人"))
			return
		}
		for _, uid := range approverIDs {
			role, err := store.GetMemberRole(context.Background(), cid, uid)
			if err != nil || role == "" {
				logger.Error(mod, "proxy:request:renew 审批人不在该公司", "companyId", cid, "userId", uid, "error", errMsg(err))
				ack(util.Fail("审批人不在该公司"))
				return
			}
		}
		ccIDs, ok := idList(d, "ccIds")
		if !ok {
			logger.Error(mod, "proxy:request:renew 抄送人 id 非法", "companyId", cid)
			ack(util.Fail("抄送人不在该公司"))
			return
		}
		for _, uid := range ccIDs {
			role, err := store.GetMemberRole(context.Background(), cid, uid)
			if err != nil || role == "" {
				logger.Error(mod, "proxy:request:renew 抄送人不在该公司", "companyId", cid, "userId", uid, "error", errMsg(err))
				ack(util.Fail("抄送人不在该公司"))
				return
			}
		}
		st := "renew_pending"
		updated, uerr := store.UpdateProxyAccessRequest(ctx, id, db.UpdateProxyAccessRequestInput{
			Status:      &st,
			ApproverIDs: approverIDs,
			CcIDs:       ccIDs,
		})
		if uerr != nil {
			logger.Error(mod, "proxy:request:renew 失败", "id", id, "error", uerr.Error())
			ack(util.Fail("操作失败"))
			return
		}
		ack(util.OK(map[string]any{"request": updated}))
	})
}

// ─── 小工具 ───

func pageArgs(d map[string]any, dftSize int64) (int64, int64) {
	page := int64(1)
	if v, present := d["page"]; present && v != nil {
		if n, ok := toInt64(v); ok && n > 0 {
			page = n
		}
	}
	size := dftSize
	if v, present := d["pageSize"]; present && v != nil {
		if n, ok := toInt64(v); ok && n > 0 {
			size = n
		}
	}
	if size > 100 {
		size = 100
	}
	return page, size
}

func strPtrStr(s string) *string { return &s }

// parseFlexTime 解析 YYYY-MM-DD 或空格完整时间/ISO（util.ParseLocalTime 兼容）。
func parseFlexTime(s string) (time.Time, error) {
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	if t, ok := util.ParseLocalTime(s); ok {
		return t.UTC(), nil
	}
	return time.Time{}, errors.New("时间不合法")
}

// toIso 转业务本地时区可解析字符串（+08:00 偏移、无 Z，与 db/util 同口径）。
func toIso(t time.Time) string { return util.LocalTimeFormat(t) }

// idList 从 payload 取 number[] 并转 []int64；缺失返回空切片（合法），元素非法返回 false。
func idList(d map[string]any, key string) ([]int64, bool) {
	v, present := d[key]
	if !present || v == nil {
		return []int64{}, true
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := []int64{}
	for _, e := range arr {
		n, ok := toInt64(e)
		if !ok || n <= 0 {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// errMsg 安全取错误文本（nil → ""）。
func errMsg(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
