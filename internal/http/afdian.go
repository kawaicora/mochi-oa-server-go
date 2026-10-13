// afdian.go 1:1 移植自 server/src/afdian.ts（爱发电开源赞助订单接入）。
//
// 签名说明（为什么用标准 crypto 库照 TS 手写）：
//   1. Webhook 验签：爱发电平台用 RSA-PKCS1v15 对原文做 SHA256 签名（Node crypto.verify('sha256', ..., {padding:1})），
//      Go 标准库 crypto/rsa.VerifyPKCS1v15 + crypto/sha256 与之逐字节等价——不能用 ECDSA/ED25519 库，
//      爱发电公钥是 PKCS#8 SPKI 的 RSA 2048，必须 x509.ParsePKIXPublicKey 还原再 VeriyPKCS1v15；
//   2. 查单签名：md5(token + "params" + paramsJson + "ts" + ts + "user_id" + userId)，
//      就是普通 md5 hex 拼接（crypto/md5 标准库），没有 HMAC。
//
// 路由：
//   POST /api/afdian/webhook  平台推送（免鉴权，靠 RSA 签名；无 sign 的旧推送走 query-order 回查兜底）
//   POST /api/afdian/query    主动查单（需登录）
//   GET  /api/afdian/orders   本地订单列表（需登录）
package http

import (
	"context"
	"crypto"
	"crypto/md5"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"mochi-oa-server-go/internal/config"
	"mochi-oa-server-go/internal/db"
	"mochi-oa-server-go/internal/logger"
)

// AFDIAN_DEFAULT_PUBLIC_KEY 爱发电平台内置公钥（从源 afdian.ts 逐字照抄，勿改）。
const AFDIAN_DEFAULT_PUBLIC_KEY = `-----BEGIN PUBLIC KEY-----
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAwwdaCg1Bt+UKZKs0R54y
lYnuANma49IpgoOwNmk3a0rhg/PQuhUJ0EOZSowIC44l0K3+fqGns3Ygi4AfmEfS
4EKbdk1ahSxu7Zkp2rHMt+R9GarQFQkwSS/5x1dYiHNVMiR8oIXDgjmvxuNes2Cr
8fw9dEF0xNBKdkKgG2qAawcN1nZrdyaKWtPVT9m2Hl0ddOO9thZmVLFOb9NVzgYf
jEgI+KWX6aY19Ka/ghv/L4t1IXmz9pctablN5S0CRWpJW3Cn0k6zSXgjVdKm4uN7j
RlgSRaf/Ind46vMCm3N2sgwxu/g3bnooW+db0iLo13zzuvyn727Q3UDQ0MmZcEWM
QIDAQAB
-----END PUBLIC KEY-----`

// AFDIAN_API_BASE 爱发电开放平台根地址。
const AFDIAN_API_BASE = "https://afdian.com"

// afdianHandler 聚合依赖。
type afdianHandler struct {
	store db.Store
	cfg   *config.ServerConfig
}

// RegisterAfdianRoutes 注册爱发电路由（对标 registerAfdianRoutes）。
func RegisterAfdianRoutes(mux *http.ServeMux, store db.Store, cfg *config.ServerConfig) {
	h := &afdianHandler{store: store, cfg: cfg}
	mux.HandleFunc("POST /api/afdian/webhook", h.webhook)
	mux.HandleFunc("POST /api/afdian/query", h.query)
	mux.HandleFunc("GET /api/afdian/orders", h.orders)
}

// afdianJSON 统一应答（{code,msg,data}）。
func afdianJSON(w http.ResponseWriter, code int, body map[string]any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// afdianCfg 取启用配置；未启用返回 nil（路由层统一 503）。
func (h *afdianHandler) afdianCfg() *config.AfdianConfig {
	if h.cfg.Afdian == nil || h.cfg.Afdian.UserID == "" {
		return nil
	}
	return h.cfg.Afdian
}

// ---- 动态 JSON 字段取值小工具（对齐 TS 的 String()/Number() 强制转换语义） ----

func afString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		return ""
	}
}

func afInt(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case string:
		n, _ := strconv.ParseFloat(t, 64)
		return int64(n)
	case json.Number:
		n, _ := t.Int64()
		return n
	default:
		return 0
	}
}

