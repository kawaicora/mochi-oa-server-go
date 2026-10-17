// Package asset 实现公司资产管理的 socket 事件。
//
// 事件：asset:create / list / get / update / delete（管理类 admin/owner），
//
//	asset:flow:create / asset:flow:list（流转记录）。
//
// code 规则：AS{YYYYMMDD}-{3位序号}，公司内唯一。
package asset

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"mochi-oa-server-go/internal/db"
	"mochi-oa-server-go/internal/logger"
	"mochi-oa-server-go/internal/socketapi"
	"mochi-oa-server-go/internal/types"
	"mochi-oa-server-go/internal/util"
)

const mod = "handlers/asset"

// 流转枚举与方向映射（可扩展：新流转在此登记）。
//
//	in 方向（入库/增加持有）：in_purchase 购买 / in_rent 租赁 / in_borrow 借入 / in_return 退回 / in_reclaim 回收
//	out 方向（出库/减少持有）：out_sell 售卖 / out_gift 赠与 / out_lend 借出 / out_carry 外带 / out_scrap 报废 / out_lost 丢失
var flowDirection = map[string]string{
	"in_purchase": "in", "in_rent": "in", "in_borrow": "in", "in_return": "in", "in_reclaim": "in",
	"out_sell": "out", "out_gift": "out", "out_lend": "out", "out_carry": "out", "out_scrap": "out", "out_lost": "out",
}

