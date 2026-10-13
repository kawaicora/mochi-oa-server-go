package socket

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mochi-oa-server-go/internal/config"
	"mochi-oa-server-go/internal/socketapi"
)

// newTestServer 起一个内存测试服务。
func newTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.ServerConfig{
		Path:       "/socket.io",
		Transports: []string{"polling", "websocket"},
		CORSStar:   true,
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// TestHandshakeOPEN 验证 polling 握手 GET 返回规范 OPEN 报文。
func TestHandshakeOPEN(t *testing.T) {
	s := newTestServer(t)
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/socket.io/?EIO=4&transport=polling&t=abc123")
	if err != nil {
		t.Fatalf("GET handshake: %v", err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 512)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])

	if body[0] != eioOPEN {
		t.Fatalf("expected OPEN(0) frame, got %q", body[:1])
	}
	var open struct {
		SID          string   `json:"sid"`
		Upgrades    []string `json:"upgrades"`
		PingInterval int      `json:"pingInterval"`
		PingTimeout  int      `json:"pingTimeout"`
		MaxPayload   int      `json:"maxPayload"`
	}
	if err := json.Unmarshal([]byte(body[1:]), &open); err != nil {
		t.Fatalf("OPEN json: %v body=%s", err, body)
	}
	if open.SID == "" || open.PingInterval == 0 || open.MaxPayload != maxHttpBufferSize {
		t.Fatalf("bad OPEN payload: %+v", open)
	}
	t.Logf("OPEN ok: sid=%s upgrades=%v", open.SID, open.Upgrades)
}

// TestEventAckRoundTrip 验证 EVENT 带 ack id → 服务端 handler → ACK 回包。
func TestEventAckRoundTrip(t *testing.T) {
	s := newTestServer(t)

	called := make(chan string, 1)
	s.On("ping:echo", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		var in struct{ Msg string `json:"msg"` }
		_ = json.Unmarshal(data, &in)
		called <- in.Msg
		ack(map[string]any{"ok": true, "echo": in.Msg})
	})

	ts := httptest.NewServer(s)
	defer ts.Close()

	// 1) 握手拿 sid。
	openBody := doGet(t, ts.URL+"/socket.io/?EIO=4&transport=polling&t=1")
	var open struct {
		SID string `json:"sid"`
	}
	_ = json.Unmarshal([]byte(openBody[1:]), &open)
	sid := open.SID

	// 2) POST 客户端报文：CONNECT(0) + EVENT(2, ack id=0)。
	//    注意：polling 上多个 MESSAGE 拼接，每个前缀 '4'。
	connectFrame := "4" + "0" + `{"token":"x","device":"test"}`
	eventFrame := "4" + "20[\"ping:echo\",{\"msg\":\"hello\"}]"
	resp, err := http.Post(ts.URL+"/socket.io/?EIO=4&transport=polling&sid="+sid+"&t=2", "text/plain",
		strings.NewReader(connectFrame+eventFrame))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()

	// 3) 长轮询 GET 收 CONNECT 响应 + ACK。
	//    服务端回 CONNECT("0") 与 ACK("30[...]")，都包在 MESSAGE('4') 里。
	got := doGetTimeout(t, ts.URL+"/socket.io/?EIO=4&transport=polling&sid="+sid+"&t=3", 2*time.Second)

	if !strings.Contains(got, "0") {
		t.Logf("warn: no CONNECT frame in %q", got)
	}
	if !strings.Contains(got, `30[`) {
		t.Fatalf("expected ACK frame 30[ in long-poll, got %q", got)
	}
	if !strings.Contains(got, `"ok":true`) {
		t.Fatalf("ack payload missing ok:true, got %q", got)
	}
	select {
	case m := <-called:
		if m != "hello" {
			t.Fatalf("handler msg=%q", m)
		}
	case <-time.After(time.Second):
		t.Fatal("handler not called")
	}
	t.Logf("ACK round-trip ok: %q", got)
}

func doGet(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	return string(buf[:n])
}

func doGetTimeout(t *testing.T, url string, d time.Duration) string {
	t.Helper()
	c := &http.Client{Timeout: d}
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 8192)
	n, _ := resp.Body.Read(buf)
	return string(buf[:n])
}

var _ = fmt.Sprintf
