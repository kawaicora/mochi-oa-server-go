// Package db 的 MySqlStore 剩余方法实现（对话、消息、文件、好友、邀请、人事、会话、爱发电、邮件、假期、任务、TAPD）。
//
// 本文件全部走 GORM（s.gorm），不再出现裸 SQL 字面量。
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"gorm.io/gorm"

	"mochi-oa-server-go/internal/types"
)

// gormNotFound 兼容 GORM 与 database/sql 两种「无记录」错误。
func gormNotFound(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, sql.ErrNoRows)
}

// ==================== 对话 ====================

// GetOrCreateGroupConversation 取群对话，不存在则按需创建（ownerId 入群）。
func (s *MySqlStore) GetOrCreateGroupConversation(ctx context.Context, groupID, ownerID int64) (*types.Conversation, error) {
	var r types.ConversationRow
	err := s.gorm.WithContext(ctx).Table("conversations").
		Where("type = ? AND group_id = ?", "group", groupID).
		Take(&r).Error
	if err == nil {
		c := rowToConversation(&r)
		return &c, nil
	}
	if !gormNotFound(err) {
		return nil, err
	}
	// 与旧实现一致：先插对话，再走 AddConversationMember（重复加入映射为 AlreadyExists）。
	nr := types.ConversationRow{Type: "group", GroupID: &groupID, CreatedAt: time.Now().UTC()}
	if err := s.gorm.WithContext(ctx).Table("conversations").Create(&nr).Error; err != nil {
		return nil, err
	}
	if ownerID != 0 {
		if err := s.AddConversationMember(ctx, nr.ID, ownerID); err != nil {
			return nil, err
		}
	}
	return s.GetConversationByID(ctx, nr.ID)
}

// GetOrCreateDmConversation 取两人私信对话（无序），不存在则创建并双向入列。
func (s *MySqlStore) GetOrCreateDmConversation(ctx context.Context, a, b int64) (*types.Conversation, error) {
	x, y := a, b
	if x > y {
		x, y = y, x
	}
	var r types.ConversationRow
	err := s.gorm.WithContext(ctx).Table("conversations").
		Where("type = ? AND dm_user_a = ? AND dm_user_b = ?", "dm", x, y).
		Take(&r).Error
	if err == nil {
		c := rowToConversation(&r)
		return &c, nil
	}
	if !gormNotFound(err) {
		return nil, err
	}
	// 与旧实现一致：先插对话，再双向入列（重复加入映射为 AlreadyExists）。
	nr := types.ConversationRow{Type: "dm", DmUserA: &x, DmUserB: &y, CreatedAt: time.Now().UTC()}
	if err := s.gorm.WithContext(ctx).Table("conversations").Create(&nr).Error; err != nil {
		return nil, err
	}
	if err := s.AddConversationMember(ctx, nr.ID, x); err != nil {
		return nil, err
	}
	if err := s.AddConversationMember(ctx, nr.ID, y); err != nil {
		return nil, err
	}
	return s.GetConversationByID(ctx, nr.ID)
}

// GetConversationByID 取对话。
func (s *MySqlStore) GetConversationByID(ctx context.Context, id int64) (*types.Conversation, error) {
	var r types.ConversationRow
	err := s.gorm.WithContext(ctx).Table("conversations").Where("id = ?", id).Take(&r).Error
	if gormNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c := rowToConversation(&r)
	return &c, nil
}

// AddConversationMember 加入对话。
func (s *MySqlStore) AddConversationMember(ctx context.Context, conversationID, userID int64) error {
	member := map[string]any{
		"conversation_id": conversationID, "user_id": userID, "pinned": false, "unread": 0,
	}
	err := s.gorm.WithContext(ctx).Table("conversation_members").Create(member).Error
	if err != nil {
		if isUniqueErr(err) {
			return &AlreadyExistsError{Msg: "already in conversation"}
		}
		return err
	}
	return nil
}

// RemoveConversationMember 移出对话。
func (s *MySqlStore) RemoveConversationMember(ctx context.Context, conversationID, userID int64) error {
	res := s.gorm.WithContext(ctx).Table("conversation_members").
		Where("conversation_id = ? AND user_id = ?", conversationID, userID).
		Delete(nil)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return &NotFoundError{Msg: "not in conversation"}
	}
	return nil
}

// SetConversationPinned 设置置顶。
// 语义与原 SQL 一致：影响行数为 0（成员不存在 或 置顶值本就相同）→ NotFound。
func (s *MySqlStore) SetConversationPinned(ctx context.Context, conversationID, userID int64, pinned bool) error {
	res := s.gorm.WithContext(ctx).Table("conversation_members").
		Where("conversation_id = ? AND user_id = ?", conversationID, userID).
		Updates(map[string]any{"pinned": pinned})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return &NotFoundError{Msg: "not in conversation"}
	}
	return nil
}

