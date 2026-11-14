// Package http 移植源仓库的 4 个 HTTP 路由模块（files/reset-pwd/web-admin/afdian），
// 注册到集成层提供的 *http.ServeMux 上（源 TS 是 http.on('request') 链：每个模块按路径前缀认领请求）。
//
// files.go 1:1 移植自 server/src/files.ts（上传/分片/列目录/裸下载 + ffmpeg 转码）。
// 关键契约（见《客户端契约.md》风险 4/5）：
//   - 单传响应同时返回 fileName 与 filename 两个 key（桌面端读 fileName、管理端读 filename）；
//   - GET /files/* 裸下载不强制鉴权（客户端 <img>/<video>/fetch 直接拉取），鉴权只在 /api/* 上传与清单接口。
package http

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"mochi-oa-server-go/internal/config"
	"mochi-oa-server-go/internal/db"
	"mochi-oa-server-go/internal/logger"
	"mochi-oa-server-go/internal/token"
	"mochi-oa-server-go/internal/types"
	"mochi-oa-server-go/internal/util"
)

// filesHandler 聚合文件路由的依赖（对标 registerUploadRoutes 闭包捕获）。
type filesHandler struct {
	store db.Store
	cfg   *config.ServerConfig
}

// RegisterUploadRoutes 注册上传/下载全部路由（对标 registerUploadRoutes）。
func RegisterUploadRoutes(mux *http.ServeMux, store db.Store, cfg *config.ServerConfig) {
	h := &filesHandler{store: store, cfg: cfg}
	mux.HandleFunc("POST /api/upload", h.uploadOne)
	mux.HandleFunc("POST /api/upload/chunk", h.uploadChunk)
	mux.HandleFunc("GET /api/upload/chunk/status", h.chunkStatus)
	mux.HandleFunc("POST /api/upload/chunk/complete", h.chunkComplete)
	mux.HandleFunc("GET /api/folder/files", h.folderFiles)
	mux.HandleFunc("GET /api/transcode", h.transcode)
	mux.HandleFunc("/files/", h.download)
	registerUpdateRoutes(mux, store, cfg)
}

// ---- 通用小工具（逐字对齐 files.ts） ----

// sanitizeRe 匹配文件名里的控制字符与危险字符（TS: /[\x00-\x1f\x7f<>:"/\\|?*]+/g）。
var sanitizeRe = regexp.MustCompile(`[\x00-\x1f\x7f<>:"/\\|?*]+`)

// urlPrefixRe 识别原始名是否是完整 URL（CDN 防盗链签名 URL 被误当文件名上传的场景）。
var urlPrefixRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://`)

// sanitizeName 清洗文件名：只取 basename，去掉控制/危险字符；URL 只取路径末段；空则 'file'。
func sanitizeName(name string) string {
	s := name
	if urlPrefixRe.MatchString(s) {
		// 取 URL pathname 的 basename（TS new URL(s).pathname → decodeURIComponent → basename）。
		if i := strings.Index(s, "://"); i >= 0 {
			rest := s[i+3:]
			if j := strings.IndexAny(rest, "/?#"); j >= 0 {
				rest = rest[:j]
			}
			if idx := strings.LastIndex(rest, "/"); idx >= 0 {
				rest = rest[idx+1:]
			}
			if rest != "" {
				s = rest
			}
		}
	}
	base := path.Base(strings.ReplaceAll(s, "\\", "/"))
	cleaned := strings.TrimSpace(sanitizeRe.ReplaceAllString(base, "_"))
	if len(cleaned) > 180 {
		cleaned = cleaned[:180]
	}
	if cleaned == "" {
		return "file"
	}
	return cleaned
}

// uniqueDiskName 磁盘同名追加 " (n)"（TS: 已存在则 stem (n)ext，n 从 2 起）。
func uniqueDiskName(dir, name string) string {
	m := regexpStemExt.FindStringSubmatch(name)
	stem, ext := name, ""
	if len(m) == 3 {
		stem, ext = m[1], m[2]
	}
	target := name
	for n := 2; ; n++ {
		if _, err := os.Stat(filepath.Join(dir, target)); err != nil {
			return target // 不存在即可用
		}
		target = fmt.Sprintf("%s (%d)%s", stem, n, ext)
	}
}

