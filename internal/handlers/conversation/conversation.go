// Package conversation 是会话/消息相关的 socket 事件 handler，1:1 复刻自
// server/src/handlers/conversation.ts。
//
// 覆盖事件：
//   - chat:send / chat:history        群聊发消息 / 群聊历史
//   - dm:send / dm:history           一对一私信发送 / 私信历史
//   - conversation:list              会话列表（群+私信统一，含 readReceipts）
//   - conversation:pin               置顶/取消置顶
//   - conversation:read              已读回执（MarkRead + 广播 chat:readReceipt）
//   - message:delete                软删除（本人可删自己；公司管理员可删群内任意）
//   - message:hardDelete             硬删除（仅公司管理员；私信仅发送者本人）
//
// 广播事件（服务端→客户端）：
//   - chat:message                   群聊新消息（群房间除发送者 + 发送者 userRoom）
//   - dm:message                      私信新消息（双方 userRoom）
//   - message:deleted                 消息删除通知
//   - chat:readReceipt                已读回执
//   - conversations:updated            会话变更触发客户端重拉列表
package conversation

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"mochi-oa-server-go/internal/config"
	"mochi-oa-server-go/internal/db"
	"mochi-oa-server-go/internal/logger"
	"mochi-oa-server-go/internal/socketapi"
	"mochi-oa-server-go/internal/types"
	"mochi-oa-server-go/internal/util"
)

// module 是日志模块前缀。
const module = "handlers/conversation"

// Ctx 是 handler 注册所需的依赖集合（对标 TS 的 Ctx：io/store/config）。
type Ctx struct {
	Srv    socketapi.Server
	Store  db.Store
	Config *config.ServerConfig
}

// ---- 请求体结构（对应 TS 各 handler 的 data 解构） ----

type chatSendReq struct {
	GroupID *int64  `json:"groupId"`
	Kind    *string `json:"kind"`
	Content *string `json:"content"`
	Text    *string `json:"text"`
}

type chatHistoryReq struct {
	GroupID  *int64 `json:"groupId"`
	BeforeTs *int64 `json:"beforeTs"`
	Limit    *int64 `json:"limit"`
}

type dmSendReq struct {
	ToUserID *int64  `json:"toUserId"`
	Kind     *string `json:"kind"`
	Content  *string `json:"content"`
	Text     *string `json:"text"`
}

type dmHistoryReq struct {
	WithUserID *int64 `json:"withUserId"`
	BeforeTs   *int64 `json:"beforeTs"`
	Limit      *int64 `json:"limit"`
}

type conversationListReq struct {
	Type *string `json:"type"`
}

type conversationPinReq struct {
	ConversationID *int64 `json:"conversationId"`
	Pinned         *bool  `json:"pinned"`
}

type conversationReadReq struct {
	ConversationID *int64 `json:"conversationId"`
	LastMessageID  *int64 `json:"lastMessageId"`
}

type messageDeleteReq struct {
	ConversationID *int64  `json:"conversationId"`
	MessageID      *string `json:"messageId"`
	Content        *string `json:"content"`
}

type messageHardDeleteReq struct {
	ConversationID *int64  `json:"conversationId"`
	MessageID      *string `json:"messageId"`
}

// convListItem 是 conversation:list 的返回条目：在 ConversationItem 基础上
// 附加 name（群名/对方昵称）和 avatar（对方头像），与 TS 的 {...it, name, avatar} 一致。
type convListItem struct {
	types.ConversationItem
	Name   string `json:"name"`
	Avatar string `json:"avatar,omitempty"`
}

// readReceiptBroadcast 是 chat:readReceipt 广播的负载（含 conversationId）。
type readReceiptBroadcast struct {
	ConversationID    int64  `json:"conversationId"`
	UserID            int64  `json:"userId"`
	LastReadMessageID *int64 `json:"lastReadMessageId"`
}

// ---- 正则（逐字复刻 TS） ----

