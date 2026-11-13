package http

import "testing"

func TestResolveExt(t *testing.T) {
	cases := map[string]string{
		"麻薯 Setup 14.8.5.exe":       "exe",
		"app-14.8.5.AppImage":          "AppImage",
		"app-14.8.5.zip":               "zip",
		"app-14.8.5-arm64.dmg":         "dmg",
	}
	for name, want := range cases {
		ext, ok := resolveExt(name)
		if !ok || ext != want {
			t.Errorf("%s: 得到 (%s,%v)，期望 %s", name, ext, ok, want)
		}
	}
	if _, ok := resolveExt("app-14.8.5.bin"); ok {
		t.Errorf(".bin 应不被支持")
	}
}

func TestParseSemVer(t *testing.T) {
	if _, ok := parseSemVer("14.8.5"); !ok {
		t.Error("合法 semver 应解析成功")
	}
	if _, ok := parseSemVer("latest"); ok {
		t.Error("非法版本应解析失败")
	}
	if cmpSemver("14.8.5", "14.8.4") <= 0 {
		t.Error("14.8.5 应高于 14.8.4")
	}
}