// GetConversationMember 取会话成员状态。
func (s *MySqlStore) GetConversationMember(ctx context.Context, conversationID, userID int64) (*types.ConversationMember, error) {
	var m types.ConversationMemberRow
	err := s.gorm.WithContext(ctx).Table("conversation_members").
		Where("conversation_id = ? AND user_id = ?", conversationID, userID).
		Take(&m).Error
	if gormNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := &types.ConversationMember{
		ConversationID: m.ConversationID, UserID: m.UserID, Pinned: m.Pinned, Unread: m.Unread,
	}
	if m.LastMessageAt != nil {
		ts := toIso(*m.LastMessageAt)
		out.LastMessageAt = &ts
	}
	out.LastPreview = m.LastPreview
	return out, nil
}

// listConversationRow ListConversations 的联表临时行。
type listConversationRow struct {
	ConversationID int64      `gorm:"column:conversation_id"`
	Pinned         bool       `gorm:"column:pinned"`
	LastMessageAt  *time.Time `gorm:"column:last_message_at"`
	LastPreview    *string    `gorm:"column:last_preview"`
	Unread         int64      `gorm:"column:unread"`
	Type           string     `gorm:"column:type"`
	GroupID        *int64     `gorm:"column:group_id"`
	DmUserA        *int64     `gorm:"column:dm_user_a"`
	DmUserB        *int64     `gorm:"column:dm_user_b"`
}

// ListConversations 对话列表：置顶优先，再按最新消息时间倒序。
func (s *MySqlStore) ListConversations(ctx context.Context, userID int64, typ string) ([]types.ConversationItem, error) {
	q := s.gorm.WithContext(ctx).Table("conversation_members m").
		Select("m.conversation_id, m.pinned, m.last_message_at, m.last_preview, m.unread, c.type, c.group_id, c.dm_user_a, c.dm_user_b").
		Joins("JOIN conversations c ON c.id = m.conversation_id").
		Where("m.user_id = ?", userID)
	if typ != "" {
		q = q.Where("c.type = ?", typ)
	}
	rows := []listConversationRow{}
	if err := q.Order("m.pinned DESC, m.last_message_at DESC").Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := []types.ConversationItem{}
	for _, m := range rows {
		var dmUserID *int64
		if m.Type == "dm" {
			if m.DmUserA != nil && m.DmUserB != nil {
				other := *m.DmUserA
				if other == userID {
					other = *m.DmUserB
				}
				dmUserID = &other
			}
		}
		var lastMsg *string
		if m.LastMessageAt != nil {
			t := toIso(*m.LastMessageAt)
			lastMsg = &t
		}
		receipts, err := s.GetReadReceipts(ctx, m.ConversationID)
		if err != nil {
			return nil, err
		}
		out = append(out, types.ConversationItem{
			ConversationID: m.ConversationID, Type: m.Type, GroupID: m.GroupID, DmUserID: dmUserID,
			Pinned: m.Pinned, LastMessageAt: lastMsg, LastPreview: m.LastPreview, Unread: m.Unread, ReadReceipts: receipts,
		})
	}
	return out, nil
}

// ==================== 消息 ====================

// SaveMessage 落库一条消息。
func (s *MySqlStore) SaveMessage(ctx context.Context, conversationID, fromID int64, kind, content string) (*types.ChatMessage, error) {
	row := map[string]any{
		"conversation_id": conversationID,
		"from_id":         fromID,
		"kind":            kind,
		"content":         content,
		"created_at":      time.Now().UTC(),
	}
	if err := s.gorm.WithContext(ctx).Table("messages").Create(row).Error; err != nil {
		return nil, err
	}
	var id int64
	if err := s.gorm.WithContext(ctx).Table("messages").
		Where("conversation_id = ? AND from_id = ? AND kind = ? AND content = ?", conversationID, fromID, kind, content).
		Order("id DESC").Limit(1).Pluck("id", &id).Error; err != nil {
		return nil, err
	}
	return s.GetMessage(ctx, conversationID, strconv.FormatInt(id, 10))
}

