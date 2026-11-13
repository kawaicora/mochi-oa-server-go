package http

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"testing"
)

func fatHeaderBytes(cpus ...uint32) []byte {
	buf := make([]byte, 8+20*len(cpus))
	binary.BigEndian.PutUint32(buf[0:4], 0xCAFEBABE) // FAT_MAGIC
	binary.BigEndian.PutUint32(buf[4:8], uint32(len(cpus)))
	for i, cpu := range cpus {
		off := 8 + i*20
		binary.BigEndian.PutUint32(buf[off:off+4], cpu)
	}
	return buf
}

func TestMachoArchsFat(t *testing.T) {
	archs := machoArchs(fatHeaderBytes(machoCpuX8664, machoCpuArm64))
	if len(archs) != 2 {
		t.Fatalf("fat 应识别 2 个架构，得到 %v", archs)
	}
	want := map[string]bool{archX8664: true, archArm64: true}
	for _, a := range archs {
		if !want[a] {
			t.Errorf("意外架构 %s", a)
		}
	}
}

func TestMachoArchsSingleArm64(t *testing.T) {
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint32(buf[0:4], 0xFEEDFACE) // MH_MAGIC（小端文件）
	binary.LittleEndian.PutUint32(buf[4:8], machoCpuArm64)
	archs := machoArchs(buf)
	if len(archs) != 1 || archs[0] != archArm64 {
		t.Fatalf("单 arm64 二进制应识别 arm64，得到 %v", archs)
	}
}

func TestZipMacArchs(t *testing.T) {
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	w, err := zw.Create("Foo.app/Contents/MacOS/Foo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(fatHeaderBytes(machoCpuX8664, machoCpuArm64)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	archs := zipMacArchs(zbuf.Bytes())
	if len(archs) != 2 {
		t.Fatalf("zip 内应识别 2 个架构，得到 %v", archs)
	}
}

func TestZipMacArchsNonZip(t *testing.T) {
	// 非 zip 字节（如 dmg 片段）应快速返回 nil，不 panic。
	if archs := zipMacArchs([]byte("not a zip")); archs != nil {
		t.Fatalf("非 zip 应返回 nil，得到 %v", archs)
	}
}
