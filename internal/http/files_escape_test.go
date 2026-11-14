package http

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"mochi-oa-server-go/internal/config"
)

// TestUrlQueryEscape 锁定 URL 编码行为（对齐 JS encodeURIComponent）。
//
// 回归用例来自真实故障：中文名的多字节 UTF-8 必须逐字节加 %，
// 否则会编出 %E6B3AA%E5B0BD... 这类非法百分号编码，上传后下载必然 404。
func TestUrlQueryEscape(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			// 真实故障文件名（data/default/泪尽铃音响翻唱+混音.mp4）
			name: "中文名+加号",
			in:   "泪尽铃音响翻唱+混音.mp4",
			want: "%E6%B3%AA%E5%B0%BD%E9%93%83%E9%9F%B3%E5%93%8D%E7%BF%BB%E5%94%B1%2B%E6%B7%B7%E9%9F%B3.mp4",
		},
		{
			name: "ASCII 未保留字符原样保留",
			in:   "aZ0-_.~.mp4",
			want: "aZ0-_.~.mp4",
		},
		{
			name: "空格编成 %20（不是 +）",
			in:   "a b.mp4",
			want: "a%20b.mp4",
		},
		{
			name: "子目录用的斜杠也要编",
			in:   "dir/file.mp4",
			want: "dir%2Ffile.mp4",
		},
		{
			name: "百分号本身要编，避免二次解码歧义",
			in:   "100%.mp4",
			want: "100%25.mp4",
		},
		{
			name: "2 字节字符（é）",
			in:   "é.mp4",
			want: "%C3%A9.mp4",
		},
		{
			name: "4 字节字符（emoji）",
			in:   "😀.mp4",
			want: "%F0%9F%98%80.mp4",
		},
		{
			name: "空串",
			in:   "",
			want: "",
		},
	}
	for _, c := range cases {
		if got := urlQueryEscape(c.in); got != c.want {
			t.Errorf("%s: urlQueryEscape(%q)\n got = %s\nwant = %s", c.name, c.in, got, c.want)
		}
	}
}

// TestUrlQueryEscapeIsValidPercentEncoding 断言输出是合法百分号编码：
// 每个 % 后面必须紧跟两个十六进制数字（旧 bug 的 `%E6B3AA` 就不满足）。
func TestUrlQueryEscapeIsValidPercentEncoding(t *testing.T) {
	const hexDigits = "0123456789ABCDEFabcdef"
	for _, in := range []string{"泪尽铃音响翻唱+混音.mp4", "日本語 テスト.mp4", "héllo wörld.png"} {
		out := urlQueryEscape(in)
		for i := 0; i < len(out); i++ {
			if out[i] != '%' {
				continue
			}
			if i+2 >= len(out) {
				t.Fatalf("urlQueryEscape(%q)=%q: %% 在末尾被截断", in, out)
			}
			h1, h2 := out[i+1], out[i+2]
			if !containsByte(hexDigits, h1) || !containsByte(hexDigits, h2) {
				t.Fatalf("urlQueryEscape(%q)=%q: 位置 %d 处的 %%%c%c 不是合法的 %%XX", in, out, i, h1, h2)
			}
			i += 2
		}
	}
}

func containsByte(s string, c byte) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return true
		}
	}
	return false
}

// TestSuffixedName 锁定重名后缀格式：扩展名前插 _n（旧版是 " (n)" 括号格式，已废弃）。
func TestSuffixedName(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"photo.jpg", 1, "photo_1.jpg"},
		{"photo.jpg", 2, "photo_2.jpg"},
		{"Mochi.dll", 1, "Mochi_1.dll"},
		{"noext", 1, "noext_1"},
	}
	for _, c := range cases {
		if got := suffixedName(c.in, c.n); got != c.want {
			t.Errorf("suffixedName(%q, %d) = %q，期望 %q", c.in, c.n, got, c.want)
		}
	}
}

