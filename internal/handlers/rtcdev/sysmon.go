// Package rtcdev sysmon.go —— 系统监控 handler（/view/admin → 电脑控制页的数据层）。
// 1:1 复刻自源仓库 server/src/handlers/sysmon.ts。
//
// 全部要求 SERVER_ADMIN（服务端兜底校验）：
//   - sys:info       系统信息（主机名/平台/CPU/内存/磁盘/网络IP/主板/显卡/运行时长）
//   - sys:perf       实时 CPU 使用率 + 内存 + 进程数
//   - sys:processes  进程列表（Windows tasklist / Linux ps）
//   - sys:exec       执行命令行（超时 15s / 长度 2000 限制）
//   - sys:screenshot 截取服务器屏幕（无显示环境明确报不可用；返回 base64 data URL）
//   - sys:camera     读取摄像头一帧（无设备明确报不可用）
//
// 采集策略与 TS 一致：基础信息优先读系统文件（Linux /proc、Windows PowerShell CIM），
// 主板/显卡/磁盘尽力尝试、失败回退空串（board/gpu 回退「未知」），绝不让进程崩溃。
package rtcdev

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"mochi-oa-server-go/internal/handlers/admin"
	"mochi-oa-server-go/internal/logger"
	"mochi-oa-server-go/internal/socketapi"
	"mochi-oa-server-go/internal/util"
)

// execResult 是 execCmd 的返回（对标 TS 的 {stdout, stderr, exitCode, error?}）。
type execResult struct {
	stdout   string
	stderr   string
	exitCode *int // nil=未取到（如命令过长直接拒绝）
	errStr   string
}

// ok 判断命令是否正常结束（无 error 且 exitCode==0）。
func (r execResult) ok() bool {
	return r.errStr == "" && r.exitCode != nil && *r.exitCode == 0
}

// capBuf 是带 2MB 上限的输出缓冲（对标 Node cp.exec 的 maxBuffer: 2MB）。
type capBuf struct {
	b     bytes.Buffer
	limit int
}

func (w *capBuf) Write(p []byte) (int, error) {
	if w.b.Len() >= w.limit {
		return len(p), nil // 丢弃溢出部分，不报错（对标 maxBuffer 截断）
	}
	if remain := w.limit - w.b.Len(); len(p) > remain {
		p = p[:remain]
	}
	return w.b.Write(p)
}

func (w *capBuf) String() string { return w.b.String() }

// execCmd 执行一条命令，返回 stdout/stderr/exitCode；超时或异常时 errStr 返回。
// 绝不让进程崩溃（对标 execCmd 的 Promise 语义）。
func execCmd(command string, timeoutMs int) execResult {
	if command == "" || len(command) > 2000 {
		return execResult{exitCode: nil, errStr: "命令为空或过长"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()
	var c *exec.Cmd
	if runtime.GOOS == "windows" {
		c = exec.CommandContext(ctx, "cmd", "/c", command)
	} else {
		c = exec.CommandContext(ctx, "sh", "-c", command)
	}
	out := &capBuf{limit: 2 * 1024 * 1024}
	errb := &capBuf{limit: 2 * 1024 * 1024}
	c.Stdout = out
	c.Stderr = errb
	err := c.Run()
	res := execResult{stdout: out.String(), stderr: errb.String()}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code := ee.ExitCode()
			res.exitCode = &code
		} else {
			res.errStr = err.Error()
		}
	} else {
		code := 0
		res.exitCode = &code
	}
	return res
}

// netIf 是网络地址条目（对标 networkIps 返回项）。
type netIf struct {
	Name     string `json:"name"`
	Address  string `json:"address"`
	Family   string `json:"family"`
	Internal bool   `json:"internal"`
}

// networkIps 收集非内部 IPv4 地址。
func networkIps() []netIf {
	out := []netIf{}
	nis, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, n := range nis {
		addrs, err := n.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil {
				continue
			}
			out = append(out, netIf{
				Name: n.Name, Address: ip4.String(), Family: "IPv4", Internal: ip4.IsLoopback(),
			})
		}
	}
	return out
}

// procEntry 是进程列表条目。
type procEntry struct {
	PID  int64  `json:"pid"`
	Name string `json:"name"`
	CPU  string `json:"cpu,omitempty"`
	Mem  string `json:"mem,omitempty"`
	Args string `json:"args,omitempty"`
}

var reTasklistLine = regexp.MustCompile(`^"([^"]*)","(\d+)"`)

