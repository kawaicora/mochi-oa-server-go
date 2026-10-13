// Package packet 封装 Engine.IO v4 (EIO4) 与 Socket.IO v4 的报文编解码。
//
// 设计依据（不写猜测，全部对齐官方协议）：
//   - Engine.IO protocol v4（socket.io 3.x/4.x 底层，对应 engine.io ~6.6.0）：
//       packet 类型  OPEN=0 CLOSE=1 PING=2 PONG=3 MESSAGE=4 UPGRADE=5 NOOP=6
//       心跳方向在 v4 反转：由【服务端】按 pingInterval 主动发 PING，客户端回 PONG；
//       服务端在 pingTimeout 内未收到 PONG 即判死链。（v3 及以前是客户端发 PING，v4 反转。）
//   - Socket.IO protocol v4（默认命名空间 "/"）：
//       packet 类型  CONNECT=0 DISCONNECT=1 EVENT=2 ACK=3 CONNECT_ERROR=4
//                    BINARY_EVENT=5 BINARY_ACK=6
//
// 一个 socket.io 报文整体作为一个 EIO MESSAGE(=4) 报文的 payload 传输。
// 即：在线路上，socket.io 层报文是 `4` + socket.io 报文本体。
package socket

import (
	"encoding/json"
	"strconv"
	"strings"
)

// ─── Engine.IO v4 packet 类型 ───────────────────────────────────────────────

const (
	eioOPEN   = '0' // 握手响应：sid/upgrades/pingInterval/pingTimeout/maxPayload
	eioCLOSE  = '1' // 关闭传输
	eioPING   = '2' // 服务端心跳探测（v4：服务端主动发）
	eioPONG   = '3' // 客户端对 PING 的应答
	eioMESSAGE = '4' // 承载 socket.io 层报文（或二进制附件）
	eioUPGRADE = '5' // 升级确认（polling→websocket）
	eioNOOP   = '6' // 空操作：用于强制结束一次长轮询 GET
)

// ─── Socket.IO v4 packet 类型 ────────────────────────────────────────────────

const (
	sioCONNECT       = 0 // 客户端→服务端：建立命名空间连接，body 为 auth JSON
	sioDISCONNECT    = 1 // 断开命名空间连接
	sioEVENT         = 2 // 事件（可带 ack id）
	sioACK           = 3 // 对 EVENT 的应答（带 ack id）
	sioCONNECT_ERROR = 4 // 连接被拒（中间件 next 返回错误）
	sioBinaryEvent   = 5 // 带二进制附件的事件
	sioBinaryAck     = 6 // 带二进制附件的 ACK
)

// sioPacket 是解析后的 socket.io 层报文。
type sioPacket struct {
	typ      int             // sio* 常量
	ns       string          // 命名空间（默认 "/"）
	ackID    int             // ack id（hasAck=true 时有效）
	hasAck   bool            // 是否携带 ack id
	attach   int             // BINARY_EVENT/BINARY_ACK 的附件数（n 前缀）
	event    string          // EVENT 解析出的事件名
	args     json.RawMessage // EVENT 的 JSON 数组体（含事件名首元素），或 ACK 的 JSON 数组体
}

// parseSIOPacket 解析 socket.io 层报文（已剥掉 EIO MESSAGE 的 '4' 前缀）。
//
// 报文文法（v4）：
//
//	<type:1 digit>[</namespace>,][<ackID:digits>]<json-body>
//
// 举例（默认命名空间 "/"）：
//
//	"0{\"token\":\"abc\"}"            → CONNECT，body 为 auth JSON
//	"2[\"chat:message\",{...}]"       → EVENT 无 ack
//	"20[\"auth:login\",{...}]"        → EVENT 带 ack id=0
//	"30[{\"ok\":true}]"               → ACK id=0，body=[result]
//	"51-[\"screen\",{\"_placeholder\":true,\"num\":0}]" → BINARY_EVENT，1 个附件
//
// 为什么这样写：事件名在 JSON 数组首元素，解析时把整段 JSON body 交给 json.Unmarshal
// 成 []json.RawMessage，首元素即事件名，其余为参数——避免手工切逗号。
func parseSIOPacket(raw []byte) (sioPacket, error) {
	var p sioPacket
	if len(raw) == 0 {
		return p, errMalformed("empty sio packet")
	}
	typ := int(raw[0] - '0')
	if typ < 0 || typ > 6 {
		return p, errMalformed("bad sio type " + string(raw[0]))
	}
	p.typ = typ
	i := 1

	// 命名空间：紧跟 type 后的 '/' 起到 ',' 止；缺省即 "/"。
	if i < len(raw) && raw[i] == '/' {
		rest := raw[i:]
		j := strings.IndexByte(string(rest), ',')
		if j < 0 {
			return p, errMalformed("missing ns comma")
		}
		p.ns = string(rest[:j])
		i += j + 1
	} else {
		p.ns = "/"
	}

	// BINARY_EVENT/BINARY_ACK 的附件数前缀：形如 "52-..."，即 type 后紧跟 <n>-。
	// 此时 attach=n，body 起点在 '-' 之后。
	if typ == sioBinaryEvent || typ == sioBinaryAck {
		start := i
		for i < len(raw) && raw[i] >= '0' && raw[i] <= '9' {
			i++
		}
		if i >= len(raw) || raw[i] != '-' || i == start {
			return p, errMalformed("bad binary attach prefix")
		}
		n, err := strconv.Atoi(string(raw[start:i]))
		if err != nil {
			return p, errMalformed("bad attach count")
		}
		p.attach = n
		i++ // 跳过 '-'
		// 二进制报文同样可能带 namespace 逗号（此处默认 ns 已处理）。
	}

	// ack id：EVENT/ACK/BINARY_EVENT/BINARY_ACK 可携带，紧跟在 type/ns 后、JSON body 前。
	if typ == sioEVENT || typ == sioACK || typ == sioBinaryEvent || typ == sioBinaryAck {
		start := i
		for i < len(raw) && raw[i] >= '0' && raw[i] <= '9' {
			i++
		}
		if i > start {
			id, err := strconv.Atoi(string(raw[start:i]))
			if err == nil {
				p.ackID = id
				p.hasAck = true
			}
		}
	}

	p.args = raw[i:]
	return p, nil
}

