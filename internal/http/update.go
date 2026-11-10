// Package http 提供更新包上传接口：POST /api/admin/update
// 上传 exe → 校验 MZ/PE 头 → 自动解析版本号 → 写入 /files/update.exe + /files/version
// 鉴权：仅需登录（与普通上传一致）
package http

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"mochi-oa-server-go/internal/config"
	"mochi-oa-server-go/internal/db"
	"mochi-oa-server-go/internal/logger"
)

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

		// 校验扩展名
		fileHeader := r.MultipartForm.File["file"][0]
		if !strings.HasSuffix(strings.ToLower(fileHeader.Filename), ".exe") {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "文件必须是 .exe"})
			return
		}

		// 校验 MZ 头
		if len(buf) < 2 || buf[0] != 'M' || buf[1] != 'Z' {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "不是有效的 PE 文件（MZ 头缺失）"})
			return
		}

		// 校验 PE 头
		peOff := int(binary.LittleEndian.Uint32(buf[0x3c:0x40]))
		if peOff+4 > len(buf) || string(buf[peOff:peOff+4]) != "PE\x00\x00" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "不是有效的 PE 文件（PE 头缺失）"})
			return
		}

		// 自动解析版本号
		version := parseExeVersion(buf)
		if version == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "无法从 exe 中解析版本号"})
			return
		}

		// 写入文件
		root, _ := filepath.Abs(cfg.File.Dir)
		updatePath := filepath.Join(root, "update.exe")
		versionPath := filepath.Join(root, "version")

		if !overwrite {
			if _, err := os.Stat(updatePath); err == nil {
				writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "更新包已存在，如需覆盖请勾选覆盖"})
				return
			}
		}

		if err := os.WriteFile(updatePath, buf, 0o644); err != nil {
			logger.Error("http/update", "写入 update.exe 失败", "error", err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "写入文件失败"})
			return
		}

		// 写入 version 文件
		versionInfo := map[string]any{
			"version":     version,
			"downloadUrl": "/files/update.exe",
		}
		versionJSON, _ := json.Marshal(versionInfo)
		if err := os.WriteFile(versionPath, versionJSON, 0o644); err != nil {
			logger.Error("http/update", "写入 version 文件失败", "error", err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "写入 version 文件失败"})
			return
		}

		logger.Info("http/update", "更新包发布成功", "version", version, "size", len(buf))
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"version": version,
			"url":     "/files/update.exe",
		})
	})
}

// parseExeVersion 从 PE 文件的版本资源中解析版本号。
// 扫描文件中的版本字符串（形如 x.y.z.w）。
func parseExeVersion(buf []byte) string {
	re := regexp.MustCompile(`(\d+\.\d+\.\d+\.\d+)`)
	matches := re.FindAllString(string(buf), -1)
	for _, m := range matches {
		if m != "0.0.0.0" {
			return m
		}
	}
	re3 := regexp.MustCompile(`(\d+\.\d+\.\d+)`)
	return re3.FindString(string(buf))
}
