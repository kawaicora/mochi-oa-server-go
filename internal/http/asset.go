// 资产二维码免鉴权查看页：GET /asset/qr/{code}。
// 安全取舍：与 /files/* 同级，免登录直接展示极简只读信息（名称/编号/状态/价格/类型），
// 不返回任何内部 ID 或敏感字段，只按 code 查库；code 不可枚举（日期+序号，但无敏感数据泄露风险）。
package http

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"mochi-oa-server-go/internal/db"
	"mochi-oa-server-go/internal/logger"
)

// RegisterAssetQRRoute 注册资产二维码页路由。
func RegisterAssetQRRoute(mux *http.ServeMux, store db.Store) {
	mux.HandleFunc("GET /asset/qr/{code}", func(w http.ResponseWriter, r *http.Request) {
		code := strings.TrimSpace(r.PathValue("code"))
		if code == "" {
			http.NotFound(w, r)
			return
		}
		row, err := store.GetAssetRowByCode(context.Background(), code)
		if err != nil {
			logger.Error("http/asset", "二维码查资产失败", "code", code, "error", err.Error())
			http.Error(w, "内部错误", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if row == nil {
			fmt.Fprint(w, `<meta charset="utf-8"><body style="font-family:sans-serif;padding:2rem">资产不存在</body>`)
			return
		}
		statusText := map[string]string{"in_stock": "在库", "out": "已出库"}[row.Status]
		if statusText == "" {
			statusText = row.Status
		}
		t := `<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<body style="font-family:-apple-system,'Segoe UI',sans-serif;max-width:420px;margin:3rem auto;padding:0 1rem;color:#222">
  <h2 style="margin-bottom:.2rem">` + template.HTMLEscapeString(row.Name) + `</h2>
  <div style="color:#888;margin-bottom:1.5rem">编号 ` + template.HTMLEscapeString(row.Code) + `</div>
  <table style="width:100%;border-collapse:collapse">
    <tr><td style="padding:.5rem 0;color:#888">状态</td><td style="text-align:right;font-weight:600">` + template.HTMLEscapeString(statusText) + `</td></tr>
    <tr><td style="padding:.5rem 0;color:#888">价格</td><td style="text-align:right;font-weight:600">¥` + fmt.Sprintf("%.2f", row.Price) + `</td></tr>
    <tr><td style="padding:.5rem 0;color:#888">类型</td><td style="text-align:right;font-weight:600">` + template.HTMLEscapeString(row.AssetType) + `</td></tr>
  </table>
</body>`
		fmt.Fprint(w, t)
	})
}