// eventNameAndArgs 从 EVENT 的 JSON body（形如 ["event",arg1,arg2]）取出事件名与参数数组。
// 返回的 args 是去掉首元素后的 JSON 数组（如 "[arg1,arg2]"）；空 body 返回 nil。
func (p sioPacket) eventNameAndArgs() (string, json.RawMessage, error) {
	if len(p.args) == 0 || string(p.args) == "null" {
		return "", nil, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(p.args, &arr); err != nil {
		return "", nil, errMalformed("event body not array: " + err.Error())
	}
	if len(arr) == 0 {
		return "", nil, nil
	}
	var name string
	if err := json.Unmarshal(arr[0], &name); err != nil {
		return "", nil, errMalformed("event name not string: " + err.Error())
	}
	if len(arr) == 1 {
		return name, nil, nil
	}
	// 其余元素重新打包成 JSON 数组，便于 HandlerFunc 整体 Unmarshal。
	rest, _ := json.Marshal(arr[1:])
	return name, rest, nil
}

// ackResultArgs 从 ACK 的 JSON body（形如 [result]）取出第一个 result。
func (p sioPacket) ackResultArgs() (json.RawMessage, error) {
	var arr []json.RawMessage
	if err := json.Unmarshal(p.args, &arr); err != nil {
		return nil, err
	}
	if len(arr) == 0 {
		return json.RawMessage("null"), nil
	}
	return arr[0], nil
}

// ─── 编码：服务端→客户端 ────────────────────────────────────────────────────

// encodeSIOEvent 编码一个服务端下发的事件（默认命名空间、无 ack id）。
// 输出：2["event",data]（外层再包 EIO MESSAGE '4'）。
func encodeSIOEvent(event string, data any) ([]byte, error) {
	body, err := json.Marshal([]any{event, data})
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 1+len(body))
	out = append(out, '2')
	return append(out, body...), nil
}

// encodeSIOAck 编码对客户端 EVENT 的应答（ACK）。
// 输出：3<ackID>[result]。
func encodeSIOAck(ackID int, result any) ([]byte, error) {
	body, err := json.Marshal([]any{result})
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 1+len(strconv.Itoa(ackID))+len(body))
	out = append(out, '3')
	out = append(out, strconv.Itoa(ackID)...)
	return append(out, body...), nil
}

// encodeSIOConnect 编码 CONNECT 成功响应：0{"sid":"<socket id>"}。
//
// 为什么必须带 sid：socket.io v4 规定服务端 CONNECT 响应 body 为 {"sid": "..."}，
// 客户端据此拿到 socket.id。只回裸 "0" 会被 v4 客户端误判为 v2 协议不兼容。
func encodeSIOConnect(sid string) []byte {
	b, _ := json.Marshal(map[string]string{"sid": sid})
	out := make([]byte, 0, 1+len(b))
	out = append(out, '0')
	return append(out, b...)
}

// encodeSIOConnectError 编码 CONNECT_ERROR：4{"message":"..."}。
func encodeSIOConnectError(msg string) []byte {
	b, _ := json.Marshal(map[string]string{"message": msg})
	out := make([]byte, 0, 1+len(b))
	out = append(out, '4')
	return append(out, b...)
}

// encodeEIOOPEN 编码握手 OPEN 报文：0{"sid":...,"upgrades":[...],"pingInterval":...,"pingTimeout":...,"maxPayload":...}。
//
// 为什么带 maxPayload：EIO4 在 OPEN 中新增该字段，告知客户端单次报文上限；
// 缺失一般也能连，但带上才是规范握手。
func encodeEIOOPEN(sid string, upgrades []string, pingInterval, pingTimeout, maxPayload int) []byte {
	m := map[string]any{
		"sid":          sid,
		"upgrades":     upgrades,
		"pingInterval": pingInterval,
		"pingTimeout":  pingTimeout,
		"maxPayload":   maxPayload,
	}
	b, _ := json.Marshal(m)
	out := make([]byte, 0, 1+len(b))
	out = append(out, eioOPEN)
	return append(out, b...)
}

// errMalformed 返回一个带上下文的协议错误。
func errMalformed(msg string) error { return &protoError{msg: msg} }

type protoError struct{ msg string }

func (e *protoError) Error() string { return "sio protocol: " + e.msg }