// regexpStemExt 拆扩展名：stem + ext（TS: /^(.+?)(\.[a-z0-9]+)?$/i）。
var regexpStemExt = regexp.MustCompile(`(?i)^(.+?)(\.[a-z0-9]+)?$`)

// safeFolderChar 判断单字符是否允许出现在文件夹路径段（TS: /^[\w\u4e00-\u9fa5\-\s.()（）]+$/）。
func safeFolderChar(r rune) bool {
	switch {
	case r == '_' || r == '-' || r == ' ' || r == '.' || r == '(' || r == ')' || r == '（' || r == '）':
		return true
	case unicode.IsLetter(r) || unicode.IsDigit(r):
		return true
	}
	return false
}

// isSafeFolderPath 文件夹相对路径校验（6/p20260921_merge 或 default/xxx）：≥2 段、拒绝穿越/根目录。
func isSafeFolderPath(v string) bool {
	var parts []string
	for _, p := range strings.Split(v, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) < 2 {
		return false
	}
	for _, s := range parts {
		if s == "." || s == ".." || s == "~" || strings.Contains(s, "\\") {
			return false
		}
		for _, r := range s {
			if !safeFolderChar(r) {
				return false
			}
		}
	}
	return true
}

// savedLocal 落盘结果（TS Saved）。
type savedLocal struct {
	storage string
	url     string
}

// saveLocal 存储到本地磁盘：{dir}/{companyKey}/{relativeDir...}/{原始文件名}（1:1 对齐 saveLocal）。
func saveLocal(cfg *config.ServerConfig, originalName string, buf []byte, companyKey, relativeDir string) (savedLocal, error) {
	root, err := filepath.Abs(cfg.File.Dir)
	if err != nil {
		return savedLocal{}, err
	}
	segments := buildSegments(companyKey, relativeDir)
	dir := filepath.Join(append([]string{root}, segments...)...)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return savedLocal{}, err
	}
	diskName := uniqueDiskName(dir, sanitizeName(originalName))
	if err := os.WriteFile(filepath.Join(dir, diskName), buf, 0o644); err != nil {
		return savedLocal{}, err
	}
	rel := segmentsJoin(segments) + "/" + strings.ReplaceAll(urlQueryEscape(diskName), "%2F", "/")
	return savedLocal{storage: "local", url: "/files/" + rel}, nil
}

// segmentsJoin 用 "/" 拼接段（filepath.Join 在 Windows 用反斜杠，URL 需要正斜杠）。
func segmentsJoin(segments []string) string {
	return strings.Join(segments, "/")
}

// buildSegments 按 companyKey/relativeDir 拼出 URL 目录段（与 saveLocal 落盘目录一致）。
func buildSegments(companyKey, relativeDir string) []string {
	key := sanitizeName(companyKey)
	if key == "" {
		key = "default"
	}
	segments := []string{key}
	if relativeDir != "" {
		for _, s := range strings.Split(relativeDir, "/") {
			if c := sanitizeName(s); c != "" {
				segments = append(segments, c)
			}
		}
	}
	return segments
}

// localRelativeURL 为一次上传生成相对 URL 路径（不写盘），供引用记录复用；目录段与 saveLocal 一致。
func localRelativeURL(originalName, companyKey, relativeDir string) string {
	segments := buildSegments(companyKey, relativeDir)
	diskName := sanitizeName(originalName)
	rel := segmentsJoin(segments) + "/" + strings.ReplaceAll(urlQueryEscape(diskName), "%2F", "/")
	return "/files/" + rel
}

