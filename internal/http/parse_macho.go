// 安装包架构解析：PE / ELF 解析在 update.go，本文件负责 macOS Mach-O（通用二进制 fat / 单二进制）
// 与 macOS zip 分发包内部主二进制抽取。所有解包逻辑均带超时与读取上限，避免恶意/损坏包卡死请求。
package http

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"io"
	"path/filepath"
	"strings"
	"time"

	"mochi-oa-server-go/internal/logger"
)

// Mach-O CPU 类型（mach-o/loader.go）。
const (
	machoCpuX8664 = 0x01000007 // CPU_TYPE_X86_64
	machoCpuArm64 = 0x0100000c // CPU_TYPE_ARM64
)

// Mach-O fat / 单二进制 magic。
// 现代 Mach-O 多为小端文件：LE 读出 MH_MAGIC=0xFEEDFACE；swapped(CIGAM) 0xCEFAEDFE 为大端。
var (
	fatMagicBE    = map[uint32]bool{0xCAFEBABE: true, 0xCAFEBABF: true} // FAT / FAT_64（大端）
	fatMagicLE    = map[uint32]bool{0xBEBAFECA: true, 0xBEBAFBAF: true} // FAT / FAT_64（小端）
	singleMagicLE = map[uint32]bool{0xFEEDFACE: true, 0xFEEDFACF: true} // MH_MAGIC / MH_MAGIC_64（小端文件）
	singleMagicBE = map[uint32]bool{0xCEFAEDFE: true, 0xCFFAEDFE: true} // MH_CIGAM / MH_CIGAM_64（大端文件）
)

const machoParseTimeout = 5 * time.Second
const machoHeadBytes = 4096   // 仅读头部即可判定 CPU 类型
const machoMaxEntries = 4     // 单个 zip 内最多解析几个主二进制

func readUint32(b []byte, off int, le bool) uint32 {
	if off+4 > len(b) {
		return 0
	}
	if le {
		return binary.LittleEndian.Uint32(b[off : off+4])
	}
	return binary.BigEndian.Uint32(b[off : off+4])
}

func machoCpuArch(cpu uint32) string {
	switch cpu {
	case machoCpuX8664:
		return archX8664
	case machoCpuArm64:
		return archArm64
	}
	return ""
}

// machoArchs 从 Mach-O fat 通用头或单二进制头解析支持的全部架构（标准名）。
func machoArchs(buf []byte) []string {
	if len(buf) < 8 {
		return nil
	}
	be := binary.BigEndian.Uint32(buf[:4])
	le := binary.LittleEndian.Uint32(buf[:4])
	switch {
	case fatMagicBE[be]:
		return fatArchs(buf, be, false)
	case fatMagicLE[le]:
		return fatArchs(buf, le, true)
	case singleMagicLE[le]:
		return singleArchs(buf, le, true)
	case singleMagicBE[be]:
		return singleArchs(buf, be, false)
	}
	return nil
}

// fatArchs 解析 fat 通用二进制头里的每个切片 CPU 类型。
func fatArchs(buf []byte, magic uint32, le bool) []string {
	isFat64 := magic == 0xCAFEBABF || magic == 0xBEBAFBAF
	n := int(readUint32(buf, 4, le))
	if n <= 0 || n > 16 {
		return nil
	}
	entrySize := 20
	if isFat64 {
		entrySize = 32
	}
	archs := []string{}
	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		aOff := 8 + i*entrySize
		if aOff+4 > len(buf) {
			break
		}
		cpu := readUint32(buf, aOff, le)
		appendArch(&archs, seen, machoCpuArch(cpu))
	}
	return archs
}

// singleArchs 解析单 Mach-O 二进制头的 CPU 类型。
func singleArchs(buf []byte, magic uint32, le bool) []string {
	cpu := readUint32(buf, 4, le)
	archs := []string{}
	appendArch(&archs, map[string]bool{}, machoCpuArch(cpu))
	return archs
}

// zipMacArchs 打开 macOS zip 分发包，定位 *.app/Contents/MacOS/<主二进制>，读头部解析架构。
// 非 zip（如 dmg）快速返回 nil，由调用方走文件名/表单兜底。
func zipMacArchs(buf []byte) []string {
	return withTimeout(machoParseTimeout, func() []string {
		zr, err := zip.NewReader(bytes.NewReader(buf), int64(len(buf)))
		if err != nil {
			return nil
		}
		archs := []string{}
		seen := map[string]bool{}
		matched := 0
		for _, f := range zr.File {
			if matched >= machoMaxEntries {
				break
			}
			idx := strings.Index(f.Name, "/Contents/MacOS/")
			if idx < 0 {
				continue
			}
			base := f.Name[idx+len("/Contents/MacOS/"):]
			if base == "" || strings.Contains(base, "/") || filepath.Ext(base) != "" {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				continue
			}
			head := make([]byte, machoHeadBytes)
			n, _ := io.ReadFull(rc, head)
			_ = rc.Close()
			for _, a := range machoArchs(head[:n]) {
				appendArch(&archs, seen, a)
			}
			matched++
		}
		return archs
	})
}

// appendArch 去重追加标准架构名。
func appendArch(dst *[]string, seen map[string]bool, arch string) {
	if arch == "" || !validArchs[arch] || seen[arch] {
		return
	}
	seen[arch] = true
	*dst = append(*dst, arch)
}

// withTimeout 让解析在独立 goroutine 中执行，超时直接放弃，避免损坏包卡死上传处理器。
func withTimeout(d time.Duration, fn func() []string) (archs []string) {
	ch := make(chan []string, 1)
	go func() {
		defer func() {
			_ = recover()
		}()
		ch <- fn()
	}()
	select {
	case r := <-ch:
		return r
	case <-time.After(d):
		logger.Warn("http/update", "解析更新包架构超时，按兜底规则处理")
		return nil
	}
}
