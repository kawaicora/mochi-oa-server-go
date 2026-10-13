// Package rtcdev dev.go —— 远程设备控制转发层（被控端=客户端 app，控制端=/view/admin 电脑控制面板）。
// 1:1 复刻自源仓库 server/src/handlers/dev.ts。
//
// 角色语义：
//   - 被控端（桌面客户端 app，登录后经 socket 上报）：dev:register/dev:heartbeat/dev:devices/dev:sys/dev:signal(dev:execResult/dev:termOut/dev:termClose 回传）；
//   - 控制端（浏览器，需 SERVER_ADMIN）：dev:view/dev:enumerate/dev:start/dev:stop/dev:execRemote/dev:termOpen/dev:termIn/dev:termClose。
//
// 在线远程设备表 devices 为包级进程内注册表（对标 TS 的 new Map）；Go 多协程并发，
// 统一由 devMu 互斥锁保护。lastSeen 在 register/heartbeat/sys 时刷新，断线即删；
// 另有防御性扫表：连接已不存在或超 90 秒无心跳/上报的条目一并清除（对标 TS 断线清理语义）。
package rtcdev

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"mochi-oa-server-go/internal/config"
	"mochi-oa-server-go/internal/logger"
	"mochi-oa-server-go/internal/socketapi"
	"mochi-oa-server-go/internal/util"
)

// deviceStaleAfter 是心跳超时阈值：超此时长无 register/heartbeat/sys 上报即视为离线。
const deviceStaleAfter = 90 * time.Second

// remoteDevice 是在线远程设备（被控端）。
type remoteDevice struct {
	deviceID   string
	conn       socketapi.Conn
	name       string
	osName     string
	ip         string
	username   string
	userID     int64
	lastSeen   int64
	controller string // 当前控制它的浏览器 socketId（""=无）
	sys        *deviceSys
}

// deviceSys 是被控端上报的系统信息快照（dev:sys）。
type deviceSys struct {
	Info any   `json:"info"`
	Perf any   `json:"perf"`
	Ts   int64 `json:"ts"`
}

// devices 在线远程设备表：deviceId -> *remoteDevice。
var (
	devMu   sync.Mutex
	devices = map[string]*remoteDevice{}
)

// DeviceSnapshot 是远程设备注册表快照条目（供管理后台 admin:listDevices 与本机设备合并）。
type DeviceSnapshot struct {
	DeviceID string
	Name     string
	Username string
	OS       string
	IP       string
	Online   bool
}

// ListDevices 返回远程设备注册表快照（只读拷贝；不改 register/heartbeat/断线任何现有逻辑）。
// online 判定与 TS remoteDevices 一致：lastSeen 在 30 秒窗口内视为在线。
func ListDevices() []DeviceSnapshot {
	devMu.Lock()
	defer devMu.Unlock()
	cutoff := time.Now().UnixMilli() - 30_000
	out := make([]DeviceSnapshot, 0, len(devices))
	for id, d := range devices {
		out = append(out, DeviceSnapshot{
			DeviceID: id,
			Name:     d.name,
			Username: d.username,
			OS:       d.osName,
			IP:       d.ip,
			Online:   d.lastSeen >= cutoff,
		})
	}
	return out
}

// devInfoIn 是 upsert 入参（空串=该字段不更新）。
type devInfoIn struct {
	deviceID string
	name     string
	osName   string
	ip       string
	username string
}

// ==================== 请求体结构 ====================

type devRegisterReq struct {
	DeviceID *string `json:"deviceId"`
	Name     *string `json:"name"`
	OS       *string `json:"os"`
	IP       *string `json:"ip"`
	Username *string `json:"username"`
}

type devDevicesReq struct {
	DeviceID *string `json:"deviceId"`
	Cams     any     `json:"cams"`
	Mics     any     `json:"mics"`
}

type devSysReq struct {
	DeviceID *string `json:"deviceId"`
	Info     any     `json:"info"`
	Perf     any     `json:"perf"`
}

type devSignalReq struct {
	DeviceID *string `json:"deviceId"`
	Signal   any     `json:"signal"`
}

type devViewReq struct {
	DeviceID *string `json:"deviceId"`
}

type devStartReq struct {
	DeviceID *string `json:"deviceId"`
	Kind     *string `json:"kind"`
	Device   any     `json:"device"`
}

type devExecReq struct {
	DeviceID *string `json:"deviceId"`
	Shell    *string `json:"shell"`
	Cmd      *string `json:"cmd"`
}