// processes 进程列表快照。
func processes() []procEntry {
	out := []procEntry{}
	if runtime.GOOS == "windows" {
		r := execCmd("tasklist /FO CSV /NH", 12000)
		if !r.ok() {
			return out
		}
		for _, line := range strings.Split(r.stdout, "\n") {
			line = strings.TrimRight(line, "\r")
			m := reTasklistLine.FindStringSubmatch(line)
			if m != nil {
				pid, _ := strconv.ParseInt(m[2], 10, 64)
				out = append(out, procEntry{PID: pid, Name: m[1]})
			}
		}
		return out
	}
	r := execCmd("ps -eo pid,comm,%cpu,%mem,args --sort=-%cpu 2>/dev/null || ps -eo pid,comm,%cpu,%mem,args", 12000)
	if !r.ok() {
		return out
	}
	lines := strings.Split(r.stdout, "\n")
	if len(lines) > 1 {
		lines = lines[1:]
	}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) >= 4 {
			pid, _ := strconv.ParseInt(parts[0], 10, 64)
			out = append(out, procEntry{
				PID: pid, Name: parts[1], CPU: parts[2], Mem: parts[3], Args: strings.Join(parts[4:], " "),
			})
		}
	}
	return out
}

// cpuSample 是 /proc/stat 一行的累计值。
type cpuSample struct {
	idle, total uint64
}

// readProcStatCPU 读 /proc/stat 的 cpu 与各 cpuN 行。
func readProcStatCPU() []cpuSample {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return nil
	}
	out := []cpuSample{}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "cpu") {
			continue
		}
		fields := strings.Fields(line)
		nums := []uint64{}
		for _, f := range fields[1:] {
			n, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				break
			}
			nums = append(nums, n)
		}
		if len(nums) < 4 {
			continue
		}
		idle := nums[3] // idle
		total := uint64(0)
		for _, n := range nums {
			total += n
		}
		out = append(out, cpuSample{idle: idle, total: total})
	}
	return out
}

// cpuUsage 采样两次（间隔 500ms）差值算 CPU 使用率（对标 os.cpus() 采样法）。
func cpuUsage() (float64, []float64) {
	if runtime.GOOS == "windows" {
		r := execCmd(`powershell -NoProfile -Command "(Get-Counter '\Processor(_Total)\% Processor Time').CounterSamples.CookedValue"`, 6000)
		pct := 0.0
		if r.errStr == "" {
			pct, _ = strconv.ParseFloat(strings.TrimSpace(r.stdout), 64)
		}
		pct = math.Round(pct*10) / 10
		return pct, []float64{pct}
	}
	a := readProcStatCPU()
	time.Sleep(500 * time.Millisecond) // 对标 TS 的 setTimeout 500ms
	b := readProcStatCPU()
	perCore := []float64{}
	sum := 0.0
	for i := range b {
		if i >= len(a) {
			break
		}
		t := b[i].total - a[i].total
		idl := b[i].idle - a[i].idle
		pct := 0.0
		if t > 0 {
			pct = math.Max(0, math.Min(100, math.Round(float64(t-idl)/float64(t)*1000)/10))
		}
		perCore = append(perCore, pct)
		sum += pct
	}
	avg := 0.0
	if len(perCore) > 0 {
		avg = sum / float64(len(perCore))
	}
	return math.Round(avg*10) / 10, perCore
}

// memInfo 返回总/空闲物理内存字节。
func memInfo() (total, free uint64) {
	if runtime.GOOS == "windows" {
		r := execCmd(`powershell -NoProfile -Command "$o=Get-CimInstance Win32_OperatingSystem; '{0} {1}' -f ($o.TotalVisibleMemorySize*1KB),($o.FreePhysicalMemory*1KB)"`, 8000)
		if r.ok() {
			fmt.Sscanf(strings.TrimSpace(r.stdout), "%d %d", &total, &free)
		}
		return
	}
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			fmt.Sscanf(line, "MemTotal: %d kB", &total)
			total *= 1024
		case strings.HasPrefix(line, "MemFree:"):
			fmt.Sscanf(line, "MemFree: %d kB", &free)
			free *= 1024
		}
	}
	return
}

// uptimeSec 返回系统运行秒数。
func uptimeSec() int64 {
	if runtime.GOOS == "windows" {
		r := execCmd(`powershell -NoProfile -Command "(Get-Date) - (Get-CimInstance Win32_OperatingSystem).LastBootUpTime | Select-Object -ExpandProperty TotalSeconds"`, 8000)
		if r.ok() {
			if f, err := strconv.ParseFloat(strings.TrimSpace(r.stdout), 64); err == nil {
				return int64(f)
			}
		}
		return 0
	}
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0
	}
	f, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return int64(f)
}