// verifyAfdianSign RSA-SHA256 / PKCS1v15 验签（原文 = out_trade_no+user_id+plan_id+total_amount，sign=base64）。
func verifyAfdianSign(order map[string]any, cfg *config.AfdianConfig) bool {
	sign, _ := order["sign"].(string)
	if sign == "" {
		return false
	}
	pem := cfg.PublicKey
	if strings.TrimSpace(pem) == "" {
		pem = AFDIAN_DEFAULT_PUBLIC_KEY
	}
	// .env 里可能写成字面 \n（TS 正则把 \\n 换成换行）。
	pem = strings.ReplaceAll(pem, "\\n", "\n")
	_, raw := pemDecode(pem)
	if raw == nil {
		logger.Warn("afdian", "Webhook 验签公钥解析失败")
		return false
	}
	pubAny, err := x509.ParsePKIXPublicKey(raw)
	if err != nil {
		logger.Warn("afdian", "Webhook 验签公钥 ParsePKIXPublicKey 失败")
		return false
	}
	rsaPub, ok := pubAny.(*rsa.PublicKey)
	if !ok {
		logger.Warn("afdian", "Webhook 验签公钥非 RSA")
		return false
	}
	msg := afString(order["out_trade_no"]) + afString(order["user_id"]) +
		afString(order["plan_id"]) + afString(order["total_amount"])
	sig, err := base64.StdEncoding.DecodeString(sign)
	if err != nil {
		return false
	}
	hashed := sha256.Sum256([]byte(msg))
	if err := rsa.VerifyPKCS1v15(rsaPub, crypto.SHA256, hashed[:], sig); err != nil {
		logger.Warn("afdian", "Webhook 签名比对失败", "error", err.Error())
		return false
	}
	return true
}

// pemDecode 极简 PEM 解码（避免引入 encoding/pem 之外的依赖——标准库 encoding/pem 本就够）。
func pemDecode(pemStr string) ([]byte, []byte) {
	lines := strings.Split(strings.TrimSpace(pemStr), "\n")
	if len(lines) < 3 {
		return nil, nil
	}
	body := strings.Join(lines[1:len(lines)-1], "")
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(body))
	if err != nil {
		return nil, nil
	}
	return []byte(lines[0]), raw
}

