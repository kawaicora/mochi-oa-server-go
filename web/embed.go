// Package web 内嵌管理后台全部静态资源与页面模板（go:embed，编译期打包进二进制）。
//
// 内容与源仓库 server/src/static/**、server/src/templates/** 逐字节一致（verbatim 复制，未改一行），
// 另附 socket.io 官方客户端脚本 socket.io.min.js（源 node_modules 内产物），
// 用于 /view/admin/socket.io.js 路由（浏览器内 io() 全局对象）。
package web

import "embed"

// Assets 内嵌目录：static/（js/css）+ templates/（管理页 HTML）。
//
//go:embed static templates
var Assets embed.FS

// SocketIOClientJS 是 socket.io 官方浏览器端脚本（/view/admin/socket.io.js 下发）。
//
//go:embed socket.io.min.js
var SocketIOClientJS []byte