type devExecResultReq struct {
	DeviceID *string  `json:"deviceId"`
	OK       *bool    `json:"ok"`
	ExitCode *float64 `json:"exitCode"`
	Stdout   *string  `json:"stdout"`
	Stderr   *string  `json:"stderr"`
}

type devTermOpenReq struct {
	DeviceID *string `json:"deviceId"`
	Shell    *string `json:"shell"`
}

type devTermInReq struct {
	DeviceID *string `json:"deviceId"`
	Data     *string `json:"data"`
}

type devTermOutReq struct {
	DeviceID *string `json:"deviceId"`
	Data     *string `json:"data"`
	IsErr    *bool   `json:"isErr"`
}

type devTermCloseReq struct {
	DeviceID *string  `json:"deviceId"`
	Code     *float64 `json:"code"`
}

// ==================== 内部函数 ====================

// send 给指定连接安全发送（对标 socket.connected 判断后 emit；Go 层 emit 本身即检查存活）。
func send(c socketapi.Conn, event string, payload any) {
	if c == nil {
		return
	}
	if err := c.Emit(event, payload); err != nil {
		logger.Error(module, "dev 转发发送失败", "event", event, "error", err.Error())
	}
}

// isSlave 判断该连接是否为被控端连接（对标 socket.data.isSlave 真值）。
func isSlave(c socketapi.Conn) bool {
	v, _ := c.Get(keyIsSlave).(bool)
	return v
}

// adminOf 判断连接是否 SERVER_ADMIN（对标 isAdminSocket）。
func adminOf(c socketapi.Conn, cfg *config.ServerConfig) bool {
	a := authed(c)
	return a != nil && isServerAdmin(a, cfg)
}

// upsertDev 按 deviceId 幂等登记：已有则更新（含 socket，重连后新 socket 覆盖）；
// 没有则用传入信息 + socket 认证用户新建。返回 nil 表示缺 deviceId。
func upsertDev(c socketapi.Conn, info devInfoIn) *remoteDevice {
	if info.deviceID == "" {
		return nil
	}
	devMu.Lock()
	defer devMu.Unlock()
	if existing := devices[info.deviceID]; existing != nil {
		existing.lastSeen = nowMs()
		if existing.conn != c {
			// 重连/迁移：绑定到新 socket，避免后续事件被「设备不匹配/socket 不匹配」丢弃
			existing.conn = c
			c.Set(keyDeviceID, info.deviceID)
			c.Set(keyIsSlave, true)
		}
		if info.name != "" {
			existing.name = info.name
		}
		if info.osName != "" {
			existing.osName = info.osName
		}
		if info.ip != "" {
			existing.ip = info.ip
		}
		if info.username != "" {
			existing.username = info.username
		}
		return existing
	}
	u := authed(c)
	name := truncate(orDefault(info.name, "电脑"), 64)
	username := info.username
	if username == "" && u != nil {
		if u.Nick != "" {
			username = u.Nick
		} else {
			username = u.Username
		}
	}
	if username == "" {
		username = "电脑"
	}
	dev := &remoteDevice{
		deviceID: info.deviceID,
		conn:     c,
		name:     name,
		osName:   truncate(info.osName, 128),
		ip:       truncate(info.ip, 128),
		username: truncate(username, 64),
		lastSeen: nowMs(),
	}
	if u != nil {
		dev.userID = u.ID
	}
	c.Set(keyDeviceID, info.deviceID)
	c.Set(keyIsSlave, true)
	devices[info.deviceID] = dev
	logger.Info(module, "被控端上线", "deviceId", dev.deviceID, "name", dev.name,
		"os", dev.osName, "ip", dev.ip, "user", dev.username, "online", len(devices))
	return dev
}

// orDefault 空串回退。
func orDefault(v, dft string) string {
	if v == "" {
		return dft
	}
	return v
}

