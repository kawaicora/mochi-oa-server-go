# 出网代理（无外网员工代理上网）

给内网无外网的员工提供一条受控出网通道：服务端同时跑一个 **HTTP 代理**（端口 `PROXY_HTTP_PORT`，默认 1081）和一个 **SOCKS5 代理**（端口 `PROXY_SOCKS5_PORT`，默认 1082）。账号由公司管理员在 OA 里动态生成，带有效期，到期自动失效。

## 启用

默认**不启用**（`PROXY_ENABLED=false`），不影响现有服务。在 `.env` 打开：

```env
PROXY_ENABLED=true
PROXY_HTTP_PORT=1081
PROXY_SOCKS5_PORT=1082
```

启动日志出现 `出网代理已启用 httpPort=1081 socks5Port=1082` 即生效。任一监听失败只打日志、主服务照常启动（降级）。表 `proxy_accounts` / `proxy_access_requests` 由 AutoMigrate 自动建。

## 协议

- **HTTP 代理**：普通 GET/POST 走 absolute-form 转发；HTTPS 走 `CONNECT` 隧道（hijack 后双向 `io.Copy`）。
- **SOCKS5**：RFC 1928 握手 → RFC 1929 用户名/密码认证 → `CONNECT`（0x01）拨号后双向转发；`UDP ASSOCIATE`(0x03)/`BIND`(0x02) 不支持，返回 0x07。
- **鉴权（HTTP 与 SOCKS5 共用）**：HTTP 用 `Proxy-Authorization: Basic username:secret`；SOCKS5 用 RFC1929。校验 = 查表 username 对应账号 `status=active` 且 `now <= expires_at`。任何失败都打日志（client addr / username / 原因）。
- **到期失效**：每次连接即校验有效期；另有每小时 sweep 把已过期 active 账号批量置 disabled 并打日志数量。

## 客户端用法

```bash
# HTTP 代理（HTTPS 走 CONNECT）
curl --proxy http://用户名:密钥@127.0.0.1:1081 https://example.com
# SOCKS5
curl --socks5 用户名:密钥@127.0.0.1:1082 https://example.com
```

## socket 事件（公司隔离，requireMember；管理事件需 admin/owner）

账号管理：`proxy:account:create / list / revoke / delete`。
上网申请审批：`proxy:request:create / list / get / approve / reject / renew`。
`secret` 仅在创建/批准/本人 get(approved) 时一次性返回，列表视图永不带出。
审批人/抄送人由申请人发起上网申请时动态选择（approverIds/ccIds，须为本公司成员）；不传则由 admin/owner 兜底审批。

## 代理实现规格（2026-10-18 用户明确拍板；今晚只记录不实现，明日按此实施）

1. 方案定为【TUN 全局接管】：采用 Go 内核 sing-box / tun2socks（跨三端、单一二进制、TCP+UDP 全接管），不自造 TUN。
2. 工作链路：系统所有流量→虚拟网卡(TUN)→sing-box 内核→SOCKS5/HTTP 出站(服务端 8080/1080)→服务器出口。UDP 必须全转发（游戏公司 Unity 许可走 UDP/不走代理，需全局接管）。
3. 平台：Windows 用 Wintun（管理员提权）、macOS 用 utun（root/授权）、Linux 用 tun（root/CAP_NET_ADMIN）。
4. 客户端（Electron 主进程）：spawn sing-box 子进程 + JSON 配置（TUN 设备+路由+出站=服务端代理），启停由客户端控制；代理地址从配置/env 取，不写死 127.0.0.1。
5. 开启→启动 TUN 全局接管；到期/关闭→停 TUN 恢复直连（配合到期自动关闭+启动检查）。
6. 服务端：代理集成在后端（服务端）上，代理地址在环境变量/配置中可修改；默认端口 HTTP/HTTPS=8080、SOCKS5=1080（原 1081/1082 改为可配置，默认按新值对齐）；代理账号/端口等都走配置。
7. 客户端逻辑：应直接设置系统代理——HTTP/HTTPS 代理直接设置系统代理(host:8080)；SOCKS5 需 TUN（系统代理/浏览器对 SOCKS 认证支持差是 ERR_PROXY_AUTH_UNSUPPORTED 根源）。