var (
	// isUuidRe 匹配 36 位 UUID（含连字符）。
	isUuidRe = regexp.MustCompile(`^[0-9a-fA-F-]{36}$`)
	// audioExtRe 匹配音频文件扩展名（大小写不敏感，可带 query）。
	audioExtRe = regexp.MustCompile(`(?i)\.(mp3|wav|flac|aac|ogg|oga|opus|weba|m4a|m4b|wma|ac3|aiff|aif|au|amr|alac|mka|mid|midi|cda|cue)(\?|$)`)
	// httpRe 匹配 http/https URL。
	httpRe = regexp.MustCompile(`(?i)^https?://`)
	// fileProtoRe 匹配 file:// 协议。
	fileProtoRe = regexp.MustCompile(`(?i)^file://`)
	// winPathRe 匹配 Windows 盘符路径（如 C:/ 或 C:\）。
	winPathRe = regexp.MustCompile(`^[a-zA-Z]:[\\/]`)
	// unixAbsRe 匹配 Unix 绝对路径（以 / 开头但不是 //）。
	unixAbsRe = regexp.MustCompile(`^/[^/]`)
	// folderSegRe 匹配文件夹路径单段：字母数字下划线+CJK+连字符+空白+.()（）。
	folderSegRe = regexp.MustCompile(`^[0-9A-Za-z_\x{4e00}-\x{9fa5}\-\s.()（）]+$`)
)

// authed 从连接取出认证用户（对标 socket.data.auth）。未登录返回 nil。
func authed(conn socketapi.Conn) *types.Auth {
	v := conn.Get("auth")
	if v == nil {
		return nil
	}
	a, _ := v.(*types.Auth)
	return a
}

// pubUrl 把相对文件路径拼成绝对 URL（复刻 files.ts:pubUrl）。
// useRelativeUrl=true 时原样返回；已是 http(s):// 原样返回；否则用 publicBase 拼接。
func pubUrl(cfg *config.ServerConfig, raw string) string {
	if cfg.File.UseRelativeURL {
		return raw
	}
	if raw == "" || httpRe.MatchString(raw) {
		return raw
	}
	base := strings.TrimRight(cfg.File.PublicBase, "/")
	if strings.HasPrefix(raw, "/") {
		return base + raw
	}
	return base + "/" + raw
}

// decorateMsg 对外返回消息：useRelativeUrl=false 时把文件类 content 与头像拼成绝对 URL。
// 默认相对路径，客户端自行拼 host。
func decorateMsg(cfg *config.ServerConfig, msg *types.ChatMessage) {
	if cfg.File.UseRelativeURL {
		return
	}
	// proxy_card / expense_card / advance_card / attendance_card 等卡片类消息 content 为业务 JSON，原样透传，不做文件 URL 改写。
	if msg.Kind != "text" && msg.Kind != "folder" && msg.Kind != "proxy_card" && msg.Kind != "expense_card" && msg.Kind != "advance_card" && msg.Kind != "attendance_card" && msg.Content != "" {
		msg.Content = pubUrl(cfg, msg.Content)
	}
	if msg.Avatar != "" {
		msg.Avatar = pubUrl(cfg, msg.Avatar)
	}
}

// isText 校验文本内容：非空、trim 后长度>0、总长≤4000。
func isText(v string) bool {
	return len(strings.TrimSpace(v)) > 0 && len(v) <= 4000
}

// isKind 校验消息种类。
func isKind(v string) bool {
	switch v {
	case "text", "image", "file", "video", "audio", "folder", "proxy_card", "expense_card", "advance_card", "attendance_card":
		return true
	}
	return false
}

// isUuidStr 校验是否为 36 位 UUID。
func isUuidStr(v string) bool {
	return isUuidRe.MatchString(v)
}

// isFolderPath 校验文件夹相对路径（如 6/p20260921_merge 或 default/p20260921_merge）。
// 逐段清洗、拒绝穿越/根目录。
func isFolderPath(v string) bool {
	parts := strings.Split(v, "/")
	filtered := parts[:0]
	for _, p := range parts {
		if p != "" {
			filtered = append(filtered, p)
		}
	}
	if len(filtered) < 2 {
		return false
	}
	for _, s := range filtered {
		if s == "." || s == ".." || s == "~" {
			return false
		}
		if strings.Contains(s, "\\") {
			return false
		}
		if !folderSegRe.MatchString(s) {
			return false
		}
	}
	return true
}

// isAudioRef 判断文本内容是否为音频引用（服务器 URL / 本地路径 / file://）。
// 发送兜底：文本内容若是音频引用则记为 audio 类型。
func isAudioRef(v string) bool {
	if !audioExtRe.MatchString(v) {
		return false
	}
	if httpRe.MatchString(v) {
		return true
	}
	if fileProtoRe.MatchString(v) {
		return true
	}
	if winPathRe.MatchString(v) {
		return true
	}
	if unixAbsRe.MatchString(v) {
		return true
	}
	return false
}

