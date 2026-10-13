// Package socketapi 定义与 socket.io v4 兼容的服务端抽象接口（纯接口层，不包含协议实现）。
// 事件处理器（internal/handlers）只依赖本包，不依赖具体引擎实现（internal/socket）。
package socketapi

import "encoding/json"

// Ack 事件的可选应答回调（对标 socket.io 的 ack callback）。
// 客户端 emit 时传了回调，服务端调用 ack(result) 将结果编码为 ACK 报文回传；客户端未传回调时 ack 为 no-op。
type Ack func(result any)

// HandlerFunc 普通事件处理器：data 为事件负载原始 JSON 字节（可能为 null，此时为空切片）。
type HandlerFunc func(c Conn, data json.RawMessage, ack Ack)

// BinaryHandlerFunc 二进制事件处理器：data 为原始二进制（如远程控制的屏幕帧）。
type BinaryHandlerFunc func(c Conn, data []byte)

// MiddlewareFunc 连接级中间件（对标 io.use）。握手认证后、connection 事件前执行；调用 next() 放行。
type MiddlewareFunc func(c Conn, next func() error) error

// Conn 表示一个已连接的客户端（对标 socket.io 的 Socket）。
type Conn interface {
	ID() string                          // 连接 id
	Emit(event string, data any) error   // 向该连接发 JSON 事件
	EmitBinary(event string, data []byte) error // 向该连接发二进制事件
	Join(room string) error
	Leave(room string) error
	Disconnect(reason string)
	Set(key string, val any)             // 连接级状态（对标 socket.data，认证用户存 key="auth"）
	Get(key string) any
	Rooms() []string
}

// Server 抽象 Socket.IO 服务端（默认命名空间 "/"）。
type Server interface {
	Use(mw MiddlewareFunc)
	On(event string, h HandlerFunc)
	OnBinary(event string, h BinaryHandlerFunc)
	OnConnection(h func(c Conn) error)        // 认证放行后调用（对标 io.on('connection')）
	OnDisconnect(h func(c Conn, reason string))
	EmitTo(room, event string, data any) error        // io.to(room).emit
	EmitToSkip(room, event string, data any, skipConnID string) error // socket.to(room).emit（排除自己）
	Broadcast(event string, data any) error           // io.emit
	Sockets() []Conn
	Close() error
}