// ListMessages 拉取消息（最新在前，排除软删；beforeTs 分页）。
func (s *MySqlStore) ListMessages(ctx context.Context, conversationID, beforeTs, limit int64) ([]types.ChatMessage, error) {
	if limit <= 0 {
		limit = 50
	}
	typ, err := s.conversationType(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	q := s.gorm.WithContext(ctx).Table("messages").
		Where("conversation_id = ? AND deleted_at IS NULL", conversationID)
	if beforeTs > 0 {
		q = q.Where("created_at < ?", beforeTs)
	}
	rows := []types.MessageRow{}
	if err := q.Order("created_at DESC").Limit(int(limit)).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := []types.ChatMessage{}
	for _, r := range rows {
		out = append(out, types.ChatMessage{
			ID: fmt.Sprintf("%d", r.ID), ConversationID: r.ConversationID, Type: typ, FromID: r.FromID,
			Nick: "", Kind: r.Kind, Content: r.Content, Ts: r.CreatedAt.UnixMilli(),
		})
	}
	return out, nil
}

// GetMessage 取单条消息（含已删除）。
func (s *MySqlStore) GetMessage(ctx context.Context, conversationID int64, messageID string) (*types.ChatMessage, error) {
	id, err := strconv.ParseInt(messageID, 10, 64)
	if err != nil {
		return nil, nil
	}
	var r types.MessageRow
	err = s.gorm.WithContext(ctx).Table("messages").
		Where("id = ? AND conversation_id = ?", id, conversationID).
		Take(&r).Error
	if gormNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	typ, err := s.conversationType(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	return &types.ChatMessage{
		ID: fmt.Sprintf("%d", r.ID), ConversationID: r.ConversationID, Type: typ, FromID: r.FromID,
		Nick: "", Kind: r.Kind, Content: r.Content, Ts: r.CreatedAt.UnixMilli(),
	}, nil
}

// FindOwnMessageByContent 找本人在某会话的同内容消息（最新）。
func (s *MySqlStore) FindOwnMessageByContent(ctx context.Context, conversationID, userID int64, content string) (*types.ChatMessage, error) {
	var r types.MessageRow
	err := s.gorm.WithContext(ctx).Table("messages").
		Where("conversation_id = ? AND from_id = ? AND content = ? AND deleted_at IS NULL", conversationID, userID, content).
		Order("created_at DESC").Limit(1).Take(&r).Error
	if gormNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	typ, err := s.conversationType(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	return &types.ChatMessage{
		ID: fmt.Sprintf("%d", r.ID), ConversationID: r.ConversationID, Type: typ, FromID: r.FromID,
		Nick: "", Kind: r.Kind, Content: r.Content, Ts: r.CreatedAt.UnixMilli(),
	}, nil
}

// FindOwnMessageByContentGlobal 全局找本人同内容消息。
func (s *MySqlStore) FindOwnMessageByContentGlobal(ctx context.Context, userID int64, content string) (*types.ChatMessage, error) {
	var r types.MessageRow
	err := s.gorm.WithContext(ctx).Table("messages").
		Where("from_id = ? AND content = ? AND deleted_at IS NULL", userID, content).
		Order("created_at DESC").Limit(1).Take(&r).Error
	if gormNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	typ, err := s.conversationType(ctx, r.ConversationID)
	if err != nil {
		return nil, err
	}
	return &types.ChatMessage{
		ID: fmt.Sprintf("%d", r.ID), ConversationID: r.ConversationID, Type: typ, FromID: r.FromID,
		Nick: "", Kind: r.Kind, Content: r.Content, Ts: r.CreatedAt.UnixMilli(),
	}, nil
}

// SoftDeleteMessage 软删除（打 deleted_at）。
func (s *MySqlStore) SoftDeleteMessage(ctx context.Context, conversationID int64, messageID string) (bool, error) {
	id, err := strconv.ParseInt(messageID, 10, 64)
	if err != nil {
		return false, nil
	}
	res := s.gorm.WithContext(ctx).Table("messages").
		Where("id = ? AND conversation_id = ? AND deleted_at IS NULL", id, conversationID).
		Updates(map[string]any{"deleted_at": time.Now().UTC()})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// HardDeleteMessage 硬删除。
func (s *MySqlStore) HardDeleteMessage(ctx context.Context, conversationID int64, messageID string) (bool, error) {
	id, err := strconv.ParseInt(messageID, 10, 64)
	if err != nil {
		return false, nil
	}
	res := s.gorm.WithContext(ctx).Table("messages").
		Where("id = ? AND conversation_id = ?", id, conversationID).
		Delete(nil)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// TouchConversation 更新某会话成员的最新消息/预览/未读（fromId 除外）。
func (s *MySqlStore) TouchConversation(ctx context.Context, conversationID, userID int64, preview string, fromID int64) error {
	if len(preview) > 200 {
		preview = preview[:200]
	}
	updates := map[string]any{
		"last_message_at": time.Now().UTC(),
		"last_preview":    preview,
	}
	if fromID != 0 && fromID != userID {
		updates["unread"] = gorm.Expr("unread + 1")
	}
	return s.gorm.WithContext(ctx).Table("conversation_members").
		Where("conversation_id = ? AND user_id = ?", conversationID, userID).
		Updates(updates).Error
}

// MarkRead 清未读并记录已读位置。
func (s *MySqlStore) MarkRead(ctx context.Context, conversationID, userID int64, lastReadMessageID *int64) error {
	if lastReadMessageID != nil && *lastReadMessageID > 0 {
		return s.gorm.WithContext(ctx).Table("conversation_members").
			Where("conversation_id = ? AND user_id = ?", conversationID, userID).
			Updates(map[string]any{"unread": 0, "last_read_message_id": *lastReadMessageID}).Error
	}
	return s.gorm.WithContext(ctx).Table("conversation_members").
		Where("conversation_id = ? AND user_id = ?", conversationID, userID).
		Updates(map[string]any{"unread": 0}).Error
}

// GetReadReceipts 会话内各成员已读位置。
func (s *MySqlStore) GetReadReceipts(ctx context.Context, conversationID int64) ([]types.ReadReceipt, error) {
	out := []types.ReadReceipt{}
	err := s.gorm.WithContext(ctx).Table("conversation_members").
		Where("conversation_id = ?", conversationID).
		Find(&out).Error
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ==================== 文件 ====================

// SaveFile 落库文件记录。
func (s *MySqlStore) SaveFile(ctx context.Context, in SaveFileInput) (*types.FileRecord, error) {
	storage := in.Storage
	if storage == "" {
		storage = "local"
	}
	var created time.Time
	if err := s.gorm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row := map[string]any{
			"uuid": in.UUID, "filename": in.Filename, "mime": in.Mime, "size": in.Size,
			"storage": storage, "url": in.URL, "created_at": time.Now().UTC(),
		}
		if err := tx.Table("files").Create(row).Error; err != nil {
			return err
		}
		// 回读落库后的 created_at（DATETIME(3) 会截断纳秒），与旧实现的返回值一致。
		var back struct {
			CreatedAt time.Time `gorm:"column:created_at"`
		}
		if err := tx.Table("files").Select("created_at").Where("uuid = ?", in.UUID).Take(&back).Error; err != nil {
			return err
		}
		created = back.CreatedAt
		return nil
	}); err != nil {
		return nil, err
	}
	return &types.FileRecord{
		UUID: in.UUID, Filename: in.Filename, Mime: in.Mime, Size: in.Size,
		Storage: storage, URL: in.URL, CreatedAt: toIso(created),
	}, nil
}

// GetFileByUUID 按 UUID 取文件。
func (s *MySqlStore) GetFileByUUID(ctx context.Context, uuid string) (*types.FileRecord, error) {
	var r types.FileRow
	err := s.gorm.WithContext(ctx).Table("files").Where("uuid = ?", uuid).Take(&r).Error
	if gormNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &types.FileRecord{UUID: r.UUID, Filename: r.Filename, Mime: r.Mime, Size: r.Size, Storage: r.Storage, URL: r.URL, CreatedAt: toIso(r.CreatedAt)}, nil
}

// ==================== 好友 ====================

// AddFriend 加好友（无序对）。
func (s *MySqlStore) AddFriend(ctx context.Context, a, b int64) error {
	x, y := a, b
	if x > y {
		x, y = y, x
	}
	err := s.gorm.WithContext(ctx).Table("friends").
		Create(map[string]any{"user_a": x, "user_b": y, "created_at": time.Now().UTC()}).Error
	if err != nil {
		if isUniqueErr(err) {
			return &AlreadyExistsError{Msg: "already friends"}
		}
		return err
	}
	return nil
}

// RemoveFriend 删好友。
func (s *MySqlStore) RemoveFriend(ctx context.Context, a, b int64) error {
	x, y := a, b
	if x > y {
		x, y = y, x
	}
	res := s.gorm.WithContext(ctx).Table("friends").
		Where("user_a = ? AND user_b = ?", x, y).
		Delete(nil)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return &NotFoundError{Msg: "not friends"}
	}
	return nil
}

// friendPairRow ListFriends 的临时行。
type friendPairRow struct {
	UserA     int64     `gorm:"column:user_a"`
	UserB     int64     `gorm:"column:user_b"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

// ListFriends 列好友（联对方用户）。
func (s *MySqlStore) ListFriends(ctx context.Context, userID int64) ([]types.Friend, error) {
	rows := []friendPairRow{}
	if err := s.gorm.WithContext(ctx).Table("friends f").
		Select("f.user_a, f.user_b, f.created_at").
		Where("f.user_a = ? OR f.user_b = ?", userID, userID).
		Order("f.created_at ASC").
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := []types.Friend{}
	for _, p := range rows {
		other := p.UserA
		if other == userID {
			other = p.UserB
		}
		u, err := s.GetUserByID(ctx, other)
		if err != nil {
			return nil, err
		}
		if u == nil {
			continue
		}
		nick := u.Nick
		if nick == "" {
			nick = u.Username
		}
		out = append(out, types.Friend{UserID: u.ID, Username: u.Username, Nick: nick, Avatar: u.Avatar, AddedAt: toIso(p.CreatedAt)})
	}
	return out, nil
}
