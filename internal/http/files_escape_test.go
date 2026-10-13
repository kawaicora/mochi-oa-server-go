package http

import "testing"

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
