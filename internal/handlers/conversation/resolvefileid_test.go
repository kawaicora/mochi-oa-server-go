package conversation

import (
	"context"
	"testing"
)

func TestResolveFileID(t *testing.T) {
	const uuid = "11111111-1111-1111-1111-111111111111"
	ctx := context.Background()

	cases := []struct {
		name        string
		kind        string
		raw         string
		explicit    string
		content     string
		want        string
		touchStore  bool
	}{
		{name: "text 不关联文件", kind: "text", raw: "hello", content: "hello", want: ""},
		{name: "folder 不关联文件", kind: "folder", raw: "/a/b", content: "/a/b", want: ""},
		{name: "显式 fileId 为合法 UUID 时优先采用", kind: "file", raw: "/files/a.bin", explicit: uuid, content: "/files/a.bin", want: uuid},
		{name: "raw 本身是 UUID 时直接采用", kind: "file", raw: uuid, content: uuid, want: uuid},
		{name: "显式非法 UUID 回退到 raw", kind: "file", raw: uuid, explicit: "not-a-uuid", content: uuid, want: uuid},
		{name: "显式与 raw 均非 UUID 时回退内容 URL 反查（需 store，单测不覆盖）", kind: "file", raw: "/files/a.bin", content: "/files/a.bin", want: "", touchStore: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.touchStore {
				t.Skip("需要 store 实现，依赖集成测试覆盖")
			}
			got := resolveFileID(ctx, nil, c.kind, c.raw, c.explicit, c.content)
			if got != c.want {
				t.Errorf("resolveFileID() = %q, want %q", got, c.want)
			}
		})
	}
}