// RegisterAssetHandlers 注册资产管理全部 socket 事件。
func RegisterAssetHandlers(srv socketapi.Server, store db.Store) {
	ctx := context.Background()

	// ---- asset:create（admin/owner，服务端生成 code） ----
	srv.On("asset:create", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		_, cid, ok := requireAdmin(c, d, store, ack)
		if !ok {
			return
		}
		name := strField(d, "name")
		if name == "" {
			ack(util.Fail("资产名称不能为空"))
			return
		}
		assetType := strField(d, "assetType")
		if assetType != "purchase" && assetType != "rent" && assetType != "borrow" {
			ack(util.Fail("资产类型不合法"))
			return
		}
		price := toFloat(d["price"])
		item, err := store.CreateAsset(ctx, db.CreateAssetInput{
			CompanyID: cid, Name: name, Price: price, AssetType: assetType,
		})
		if err != nil {
			logger.Error(mod, "asset:create 失败", "companyId", cid, "name", name, "error", err.Error())
			ack(util.Fail("新建资产失败"))
			return
		}
		ack(util.OK(map[string]any{"asset": item}))
	})

	// ---- asset:list（成员；keyword 匹配 name/code） ----
	srv.On("asset:list", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		_, cid, _, ok := requireMember(c, d, store, ack)
		if !ok {
			return
		}
		keyword := strField(d, "keyword")
		page, pageSize := pageArgs(d, 20)
		items, total, err := store.ListAssets(ctx, cid, keyword, page, pageSize)
		if err != nil {
			logger.Error(mod, "asset:list 失败", "companyId", cid, "error", err.Error())
			ack(util.Fail("资产列表失败"))
			return
		}
		if items == nil {
			items = []types.AssetItem{}
		}
		ack(util.OK(map[string]any{"assets": items, "total": total}))
	})

	// ---- asset:get（成员） ----
	srv.On("asset:get", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		_, cid, _, ok := requireMember(c, d, store, ack)
		if !ok {
			return
		}
		id, ok := toInt64(d["id"])
		if !ok || id <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		item, err := store.GetAssetByID(ctx, id)
		if err != nil {
			logger.Error(mod, "asset:get 失败", "id", id, "error", err.Error())
			ack(util.Fail("资产详情失败"))
			return
		}
		if item == nil || item.CompanyID != cid {
			ack(util.Fail("资产不存在"))
			return
		}
		ack(util.OK(map[string]any{"asset": item}))
	})

	// ---- asset:update（admin/owner） ----
	srv.On("asset:update", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
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
		cur, err := store.GetAssetByID(ctx, id)
		if err != nil {
			logger.Error(mod, "asset:update 查资产失败", "id", id, "error", err.Error())
			ack(util.Fail("资产不存在"))
			return
		}
		if cur == nil || cur.CompanyID != cid {
			ack(util.Fail("资产不存在"))
			return
		}
		in := db.UpdateAssetInput{}
		if v, has := d["name"]; has && v != nil {
			s := strings.TrimSpace(toString(v))
			in.Name = &s
		}
		if v, has := d["price"]; has && v != nil {
			p := toFloat(v)
			in.Price = &p
		}
		if v, has := d["assetType"]; has && v != nil {
			s := strings.TrimSpace(toString(v))
			in.AssetType = &s
		}
		if v, has := d["status"]; has && v != nil {
			s := strings.TrimSpace(toString(v))
			in.Status = &s
		}
		item, err := store.UpdateAsset(ctx, id, in)
		if err != nil {
			logger.Error(mod, "asset:update 失败", "id", id, "error", err.Error())
			ack(util.Fail("更新资产失败"))
			return
		}
		ack(util.OK(map[string]any{"asset": item}))
	})

	// ---- asset:delete（admin/owner，连同流转记录一起删） ----
	srv.On("asset:delete", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
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
		cur, err := store.GetAssetByID(ctx, id)
		if err != nil {
			logger.Error(mod, "asset:delete 查资产失败", "id", id, "error", err.Error())
			ack(util.Fail("资产不存在"))
			return
		}
		if cur == nil || cur.CompanyID != cid {
			ack(util.Fail("资产不存在"))
			return
		}
		if err := store.DeleteAsset(ctx, id); err != nil {
			logger.Error(mod, "asset:delete 失败", "id", id, "error", err.Error())
			ack(util.Fail("删除资产失败"))
			return
		}
		ack(util.OK(map[string]any{}))
	})

	// ---- asset:flow:create（admin/owner；按方向更新资产 status） ----
	srv.On("asset:flow:create", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		a, cid, ok := requireAdmin(c, d, store, ack)
		if !ok {
			return
		}
		assetID, ok := toInt64(d["assetId"])
		if !ok || assetID <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		cur, err := store.GetAssetByID(ctx, assetID)
		if err != nil {
			logger.Error(mod, "asset:flow:create 查资产失败", "assetId", assetID, "error", err.Error())
			ack(util.Fail("资产不存在"))
			return
		}
		if cur == nil || cur.CompanyID != cid {
			ack(util.Fail("资产不存在"))
			return
		}
		flowType := strField(d, "flowType")
		dir, valid := flowDirection[flowType]
		if !valid {
			ack(util.Fail("流转类型不合法"))
			return
		}
		in := db.CreateAssetFlowInput{
			CompanyID: cid, AssetID: assetID, FlowType: flowType, Direction: dir,
			OperatorID: a.ID,
		}
		if v, has := d["relatedTo"]; has && v != nil {
			s := strings.TrimSpace(toString(v))
			in.RelatedTo = &s
		}
		if v, has := d["note"]; has && v != nil {
			s := toString(v)
			in.Note = &s
		}
		flow, asset, err := store.CreateAssetFlow(ctx, in)
		if err != nil {
			logger.Error(mod, "asset:flow:create 失败", "assetId", assetID, "flowType", flowType, "error", err.Error())
			ack(util.Fail("资产流转失败"))
			return
		}
		ack(util.OK(map[string]any{"flow": flow, "asset": asset}))
	})

	// ---- asset:flow:list（成员；assetId 缺省=公司全部） ----
	srv.On("asset:flow:list", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		_, cid, _, ok := requireMember(c, d, store, ack)
		if !ok {
			return
		}
		assetID := int64(0)
		if v, present := d["assetId"]; present && v != nil {
			if n, ok := toInt64(v); ok && n > 0 {
				assetID = n
			}
		}
		page, pageSize := pageArgs(d, 20)
		flows, total, err := store.ListAssetFlows(ctx, cid, assetID, page, pageSize)
		if err != nil {
			logger.Error(mod, "asset:flow:list 失败", "companyId", cid, "error", err.Error())
			ack(util.Fail("流转列表失败"))
			return
		}
		if flows == nil {
			flows = []types.FlowItem{}
		}
		ack(util.OK(map[string]any{"flows": flows, "total": total}))
	})
}

// ─── 私有工具（与其他 handler 包同构副本） ───

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

func toFloat(v any) float64 {
	if n, ok := v.(float64); ok {
		return n
	}
	return 0
}

func strField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return ""
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
