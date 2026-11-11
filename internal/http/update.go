// Package http 提供更新包上传接口：POST /api/admin/update
// 上传 exe → 校验 MZ/PE 头 → 自动解析版本号 → 写入 /files/update.exe + /files/version
// 鉴权：仅需登录（与普通上传一致）
package http

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
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

// parseExeVersion 从 PE 文件的 VS_VERSIONINFO 资源中解析版本号。
// 通过解析 PE 资源目录找到 RT_VERSION 资源，读取 VS_FIXEDFILEINFO 的 FileVersionMS/LS。
func parseExeVersion(buf []byte) string {
	peOff := int(binary.LittleEndian.Uint32(buf[0x3c:0x40]))
	if peOff+24 > len(buf) {
		return ""
	}
	// PE 签名 + COFF 头 (20 bytes)
	coffOff := peOff + 4
	numSections := int(binary.LittleEndian.Uint16(buf[coffOff+2 : coffOff+4]))
	optHdrOff := coffOff + 20
	magic := binary.LittleEndian.Uint16(buf[optHdrOff : optHdrOff+2])
	var dataDirOff int
	if magic == 0x10b { // PE32
		dataDirOff = optHdrOff + 96
	} else { // PE32+
		dataDirOff = optHdrOff + 112
	}
	// Resource directory is index 2
	resDirOff := dataDirOff + 2*8
	resRVA := binary.LittleEndian.Uint32(buf[resDirOff : resDirOff+4])
	resSize := binary.LittleEndian.Uint32(buf[resDirOff+4 : resDirOff+8])
	if resRVA == 0 || resSize == 0 {
		return ""
	}
	// Section table starts after optional header
	sizeOptHdr := int(binary.LittleEndian.Uint16(buf[coffOff+16 : coffOff+18]))
	secTabOff := optHdrOff + sizeOptHdr
	// RVA → file offset
	rvaToOff := func(rva uint32) int {
		for i := 0; i < numSections; i++ {
			sOff := secTabOff + i*40
			vAddr := binary.LittleEndian.Uint32(buf[sOff+12 : sOff+16])
			vSize := binary.LittleEndian.Uint32(buf[sOff+8 : sOff+12])
			rawOff := binary.LittleEndian.Uint32(buf[sOff+20 : sOff+24])
			if rva >= vAddr && rva < vAddr+vSize {
				return int(rva - vAddr + rawOff)
			}
		}
		return -1
	}
	resBase := rvaToOff(resRVA)
	if resBase < 0 {
		return ""
	}
	// 遍历资源目录找 RT_VERSION (type=16)
	type resEntry struct {
		id     uint32
		isDir  bool
		offset uint32
	}
	readDir := func(dirOff int) []resEntry {
		var entries []resEntry
		numNamed := int(binary.LittleEndian.Uint16(buf[dirOff+12 : dirOff+14]))
		numId := int(binary.LittleEndian.Uint16(buf[dirOff+14 : dirOff+16]))
		for i := 0; i < numNamed+numId; i++ {
			eOff := dirOff + 16 + i*8
			id := binary.LittleEndian.Uint32(buf[eOff : eOff+4])
			dataOrSub := binary.LittleEndian.Uint32(buf[eOff+4 : eOff+8])
			entries = append(entries, resEntry{id: id, isDir: dataOrSub&0x80000000 != 0, offset: dataOrSub & 0x7fffffff})
		}
		return entries
	}
	// Level 0: type directory
	lvl0 := readDir(resBase)
	for _, e := range lvl0 {
		if e.id != 16 { // RT_VERSION
			continue
		}
		// Level 1: ID (language) directory
		lvl1 := readDir(resBase + int(e.offset))
		for _, e1 := range lvl1 {
			// Level 2: leaf directory
			lvl2 := readDir(resBase + int(e1.offset))
			for _, e2 := range lvl2 {
				if !e2.isDir {
					// Data entry: RVA of the resource data
					dataEntryOff := resBase + int(e2.offset)
					dataRVA := binary.LittleEndian.Uint32(buf[dataEntryOff : dataEntryOff+4])
					dataSize := binary.LittleEndian.Uint32(buf[dataEntryOff+4 : dataEntryOff+8])
					dataOff := rvaToOff(dataRVA)
					if dataOff < 0 || dataOff+int(dataSize) > len(buf) {
						continue
					}
					// VS_VERSIONINFO: signature starts at offset 0 with "VS_VERSION_INFO" (UTF-16)
					// VS_FIXEDFILEINFO is at offset 40 (after signature + padding)
					// struct { DWORD dwSignature; DWORD dwStrucVersion; DWORD dwFileVersionMS; ... }
					// dwSignature == 0xFEEF04BD
					fixedOff := dataOff + 40
					if fixedOff+52 > len(buf) {
						continue
					}
					sig := binary.LittleEndian.Uint32(buf[fixedOff : fixedOff+4])
					if sig != 0xFEEF04BD {
						continue
					}
					fileVerMS := binary.LittleEndian.Uint32(buf[fixedOff+8 : fixedOff+12])
					fileVerLS := binary.LittleEndian.Uint32(buf[fixedOff+12 : fixedOff+16])
					major := fileVerMS >> 16
					minor := fileVerMS & 0xFFFF
					build := fileVerLS >> 16
					patch := fileVerLS & 0xFFFF
					_ = patch
					// 去掉末尾的 .0（exe 版本是 4 段，package.json 是 3 段）
					return fmt.Sprintf("%d.%d.%d", major, minor, build)
				}
			}
		}
	}
	return "0.0.0"
}
