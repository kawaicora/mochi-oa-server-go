package socket

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"mochi-oa-server-go/internal/socketapi"
)

// TestRealSocketIOClientInterop 用真实 socket.io-client(Node) 连接本 Go 服务端，
// 验证 EIO4 握手、CONNECT、EVENT+ACK 全链路互通。需要本机 node 与源仓库 node_modules。
func TestRealSocketIOClientInterop(t *testing.T) {
	if testing.Short() {
		t.Skip("跳过 node 互操作测试（short 模式）")
	}
	nodePath := `D:\___SYSTEM_PATH\source\repos\mochi-oa-server\server\node_modules`
	if _, err := os.Stat(nodePath); err != nil {
		t.Skip("跳过：找不到源仓库 node_modules")
	}

	s := newTestServer(t)
	s.On("echo:ping", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		var in struct {
			Msg string `json:"msg"`
		}
		_ = json.Unmarshal(data, &in)
		ack(map[string]any{"ok": true, "echo": in.Msg})
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go http.Serve(ln, s)
	port := ln.Addr().(*net.TCPAddr).Port

	// 在源仓库目录下跑 node（那里有 socket.io-client）。
	_, selfFile, _, _ := runtime.Caller(0)
	clientJS := filepath.Join(filepath.Dir(selfFile), "interop_client.js")
	cmd := exec.Command("node", clientJS, fmt.Sprintf("%d", port))
	cmd.Dir = `D:\___SYSTEM_PATH\source\repos\mochi-oa-server\server`
	cmd.Env = append(os.Environ(), "NODE_PATH="+nodePath)
	out, err := cmd.CombinedOutput()
	t.Logf("node output:\n%s", string(out))
	if err != nil {
		t.Fatalf("node interop failed: %v", err)
	}
	if !contains(string(out), "INTEROP_OK") {
		t.Fatalf("未收到 INTEROP_OK，输出=%s", string(out))
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

var _ = runtime.GOOS
var _ = time.Second