// strVal 安全读 *string。
func strVal(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// shellOf 取 shell 字段，空兜底 auto（对标 d.shell ?? 'auto'）。
func shellOf(p *string) string {
	if p == nil || strings.TrimSpace(*p) == "" {
		return "auto"
	}
	return *p
}

// ==================== 注册入口 ====================

// RegisterDev 注册全部远程设备控制事件（对标 registerDevHandlers）。
func RegisterDev(c Ctx) {
	srv, cfg := c.Srv, c.Config

	// 防御性扫表：断线清理的兜底——连接已消失或长时间无心跳的条目剔除。
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for range t.C {
			sweepDevices(srv)
		}
	}()

	// ---- 被控端注册/上线 ----
	srv.On("dev:register", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		var d devRegisterReq
		_ = json.Unmarshal(raw, &d)
		deviceID := strings.TrimSpace(strVal(d.DeviceID))
		u := authed(c)
		if deviceID == "" || u == nil {
			// logger.Info(module, "dev:register 拒绝", "deviceId", deviceID, "loggedIn", u != nil)
			ack(util.Fail("参数不合法"))
			return
		}
		devMu.Lock()
		_, existed := devices[deviceID]
		devMu.Unlock()
		dev := upsertDev(c, devInfoIn{
			deviceID: deviceID,
			name:     strVal(d.Name),
			osName:   strVal(d.OS),
			ip:       strVal(d.IP),
			username: strVal(d.Username),
		})
		if dev == nil {
			ack(util.Fail("参数不合法"))
			return
		}
		status := "added"
		if existed {
			status = "updated"
		}
		logger.Info(module, "被控端注册", "deviceId", deviceID, "status", status)
		ack(util.OK(map[string]any{"deviceId": deviceID, "status": status}))
	})

	// ---- 心跳保活（fire-and-forget：客户端不等 ack，服务端也不回） ----
	srv.On("dev:heartbeat", func(c socketapi.Conn, raw json.RawMessage, _ socketapi.Ack) {
		var d devRegisterReq
		_ = json.Unmarshal(raw, &d)
		deviceID := strings.TrimSpace(strVal(d.DeviceID))
		if deviceID == "" {
			return
		}
		dev := upsertDev(c, devInfoIn{
			deviceID: deviceID,
			name:     strVal(d.Name),
			osName:   strVal(d.OS),
			ip:       strVal(d.IP),
			username: strVal(d.Username),
		})
		if dev != nil {
			logger.Info(module, "心跳", "deviceId", deviceID, "name", dev.name)
		}
	})

	// ---- 被控端上报枚举设备 → 转发给控制端 ----
	srv.On("dev:devices", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		var d devDevicesReq
		_ = json.Unmarshal(raw, &d)
		deviceID := strings.TrimSpace(strVal(d.DeviceID))
		dev := upsertDev(c, devInfoIn{deviceID: deviceID})
		if dev == nil {
			logger.Info(module, "dev:devices 缺 deviceId")
			ack(util.Fail("缺少设备号"))
			return
		}
		cams, mics := d.Cams, d.Mics
		if cams == nil {
			cams = []any{}
		}
		if mics == nil {
			mics = []any{}
		}
		payload := map[string]any{"deviceId": deviceID, "cams": cams, "mics": mics}
		devMu.Lock()
		ctrlID := dev.controller
		devMu.Unlock()
		if ctrlID != "" {
			send(connByID(srv, ctrlID), "dev:devices", payload)
		}
		ack(util.OK(map[string]any{"deviceId": deviceID}))
	})

	// ---- 被控端上报系统信息 → 存快照并转发给控制端 ----
	srv.On("dev:sys", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		var d devSysReq
		_ = json.Unmarshal(raw, &d)
		deviceID := strings.TrimSpace(strVal(d.DeviceID))
		// 动态包携带 info.user → 作为 username 兜底登记
		username := ""
		if m, _ := d.Info.(map[string]any); m != nil {
			username = strOf(m["user"])
		}
		dev := upsertDev(c, devInfoIn{deviceID: deviceID, username: username})
		if dev == nil {
			ack(util.Fail("缺少设备号"))
			return
		}
		now := nowMs()
		devMu.Lock()
		dev.lastSeen = now
		dev.sys = &deviceSys{Info: d.Info, Perf: d.Perf, Ts: now}
		// 完整基础信息（os/ip 齐全）→ updated，否则 lost（客户端将重发全量 register）
		complete := dev.osName != "" && dev.ip != ""
		ctrlID := dev.controller
		devMu.Unlock()
		if ctrlID != "" {
			ctrl := connByID(srv, ctrlID)
			if ctrl != nil {
				send(ctrl, "dev:sys", map[string]any{"deviceId": deviceID, "info": d.Info, "perf": d.Perf})
			} else {
				// 控制端已断开/刷新页面，清绑定，避免后续推送到死 socket
				devMu.Lock()
				if dev.controller == ctrlID {
					dev.controller = ""
				}
				devMu.Unlock()
			}
		}
		status := "lost"
		if complete {
			status = "updated"
		}
		ack(util.OK(map[string]any{"deviceId": deviceID, "status": status}))
	})

	// ---- 信令：按角色分发（被控端→控制端 / 控制端→被控端） ----
	srv.On("dev:signal", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		var d devSignalReq
		_ = json.Unmarshal(raw, &d)
		deviceID := strings.TrimSpace(strVal(d.DeviceID))
		devMu.Lock()
		dev := devices[deviceID]
		devMu.Unlock()
		if dev == nil {
			ack(util.Fail("设备不存在"))
			return
		}
		if isSlave(c) {
			// 被控端：转发给当前控制端
			devMu.Lock()
			match := dev.conn == c
			ctrlID := dev.controller
			devMu.Unlock()
			if !match {
				ack(util.Fail("设备不匹配"))
				return
			}
			if ctrlID != "" {
				send(connByID(srv, ctrlID), "dev:signal", map[string]any{"deviceId": deviceID, "signal": d.Signal})
			}
			ack(util.OK(nil))
			return
		}
		// 控制端：需 SERVER_ADMIN 且为当前控制器
		if !adminOf(c, cfg) {
			ack(util.Fail("需要 SERVER_ADMIN 权限"))
			return
		}
		devMu.Lock()
		ctrlID := dev.controller
		devMu.Unlock()
		if ctrlID != c.ID() {
			ack(util.Fail("你不是该设备的控制器"))
			return
		}
		send(dev.conn, "dev:signal", map[string]any{"deviceId": deviceID, "signal": d.Signal})
		ack(util.OK(nil))
	})

	// ---- 断开：清在线表 + 解除控制端绑定 ----
	srv.OnDisconnect(func(c socketapi.Conn, _ string) {
		devMu.Lock()
		removed := []string{}
		for k, v := range devices {
			if v.conn == c {
				delete(devices, k)
				removed = append(removed, k)
			}
			if v.controller == c.ID() {
				v.controller = ""
			}
		}
		devMu.Unlock()
		if len(removed) > 0 {
			logger.Info(module, "被控端离线", "devices", strings.Join(removed, ","))
		}
	})

	// ---- 控制端请求查看某台远程电脑（绑定控制器；同一设备仅一个，抢占替换） ----
	srv.On("dev:view", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		if isSlave(c) {
			ack(util.Fail("被控端连接不可控制设备"))
			return
		}
		if !adminOf(c, cfg) {
			ack(util.Fail("需要 SERVER_ADMIN 权限"))
			return
		}
		var d devViewReq
		_ = json.Unmarshal(raw, &d)
		deviceID := strings.TrimSpace(strVal(d.DeviceID))
		devMu.Lock()
		dev := devices[deviceID]
		if dev != nil {
			dev.controller = c.ID()
		}
		devMu.Unlock()
		if dev == nil {
			logger.Info(module, "dev:view 目标不存在", "deviceId", deviceID)
			ack(util.Fail("该设备已离线"))
			return
		}
		send(dev.conn, "dev:view", map[string]any{"deviceId": deviceID})
		logger.Info(module, "控制端查看设备", "deviceId", deviceID, "name", dev.name)
		ack(util.OK(map[string]any{
			"deviceId":   deviceID,
			"name":       dev.name,
			"username":   dev.username,
			"os":         dev.osName,
			"ip":         dev.ip,
			"iceServers": cfg.IceServers, // 控制端信令用静态 ICE（照 TS dev.ts，不走 CF 动态）
			"sys":        dev.sys,
		}))
	})

	// ---- 请求被控端枚举设备 ----
	srv.On("dev:enumerate", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		if isSlave(c) {
			ack(util.Fail("被控端连接不可控制设备"))
			return
		}
		if !adminOf(c, cfg) {
			ack(util.Fail("需要 SERVER_ADMIN 权限"))
			return
		}
		var d devViewReq
		_ = json.Unmarshal(raw, &d)
		deviceID := strings.TrimSpace(strVal(d.DeviceID))
		devMu.Lock()
		dev := devices[deviceID]
		if dev != nil {
			dev.controller = c.ID()
		}
		devMu.Unlock()
		if dev == nil {
			ack(util.Fail("该设备已离线"))
			return
		}
		send(dev.conn, "dev:enumerate", map[string]any{"deviceId": deviceID})
		ack(util.OK(map[string]any{"deviceId": deviceID}))
	})

	// ---- 开始采集（camera/screen/mic） ----
	srv.On("dev:start", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		if isSlave(c) {
			ack(util.Fail("被控端连接不可控制设备"))
			return
		}
		if !adminOf(c, cfg) {
			ack(util.Fail("需要 SERVER_ADMIN 权限"))
			return
		}
		var d devStartReq
		_ = json.Unmarshal(raw, &d)
		deviceID := strings.TrimSpace(strVal(d.DeviceID))
		devMu.Lock()
		dev := devices[deviceID]
		if dev != nil {
			dev.controller = c.ID()
		}
		devMu.Unlock()
		if dev == nil {
			ack(util.Fail("该设备已离线"))
			return
		}
		kind := strVal(d.Kind)
		if kind != "camera" && kind != "screen" && kind != "mic" {
			ack(util.Fail("采集类型不合法"))
			return
		}
		send(dev.conn, "dev:start", map[string]any{
			"deviceId":   deviceID,
			"kind":       kind,
			"device":     d.Device,
			"iceServers": cfg.IceServers,
		})
		ack(util.OK(map[string]any{"deviceId": deviceID, "kind": kind, "iceServers": cfg.IceServers}))
	})

	// ---- 停止采集 ----
	srv.On("dev:stop", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		if isSlave(c) {
			ack(util.Fail("被控端连接不可控制设备"))
			return
		}
		if !adminOf(c, cfg) {
			ack(util.Fail("需要 SERVER_ADMIN 权限"))
			return
		}
		var d devViewReq
		_ = json.Unmarshal(raw, &d)
		deviceID := strings.TrimSpace(strVal(d.DeviceID))
		devMu.Lock()
		dev := devices[deviceID]
		devMu.Unlock()
		if dev == nil {
			ack(util.Fail("该设备已离线"))
			return
		}
		send(dev.conn, "dev:stop", map[string]any{"deviceId": deviceID})
		ack(util.OK(map[string]any{"deviceId": deviceID}))
	})

	// ---- 控制端远程执行命令 → 转发被控端 ----
	srv.On("dev:execRemote", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		if isSlave(c) {
			ack(util.Fail("被控端连接不可控制设备"))
			return
		}
		if !adminOf(c, cfg) {
			ack(util.Fail("需要 SERVER_ADMIN 权限"))
			return
		}
		var d devExecReq
		_ = json.Unmarshal(raw, &d)
		deviceID := strings.TrimSpace(strVal(d.DeviceID))
		cmd := strings.TrimSpace(strVal(d.Cmd))
		devMu.Lock()
		dev := devices[deviceID]
		devMu.Unlock()
		if dev == nil {
			ack(util.Fail("该设备已离线"))
			return
		}
		if cmd == "" {
			ack(util.Fail("命令为空"))
			return
		}
		send(dev.conn, "dev:exec", map[string]any{
			"deviceId": deviceID,
			"shell":    shellOf(d.Shell),
			"cmd":      cmd,
		})
		ack(util.OK(map[string]any{"deviceId": deviceID}))
	})

	// ---- 被控端执行结果 → 转发给当前控制端 ----
	srv.On("dev:execResult", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		var d devExecResultReq
		_ = json.Unmarshal(raw, &d)
		deviceID := strings.TrimSpace(strVal(d.DeviceID))
		devMu.Lock()
		dev := devices[deviceID]
		match := dev != nil && dev.conn == c
		ctrlID := ""
		if match {
			ctrlID = dev.controller
		}
		devMu.Unlock()
		if !match {
			ack(util.Fail("设备不存在"))
			return
		}
		ok := false
		if d.OK != nil {
			ok = *d.OK
		}
		exitCode := 0.0
		if d.ExitCode != nil {
			exitCode = *d.ExitCode
		}
		payload := map[string]any{
			"deviceId": deviceID,
			"ok":       ok,
			"exitCode": int64(exitCode),
			"stdout":   strVal(d.Stdout),
			"stderr":   strVal(d.Stderr),
		}
		if ctrlID != "" {
			send(connByID(srv, ctrlID), "dev:execResult", payload)
		}
		ack(util.OK(map[string]any{"deviceId": deviceID}))
	})

	// ---- 交互终端：打开 ----
	srv.On("dev:termOpen", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		if isSlave(c) {
			ack(util.Fail("被控端连接不可控制设备"))
			return
		}
		if !adminOf(c, cfg) {
			ack(util.Fail("需要 SERVER_ADMIN 权限"))
			return
		}
		var d devTermOpenReq
		_ = json.Unmarshal(raw, &d)
		deviceID := strings.TrimSpace(strVal(d.DeviceID))
		devMu.Lock()
		dev := devices[deviceID]
		if dev != nil {
			dev.controller = c.ID()
		}
		devMu.Unlock()
		if dev == nil {
			ack(util.Fail("该设备已离线"))
			return
		}
		send(dev.conn, "dev:termOpen", map[string]any{"deviceId": deviceID, "shell": shellOf(d.Shell)})
		ack(util.OK(map[string]any{"deviceId": deviceID}))
	})

	// ---- 交互终端：输入 ----
	srv.On("dev:termIn", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		if isSlave(c) {
			ack(util.Fail("被控端连接不可控制设备"))
			return
		}
		if !adminOf(c, cfg) {
			ack(util.Fail("需要 SERVER_ADMIN 权限"))
			return
		}
		var d devTermInReq
		_ = json.Unmarshal(raw, &d)
		deviceID := strings.TrimSpace(strVal(d.DeviceID))
		devMu.Lock()
		dev := devices[deviceID]
		ctrlID := ""
		if dev != nil {
			ctrlID = dev.controller
		}
		devMu.Unlock()
		if dev == nil {
			ack(util.Fail("该设备已离线"))
			return
		}
		if ctrlID != c.ID() {
			ack(util.Fail("你不是该设备的控制器"))
			return
		}
		send(dev.conn, "dev:termIn", map[string]any{"deviceId": deviceID, "data": strVal(d.Data)})
		ack(util.OK(map[string]any{"deviceId": deviceID}))
	})

	// ---- 交互终端：输出（被控端→控制端） ----
	srv.On("dev:termOut", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		var d devTermOutReq
		_ = json.Unmarshal(raw, &d)
		deviceID := strings.TrimSpace(strVal(d.DeviceID))
		devMu.Lock()
		dev := devices[deviceID]
		match := dev != nil && dev.conn == c
		ctrlID := ""
		if match {
			ctrlID = dev.controller
		}
		devMu.Unlock()
		if !match {
			ack(util.Fail("设备不存在"))
			return
		}
		isErr := false
		if d.IsErr != nil {
			isErr = *d.IsErr
		}
		if ctrlID != "" {
			send(connByID(srv, ctrlID), "dev:termOut",
				map[string]any{"deviceId": deviceID, "data": strVal(d.Data), "isErr": isErr})
		}
		ack(util.OK(map[string]any{"deviceId": deviceID}))
	})

	// ---- 交互终端：关闭（双向） ----
	srv.On("dev:termClose", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		var d devTermCloseReq
		_ = json.Unmarshal(raw, &d)
		deviceID := strings.TrimSpace(strVal(d.DeviceID))
		devMu.Lock()
		dev := devices[deviceID]
		ctrlID := ""
		if dev != nil && dev.conn == c {
			ctrlID = dev.controller
		}
		devMu.Unlock()
		if dev == nil {
			ack(util.Fail("设备不存在"))
			return
		}
		if ctrlID != "" {
			// 被控端会话已结束 → 通知控制端
			code := 0.0
			if d.Code != nil {
				code = *d.Code
			}
			send(connByID(srv, ctrlID), "dev:termClose", map[string]any{"deviceId": deviceID, "code": int64(code)})
		} else {
			// 控制端主动关闭终端 → 转发被控端结束会话
			if !adminOf(c, cfg) {
				ack(util.Fail("需要 SERVER_ADMIN 权限"))
				return
			}
			send(dev.conn, "dev:termClose", map[string]any{"deviceId": deviceID})
		}
		ack(util.OK(map[string]any{"deviceId": deviceID}))
	})
}

// sweepDevices 防御性扫表：剔除连接已消失或超 deviceStaleAfter 无心跳的设备。
func sweepDevices(srv socketapi.Server) {
	cutoff := nowMs() - int64(deviceStaleAfter/time.Millisecond)
	devMu.Lock()
	removed := []string{}
	for k, v := range devices {
		gone := connByID(srv, v.conn.ID()) == nil
		if gone || v.lastSeen < cutoff {
			delete(devices, k)
			removed = append(removed, k)
		}
	}
	devMu.Unlock()
	if len(removed) > 0 {
		logger.Info(module, "设备超时清理", "devices", strings.Join(removed, ","), "online", len(devices))
	}
}