// urlQueryEscape URL 编码文件名（对齐 encodeURIComponent）。
//
// 注意：必须「逐字节」加 %。旧写法 fmt.Sprintf("%%%02X", []byte(string(r))) 会把整个
// []byte 当成一个十六进制串输出（%X 对切片不插入分隔符），只在最前面产生一个 %，
// 于是中文名被编成 %E6B3AA%E5B0BD... 这种非法百分号编码，客户端拿它去下载必然 404。
func urlQueryEscape(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for _, r := range s {
		if !shouldEscape(r) {
			b.WriteRune(r)
			continue
		}
		for _, c := range []byte(string(r)) {
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0x0f])
		}
	}
	return b.String()
}

// shouldEscape 对齐 encodeURIComponent：不转字母数字和 -_.~，其余转义。
func shouldEscape(r rune) bool {
	if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
		return false
	}
	switch r {
	case '-', '_', '.', '~':
		return false
	}
	return true
}

// pubUrl 对外文件 URL：USE_RELATIVE_FILE_URL=true 原样返回相对路径；false 拼 publicBase（对齐 pubUrl）。
func pubUrl(cfg *config.ServerConfig, u string) string {
	if cfg.File.UseRelativeURL {
		return u
	}
	if u == "" || strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		return u
	}
	base := strings.TrimRight(cfg.File.PublicBase, "/")
	if strings.HasPrefix(u, "/") {
		return base + u
	}
	return base + "/" + u
}

// authedUserId 解析登录用户（JWT 或会话 token），未登录返回 0（逐字对齐 files.ts）。
func authedUserId(r *http.Request, store db.Store, cfg *config.ServerConfig) int64 {
	bearer := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(bearer), "bearer") {
		bearer = strings.TrimSpace(bearer[len("bearer"):])
	}
	tok := r.URL.Query().Get("token")
	if tok == "" {
		tok = strings.TrimSpace(bearer)
	}
	if tok == "" {
		return 0
	}
	if p := token.VerifyToken(tok, cfg.JWTSecret); p != nil {
		if id, err := strconv.ParseInt(strings.TrimSpace(p.Sub), 10, 64); err == nil && id > 0 {
			return id
		}
	}
	session, err := store.GetSessionByTokenHash(context.Background(), token.HashToken(tok))
	if err == nil && session != nil && sessionNotExpired(session.ExpiresAt) {
		return session.UserID
	}
	return 0
}

// sessionNotExpired 会话未过期（ExpiresAt 为本地空格完整时间或旧 ISO；util.ParseLocalTime 两者皆收）。
func sessionNotExpired(expiresAt string) bool {
	t, ok := util.ParseLocalTime(expiresAt)
	if !ok {
		return false
	}
	return t.After(time.Now())
}

// mimeOf 按扩展名推断 MIME（媒体 inline 可播，其余 octet-stream；对照表与 files.ts 一致）。
func mimeOf(name string) string {
	e := strings.ToLower(path.Ext(name))
	m := map[string]string{
		".mp4": "video/mp4", ".m4v": "video/x-m4v", ".webm": "video/webm", ".mov": "video/quicktime",
		".mkv": "video/x-matroska", ".avi": "video/x-msvideo", ".flv": "video/x-flv", ".ts": "video/mp2t",
		".rmvb": "application/vnd.rn-realmedia-vbr", ".rm": "application/vnd.rn-realmedia",
		".3gp": "video/3gpp", ".3gpp": "video/3gpp", ".ogv": "video/ogg",
		".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png", ".gif": "image/gif",
		".webp": "image/webp", ".bmp": "image/bmp", ".svg": "image/svg+xml",
		".mp3": "audio/mpeg", ".wav": "audio/wav", ".ogg": "audio/ogg", ".m4a": "audio/mp4", ".flac": "audio/flac", ".aac": "audio/aac",
	}
	if v, ok := m[e]; ok {
		return v
	}
	return "application/octet-stream"
}

// writeJSON 统一 JSON 应答。
func writeJSON(w http.ResponseWriter, code int, body map[string]any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// setFileCORS 允许跨域读取文件（头像 canvas 绘制、视频流播、fetch 转 blob 需要）。
func setFileCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Range")
}