// resolveContent 内容归一：
//   - text: trim 后直接存
//   - folder: 校验路径合法后直接存
//   - image/file/video/audio: 若是 UUID 则查文件表取 URL；否则原样存
//
// 返回 nil 表示无法解析（如文件不存在）。
func resolveContent(ctx context.Context, store db.Store, kind string, raw string) (*string, error) {
	if kind == "text" {
		s := strings.TrimSpace(raw)
		return &s, nil
	}
	if kind == "folder" {
		if !isFolderPath(raw) {
			return nil, nil
		}
		return &raw, nil
	}
	if isUuidStr(raw) {
		f, err := store.GetFileByUUID(ctx, raw)
		if err != nil {
			return nil, err
		}
		if f == nil {
			return nil, nil
		}
		return &f.URL, nil
	}
	return &raw, nil
}

// previewOf 对话列表预览文本：文本直接显示，图片/视频/文件给占位符。
func previewOf(kind string, content string) string {
	switch kind {
	case "text":
		return content
	case "image":
		return "[图片]"
	case "video":
		return "[视频]"
	case "audio":
		return "[音频]"
	case "folder":
		return "[文件夹]"
	case "proxy_card":
		return "[上网申请]"
	case "expense_card":
		return "[报销申请]"
	case "advance_card":
		return "[备用金申请]"
	case "attendance_card":
		return "[考勤申请]"
	default:
		return "[文件]"
	}
}

// fillNicks 批量填充消息的发送者昵称 + 头像（按 fromId 去重查库）。
func fillNicks(ctx context.Context, store db.Store, msgs []types.ChatMessage) error {
	idSet := map[int64]struct{}{}
	for i := range msgs {
		idSet[msgs[i].FromID] = struct{}{}
	}
	cache := map[int64]struct {
		nick   string
		avatar string
	}{}
	for id := range idSet {
		u, err := store.GetUserByID(ctx, id)
		if err != nil {
			return err
		}
		nick := ""
		avatar := ""
		if u != nil {
			nick = u.Nick
			if nick == "" {
				nick = u.Username
			}
			avatar = u.Avatar
		}
		cache[id] = struct {
			nick   string
			avatar string
		}{nick, avatar}
	}
	for i := range msgs {
		if c, ok := cache[msgs[i].FromID]; ok {
			msgs[i].Nick = c.nick
			msgs[i].Avatar = c.avatar
		}
	}
	return nil
}

// isCompanyAdmin 判断是否公司管理员（owner/admin）。
func isCompanyAdmin(ctx context.Context, store db.Store, companyID int64, userID int64) (bool, error) {
	role, err := store.GetMemberRole(ctx, companyID, userID)
	if err != nil {
		return false, err
	}
	return role == "owner" || role == "admin", nil
}

// broadcastDeleted 删除后通知同对话的在线客户端隐藏该消息。
// 群聊→群房间；私信→双方 userRoom。
func broadcastDeleted(srv socketapi.Server, conv *types.Conversation, conversationID int64, messageID string) {
	payload := map[string]any{"conversationId": conversationID, "messageId": messageID}
	if conv.Type == "group" && conv.GroupID != nil {
		_ = srv.EmitTo(util.GroupRoom(*conv.GroupID), "message:deleted", payload)
	} else {
		if conv.DmUserA != nil {
			_ = srv.EmitTo(util.UserRoom(*conv.DmUserA), "message:deleted", payload)
		}
		if conv.DmUserB != nil {
			_ = srv.EmitTo(util.UserRoom(*conv.DmUserB), "message:deleted", payload)
		}
	}
}

