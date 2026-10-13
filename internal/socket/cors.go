package socket

import (
	"net/http"
	"strings"
)

// corsWriter 按 config 决定 Access-Control-Allow-* 头的写入。
//
// 对齐源服务端：cors: { origin: config.corsOrigin, methods: ['GET','POST'] }。
//   - CORSStar=true → Access-Control-Allow-Origin: *
//   - CORSStar=false → 回显请求 Origin（命中 CORSList 才放行），不命中则不写 ACAO。
//
// engine.io 的轮询握手是浏览器跨域 XHR/fetch，必须在每个响应（含 OPTIONS 预检）
// 上带对 CORS 头，否则客户端连不上。
type corsPolicy struct {
	star bool
	list []string
}

func newCORSPolicy(star bool, list []string) corsPolicy {
	return corsPolicy{star: star, list: list}
}

// allowOrigin 判断请求 Origin 是否放行，并返回应写入的 ACAO 值（空串=不写）。
func (p corsPolicy) allowOrigin(origin string) string {
	if origin == "" {
		return "" // 非浏览器客户端，无需 CORS
	}
	if p.star {
		return "*"
	}
	for _, o := range p.list {
		if strings.EqualFold(o, origin) {
			return origin // 回显具体 origin（带 credentials 时不能用 *）
		}
	}
	return ""
}

// writeHeaders 写通用 CORS 响应头；allowedMethods 通常为 "GET, POST, OPTIONS"。
func (p corsPolicy) writeHeaders(w http.ResponseWriter, r *http.Request, allowedMethods string) {
	origin := r.Header.Get("Origin")
	if acao := p.allowOrigin(origin); acao != "" {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", acao)
		h.Set("Vary", "Origin")
		h.Set("Access-Control-Allow-Methods", allowedMethods)
		// engine.io 握手会带 query/auth，预检需放行该头。
		reqHeaders := r.Header.Get("Access-Control-Request-Headers")
		if reqHeaders != "" {
			h.Set("Access-Control-Allow-Headers", reqHeaders)
		} else {
			h.Set("Access-Control-Allow-Headers", "content-type")
		}
		h.Set("Access-Control-Max-Age", "86400")
	}
}

// handlePreflight 处理 OPTIONS 预检：写 CORS 头后直接 204 返回 true。
// 非 OPTIONS 返回 false，由调用方继续走正常逻辑。
func (p corsPolicy) handlePreflight(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodOptions {
		return false
	}
	p.writeHeaders(w, r, "GET, POST, OPTIONS")
	w.WriteHeader(http.StatusNoContent)
	return true
}