// safeDownloadPath 校验 /files/ 请求路径不越界（返回文件绝对路径，非法返回 ""）。
func safeDownloadPath(cfg *config.ServerConfig, decoded string) string {
	root, err := filepath.Abs(cfg.File.Dir)
	if err != nil {
		return ""
	}
	if decoded == "" || strings.Contains(decoded, "\x00") {
		return ""
	}
	full := filepath.Join(root, filepath.FromSlash(decoded))
	if full != root && !strings.HasPrefix(full, root+string(os.PathSeparator)) {
		return ""
	}
	return full
}

// ---- 路由实现 ----

// download 裸下载 GET /files/*（不鉴权；支持 Range/正确 MIME/inline）。
// 先按记录 URL 查 files 表：实体记录直接取其物理文件；引用记录沿 using_file_id 跳到实体取字节，
// 展示名用引用记录原名。只有库里查不到记录的旧数据，才按请求路径直取磁盘文件。
func (h *filesHandler) download(w http.ResponseWriter, r *http.Request) {
	setFileCORS(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	decoded := strings.TrimPrefix(r.URL.Path, "/files/")
	var err error
	if decoded, err = urlPathUnescape(decoded); err != nil {
		http.NotFound(w, r)
		return
	}

	physRel := decoded
	dispName := ""
	recordMime := ""
	if rec := h.lookupFileRecord(r.Context(), decoded); rec != nil {
		target := rec
		if rec.UsingFileID != "" {
			// 引用记录：必须沿 using_file_id 解析到实体；实体缺失直接 404，
			// 绝不能拿引用记录自己的路径去物理直取（磁盘上没有这个文件）。
			ent, e2 := h.store.GetFileByUUID(r.Context(), rec.UsingFileID)
			if e2 != nil {
				logger.Error("http/files", "引用记录解析实体失败", "ref", rec.UUID, "using", rec.UsingFileID, "error", e2.Error())
				http.NotFound(w, r)
				return
			}
			if ent == nil {
				logger.Error("http/files", "引用记录指向的实体不存在", "ref", rec.UUID, "using", rec.UsingFileID)
				http.NotFound(w, r)
				return
			}
			target = ent
		}
		physRel = strings.TrimPrefix(target.URL, "/files/")
		// 实体 URL 是百分号编码的，映射磁盘路径前必须先解码，否则中文文件名对不上磁盘文件。
		if decodedRel, derr := urlPathUnescape(physRel); derr == nil {
			physRel = decodedRel
		}
		dispName = rec.Filename
		recordMime = rec.Mime
	}
	// physRel 为空时（库里无记录的旧数据）才按请求路径直取磁盘；引用记录不会走到这里。

	full := safeDownloadPath(h.cfg, physRel)
	if full == "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	fi, err := os.Stat(full)
	if err != nil || !fi.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	if dispName == "" {
		dispName = filepath.Base(full)
	}
	mime := recordMime
	if mime == "" {
		mime = mimeOf(full)
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Disposition", "inline; filename*=UTF-8''"+urlQueryEscape(dispName))
	// http.ServeContent 原生支持 Range(206)/416/HEAD，语义与 TS createReadStream(start,end) 一致。
	f, err := os.Open(full)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	http.ServeContent(w, r, dispName, fi.ModTime(), f)
}

// lookupFileRecord 按下载路径查 files 记录：先按转义后的存储 URL 匹配，再按原始解码路径兜底。
func (h *filesHandler) lookupFileRecord(ctx context.Context, decoded string) *types.FileRecord {
	if h.store == nil {
		return nil
	}
	escaped := "/files/" + escapeBasenamePath(decoded)
	if rec, err := h.store.GetFileByURL(ctx, escaped); err == nil && rec != nil {
		return rec
	}
	if rec, err := h.store.GetFileByURL(ctx, "/files/"+decoded); err == nil && rec != nil {
		return rec
	}
	return nil
}

// escapeBasenamePath 只对路径末段（文件名）做 URL 转义，目录段保持原样（与落盘 URL 生成一致）。
func escapeBasenamePath(decoded string) string {
	parts := strings.Split(decoded, "/")
	if n := len(parts); n > 0 {
		parts[n-1] = strings.ReplaceAll(urlQueryEscape(parts[n-1]), "%2F", "/")
	}
	return strings.Join(parts, "/")
}

// urlPathUnescape 解码 URL 路径段（对百分号编码宽容：失败则返回原串，交由后续校验拦截）。
func urlPathUnescape(s string) (string, error) {
	return url.PathUnescape(s)
}

// uploadOne 单文件上传 POST /api/upload（保留原文件名，multipart 字段名 file）。
func (h *filesHandler) uploadOne(w http.ResponseWriter, r *http.Request) {
	setFileCORS(w)
	uid := authedUserId(r, h.store, h.cfg)
	if uid <= 0 {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "未登录"})
		return
	}
	company := "default"
	if cid, err := strconv.ParseInt(r.URL.Query().Get("company"), 10, 64); err == nil && cid > 0 {
		// P2-1 修复：指定公司目录时校验登录用户为该公司成员，否则 403。
		if role, err := h.store.GetMemberRole(r.Context(), cid, uid); err != nil {
			logger.Error("http/files", "上传查询成员角色失败", "companyId", cid, "userId", uid, "error", err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "校验失败"})
			return
		} else if role == "" {
			logger.Warn("http/files", "上传非公司成员拒绝", "companyId", cid, "userId", uid)
			writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "未加入该公司"})
			return
		}
		company = strconv.FormatInt(cid, 10)
	}
	// multer .single('file') 语义：取字段 file；大小超 maxBytes 拒收。
	if err := r.ParseMultipartForm(h.cfg.File.MaxBytes); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "上传失败: " + err.Error()})
		return
	}
	fh, _, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "缺少文件字段 file"})
		return
	}
	defer fh.Close()
	size := r.MultipartForm.File["file"][0].Size
	if size > h.cfg.File.MaxBytes {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "文件超出大小限制"})
		return
	}
	buf, err := io.ReadAll(fh)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "上传失败: " + err.Error()})
		return
	}
	originalName := r.MultipartForm.File["file"][0].Filename
	mimetype := r.MultipartForm.File["file"][0].Header.Get("Content-Type")
	if mimetype == "" {
		mimetype = "application/octet-stream"
	}
	h.saveAndRespond(w, originalName, mimetype, buf, company, "")
}