// Register 注册所有会话/消息相关的 socket 事件 handler。
func Register(c Ctx) {
	srv := c.Srv
	store := c.Store
	cfg := c.Config

	srv.OnConnection(func(conn socketapi.Conn) error {
		// ---- 群聊发消息（落库 + 群内广播） ----
		srv.On("chat:send", func(conn socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
			ctx := context.Background()
			auth := authed(conn)
			if auth == nil {
				ack(util.Fail("未登录"))
				return
			}
			var req chatSendReq
			_ = json.Unmarshal(data, &req)
			groupId := int64(0)
			if req.GroupID != nil {
				groupId = *req.GroupID
			}
			kind0 := "text"
			if req.Kind != nil && isKind(*req.Kind) {
				kind0 = *req.Kind
			}
			// content 优先，其次 text（对齐 TS: d.content !== undefined ? d.content : d.text）
			raw := ""
			if req.Content != nil {
				raw = *req.Content
			} else if req.Text != nil {
				raw = *req.Text
			}
			// 发送兜底：文本内容若是音频引用 → 记为 audio 类型
			kind := kind0
			if kind0 == "text" && isAudioRef(raw) {
				kind = "audio"
			}
			if groupId <= 0 {
				ack(util.Fail("参数不合法"))
				return
			}
			if kind == "text" && !isText(raw) {
				ack(util.Fail("消息不合法（1-4000 字）"))
				return
			}
			if (kind == "image" || kind == "file" || kind == "video" || kind == "audio" || kind == "folder") && raw == "" {
				ack(util.Fail("需要文件地址、UUID 或文件夹路径"))
				return
			}
			// 校验是否在群中
			role, err := store.GetGroupMemberRole(ctx, groupId, auth.ID)
			if err != nil {
				logger.Error(module, "GetGroupMemberRole 失败", "error", err)
				ack(util.Fail("服务器错误"))
				return
			}
			if role == "" {
				ack(util.Fail("不在群中"))
				return
			}
			// 校验禁言
			_, mutedUntil, err := store.GetGroupMember(ctx, groupId, auth.ID)
			if err != nil {
				logger.Error(module, "GetGroupMember 失败", "error", err)
				ack(util.Fail("服务器错误"))
				return
			}
			if mutedUntil != nil && *mutedUntil != "" {
				// TS: me.mutedUntil.getTime() > Date.now() → 禁言未过则拒绝
				if t, ok := util.ParseLocalTime(*mutedUntil); ok && t.After(time.Now()) {
					ack(util.Fail("您已被禁言"))
					return
				}
			}

			content, err := resolveContent(ctx, store, kind, raw)
			if err != nil {
				logger.Error(module, "resolveContent 失败", "error", err)
				ack(util.Fail("服务器错误"))
				return
			}
			if content == nil {
				ack(util.Fail("文件不存在"))
				return
			}
			conv, err := store.GetOrCreateGroupConversation(ctx, groupId, 0)
			if err != nil {
				logger.Error(module, "GetOrCreateGroupConversation 失败", "error", err)
				ack(util.Fail("服务器错误"))
				return
			}
			msg, err := store.SaveMessage(ctx, conv.ID, auth.ID, kind, *content)
			if err != nil {
				logger.Error(module, "SaveMessage 失败", "error", err)
				ack(util.Fail("服务器错误"))
				return
			}
			// 填充发送者昵称/头像
			msg.Nick = auth.Nick
			u, err := store.GetUserByID(ctx, auth.ID)
			if err != nil {
				logger.Error(module, "GetUserByID(auth) 失败", "error", err)
			}
			if u != nil {
				msg.Avatar = u.Avatar
			}

			// 更新该群所有成员的会话列表（最新消息/预览/未读）
			members, err := store.GetGroupMembers(ctx, groupId)
			if err != nil {
				logger.Error(module, "GetGroupMembers 失败", "error", err)
			} else {
				for _, mem := range members {
					_ = store.TouchConversation(ctx, conv.ID, mem.UserID, previewOf(kind, *content), auth.ID)
				}
			}
			// 群内其他成员收一份；发送方经自己的 userRoom 收一份（避免双份）
			decorateMsg(cfg, msg)
			_ = srv.EmitToSkip(util.GroupRoom(groupId), "chat:message", msg, conn.ID())
			_ = srv.EmitTo(util.UserRoom(auth.ID), "chat:message", msg)
			ack(util.OK(map[string]any{"id": msg.ID}))
		})

		// ---- 群聊历史 ----
		srv.On("chat:history", func(conn socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
			ctx := context.Background()
			auth := authed(conn)
			if auth == nil {
				ack(util.Fail("未登录"))
				return
			}
			var req chatHistoryReq
			_ = json.Unmarshal(data, &req)
			groupId := int64(0)
			if req.GroupID != nil {
				groupId = *req.GroupID
			}
			if groupId <= 0 {
				ack(util.Fail("参数不合法"))
				return
			}
			role, err := store.GetGroupMemberRole(ctx, groupId, auth.ID)
			if err != nil {
				logger.Error(module, "GetGroupMemberRole 失败", "error", err)
				ack(util.Fail("服务器错误"))
				return
			}
			if role == "" {
				ack(util.Fail("不在群中"))
				return
			}
			beforeTs := int64(0)
			if req.BeforeTs != nil {
				beforeTs = *req.BeforeTs
			}
			limit := int64(50)
			if req.Limit != nil {
				limit = *req.Limit
			}
			if limit < 1 {
				limit = 1
			}
			if limit > 200 {
				limit = 200
			}
			conv, err := store.GetOrCreateGroupConversation(ctx, groupId, 0)
			if err != nil {
				logger.Error(module, "GetOrCreateGroupConversation 失败", "error", err)
				ack(util.Fail("服务器错误"))
				return
			}
			msgs, err := store.ListMessages(ctx, conv.ID, beforeTs, limit)
			if err != nil {
				logger.Error(module, "ListMessages 失败", "error", err)
				ack(util.Fail("服务器错误"))
				return
			}
			if err := fillNicks(ctx, store, msgs); err != nil {
				logger.Error(module, "fillNicks 失败", "error", err)
			}
			for i := range msgs {
				decorateMsg(cfg, &msgs[i])
			}
			ack(util.OK(map[string]any{"messages": msgs}))
		})

		// ---- 一对一私信 ----
		srv.On("dm:send", func(conn socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
			ctx := context.Background()
			auth := authed(conn)
			if auth == nil {
				ack(util.Fail("未登录"))
				return
			}
			var req dmSendReq
			_ = json.Unmarshal(data, &req)
			toUserID := int64(0)
			if req.ToUserID != nil {
				toUserID = *req.ToUserID
			}
			kind0 := "text"
			if req.Kind != nil && isKind(*req.Kind) {
				kind0 = *req.Kind
			}
			raw := ""
			if req.Content != nil {
				raw = *req.Content
			} else if req.Text != nil {
				raw = *req.Text
			}
			kind := kind0
			if kind0 == "text" && isAudioRef(raw) {
				kind = "audio"
			}
			if toUserID <= 0 || toUserID == auth.ID {
				ack(util.Fail("参数不合法"))
				return
			}
			if kind == "text" && !isText(raw) {
				ack(util.Fail("消息不合法（1-4000 字）"))
				return
			}
			if (kind == "image" || kind == "file" || kind == "video" || kind == "audio" || kind == "folder") && raw == "" {
				ack(util.Fail("需要文件地址、UUID 或文件夹路径"))
				return
			}
			// 校验对方存在
			other, err := store.GetUserByID(ctx, toUserID)
			if err != nil {
				logger.Error(module, "GetUserByID(toUser) 失败", "error", err)
				ack(util.Fail("服务器错误"))
				return
			}
			if other == nil {
				ack(util.Fail("对方不存在"))
				return
			}

			content, err := resolveContent(ctx, store, kind, raw)
			if err != nil {
				logger.Error(module, "resolveContent 失败", "error", err)
				ack(util.Fail("服务器错误"))
				return
			}
			if content == nil {
				ack(util.Fail("文件不存在"))
				return
			}
			conv, err := store.GetOrCreateDmConversation(ctx, auth.ID, toUserID)
			if err != nil {
				logger.Error(module, "GetOrCreateDmConversation 失败", "error", err)
				ack(util.Fail("服务器错误"))
				return
			}
			msg, err := store.SaveMessage(ctx, conv.ID, auth.ID, kind, *content)
			if err != nil {
				logger.Error(module, "SaveMessage 失败", "error", err)
				ack(util.Fail("服务器错误"))
				return
			}
			msg.Nick = auth.Nick
			u, err := store.GetUserByID(ctx, auth.ID)
			if err != nil {
				logger.Error(module, "GetUserByID(auth) 失败", "error", err)
			}
			if u != nil {
				msg.Avatar = u.Avatar
			}
			msg.Type = "dm"

			// 更新双方会话列表；发送方不增未读（fromID=0 表示自己发的），接收方增未读（fromID=auth.ID）
			_ = store.TouchConversation(ctx, conv.ID, auth.ID, previewOf(kind, *content), 0)
			_ = store.TouchConversation(ctx, conv.ID, toUserID, previewOf(kind, *content), auth.ID)

			decorateMsg(cfg, msg)
			_ = srv.EmitTo(util.UserRoom(auth.ID), "dm:message", msg)
			_ = srv.EmitTo(util.UserRoom(toUserID), "dm:message", msg)
			ack(util.OK(map[string]any{"id": msg.ID}))
		})

		// ---- 私信历史 ----
		srv.On("dm:history", func(conn socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
			ctx := context.Background()
			auth := authed(conn)
			if auth == nil {
				ack(util.Fail("未登录"))
				return
			}
			var req dmHistoryReq
			_ = json.Unmarshal(data, &req)
			withUserID := int64(0)
			if req.WithUserID != nil {
				withUserID = *req.WithUserID
			}
			if withUserID <= 0 {
				ack(util.Fail("参数不合法"))
				return
			}
			beforeTs := int64(0)
			if req.BeforeTs != nil {
				beforeTs = *req.BeforeTs
			}
			limit := int64(50)
			if req.Limit != nil {
				limit = *req.Limit
			}
			if limit < 1 {
				limit = 1
			}
			if limit > 200 {
				limit = 200
			}
			conv, err := store.GetOrCreateDmConversation(ctx, auth.ID, withUserID)
			if err != nil {
				logger.Error(module, "GetOrCreateDmConversation 失败", "error", err)
				ack(util.Fail("服务器错误"))
				return
			}
			msgs, err := store.ListMessages(ctx, conv.ID, beforeTs, limit)
			if err != nil {
				logger.Error(module, "ListMessages 失败", "error", err)
				ack(util.Fail("服务器错误"))
				return
			}
			for i := range msgs {
				msgs[i].Type = "dm"
			}
			if err := fillNicks(ctx, store, msgs); err != nil {
				logger.Error(module, "fillNicks 失败", "error", err)
			}
			for i := range msgs {
				decorateMsg(cfg, &msgs[i])
			}
			ack(util.OK(map[string]any{"messages": msgs}))
		})

		// ---- 对话列表（群聊 + 私信统一；按置顶优先、最新消息倒序） ----
		srv.On("conversation:list", func(conn socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
			ctx := context.Background()
			auth := authed(conn)
			if auth == nil {
				ack(util.Fail("未登录"))
				return
			}
			var req conversationListReq
			_ = json.Unmarshal(data, &req)
			typ := ""
			if req.Type != nil && (*req.Type == "group" || *req.Type == "dm") {
				typ = *req.Type
			}
			items, err := store.ListConversations(ctx, auth.ID, typ)
			if err != nil {
				logger.Error(module, "ListConversations 失败", "error", err)
				ack(util.Fail("服务器错误"))
				return
			}
			named := make([]convListItem, 0, len(items))
			for _, it := range items {
				name := ""
				avatar := ""
				if it.Type == "group" && it.GroupID != nil {
					g, err := store.GetGroupByID(ctx, *it.GroupID)
					if err != nil {
						logger.Error(module, "GetGroupByID 失败", "error", err)
					}
					if g != nil {
						name = g.Name
					}
				} else if it.DmUserID != nil {
					u, err := store.GetUserByID(ctx, *it.DmUserID)
					if err != nil {
						logger.Error(module, "GetUserByID(dmUser) 失败", "error", err)
					}
					if u != nil {
						name = u.Nick
						if name == "" {
							name = u.Username
						}
						avatar = u.Avatar
					}
				}
				named = append(named, convListItem{
					ConversationItem: it,
					Name:             name,
					Avatar:           avatar,
				})
			}
			ack(util.OK(map[string]any{"conversations": named}))
		})

		// ---- 置顶 ----
		srv.On("conversation:pin", func(conn socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
			ctx := context.Background()
			auth := authed(conn)
			if auth == nil {
				ack(util.Fail("未登录"))
				return
			}
			var req conversationPinReq
			_ = json.Unmarshal(data, &req)
			conversationID := int64(0)
			if req.ConversationID != nil {
				conversationID = *req.ConversationID
			}
			if conversationID <= 0 {
				ack(util.Fail("参数不合法"))
				return
			}
			pinned := false
			if req.Pinned != nil {
				pinned = *req.Pinned
			}
			err := store.SetConversationPinned(ctx, conversationID, auth.ID, pinned)
			if err != nil {
				// TS: catch → fail('不在该对话中')
				logger.Error(module, "SetConversationPinned 失败", "error", err)
				ack(util.Fail("不在该对话中"))
				return
			}
			_ = srv.EmitTo(util.UserRoom(auth.ID), "conversations:updated", map[string]any{
				"conversationId": conversationID,
				"pinned":         pinned,
			})
			ack(util.OK(map[string]any{"pinned": pinned}))
		})

		// ---- 已读 ----
		srv.On("conversation:read", func(conn socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
			ctx := context.Background()
			auth := authed(conn)
			if auth == nil {
				ack(util.Fail("未登录"))
				return
			}
			var req conversationReadReq
			_ = json.Unmarshal(data, &req)
			conversationID := int64(0)
			if req.ConversationID != nil {
				conversationID = *req.ConversationID
			}
			if conversationID <= 0 {
				ack(util.Fail("参数不合法"))
				return
			}
			var lastReadMessageID *int64
			if req.LastMessageID != nil && *req.LastMessageID > 0 {
				v := *req.LastMessageID
				lastReadMessageID = &v
			}
			// 客户端只在「窗口可见 + 消息真的滚进可视区域」时才上报可视区内最大的消息 id，
			// 因此这里要按位置重算未读（不是一律清零），并广播实际生效的位置。
			unread, readPos, err := store.MarkRead(ctx, conversationID, auth.ID, lastReadMessageID)
			if err != nil {
				logger.Error(module, "MarkRead 失败", "error", err)
			}
			// 已读回执广播：通知会话内其他成员"我读到了 lastReadMessageId"
			conv, err := store.GetConversationByID(ctx, conversationID)
			if err != nil {
				logger.Error(module, "GetConversationByID 失败", "error", err)
			}
			if conv != nil {
				rc := readReceiptBroadcast{
					ConversationID:    conversationID,
					UserID:            auth.ID,
					LastReadMessageID: readPos,
				}
				if conv.Type == "group" && conv.GroupID != nil {
					_ = srv.EmitToSkip(util.GroupRoom(*conv.GroupID), "chat:readReceipt", rc, conn.ID())
				} else if conv.DmUserA != nil && conv.DmUserB != nil {
					_ = srv.EmitToSkip(util.UserRoom(*conv.DmUserA), "chat:readReceipt", rc, conn.ID())
					_ = srv.EmitToSkip(util.UserRoom(*conv.DmUserB), "chat:readReceipt", rc, conn.ID())
				}
			}
			_ = srv.EmitTo(util.UserRoom(auth.ID), "conversations:updated", map[string]any{
				"conversationId": conversationID,
				"unread":         unread,
			})
			ack(util.OK(nil))
		})

		// ---- 软删除（客户端删除/清空：只打 deletedAt 标记，不真删） ----
		srv.On("message:delete", func(conn socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
			ctx := context.Background()
			auth := authed(conn)
			if auth == nil {
				ack(util.Fail("未登录"))
				return
			}
			var req messageDeleteReq
			_ = json.Unmarshal(data, &req)
			conversationID := int64(0)
			if req.ConversationID != nil {
				conversationID = *req.ConversationID
			}
			messageID := ""
			if req.MessageID != nil {
				messageID = *req.MessageID
			}
			content := ""
			if req.Content != nil {
				// TS: d.content.slice(0, 1000)
				content = *req.Content
				if len(content) > 1000 {
					content = content[:1000]
				}
			}
			if conversationID <= 0 || messageID == "" {
				ack(util.Fail("参数不合法"))
				return
			}
			conv, err := store.GetConversationByID(ctx, conversationID)
			if err != nil {
				logger.Error(module, "GetConversationByID 失败", "error", err)
				ack(util.Fail("服务器错误"))
				return
			}
			if conv == nil {
				ack(util.Fail("对话不存在"))
				return
			}
			member, err := store.GetConversationMember(ctx, conversationID, auth.ID)
			if err != nil {
				logger.Error(module, "GetConversationMember 失败", "error", err)
				ack(util.Fail("服务器错误"))
				return
			}
			if member == nil {
				ack(util.Fail("不在该对话中"))
				return
			}
			msg, err := store.GetMessage(ctx, conversationID, messageID)
			if err != nil {
				logger.Error(module, "GetMessage 失败", "error", err)
			}
			// 本地临时消息/旧消息（id 或会话未匹配）：按内容匹配本人最近消息软删
			if msg == nil && content != "" {
				msg, err = store.FindOwnMessageByContent(ctx, conversationID, auth.ID, content)
				if err != nil {
					logger.Error(module, "FindOwnMessageByContent 失败", "error", err)
				}
			}
			if msg == nil && content != "" {
				msg, err = store.FindOwnMessageByContentGlobal(ctx, auth.ID, content)
				if err != nil {
					logger.Error(module, "FindOwnMessageByContentGlobal 失败", "error", err)
				}
			}
			if msg == nil {
				ack(util.OK(nil))
				return
			}
			// 权限：本人可删自己的；公司管理员可删群内任意
			own := msg.FromID == auth.ID
			admin := false
			if conv.Type == "group" && conv.GroupID != nil {
				g, err := store.GetGroupByID(ctx, *conv.GroupID)
				if err != nil {
					logger.Error(module, "GetGroupByID 失败", "error", err)
				}
				if g != nil && g.CompanyID > 0 {
					admin, err = isCompanyAdmin(ctx, store, g.CompanyID, auth.ID)
					if err != nil {
						logger.Error(module, "isCompanyAdmin 失败", "error", err)
					}
				}
			}
			if !own && !admin {
				ack(util.Fail("仅可删除自己的消息"))
				return
			}
			ok, err := store.SoftDeleteMessage(ctx, conversationID, msg.ID)
			if err != nil {
				logger.Error(module, "SoftDeleteMessage 失败", "error", err)
				ack(util.Fail("服务器错误"))
				return
			}
			if !ok {
				ack(util.Fail("消息不存在或已删除"))
				return
			}
			broadcastDeleted(srv, conv, conversationID, msg.ID)
			ack(util.OK(nil))
		})

		// ---- 真正删除（仅公司管理员；私信仅发送者本人） ----
		srv.On("message:hardDelete", func(conn socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
			ctx := context.Background()
			auth := authed(conn)
			if auth == nil {
				ack(util.Fail("未登录"))
				return
			}
			var req messageHardDeleteReq
			_ = json.Unmarshal(data, &req)
			conversationID := int64(0)
			if req.ConversationID != nil {
				conversationID = *req.ConversationID
			}
			messageID := ""
			if req.MessageID != nil {
				messageID = *req.MessageID
			}
			if conversationID <= 0 || messageID == "" {
				ack(util.Fail("参数不合法"))
				return
			}
			conv, err := store.GetConversationByID(ctx, conversationID)
			if err != nil {
				logger.Error(module, "GetConversationByID 失败", "error", err)
				ack(util.Fail("服务器错误"))
				return
			}
			if conv == nil {
				ack(util.Fail("对话不存在"))
				return
			}
			msg, err := store.GetMessage(ctx, conversationID, messageID)
			if err != nil {
				logger.Error(module, "GetMessage 失败", "error", err)
				ack(util.Fail("服务器错误"))
				return
			}
			if msg == nil {
				ack(util.Fail("消息不存在"))
				return
			}
			allowed := false
			if conv.Type == "group" && conv.GroupID != nil {
				g, err := store.GetGroupByID(ctx, *conv.GroupID)
				if err != nil {
					logger.Error(module, "GetGroupByID 失败", "error", err)
				}
				if g != nil && g.CompanyID > 0 {
					allowed, err = isCompanyAdmin(ctx, store, g.CompanyID, auth.ID)
					if err != nil {
						logger.Error(module, "isCompanyAdmin 失败", "error", err)
					}
				}
			} else {
				// 私信无公司管理员，仅发送者本人
				allowed = msg.FromID == auth.ID
			}
			if !allowed {
				ack(util.Fail("仅管理员可真正删除"))
				return
			}
			ok, err := store.HardDeleteMessage(ctx, conversationID, messageID)
			if err != nil {
				logger.Error(module, "HardDeleteMessage 失败", "error", err)
				ack(util.Fail("服务器错误"))
				return
			}
			if !ok {
				ack(util.Fail("消息不存在"))
				return
			}
			broadcastDeleted(srv, conv, conversationID, messageID)
			ack(util.OK(nil))
		})

		return nil
	})
}
