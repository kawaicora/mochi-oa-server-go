// resetpwd.go 1:1 移植自 server/src/reset-pwd.ts（服务端渲染的「邮箱重置密码」页）。
//
// GET  /view/reset-pwd?token=…  → 渲染重置表单（或失败页）
// POST /view/reset-pwd?token=…  → 校验 token + 邮箱一致 → 改密 → 断开该用户全部在线会话
//
// 页面为极简移动端友好 HTML（TS 内联样式照抄；templates 目录无此页模板）。
package http

import (
	"context"
	"fmt"
	"html"
	"net/http"

	"mochi-oa-server-go/internal/config"
	"mochi-oa-server-go/internal/db"
	"mochi-oa-server-go/internal/logger"
	"mochi-oa-server-go/internal/password"
	"mochi-oa-server-go/internal/resettoken"
	"mochi-oa-server-go/internal/socketapi"
	"mochi-oa-server-go/internal/util"
)

// RegisterResetPwd 注册重置密码路由（对标 registerResetPwdRoutes）。
func RegisterResetPwd(mux *http.ServeMux, store db.Store, srv socketapi.Server, cfg *config.ServerConfig) {
	h := &resetPwdHandler{store: store, srv: srv, cfg: cfg}
	mux.HandleFunc("/view/reset-pwd", h.page)
}

type resetPwdHandler struct {
	store db.Store
	srv   socketapi.Server
	cfg   *config.ServerConfig
}

// page 同时认领 GET（表单）与 POST（提交）。
func (h *resetPwdHandler) page(w http.ResponseWriter, r *http.Request) {
	tokenStr := r.URL.Query().Get("token")
	if r.Method == http.MethodPost {
		h.doReset(w, r, tokenStr)
		return
	}
	// GET：校验 token 是否有效（peek 不消费），无效直接失败页。
	if tokenStr == "" || resettoken.Peek(tokenStr) == nil {
		renderResetPwdPage(w, false, "链接无效或已过期，请重新发起", "")
		return
	}
	renderResetPwdPage(w, true, "", "")
}

// doReset 提交新密码：token 消费 + 邮箱一致校验 + 改密 + 踢下线。
func (h *resetPwdHandler) doReset(w http.ResponseWriter, r *http.Request, tokenStr string) {
	if tokenStr == "" {
		renderResetPwdPage(w, false, "链接无效或已过期，请重新发起", "")
		return
	}
	ti := resettoken.Peek(tokenStr)
	if ti == nil {
		renderResetPwdPage(w, false, "链接无效或已过期，请重新发起", "")
		return
	}
	user, err := h.store.GetUserByID(context.Background(), ti.UserID)
	if err != nil || user == nil || user.Email == "" || user.Email != ti.Email {
		logger.Warn("http/reset-pwd", "重置密码：用户邮箱与 token 不一致", "userId", ti.UserID)
		renderResetPwdPage(w, false, "链接无效或已过期，请重新发起", "")
		return
	}
	_ = r.ParseForm()
	password1 := r.PostFormValue("password")
	password2 := r.PostFormValue("password2")
	if password1 == "" || len(password1) < 6 || password1 != password2 {
		renderResetPwdPage(w, false, "两次输入不一致或密码少于 6 位", tokenStr)
		return
	}
	hash, err := password.HashPassword(password1)
	if err != nil {
		logger.Error("http/reset-pwd", "重置密码：哈希失败", "error", err.Error())
		renderResetPwdPage(w, false, "服务错误，请稍后再试", "")
		return
	}
	// 消费 token（一次性；Consume 返回 nil = 已用或过期）。
	if resettoken.Consume(tokenStr) == nil {
		renderResetPwdPage(w, false, "链接无效或已过期，请重新发起", "")
		return
	}
	if err := h.store.UpdateUserPassword(context.Background(), ti.UserID, hash); err != nil {
		logger.Error("http/reset-pwd", "重置密码：改密失败", "error", err.Error())
		renderResetPwdPage(w, false, "修改失败：" + err.Error(), "")
		return
	}
	// 踢下线：通知旧客户端会话失效并删除会话记录（对标 kickSessions）。
	if sessions, err := h.store.ListSessionsByUser(context.Background(), ti.UserID); err == nil {
		for _, s := range sessions {
			_ = h.srv.EmitTo(util.SessionRoom(s.ID), "auth:sessionRevoked", map[string]any{
				"reason": "密码已重置", "kick": true,
			})
			_ = h.store.DeleteSession(context.Background(), s.ID)
		}
	}
	logger.Info("http/reset-pwd", "重置密码成功", "userId", ti.UserID)
	renderResetPwdPage(w, false, "密码已重置，请用新密码登录", "")
}

// renderResetPwdPage 渲染页面（showForm=true 表单；否则提示 + 可回填原 token 再试）。
func renderResetPwdPage(w http.ResponseWriter, showForm bool, message, token string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	page := resetPwdHTML
	if showForm {
		page = fmt.Sprintf(`<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>重置密码</title>
<style>
body{font-family:system-ui,-apple-system,"PingFang SC","Microsoft YaHei",sans-serif;background:#141822;color:#e6e6e6;display:flex;justify-content:center;align-items:center;min-height:100vh;margin:0}
.box{background:#1c2230;padding:28px;border-radius:12px;width:90%;max-width:360px;box-shadow:0 8px 24px rgba(0,0,0,.4)}
h2{margin:0 0 18px;font-size:20px}
input{width:100%;height:42px;margin:8px 0 14px;padding:0 12px;border:1px solid #333a49;border-radius:8px;background:#141822;color:#e6e6e6;box-sizing:border-box;font-size:14px}
button{width:100%;height:42px;border:0;border-radius:8px;background:#4c7dff;color:#fff;font-size:15px;cursor:pointer}
button:active{opacity:.85}
</style></head><body><div class="box">
<h2>重置密码</h2>
<form method="post">
<input type="password" name="password" placeholder="新密码（≥6 位）" required>
<input type="password" name="password2" placeholder="确认新密码" required>
<button type="submit">确认修改</button>
</form>
</div></body></html>`)
	}
	if message != "" {
		page = fmt.Sprintf(`<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>重置密码</title>
<style>
body{font-family:system-ui,-apple-system,"PingFang SC","Microsoft YaHei",sans-serif;background:#141822;color:#e6e6e6;display:flex;justify-content:center;align-items:center;min-height:100vh;margin:0}
.box{background:#1c2230;padding:28px;border-radius:12px;width:90%%;max-width:360px;text-align:center;box-shadow:0 8px 24px rgba(0,0,0,.4)}
h2{margin:0 0 12px;font-size:18px}
p{color:#9aa3b2;font-size:14px}
</style></head><body><div class="box">
<h2>%s</h2><p>如仍未解决，请重新在客户端发起重置。</p>
</div></body></html>`, html.EscapeString(message))
	}
	fmt.Fprint(w, page)
}

// resetPwdHTML 提示态默认页占位（renderResetPwdPage 内部会覆盖）。
const resetPwdHTML = `<!DOCTYPE html><html><body></body></html>`
