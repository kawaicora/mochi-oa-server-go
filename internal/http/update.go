// Package http 提供更新包上传接口：POST /api/admin/update
// 职责边界：平台 / 版本号由客户端（Node 主进程）识别后随 multipart 上报，
// 服务端不再解包、不解析二进制头，也不区分架构（一个安装包同时覆盖 arm64 + x86_64），
// 只做校验、把安装包落到指定目录、合并写入 version 文件，且只更新本次上传平台自己的条目。
// 鉴权：仅需登录（与普通上传一致）。
package http

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"mochi-oa-server-go/internal/config"
	"mochi-oa-server-go/internal/db"
	"mochi-oa-server-go/internal/logger"
)

// updatePlatformEntry 单个平台的更新包信息。
type updatePlatformEntry struct {
	DownloadURL string `json:"downloadUrl"`
	Version     string `json:"version,omitempty"`
}

// updateVersionFile /files/version 的持久化结构。
// 顶层 version 为当前最新版本；downloadUrl 为老写法扁平兼容项（旧 Windows 客户端读取，仅随 Windows 包刷新）；
// platforms 以平台名为键（windows / linux / darwin），每个条目携带 downloadUrl 与版本号。
type updateVersionFile struct {
	Version      string                        `json:"version"`
	DownloadURL  string                        `json:"downloadUrl,omitempty"`
	Platforms    map[string]updatePlatformEntry `json:"platforms"`
}

// 标准平台名：windows / linux / darwin（与 GOOS、macOS 标准名一致）。
const (
	platWindows = "windows"
	platLinux   = "linux"
	platDarwin  = "darwin"
)

// 平台合法值。
var validPlatforms = map[string]bool{platWindows: true, platLinux: true, platDarwin: true}

var semverRe = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// 安装包扩展名白名单：仅按文件名后缀决定落盘扩展名，不读取包内容。
var extBySuffix = map[string]string{
	".exe":      "exe",
	".appimage": "AppImage",
	".zip":      "zip",
	".dmg":      "dmg",
}

// registerUpdateRoutes 注册更新包管理路由。
func registerUpdateRoutes(mux *http.ServeMux, store db.Store, cfg *config.ServerConfig) {
	mux.HandleFunc("POST /api/admin/update", func(w http.ResponseWriter, r *http.Request) {
		setFileCORS(w)
		uid := authedUserId(r, store, cfg)
		if uid <= 0 {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "未登录"})
			return
		}

		overwrite := r.URL.Query().Get("overwrite") == "true"

		if err := r.ParseMultipartForm(500 << 20); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "上传失败: " + err.Error()})
			return
		}
		fh, _, err := r.FormFile("file")
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "缺少文件字段 file"})
			return
		}
		defer fh.Close()

		buf, err := io.ReadAll(fh)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "读取文件失败"})
			return
		}

		fileHeader := r.MultipartForm.File["file"][0]
		origName := fileHeader.Filename

		// 客户端上报字段：platform / version。服务端只校验，不自行识别，也不区分架构。
		platform := strings.TrimSpace(r.FormValue("platform"))
		version := strings.TrimSpace(r.FormValue("version"))

		if platform == "" || !validPlatforms[platform] {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "缺少或非法平台: " + platform})
			return
		}

		if _, ok := parseSemVer(version); !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "缺少或非法版本号（x.y.z）"})
			return
		}

		ext, ok := resolveExt(origName)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "不支持的安装包格式（仅支持 .exe / .AppImage / .zip / .dmg）"})
			return
		}

		// 一个安装包覆盖全部架构；文件名按平台落盘。
		root, _ := filepath.Abs(cfg.File.Dir)
		savedName := fmt.Sprintf("update_%s.%s", platform, ext)
		updatePath := filepath.Join(root, savedName)
		versionPath := filepath.Join(root, "version")

		if !overwrite {
			if _, err := os.Stat(updatePath); err == nil {
				writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "该平台更新包已存在，如需覆盖请勾选覆盖"})
				return
			}
		}

		if err := os.WriteFile(updatePath, buf, 0o644); err != nil {
			logger.Error("http/update", "写入更新包失败", "error", err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "写入文件失败"})
			return
		}

		// 合并到 version 文件（保留其它平台条目），按平台写条目。
		info := loadVersionFile(versionPath)
		if info.Platforms == nil {
			info.Platforms = map[string]updatePlatformEntry{}
		}
		info.Platforms[platform] = updatePlatformEntry{
			DownloadURL: "/files/" + savedName,
			Version:     version,
		}
		keys := []string{platform}

		// 顶层版本取所有条目中的最高 semver。
		info.Version = latestVersion(info.Platforms, version)

		// 老写法扁平兼容项：仅 Windows 包刷新顶层 downloadUrl（旧 Windows 客户端只认扁平地址）；
		// 上传 Linux/macOS 包时保留上一次写入的扁平项，不清空。
		if platform == platWindows {
			info.DownloadURL = "/files/" + savedName
		}

		versionJSON, _ := json.Marshal(info)
		if err := os.WriteFile(versionPath, versionJSON, 0o644); err != nil {
			logger.Error("http/update", "写入 version 文件失败", "error", err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "写入 version 文件失败"})
			return
		}

		logger.Info("http/update", "更新包发布成功", "keys", keys, "version", version, "size", len(buf))
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":       true,
			"version":  version,
			"platform": platform,
			"keys":     keys,
			"url":      "/files/" + savedName,
		})
	})
}

// resolveExt 按文件名后缀返回落盘扩展名；不支持的后缀返回 false。
func resolveExt(filename string) (string, bool) {
	lower := strings.ToLower(filename)
	for suffix, ext := range extBySuffix {
		if strings.HasSuffix(lower, suffix) {
			return ext, true
		}
	}
	return "", false
}

// loadVersionFile 读取既有 version 文件；损坏或缺失时返回空结构。
func loadVersionFile(path string) updateVersionFile {
	info := updateVersionFile{Platforms: map[string]updatePlatformEntry{}}
	b, err := os.ReadFile(path)
	if err != nil {
		return info
	}
	_ = json.Unmarshal(b, &info)
	if info.Platforms == nil {
		info.Platforms = map[string]updatePlatformEntry{}
	}
	return info
}

// latestVersion 在所有条目版本与本次版本中取最高 semver。
func latestVersion(platforms map[string]updatePlatformEntry, fallback string) string {
	best := fallback
	for _, e := range platforms {
		if cmpSemver(e.Version, best) > 0 {
			best = e.Version
		}
	}
	return best
}

// semVer 拆分后的语义化版本。
type semVer struct{ major, minor, patch int }

func parseSemVer(s string) (semVer, bool) {
	m := semverRe.FindStringSubmatch(s)
	if m == nil {
		return semVer{}, false
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	patch, _ := strconv.Atoi(m[3])
	return semVer{major, minor, patch}, true
}

// cmpSemver 返回 1(a>b) / -1(a<b) / 0(相等)；无法解析的按 0.0.0 处理。
func cmpSemver(a, b string) int {
	av, _ := parseSemVer(a)
	bv, _ := parseSemVer(b)
	if av.major != bv.major {
		return cmpInt(av.major, bv.major)
	}
	if av.minor != bv.minor {
		return cmpInt(av.minor, bv.minor)
	}
	return cmpInt(av.patch, bv.patch)
}

func cmpInt(a, b int) int {
	switch {
	case a > b:
		return 1
	case a < b:
		return -1
	default:
		return 0
	}
}
