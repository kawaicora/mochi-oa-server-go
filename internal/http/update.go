// Package http 提供更新包上传接口：POST /api/admin/update
// 上传安装包 → 自动识别平台/架构/版本号 → 按「平台_架构」写入 /files/ 并合并到 /files/version。
// 支持格式：Windows .exe、Linux .AppImage、macOS .dmg / .zip。
// 鉴权：仅需登录（与普通上传一致）。
package http

import (
	"encoding/binary"
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

// updatePlatformEntry 单个平台_架构的更新包信息。
type updatePlatformEntry struct {
	DownloadURL string `json:"downloadUrl"`
	Version     string `json:"version,omitempty"`
}

// updateVersionFile /files/version 的持久化结构。
// 顶层 version 为当前最新版本；platforms 以「平台_架构」为键（如 win_x64、linux_arm64、mac_arm64）。
// downloadUrl 为旧版扁平字段，仅作兼容兜底。
type updateVersionFile struct {
	Version     string                        `json:"version"`
	Platforms   map[string]updatePlatformEntry `json:"platforms"`
	DownloadURL string                        `json:"downloadUrl,omitempty"`
}

// 平台/架构合法值。
var validPlatforms = map[string]bool{"win": true, "linux": true, "mac": true}
var validArchs = map[string]bool{"x64": true, "arm64": true}

var archRe = regexp.MustCompile(`(?i)(arm64|aarch64|x64|x86_64|amd64)`)
var semverRe = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

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

		// 表单可选覆盖项：platform / arch / version（缺省时由文件自动识别）。
		formPlatform := strings.TrimSpace(r.FormValue("platform"))
		formArch := strings.TrimSpace(r.FormValue("arch"))
		formVersion := strings.TrimSpace(r.FormValue("version"))

		platform, ext, ok := detectPlatform(origName, buf)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "不支持的安装包格式（仅支持 .exe / .AppImage / .dmg / .zip）"})
			return
		}
		if formPlatform != "" {
			if !validPlatforms[formPlatform] {
				writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "非法平台: " + formPlatform})
				return
			}
			platform = formPlatform
		}

		arch := detectArch(platform, origName, buf)
		if formArch != "" {
			if !validArchs[formArch] {
				writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "非法架构: " + formArch})
				return
			}
			arch = formArch
		}
		if arch == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "无法识别架构，请在文件名中包含 arm64 / x64，或在表单指定 arch"})
			return
		}

		version := detectVersion(platform, origName, buf)
		if formVersion != "" {
			version = formVersion
		}
		if version == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "无法解析版本号，请在文件名中包含 x.y.z，或在表单指定 version"})
			return
		}

		key := platform + "_" + arch

		// 写入文件：update_<平台>_<架构>.<扩展名>
		root, _ := filepath.Abs(cfg.File.Dir)
		savedName := fmt.Sprintf("update_%s.%s", key, ext)
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

		// 合并到 version 文件（保留其它平台条目）。
		info := loadVersionFile(versionPath)
		if info.Platforms == nil {
			info.Platforms = map[string]updatePlatformEntry{}
		}
		info.Platforms[key] = updatePlatformEntry{
			DownloadURL: "/files/" + savedName,
			Version:     version,
		}

		// 顶层版本取所有条目中的最高 semver。
		info.Version = latestVersion(info.Platforms, version)
		// 旧版扁平兜底：优先 win_x64，其次本次上传条目。
		if win, exists := info.Platforms["win_x64"]; exists {
			info.DownloadURL = win.DownloadURL
		} else {
			info.DownloadURL = info.Platforms[key].DownloadURL
		}

		versionJSON, _ := json.Marshal(info)
		if err := os.WriteFile(versionPath, versionJSON, 0o644); err != nil {
			logger.Error("http/update", "写入 version 文件失败", "error", err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "写入 version 文件失败"})
			return
		}

		logger.Info("http/update", "更新包发布成功", "key", key, "version", version, "size", len(buf))
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":       true,
			"version":  version,
			"platform": platform,
			"arch":     arch,
			"key":      key,
			"url":      "/files/" + savedName,
		})
	})
}

// detectPlatform 按扩展名与文件头判定平台与保存扩展名。
// 返回 (平台, 扩展名, 是否识别成功)。
func detectPlatform(filename string, buf []byte) (platform string, ext string, ok bool) {
	lower := strings.ToLower(filename)
	switch {
	case strings.HasSuffix(lower, ".exe"):
		// Windows PE：校验 MZ + PE 头。
		if len(buf) < 2 || buf[0] != 'M' || buf[1] != 'Z' {
			return "", "", false
		}
		peOff := int(binary.LittleEndian.Uint32(buf[0x3c:0x40]))
		if peOff+4 > len(buf) || string(buf[peOff:peOff+4]) != "PE\x00\x00" {
			return "", "", false
		}
		return "win", "exe", true
	case strings.HasSuffix(lower, ".appimage"):
		// AppImage 本质是 ELF 可执行文件，校验 ELF 魔数。
		if len(buf) < 4 || string(buf[:4]) != "\x7fELF" {
			return "", "", false
		}
		return "linux", "AppImage", true
	case strings.HasSuffix(lower, ".zip"):
		if len(buf) < 4 || string(buf[:4]) != "PK\x03\x04" {
			return "", "", false
		}
		return "mac", "zip", true
	case strings.HasSuffix(lower, ".dmg"):
		// dmg 尾缀为 koly（末尾 4 字节）；放宽校验，扩展名即视为 macOS 包。
		if len(buf) >= 4 && string(buf[len(buf)-4:]) != "koly" {
			logger.Warn("http/update", "dmg 未检测到 koly 尾缀，按扩展名接受", "name", filename)
		}
		return "mac", "dmg", true
	}
	return "", "", false
}

// detectArch 判定架构：exe 读 PE Machine；其它包从文件名识别，失败返回空串。
func detectArch(platform string, filename string, buf []byte) string {
	if platform == "win" {
		if arch := peMachineArch(buf); arch != "" {
			return arch
		}
	}
	if m := archRe.FindString(strings.ToLower(filename)); m != "" {
		switch m {
		case "arm64", "aarch64":
			return "arm64"
		case "x64", "x86_64", "amd64":
			return "x64"
		}
	}
	return ""
}

// peMachineArch 从 PE COFF 头 Machine 字段解析架构。
func peMachineArch(buf []byte) string {
	if len(buf) < 0x40 {
		return ""
	}
	peOff := int(binary.LittleEndian.Uint32(buf[0x3c:0x40]))
	coffOff := peOff + 4
	if coffOff+2 > len(buf) {
		return ""
	}
	machine := binary.LittleEndian.Uint16(buf[coffOff : coffOff+2])
	switch machine {
	case 0x8664: // AMD64
		return "x64"
	case 0xAA64: // ARM64
		return "arm64"
	case 0x014C: // i386
		return "x86"
	}
	return ""
}

// detectVersion 解析版本号：exe 读 PE 资源；其它包从文件名正则提取。
func detectVersion(platform string, filename string, buf []byte) string {
	if platform == "win" {
		if v := parseExeVersion(buf); v != "" && v != "0.0.0" {
			return v
		}
	}
	if m := semverRe.FindString(filename); m != "" {
		return m
	}
	return ""
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
					// VS_FIXEDFILEINFO 位于偏移 40；dwSignature == 0xFEEF04BD
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
					return fmt.Sprintf("%d.%d.%d", major, minor, build)
				}
			}
		}
	}
	return "0.0.0"
}