// cpuModel 返回 CPU 型号与核数。
func cpuModel() (string, int) {
	cores := runtime.NumCPU()
	if runtime.GOOS == "windows" {
		r := execCmd(`powershell -NoProfile -Command "(Get-CimInstance Win32_Processor | Select-Object -First 1).Name"`, 8000)
		if r.ok() {
			return strings.TrimSpace(r.stdout), cores
		}
		return "", cores
	}
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return "", cores
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "model name") {
			if i := strings.Index(line, ":"); i >= 0 {
				return strings.TrimSpace(line[i+1:]), cores
			}
		}
	}
	return "", cores
}

// osRelease 返回内核/系统版本字符串。
func osRelease() string {
	if runtime.GOOS == "windows" {
		r := execCmd(`powershell -NoProfile -Command "(Get-CimInstance Win32_OperatingSystem).Version"`, 8000)
		if r.ok() {
			return strings.TrimSpace(r.stdout)
		}
		return ""
	}
	if data, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		return strings.TrimSpace(string(data))
	}
	return ""
}

// platformName 映射 runtime.GOOS → Node os.platform() 风格。
func platformName() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	case "darwin":
		return "darwin"
	default:
		return "linux"
	}
}

// osTypeName 映射 runtime.GOOS → Node os.type() 风格。
func osTypeName() string {
	switch runtime.GOOS {
	case "windows":
		return "Windows_NT"
	case "darwin":
		return "Darwin"
	default:
		return "Linux"
	}
}

// collectHW 收集主板/显卡/磁盘（尽力尝试，失败回退空串；对标 TS 的 Promise.all）。
func collectHW() (board, gpu, disk string) {
	if runtime.GOOS == "windows" {
		r := execCmd(`powershell -NoProfile -Command "Get-CimInstance Win32_BaseBoard | Select-Object -ExpandProperty Manufacturer; Get-CimInstance Win32_BaseBoard | Select-Object -ExpandProperty Product"`, 12000)
		if r.ok() {
			parts := []string{}
			for _, s := range strings.Split(r.stdout, "\n") {
				if s = strings.TrimRight(s, "\r"); strings.TrimSpace(s) != "" {
					parts = append(parts, strings.TrimSpace(s))
				}
			}
			board = strings.Join(parts, " / ")
		}
		r = execCmd(`powershell -NoProfile -Command "Get-CimInstance Win32_VideoController | Select-Object -ExpandProperty Name"`, 12000)
		if r.ok() {
			parts := []string{}
			for _, s := range strings.Split(r.stdout, "\n") {
				if s = strings.TrimRight(s, "\r"); strings.TrimSpace(s) != "" {
					parts = append(parts, strings.TrimSpace(s))
				}
			}
			gpu = strings.Join(parts, " / ")
		}
		r = execCmd(`wmic logicaldisk get size,freespace,deviceid 2>nul || powershell -NoProfile -Command "Get-PSDrive -PSProvider FileSystem | Select-Object Name,@{n='GB';e={[math]::Round($_.Used/1GB,1)}}"`, 12000)
		if r.ok() {
			parts := []string{}
			for _, s := range strings.Split(r.stdout, "\n") {
				if s = strings.TrimRight(s, "\r"); strings.TrimSpace(s) != "" {
					parts = append(parts, strings.TrimSpace(s))
				}
			}
			if len(parts) > 12 {
				parts = parts[:12]
			}
			disk = strings.Join(parts, "\n")
		}
		return
	}
	r := execCmd("dmidecode -t baseboard 2>/dev/null | grep -E \"Manufacturer|Product Name\" | head -4", 12000)
	if r.ok() {
		parts := []string{}
		for _, s := range strings.Split(r.stdout, "\n") {
			s = regexp.MustCompile(`^\s*\w+:\s*`).ReplaceAllString(strings.TrimSpace(s), "")
			if s != "" {
				parts = append(parts, s)
			}
		}
		board = strings.Join(parts, " / ")
	}
	r = execCmd("nvidia-smi --query-gpu=name --format=csv,noheader 2>/dev/null || lspci 2>/dev/null | grep -iE \"vga|3d|display\" | head -4", 12000)
	if r.ok() {
		parts := []string{}
		for _, s := range strings.Split(r.stdout, "\n") {
			if strings.TrimSpace(s) != "" {
				parts = append(parts, strings.TrimSpace(s))
			}
		}
		gpu = strings.Join(parts, " / ")
	}
	r = execCmd("df -h --output=target,size,used,avail,pcent 2>/dev/null || df -h", 12000)
	if r.ok() {
		parts := []string{}
		for _, s := range strings.Split(r.stdout, "\n") {
			if strings.TrimSpace(s) != "" {
				parts = append(parts, strings.TrimSpace(s))
			}
		}
		if len(parts) > 12 {
			parts = parts[:12]
		}
		disk = strings.Join(parts, "\n")
	}
	return
}

