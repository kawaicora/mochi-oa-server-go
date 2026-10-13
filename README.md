# mochi-oa-server-go

麻薯 OA 服务端的 Go 1:1 复刻版（源仓库为 `mochi-oa-server` 的 Node.js/TypeScript 实现）。
Socket.IO 实时通信 + REST HTTP 路由 + MySQL 持久化（GORM）。

## 功能

- Socket.IO 实时服务（默认路径 `/socket.io`）：认证握手、组织/群聊/消息/任务/PM/RTC/远程设备/邮件假期 等事件
- HTTP 路由：文件上传（单传/分片/列目录/裸下载）、重置密码页、管理后台静态页、爱发电 webhook/查单
- 管理后台：`/view/admin`（含 users/companies/sysmon/room/terminal/remote-window 子页）

## 快速运行

### 本地运行

```powershell
Copy-Item .env.example .env   # 改 MYSQL_* / JWT_SECRET / ADMIN_*
go build -o bin\mochioa-server.exe .\cmd\server
.\bin\mochioa-server.exe
```

无需事先建库建表：默认 `AUTO_CREATE_DATABASE` / `AUTO_MIGRATE` 均为 `true`，服务端启动时会自己
`CREATE DATABASE IF NOT EXISTS` 并按 GORM 模型 `AutoMigrate` 建全部表（库名取 `MYSQL_DATABASE`）。
只要 MySQL 账号有 `CREATE` 权限即可；库表已存在时重复启动是幂等的，不会改数据。

### Docker

```powershell
Copy-Item .env.example .env   # 至少改 MYSQL_ROOT_PASSWORD / JWT_SECRET
docker compose up -d --build
```

- 服务端口：**3001**（`${SERVER_PORT:-3001}:3001`）
- 上传文件持久化：`./data:/app/data`

## 环境变量

见 `.env.example`（与源仓库 `.env` 逐项对齐）。关键项：

| 变量 | 默认 | 说明 |
|---|---|---|
| `HOST` / `PORT` | `0.0.0.0` / `3001` | 监听地址 |
| `STORAGE` | `mysql` | 仅 `mysql`（内存模式已移除，保留仅为兼容旧 .env） |
| `MYSQL_HOST/PORT/USER/PASSWORD/DATABASE` | — | MySQL 连接 |
| `AUTO_CREATE_DATABASE` | `true` | 库不存在时自动 `CREATE DATABASE IF NOT EXISTS` |
| `AUTO_MIGRATE` | `true` | 启动时按 GORM 模型自动建表/补列（不删列、不改数据） |
| `JWT_SECRET` | — | 会话签名密钥（必改） |
| `ADMIN_USER` / `ADMIN_PASSWORD` | 空 | 首次启动播种初始管理员 |
| `SERVER_ADMIN_USERS` | 空 | SERVER_ADMIN 用户名列表（逗号分隔） |
| `SSL_KEY` / `SSL_CERT` | 空 | 两者同时配置才启用 HTTPS |
| `CORS_ORIGIN` | `*` | 跨域来源（`*` 或逗号列表） |
| `FILE_DIR` / `FILE_MAX_BYTES` | `./data` / 20MB | 上传存储目录与大小上限 |
| `USE_RELATIVE_FILE_URL` | `true` | 文件 URL 相对/绝对开关 |
| `ICE_SERVERS` / `ICE_USE_CLOUDFLARE` | STUN 谷歌 / `false` | WebRTC |
| `AFDIAN_USER_ID` / `AFDIAN_TOKEN` | 空 | 爱发电（`AFDIAN_USER_ID` 存在即启用） |


## 目录结构

- `cmd/server`：入口，全部组件接线
- `internal/socket`：Socket.IO 引擎
- `internal/handlers/*`：socket 事件 handler（auth/org/conversation/task/pm/rtcdev/mailholiday）
- `internal/http`：HTTP 路由模块（files/resetpwd/adminweb/afdian）
- `internal/presence`：在线状态注册表 + 群广播
- `web/`：go:embed 内嵌的管理后台静态资源与模板（逐字复制自源仓库）