// saveAndRespond 落盘 + 写库 + 应答（单传与分片合并共用）。
// 注意：契约要求单传响应同时给 fileName 与 filename 两个 key（客户端两处读取不一致）。
func (h *filesHandler) saveAndRespond(w http.ResponseWriter, originalName, mime string, buf []byte, companyKey, relativeDir string) {
	// 计算 SHA256 hash，相同文件直接复用已有记录
	sum := sha256.Sum256(buf)
	hash := hex.EncodeToString(sum[:])
	// hash 一致只代表数据相同。FindFileByHash 仅命中实体记录（using_file_id 为空），
	// 新建一条引用记录指向该实体：数据沿用实体（UUID/URL/磁盘文件），文件名以本次上传原名为准，不重复写盘。
	if existing, err := h.store.FindFileByHash(context.Background(), hash); err == nil && existing != nil {
		physical := existing
		name := sanitizeName(originalName)
		if name == "file" {
			name = physical.Filename
		}
		// 引用记录：除字节数据来自实体外，文件名/mime/size 等信息都用本次上传带来的值；
		// 引用记录持有自己的 URL（目录段/文件名按本次上传生成，不写盘）。
		refURL := localRelativeURL(name, companyKey, relativeDir)
		ref, err := h.store.SaveFile(context.Background(), db.SaveFileInput{
			UUID:        newUUID(),
			Filename:    name,
			Mime:        mime,
			Size:        int64(len(buf)),
			Storage:     physical.Storage,
			URL:         refURL,
			Hash:        hash,
			UsingFileID: physical.UUID,
		})
		if err != nil {
			logger.Error("http/files", "引用文件记录入库失败", "error", err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "存储失败"})
			return
		}
		logger.Info("http/files", "文件 hash 已存在，复用实体并新建引用记录", "hash", hash, "physical", physical.UUID, "ref", ref.UUID, "name", name, "url", ref.URL)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "uuid": ref.UUID,
			"filename": name, "fileName": name,
			"mime": mime, "size": int64(len(buf)),
			"url": pubUrl(h.cfg, ref.URL),
		})
		return
	}

	saved, err := saveLocal(h.cfg, originalName, buf, companyKey, relativeDir)
	if err != nil {
		logger.Error("http/files", "文件落盘失败", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "存储失败"})
		return
	}
	rec, err := h.store.SaveFile(context.Background(), db.SaveFileInput{
		UUID:        newUUID(),
		Filename:    sanitizeName(originalName),
		Mime:        mime,
		Size:        int64(len(buf)),
		Storage:     saved.storage,
		URL:         saved.url,
		Hash:        hash,
		UsingFileID: "",
	})
	if err != nil {
		logger.Error("http/files", "文件记录入库失败", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "存储失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "uuid": rec.UUID,
		"filename": rec.Filename, "fileName": rec.Filename, // 两个 key 都给（契约风险4）
		"mime": rec.Mime, "size": rec.Size,
		"url": pubUrl(h.cfg, rec.URL),
	})
}