// afdianMd5 构造查单签名 md5(token + "params" + paramsJson + "ts" + ts + "user_id" + userId)
// （逐字对齐 TS: md5(`${cfg.token}params${params}ts${ts}user_id${cfg.userId}`)——params 紧跟 "params" 字面量之后）。
func afdianMd5(cfg *config.AfdianConfig, paramsJSON string, ts int64) string {
	s := cfg.Token + "params" + paramsJSON + "ts" + strconv.FormatInt(ts, 10) + "user_id" + cfg.UserID
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// callAfdianApi POST 爱发电开放平台 API（query-order/ping），返回原始 JSON。
func callAfdianApi(cfg *config.AfdianConfig, endpoint string, params map[string]any) (map[string]any, error) {
	ts := time.Now().Unix()
	paramJSON, _ := json.Marshal(params)
	sign := afdianMd5(cfg, string(paramJSON), ts)
	u := fmt.Sprintf("%s/api/open/%s?user_id=%s&params=%s&ts=%d&sign=%s",
		AFDIAN_API_BASE, endpoint,
		strings.ReplaceAll(urlQueryEscape(cfg.UserID), "%2F", "/"),
		urlQueryEscape(string(paramJSON)), ts, sign)
	req, err := http.NewRequest(http.MethodPost, u, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 15 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("爱发电 %s 响应 JSON 解析失败", endpoint)
	}
	return out, nil
}

// getAfdianUserProfile 取下单用户公开资料（昵称/头像，免鉴权；失败返回 nil 不阻断）。
func getAfdianUserProfile(userID string) (name, avatar string) {
	if userID == "" {
		return "", ""
	}
	u := fmt.Sprintf("%s/api/user/get-profile?user_id=%s", AFDIAN_API_BASE, urlQueryEscape(userID))
	client := &http.Client{Timeout: 10 * time.Second}
	res, err := client.Get(u)
	if err != nil {
		logger.Warn("afdian", "get-profile 请求失败", "userId", userID, "error", err.Error())
		return "", ""
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var out map[string]any
	if json.Unmarshal(body, &out) != nil {
		return "", ""
	}
	if ec, _ := out["ec"].(float64); ec != 200 {
		return "", ""
	}
	data, _ := out["data"].(map[string]any)
	user, _ := data["user"].(map[string]any)
	return afString(user["name"]), afString(user["avatar"])
}

// orderToStore 把 snake_case 订单对象转成落库输入（逐字段对齐 TS orderToStore）。
func orderToStore(o map[string]any) db.UpsertAfdianOrderInput {
	amount := afString(o["total_amount"])
	if amount == "" {
		amount = "0.00"
	}
	showAmount := afString(o["show_amount"])
	if showAmount == "" {
		showAmount = "0.00"
	}
	discount := afString(o["discount"])
	if discount == "" {
		discount = "0.00"
	}
	skuDetail := o["sku_detail"]
	if skuDetail == nil {
		skuDetail = []any{}
	}
	skuJSON, _ := json.Marshal(skuDetail)
	rawJSON, _ := json.Marshal(o)
	return db.UpsertAfdianOrderInput{
		OutTradeNo:    afString(o["out_trade_no"]),
		CustomOrderID: afString(o["custom_order_id"]),
		PlanID:        afString(o["plan_id"]),
		PlanTitle:     afString(o["plan_title"]),
		Title:         afString(o["title"]),
		Month:         afInt(o["month"]),
		Amount:        amount,
		ShowAmount:    showAmount,
		Status:        afInt(o["status"]),
		Remark:        afString(o["remark"]),
		RedeemID:      afString(o["redeem_id"]),
		ProductType:   afInt(o["product_type"]),
		Discount:      discount,
		BuyerUserID:   afString(o["user_id"]),
		CreateTime:    afInt(o["create_time"]),
		SkuDetail:     string(skuJSON),
		Raw:           string(rawJSON),
	}
}

// webhook POST /api/afdian/webhook —— 平台推送（免鉴权）。
func (h *afdianHandler) webhook(w http.ResponseWriter, r *http.Request) {
	cfg := h.afdianCfg()
	if cfg == nil {
		afdianJSON(w, http.StatusServiceUnavailable, map[string]any{"code": -1, "msg": "爱发电未配置（.env 缺 AFDIAN_USER_ID）", "data": nil})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err != nil {
		afdianJSON(w, http.StatusOK, map[string]any{"ec": 400, "em": "read body"})
		return
	}
	var parsed map[string]any
	if json.Unmarshal(body, &parsed) != nil {
		afdianJSON(w, http.StatusOK, map[string]any{"ec": 400, "em": "bad json"})
		return
	}
	// body.data.order
	data, _ := parsed["data"].(map[string]any)
	order, _ := data["order"].(map[string]any)
	if order == nil {
		logger.Warn("afdian", "Webhook 无订单数据")
		afdianJSON(w, http.StatusOK, map[string]any{"ec": 400, "em": "no order data"})
		return
	}
	outTradeNo := afString(order["out_trade_no"])
	verified := verifyAfdianSign(order, cfg)
	if !verified {
		if _, hasSign := order["sign"]; !hasSign || afString(order["sign"]) == "" {
			// 无签名旧推送 → query-order 回查兜底
			logger.Warn("afdian", "Webhook 无签名，回查验证", "outTradeNo", outTradeNo)
			if _, err := callAfdianApi(cfg, "query-order", map[string]any{"out_trade_no": outTradeNo}); err != nil {
				logger.Error("afdian", "回查失败", "outTradeNo", outTradeNo, "error", err.Error())
				afdianJSON(w, http.StatusOK, map[string]any{"ec": 500, "em": "query failed"})
				return
			}
		} else {
			logger.Warn("afdian", "Webhook 签名验证失败", "outTradeNo", outTradeNo)
			afdianJSON(w, http.StatusOK, map[string]any{"ec": 401, "em": "bad sign"})
			return
		}
	}
	// 用户资料（失败不阻断）
	name, avatar := getAfdianUserProfile(afString(order["user_id"]))
	stored := orderToStore(order)
	stored.BuyerName = name
	stored.BuyerAvatar = avatar
	if _, err := h.store.UpsertAfdianOrder(context.Background(), stored); err != nil {
		logger.Error("afdian", "落库失败", "outTradeNo", outTradeNo, "error", err.Error())
	} else {
		buyer := stored.BuyerName
		if buyer == "" {
			buyer = stored.BuyerUserID
		}
		logger.Info("afdian", "订单推送落地", "outTradeNo", outTradeNo, "amount", stored.Amount, "buyer", buyer)
	}
	afdianJSON(w, http.StatusOK, map[string]any{"ec": 200, "em": ""})
}

// query POST /api/afdian/query —— 主动查单（需登录）。
func (h *afdianHandler) query(w http.ResponseWriter, r *http.Request) {
	cfg := h.afdianCfg()
	if cfg == nil {
		afdianJSON(w, http.StatusServiceUnavailable, map[string]any{"code": -1, "msg": "爱发电未配置（.env 缺 AFDIAN_USER_ID）", "data": nil})
		return
	}
	uid := authedUserId(r, h.store, h.cfg)
	if uid == 0 {
		logger.Warn("afdian", "未登录访问 /api/afdian/query")
		afdianJSON(w, http.StatusUnauthorized, map[string]any{"code": -1, "msg": "未登录", "data": nil})
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	var d struct {
		OutTradeNo string `json:"out_trade_no"`
		Page       int64  `json:"page"`
	}
	_ = json.Unmarshal(body, &d)
	params := map[string]any{}
	if d.OutTradeNo != "" {
		params["out_trade_no"] = d.OutTradeNo
	}
	if d.Page > 0 {
		params["page"] = d.Page
	}
	resp, err := callAfdianApi(cfg, "query-order", params)
	if err != nil {
		logger.Error("afdian", "查单失败", "error", err.Error())
		afdianJSON(w, http.StatusInternalServerError, map[string]any{"code": -1, "msg": "查单失败", "data": nil})
		return
	}
	data, _ := resp["data"].(map[string]any)
	if list, _ := data["list"].([]any); len(list) > 0 {
		for _, item := range list {
			o, _ := item.(map[string]any)
			if o == nil {
				continue
			}
			stored := orderToStore(o)
			name, avatar := getAfdianUserProfile(stored.BuyerUserID)
			stored.BuyerName = name
			stored.BuyerAvatar = avatar
			_, _ = h.store.UpsertAfdianOrder(context.Background(), stored)
		}
	}
	ec := 0.0
	if e, ok := resp["ec"].(float64); ok {
		ec = e
	}
	msg, _ := resp["em"].(string)
	code := -1
	if ec == 1 {
		code = 1
	}
	afdianJSON(w, http.StatusOK, map[string]any{"code": code, "msg": msg, "data": resp["data"]})
}

// orders GET /api/afdian/orders?limit&offset —— 本地订单列表（需登录）。
func (h *afdianHandler) orders(w http.ResponseWriter, r *http.Request) {
	if h.afdianCfg() == nil {
		afdianJSON(w, http.StatusServiceUnavailable, map[string]any{"code": -1, "msg": "爱发电未配置（.env 缺 AFDIAN_USER_ID）", "data": nil})
		return
	}
	uid := authedUserId(r, h.store, h.cfg)
	if uid == 0 {
		afdianJSON(w, http.StatusUnauthorized, map[string]any{"code": -1, "msg": "未登录", "data": nil})
		return
	}
	limit := int64(50)
	if v, err := strconv.ParseInt(r.URL.Query().Get("limit"), 10, 64); err == nil && v > 0 {
		limit = v
	}
	if limit > 200 {
		limit = 200
	}
	offset := int64(0)
	if v, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64); err == nil {
		offset = v
	}
	list, err := h.store.ListAfdianOrders(context.Background(), limit, offset)
	if err != nil {
		list = nil
	}
	logger.Info("afdian", "本地订单列表", "count", len(list))
	afdianJSON(w, http.StatusOK, map[string]any{"code": 1, "msg": "success", "data": list})
}
