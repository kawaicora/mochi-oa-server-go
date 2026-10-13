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