// uploadChunk 分片上传 POST /api/upload/chunk（multipart: file + uploadId + chunkIndex + totalChunks）。
func (h *filesHandler) uploadChunk(w http.ResponseWriter, r *http.Request) {
	setFileCORS(w)
	uid := authedUserId(r, h.store, h.cfg)
	if uid <= 0 {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "未登录"})
		return
	}
	// 单块上限 = max(4MB, maxBytes/4)（对齐 TS multer 限制；合并时再校验总量）。
	chunkMax := h.cfg.File.MaxBytes / 4
	if min := int64(4 * 1024 * 1024); chunkMax < min {
		chunkMax = min
	}
	if err := r.ParseMultipartForm(chunkMax); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "上传失败: " + err.Error()})
		return
	}
	fh, _, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "缺少文件字段 file"})
		return
	}
	defer fh.Close()
	uploadID := strings.TrimSpace(r.FormValue("uploadId"))
	chunkIndex, err1 := strconv.ParseInt(r.FormValue("chunkIndex"), 10, 64)
	totalChunks, err2 := strconv.ParseInt(r.FormValue("totalChunks"), 10, 64)
	if uploadID == "" || err1 != nil || chunkIndex < 0 || err2 != nil || totalChunks <= 0 || chunkIndex >= totalChunks {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "参数不合法（需要 file/uploadId/chunkIndex/totalChunks）"})
		return
	}
	buf, err := io.ReadAll(fh)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "上传失败: " + err.Error()})
		return
	}
	chunkDir := filepath.Join(h.cfg.File.Dir, ".chunks", uploadID)
	if err := os.MkdirAll(chunkDir, 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "存储失败"})
		return
	}
	if err := os.WriteFile(filepath.Join(chunkDir, strconv.FormatInt(chunkIndex, 10)), buf, 0o644); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "存储失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "uploadId": uploadID, "chunkIndex": chunkIndex,
		"received": chunkIndex + 1, "totalChunks": totalChunks,
	})
}

