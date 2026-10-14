// adminweb.go 1:1 移植自 server/src/web/admin.ts（管理后台静态页托管）。
//
// 路由（一个不少，逐一对齐 TS）：
//
//	GET /view/admin                 → 管理后台主页（admin.html 内嵌 room/sysmon/users/companies 四个子页 @@占位符替换@@）
//	GET /view/admin/terminal        → terminal.html 整页（终端窗口）
//	GET /view/admin/remote-window   → remote-window.html 整页（远程窗口）
//	GET /view/admin/config.js       → 注入 socket path/transports 的运行时配置脚本
//	GET /view/admin/socket.io.js    → socket.io 官方浏览器端脚本（embed）
//	GET /view/admin/static/*        → css/js 静态资源（embed）
package http

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"strings"

	"mochi-oa-server-go/internal/config"
	"mochi-oa-server-go/web"
)

// assetVer 是前端资源的缓存版本号（自动生成，不需要手动改）。
//
// 静态资源按 Cache-Control: max-age=86400 下发，所以模板里的 js/css 都挂 ?v=版本号。
// 这里在进程启动时对整个 embed 的前端资源（static/、templates/、socket.io.min.js）
// 做一次 SHA-256 取前 12 位：
//   - 改了任何前端文件 → 版本号变化 → 浏览器必然重新拉取；
//   - 没改 → 版本号不变 → 缓存继续命中。
//
// 这样就不用每次改完前端手动去模板里改 ?v=xxxx 了。
var assetVer = computeAssetVer()

func computeAssetVer() string {
	h := sha256.New()
	// fs.WalkDir 按字典序遍历，结果稳定。
	_ = fs.WalkDir(web.Assets, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, rerr := web.Assets.ReadFile(p)
		if rerr != nil {
			return nil
		}
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(b)
		return nil
	})
	_, _ = h.Write(web.SocketIOClientJS)
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// withAssetVer 把模板里的 @@ASSET_VER@@ 占位符替换成实际版本号。
func withAssetVer(html string) string {
	return strings.ReplaceAll(html, "@@ASSET_VER@@", assetVer)
}

// RegisterAdminWeb 注册管理后台路由（对标 registerAdminRoutes）。
func RegisterAdminWeb(mux *http.ServeMux, cfg *config.ServerConfig) {
	h := &adminWebHandler{cfg: cfg, tmpl: loadTemplates(web.Assets)}
	mux.HandleFunc("GET /view/admin", h.index)
	mux.HandleFunc("GET /view/admin/", h.subtree)
}

type adminWebHandler struct {
	cfg  *config.ServerConfig
	tmpl map[string]string
}

// loadTemplates 从 embed.FS 读出全部页面模板。
func loadTemplates(efs embed.FS) map[string]string {
	out := map[string]string{}
	entries, err := fs.ReadDir(efs, "templates")
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".html") {
			continue
		}
		b, err := efs.ReadFile("templates/" + e.Name())
		if err == nil {
			out[e.Name()] = string(b)
		}
	}
	return out
}

// pageHtml 组装管理主页：把四个子页内容原地填进 admin.html 的 @@占位符@@（与 TS 同一套规则）。
func (h *adminWebHandler) pageHtml() string {
	html := h.tmpl["admin.html"]
	repl := map[string]string{
		"@@ROOM_PAGE@@":      h.tmpl["room.html"],
		"@@SYSMON_PAGE@@":    h.tmpl["sysmon.html"],
		"@@USERS_PAGE@@":     h.tmpl["users.html"],
		"@@COMPANIES_PAGE@@": h.tmpl["companies.html"],
	}
	for k, v := range repl {
		html = strings.ReplaceAll(html, k, v)
	}
	return html
}

// index GET /view/admin → 主页。
func (h *adminWebHandler) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// HTML 里引用的是带 ?v= 版本号的静态资源；HTML 本身必须每次校验，
	// 否则浏览器拿旧 HTML 就永远引用旧版本的 js/css（静态资源缓存 24h）。
	w.Header().Set("Cache-Control", "no-cache")
	fmt.Fprint(w, withAssetVer(h.pageHtml()))
}

// subtree 认领 /view/admin/ 下所有子路由（config.js / socket.io.js / terminal / remote-window / static/*）。
func (h *adminWebHandler) subtree(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case p == "/view/admin/config.js":
		// 注入运行时配置：管理页 socket.io 连接沿用 config.Path（管理端 HTML 不硬编码路径）。
		js := "window.__ADMIN_CONFIG__ = { path: '" + h.cfg.Path + "', transports: [\"websocket\",\"polling\"] };"
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		fmt.Fprint(w, js)
	case p == "/view/admin/socket.io.js":
		// socket.io 官方浏览器端脚本（源 node_modules 产物，随二进制 embed）。
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(web.SocketIOClientJS)
	case p == "/view/admin/terminal":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		fmt.Fprint(w, withAssetVer(h.tmpl["terminal.html"]))
	case p == "/view/admin/remote-window":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		fmt.Fprint(w, withAssetVer(h.tmpl["remote-window.html"]))
	case strings.HasPrefix(p, "/view/admin/static/"):
		// 静态资源子树：embed 的 static/ 目录。
		rel := strings.TrimPrefix(p, "/view/admin/static/")
		b, err := web.Assets.ReadFile("static/" + rel)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", guessStaticType(rel))
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(b)
	default:
		http.NotFound(w, r)
	}
}

// guessStaticType 简单按扩展名回 Content-Type。
func guessStaticType(name string) string {
	switch {
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".js"):
		return "application/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".png"):
		return "image/png"
	case strings.HasSuffix(name, ".jpg"), strings.HasSuffix(name, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(name, ".svg"):
		return "image/svg+xml"
	default:
		if bytes.HasSuffix([]byte(name), []byte(".js")) {
			return "application/javascript; charset=utf-8"
		}
		return "application/octet-stream"
	}
}