// TestUniqueDiskName 磁盘重名时依次返回原名、_1、_2…。
func TestUniqueDiskName(t *testing.T) {
	dir := t.TempDir()
	const base = "FD5E8EAD974A93BB97117510962A54EF.jpg"
	if got := uniqueDiskName(dir, base); got != base {
		t.Fatalf("首次可用名 = %q，期望原名", got)
	}
	if err := os.WriteFile(filepath.Join(dir, base), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := uniqueDiskName(dir, base); got != "FD5E8EAD974A93BB97117510962A54EF_1.jpg" {
		t.Fatalf("重名第二次 = %q，期望 _1", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "FD5E8EAD974A93BB97117510962A54EF_1.jpg"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := uniqueDiskName(dir, base); got != "FD5E8EAD974A93BB97117510962A54EF_2.jpg" {
		t.Fatalf("重名第三次 = %q，期望 _2", got)
	}
}

// TestLocalRelativeURL 引用记录 URL 由目录段 + 文件名（编码）构成，不带 UUID 目录段。
func TestLocalRelativeURL(t *testing.T) {
	const name = "FD5E8EAD974A93BB97117510962A54EF.jpg"
	u := localRelativeURL(name, "6", "")
	if u != "/files/6/"+urlQueryEscape(name) {
		t.Errorf("URL = %s，期望 /files/6/<编码名>", u)
	}
}

// TestUrlPathUnescape 锁定百分号解码行为：实体记录落库的 URL 是百分号编码的，
// 映射磁盘路径前必须正确解码回中文文件名；旧的手写 Sscanf 解码失败会把 % 原样写回导致 404。
func TestUrlPathUnescape(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "中文名逐字节解码",
			in:   "6/1794630316814_%E7%B2%98%E8%B4%B4%E6%96%87%E4%BB%B6_1794630316793.x-msdownload",
			want: "6/1794630316814_粘贴文件_1794630316793.x-msdownload",
		},
		{
			name: "加号在路径段保持原样",
			in:   "6/%E6%B3%AA%E5%B0%BD.mp4",
			want: "6/泪尽.mp4",
		},
		{name: "ASCII 原样", in: "6/Mochi.dll", want: "6/Mochi.dll"},
		{name: "无编码空串", in: "", want: ""},
	}
	for _, c := range cases {
		got, err := urlPathUnescape(c.in)
		if err != nil {
			t.Fatalf("%s: 解码出错: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s: urlPathUnescape(%q)\n got = %s\nwant = %s", c.name, c.in, got, c.want)
		}
	}
}

// TestUrlPathUnescapeRoundTrip 编码再解码必须还原原始文件名。
func TestUrlPathUnescapeRoundTrip(t *testing.T) {
	for _, in := range []string{"泪尽铃音响翻唱+混音.mp4", "Mochi.dll", "日本語 テスト.png", "100%报告.pdf"} {
		escaped := urlQueryEscape(in)
		back, err := urlPathUnescape(escaped)
		if err != nil {
			t.Fatalf("round-trip %q: %v", in, err)
		}
		if back != in {
			t.Errorf("round-trip %q: 编码 %q → 解码 %q", in, escaped, back)
		}
	}
}

// TestDownloadChineseFilename 端到端回归：把中文名文件放进 FILE_DIR，
// 用 urlQueryEscape 生成的 URL 去请求 /files/，必须 200 且字节一致。
// 修复前该用例会 404（编码非法 → 解码出的路径不是真实文件名）。
func TestDownloadChineseFilename(t *testing.T) {
	const name = "泪尽铃音响翻唱+混音.mp4"
	const body = "fake-mp4-bytes"

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "default"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "default", name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	h := &filesHandler{cfg: &config.ServerConfig{File: config.FileConfig{Dir: dir}}}
	url := "/files/default/" + urlQueryEscape(name)

	req := httptest.NewRequest("GET", url, nil)
	rec := httptest.NewRecorder()
	h.download(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s → %d（期望 200）；编码必须能被服务端正确解码回真实文件名", url, rec.Code)
	}
	if got := rec.Body.String(); got != body {
		t.Fatalf("响应体 = %q，期望 %q", got, body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "video/mp4" {
		t.Fatalf("Content-Type = %q，期望 video/mp4", ct)
	}
}