// chunkStatus 断点续传查询 GET /api/upload/chunk/status?uploadId=。
func (h *filesHandler) chunkStatus(w http.ResponseWriter, r *http.Request) {
	setFileCORS(w)
	uid := authedUserId(r, h.store, h.cfg)
	if uid <= 0 {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "未登录"})
		return
	}
	uploadID := r.URL.Query().Get("uploadId")
	if uploadID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "缺少 uploadId"})
		return
	}
	chunkDir := filepath.Join(h.cfg.File.Dir, ".chunks", uploadID)
	var received []int64
	if entries, err := os.ReadDir(chunkDir); err == nil {
		for _, e := range entries {
			if n, err := strconv.ParseInt(e.Name(), 10, 64); err == nil && n >= 0 {
				received = append(received, n)
			}
		}
		// 排序（对齐 TS .sort((a,b)=>a-b)）。
		for i := 1; i < len(received); i++ {
			for j := i; j > 0 && received[j-1] > received[j]; j-- {
				received[j-1], received[j] = received[j], received[j-1]
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uploadId": uploadID, "received": received})
}

// chunkComplete 合并分片 POST /api/upload/chunk/complete（JSON: uploadId/filename/totalChunks/mime/relativePath）。
func (h *filesHandler) chunkComplete(w http.ResponseWriter, r *http.Request) {
	setFileCORS(w)
	uid := authedUserId(r, h.store, h.cfg)
	if uid <= 0 {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "未登录"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "读取失败"})
		return
	}
	var d struct {
		UploadID     string `json:"uploadId"`
		Filename     string `json:"filename"`
		TotalChunks  int64  `json:"totalChunks"`
		Mime         string `json:"mime"`
		RelativePath string `json:"relativePath"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "JSON 解析失败"})
		return
	}
	company := "default"
	if cid, err := strconv.ParseInt(r.URL.Query().Get("company"), 10, 64); err == nil && cid > 0 {
		// P2-1 修复：指定公司目录时校验登录用户为该公司成员，否则 403。
		if role, err := h.store.GetMemberRole(r.Context(), cid, uid); err != nil {
			logger.Error("http/files", "上传查询成员角色失败", "companyId", cid, "userId", uid, "error", err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "校验失败"})
			return
		} else if role == "" {
			logger.Warn("http/files", "上传非公司成员拒绝", "companyId", cid, "userId", uid)
			writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "未加入该公司"})
			return
		}
		company = strconv.FormatInt(cid, 10)
	}
	if d.UploadID == "" || d.TotalChunks <= 0 || d.TotalChunks > 10000 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "参数不合法"})
		return
	}
	chunkDir := filepath.Join(h.cfg.File.Dir, ".chunks", d.UploadID)
	var buf []byte
	for i := int64(0); i < d.TotalChunks; i++ {
		b, err := os.ReadFile(filepath.Join(chunkDir, strconv.FormatInt(i, 10)))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": fmt.Sprintf("分块不完整，请先补传（%d）", d.TotalChunks)})
			return
		}
		buf = append(buf, b...)
	}
	if len(buf) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "文件为空"})
		return
	}
	mime := d.Mime
	if mime == "" {
		mime = "application/octet-stream"
	}
	// 合并成功后清理临时分片目录（TS .catch(()=>{})，失败不阻断）。
	defer func() { _ = os.RemoveAll(chunkDir) }()
	h.saveAndRespond(w, d.Filename, mime, buf, company, d.RelativePath)
}

// folderFiles 文件夹清单 GET /api/folder/files?path={companyKey}/{folderName}。
func (h *filesHandler) folderFiles(w http.ResponseWriter, r *http.Request) {
	setFileCORS(w)
	uid := authedUserId(r, h.store, h.cfg)
	if uid <= 0 {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "未登录"})
		return
	}
	p := r.URL.Query().Get("path")
	if !isSafeFolderPath(p) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "文件夹路径不合法"})
		return
	}
	// 公司校验：首段为数字公司 id 时须为公司成员；default 放行。
	if first := strings.Split(p, "/")[0]; regexp.MustCompile(`^\d+$`).MatchString(first) {
		cid, _ := strconv.ParseInt(first, 10, 64)
		role, err := h.store.GetMemberRole(context.Background(), cid, uid)
		if err != nil || role == "" {
			writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "非该公司成员"})
			return
		}
	}
	root, err := filepath.Abs(h.cfg.File.Dir)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "路径越界"})
		return
	}
	var segs []string
	for _, s := range strings.Split(p, "/") {
		if c := sanitizeName(s); c != "" {
			segs = append(segs, c)
		}
	}
	rel := strings.Join(segs, "/")
	dir := filepath.Join(root, filepath.FromSlash(rel))
	if dir != root && !strings.HasPrefix(dir, root+string(os.PathSeparator)) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "路径越界"})
		return
	}
	type fileEntry struct {
		Name    string `json:"name"`
		RelPath string `json:"relPath"`
		URL     string `json:"url"`
	}
	files := []fileEntry{}
	var walk func(curDir, curRel string)
	walk = func(curDir, curRel string) {
		entries, err := os.ReadDir(curDir)
		if err != nil {
			return
		}
		for _, e := range entries {
			childRel := curRel + "/" + e.Name()
			if e.IsDir() {
				walk(filepath.Join(curDir, e.Name()), childRel)
			} else {
				var segEsc []string
				// curRel 无前导斜杠，childRel 形如 default/文件夹/文件；不能用 childRel[1:]，
				// 那会把首段首字母（如 default 的 d）啃掉，导致文件 URL 404。
				for _, s := range strings.Split(childRel, "/") {
					segEsc = append(segEsc, urlQueryEscape(s))
				}
				files = append(files, fileEntry{
					Name:    e.Name(),
					RelPath: childRel,
					URL:     pubUrl(h.cfg, "/files/"+strings.Join(segEsc, "/")),
				})
			}
		}
	}
	walk(dir, rel)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": rel, "files": files})
}

// transcode ffmpeg 转码 GET /api/transcode?src=（rm/avi/mkv → h264 mp4，结果缓存 .transcode/）。
func (h *filesHandler) transcode(w http.ResponseWriter, r *http.Request) {
	setFileCORS(w)
	uid := authedUserId(r, h.store, h.cfg)
	if uid <= 0 {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "未登录"})
		return
	}
	src := r.URL.Query().Get("src")
	if src == "" || strings.Contains(src, "\x00") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "路径不合法"})
		return
	}
	for _, s := range strings.Split(src, "/") {
		if s == "." || s == ".." || s == "~" || strings.Contains(s, "\\") {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "路径不合法"})
			return
		}
	}
	root, err := filepath.Abs(h.cfg.File.Dir)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "路径越界"})
		return
	}
	full := filepath.Join(root, filepath.FromSlash(src))
	if full != root && !strings.HasPrefix(full, root+string(os.PathSeparator)) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "路径越界"})
		return
	}
	fi, err := os.Stat(full)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "文件不存在"})
		return
	}
	if !fi.Mode().IsRegular() {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "非文件"})
		return
	}
	outDir := filepath.Join(root, ".transcode")
	_ = os.MkdirAll(outDir, 0o755)
	sum := sha1.Sum([]byte(fmt.Sprintf("%s:%d", src, fi.Size())))
	out := filepath.Join(outDir, hex.EncodeToString(sum[:])+".mp4")
	if _, err := os.Stat(out); err != nil {
		// 源文件不可变缓存；失败 500（容器内未装 ffmpeg 时返回转码失败，与 TS 一致）。
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "ffmpeg",
			"-y", "-i", full,
			"-c:v", "libx264", "-preset", "veryfast", "-crf", "23",
			"-c:a", "aac", "-b:a", "128k", "-movflags", "+faststart", out)
		if out, err := cmd.Output(); err != nil {
			logger.Error("http/files", "ffmpeg 转码失败", "src", src, "error", err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "转码失败: " + string(out)})
			return
		}
	}
	// 流播转码产物（Range 支持）。
	name := strings.TrimSuffix(filepath.Base(full), path.Ext(full)) + ".mp4"
	f, err := os.Open(out)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "转码产物不存在"})
		return
	}
	defer f.Close()
	ofi, _ := f.Stat()
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Disposition", "inline; filename*=UTF-8''"+urlQueryEscape(name))
	http.ServeContent(w, r, name, ofi.ModTime(), f)
}

// newUUID 生成 RFC4122 v4 UUID（TS randomUUID）。
func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", b)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