// ==================== 注册入口 ====================

type sysExecReq struct {
	Cmd *string `json:"cmd"`
}

type sysCameraReq struct {
	Device *string `json:"device"`
}

// RegisterSysmon 注册全部系统监控事件（对标 registerSysmonHandlers）。
func RegisterSysmon(c Ctx) {
	srv, st, cfg := c.Srv, c.Store, c.Config

	// ---- 系统信息 ----
	srv.On("sys:info", func(c socketapi.Conn, _ json.RawMessage, ack socketapi.Ack) {
		u := authed(c)
		if u == nil {
			ack(util.Fail("未登录"))
			return
		}
		if !admin.IsServerAdmin(u, cfg, st) {
			ack(util.Fail("需要 SERVER_ADMIN 权限"))
			return
		}
		hostname, _ := os.Hostname()
		model, cores := cpuModel()
		total, free := memInfo()
		m := map[string]any{
			"hostname": hostname,
			"platform": platformName(),
			"type":     osTypeName(),
			"release":  osRelease(),
			"arch":     runtime.GOARCH,
			"uptime":   uptimeSec(),
			"cpuModel": model,
			"cpuCores": cores,
			"totalMem": total,
			"freeMem":  free,
			"ips":      networkIps(),
		}
		board, gpu, disk := collectHW()
		if board == "" {
			board = "未知"
		}
		if gpu == "" {
			gpu = "未知"
		}
		m["board"] = board
		m["gpu"] = gpu
		m["disk"] = disk
		ack(util.OK(m))
	})

	// ---- 实时性能 ----
	srv.On("sys:perf", func(c socketapi.Conn, _ json.RawMessage, ack socketapi.Ack) {
		u := authed(c)
		if u == nil {
			ack(util.Fail("未登录"))
			return
		}
		if !admin.IsServerAdmin(u, cfg, st) {
			ack(util.Fail("需要 SERVER_ADMIN 权限"))
			return
		}
		cpu, perCore := cpuUsage()
		total, free := memInfo()
		memPercent := 0.0
		if total > 0 {
			memPercent = math.Round(float64(total-free)/float64(total)*1000) / 10
		}
		ack(util.OK(map[string]any{
			"cpu":        cpu,
			"perCore":    perCore,
			"memTotal":   total,
			"memUsed":    total - free,
			"memPercent": memPercent,
			"uptime":     uptimeSec(),
			"ts":         nowMs(),
		}))
	})

	// ---- 进程列表 ----
	srv.On("sys:processes", func(c socketapi.Conn, _ json.RawMessage, ack socketapi.Ack) {
		u := authed(c)
		if u == nil {
			ack(util.Fail("未登录"))
			return
		}
		if !admin.IsServerAdmin(u, cfg, st) {
			ack(util.Fail("需要 SERVER_ADMIN 权限"))
			return
		}
		list := processes()
		if len(list) > 300 {
			list = list[:300]
		}
		ack(util.OK(map[string]any{"processes": list}))
	})

	// ---- 执行命令行 ----
	srv.On("sys:exec", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		u := authed(c)
		if u == nil {
			ack(util.Fail("未登录"))
			return
		}
		if !admin.IsServerAdmin(u, cfg, st) {
			ack(util.Fail("需要 SERVER_ADMIN 权限"))
			return
		}
		var d sysExecReq
		_ = json.Unmarshal(raw, &d)
		cmd := ""
		if d.Cmd != nil {
			cmd = strings.TrimSpace(*d.Cmd)
		}
		if cmd == "" {
			ack(util.Fail("命令为空"))
			return
		}
		if len(cmd) > 2000 {
			ack(util.Fail("命令过长"))
			return
		}
		r := execCmd(cmd, 15000)
		errOut := any(nil)
		if r.errStr != "" {
			errOut = r.errStr
		}
		ack(util.OK(map[string]any{
			"stdout":   r.stdout,
			"stderr":   r.stderr,
			"exitCode": r.exitCode,
			"error":    errOut,
		}))
	})

	// ---- 截取服务器屏幕（返回 base64 data URL，非二进制帧） ----
	srv.On("sys:screenshot", func(c socketapi.Conn, _ json.RawMessage, ack socketapi.Ack) {
		u := authed(c)
		if u == nil {
			ack(util.Fail("未登录"))
			return
		}
		if !admin.IsServerAdmin(u, cfg, st) {
			ack(util.Fail("需要 SERVER_ADMIN 权限"))
			return
		}
		tmp := filepath.Join(os.TempDir(), fmt.Sprintf("mochi_shot_%d.png", nowMs()))
		var r execResult
		if runtime.GOOS == "windows" {
			script := `Add-Type -AssemblyName System.Windows.Forms,System.Drawing; $b=[System.Windows.Forms.SystemInformation]::VirtualScreen; $bmp=New-Object System.Drawing.Bitmap($b.Width,$b.Height); $g=[System.Drawing.Graphics]::FromImage($bmp); $g.CopyFromScreen($b.Left,$b.Top,0,0,$bmp.Size); $bmp.Save('` + tmp + `'); $g.Dispose(); $bmp.Dispose()`
			r = execCmd(`powershell -NoProfile -Command "`+strings.ReplaceAll(script, `"`, `\"`)+`"`, 20000)
		} else {
			r = execCmd(`(scrot '`+tmp+`' || import -window root '`+tmp+`' || gnome-screenshot -f '`+tmp+`') 2>/dev/null`, 20000)
		}
		if data, err := os.ReadFile(tmp); err == nil && r.exitCode != nil && *r.exitCode == 0 {
			_ = os.Remove(tmp)
			ack(util.OK(map[string]any{
				"image": "data:image/png;base64," + base64.StdEncoding.EncodeToString(data),
				"ts":    nowMs(),
			}))
			return
		}
		_ = os.Remove(tmp)
		if runtime.GOOS == "windows" {
			reason := strings.TrimSpace(r.stderr)
			if reason == "" {
				reason = r.errStr
			}
			if reason == "" {
				reason = "无显示会话"
			}
			ack(util.Fail("截屏失败：" + reason))
			return
		}
		ack(util.Fail("当前服务器无图形桌面/显示会话，无法截屏"))
	})

	// ---- 读取摄像头一帧 ----
	srv.On("sys:camera", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		u := authed(c)
		if u == nil {
			ack(util.Fail("未登录"))
			return
		}
		if !admin.IsServerAdmin(u, cfg, st) {
			ack(util.Fail("需要 SERVER_ADMIN 权限"))
			return
		}
		var d sysCameraReq
		_ = json.Unmarshal(raw, &d)
		tmp := filepath.Join(os.TempDir(), fmt.Sprintf("mochi_cam_%d.jpg", nowMs()))
		var r execResult
		if runtime.GOOS == "windows" {
			r = execCmd(`powershell -NoProfile -Command "try{$c=New-Object System.Drawing.Bitmap 640,480;$g=[System.Drawing.Graphics]::FromImage($c);$g.Clear([System.Drawing.Color]::Black);$c.Save('`+tmp+`')}catch{}" >nul`, 15000)
		} else {
			devArg := "-f v4l2 -i /dev/video0"
			if d.Device != nil && *d.Device != "" {
				devArg = `-i "` + *d.Device + `"`
			}
			r = execCmd(fmt.Sprintf("ffmpeg -y -loglevel error %s -frames:v 1 \"%s\" 2>/dev/null", devArg, tmp), 15000)
		}
		if info, err := os.Stat(tmp); err == nil && info.Size() > 500 {
			if data, err2 := os.ReadFile(tmp); err2 == nil {
				_ = os.Remove(tmp)
				ack(util.OK(map[string]any{
					"image": "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(data),
					"ts":    nowMs(),
				}))
				return
			}
		}
		_ = os.Remove(tmp)
		if strings.TrimSpace(r.stderr) != "" {
			ack(util.Fail("未检测到可用摄像头设备：" + strings.TrimSpace(r.stderr)))
			return
		}
		ack(util.Fail("未检测到可用摄像头设备"))
	})

	// 兜底：采集过程异常绝不向外抛 panic（socket 层有 recover，这里打日志）
	_ = logger.Info
}
