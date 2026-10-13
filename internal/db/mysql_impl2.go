// Package db 的 MySqlStore 剩余方法实现（对话、消息、文件、好友、邀请、人事、会话、爱发电、邮件、假期、任务、TAPD）。
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"mochi-oa-server-go/internal/types"
)

// ==================== 对话 ====================

// GetOrCreateGroupConversation 取群对话，不存在则按需创建（ownerId 入群）。
func (s *MySqlStore) GetOrCreateGroupConversation(ctx context.Context, groupID, ownerID int64) (*types.Conversation, error) {
	var r types.ConversationRow
	err := s.db.QueryRowContext(ctx, `SELECT id, type, group_id, dm_user_a, dm_user_b, created_at FROM conversations WHERE type='group' AND group_id=?`, groupID).
		Scan(&r.ID, &r.Type, &r.GroupID, &r.DmUserA, &r.DmUserB, &r.CreatedAt)
	if err == nil {
		c := rowToConversation(&r)
		return &c, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO conversations (type, group_id, created_at) VALUES ('group',?,NOW(3))`, groupID)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	if ownerID != 0 {
		if err := s.AddConversationMember(ctx, id, ownerID); err != nil {
			return nil, err
		}
	}
	return s.GetConversationByID(ctx, id)
}

// GetOrCreateDmConversation 取两人私信对话（无序），不存在则创建并双向入列。
func (s *MySqlStore) GetOrCreateDmConversation(ctx context.Context, a, b int64) (*types.Conversation, error) {
	x, y := a, b
	if x > y {
		x, y = y, x
	}
	var r types.ConversationRow
	err := s.db.QueryRowContext(ctx, `SELECT id, type, group_id, dm_user_a, dm_user_b, created_at FROM conversations WHERE type='dm' AND dm_user_a=? AND dm_user_b=?`, x, y).
		Scan(&r.ID, &r.Type, &r.GroupID, &r.DmUserA, &r.DmUserB, &r.CreatedAt)
	if err == nil {
		c := rowToConversation(&r)
		return &c, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO conversations (type, dm_user_a, dm_user_b, created_at) VALUES ('dm',?,?,NOW(3))`, x, y)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	if err := s.AddConversationMember(ctx, id, x); err != nil {
		return nil, err
	}
	if err := s.AddConversationMember(ctx, id, y); err != nil {
		return nil, err
	}
	return s.GetConversationByID(ctx, id)
}

// GetConversationByID 取对话。
func (s *MySqlStore) GetConversationByID(ctx context.Context, id int64) (*types.Conversation, error) {
	var r types.ConversationRow
	err := s.db.QueryRowContext(ctx, `SELECT id, type, group_id, dm_user_a, dm_user_b, created_at FROM conversations WHERE id=?`, id).
		Scan(&r.ID, &r.Type, &r.GroupID, &r.DmUserA, &r.DmUserB, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
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
	_, err := s.db.ExecContext(ctx, `INSERT INTO conversation_members (conversation_id, user_id, pinned, unread) VALUES (?,?,false,0)`, conversationID, userID)
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
	res, err := s.db.ExecContext(ctx, `DELETE FROM conversation_members WHERE conversation_id=? AND user_id=?`, conversationID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{Msg: "not in conversation"}
	}
	return nil
}

// SetConversationPinned 设置置顶。
func (s *MySqlStore) SetConversationPinned(ctx context.Context, conversationID, userID int64, pinned bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE conversation_members SET pinned=? WHERE conversation_id=? AND user_id=?`, pinned, conversationID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{Msg: "not in conversation"}
	}
	return nil
}

// GetConversationMember 取会话成员状态。
func (s *MySqlStore) GetConversationMember(ctx context.Context, conversationID, userID int64) (*types.ConversationMember, error) {
	var m types.ConversationMemberRow
	err := s.db.QueryRowContext(ctx,
		`SELECT conversation_id, user_id, pinned, last_message_at, last_preview, unread, last_read_message_id FROM conversation_members WHERE conversation_id=? AND user_id=?`, conversationID, userID).
		Scan(&m.ConversationID, &m.UserID, &m.Pinned, &m.LastMessageAt, &m.LastPreview, &m.Unread, &m.LastReadMessageID)
	if errors.Is(err, sql.ErrNoRows) {
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

// ListConversations 对话列表：置顶优先，再按最新消息时间倒序。
func (s *MySqlStore) ListConversations(ctx context.Context, userID int64, typ string) ([]types.ConversationItem, error) {
	q := `SELECT m.conversation_id, m.pinned, m.last_message_at, m.last_preview, m.unread, c.type, c.group_id, c.dm_user_a, c.dm_user_b
		FROM conversation_members m JOIN conversations c ON c.id=m.conversation_id WHERE m.user_id=?`
	args := []any{userID}
	if typ != "" {
		q += ` AND c.type=?`
		args = append(args, typ)
	}
	q += ` ORDER BY m.pinned DESC, m.last_message_at DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.ConversationItem{}
	for rows.Next() {
		var m types.ConversationMemberRow
		var cType string
		var groupID *int64
		var dmA, dmB sql.NullInt64
		if err := rows.Scan(&m.ConversationID, &m.Pinned, &m.LastMessageAt, &m.LastPreview, &m.Unread, &cType, &groupID, &dmA, &dmB); err != nil {
			return nil, err
		}
		var dmUserID *int64
		if cType == "dm" {
			if dmA.Valid && dmB.Valid {
				other := dmA.Int64
				if other == userID {
					other = dmB.Int64
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
			ConversationID: m.ConversationID, Type: cType, GroupID: groupID, DmUserID: dmUserID,
			Pinned: m.Pinned, LastMessageAt: lastMsg, LastPreview: m.LastPreview, Unread: m.Unread, ReadReceipts: receipts,
		})
	}
	return out, rows.Err()
}

// ==================== 消息 ====================

// SaveMessage 落库一条消息。
func (s *MySqlStore) SaveMessage(ctx context.Context, conversationID, fromID int64, kind, content string) (*types.ChatMessage, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO messages (conversation_id, from_id, kind, content, created_at) VALUES (?,?,?,?,NOW(3))`, conversationID, fromID, kind, content)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.GetMessage(ctx, conversationID, fmt.Sprintf("%d", id))
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
	q := `SELECT id, conversation_id, from_id, kind, content, created_at FROM messages WHERE conversation_id=? AND deleted_at IS NULL`
	args := []any{conversationID}
	if beforeTs > 0 {
		q += ` AND created_at < ?`
		args = append(args, beforeTs)
	}
	q += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.ChatMessage{}
	for rows.Next() {
		var r types.MessageRow
		if err := rows.Scan(&r.ID, &r.ConversationID, &r.FromID, &r.Kind, &r.Content, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, types.ChatMessage{
			ID: fmt.Sprintf("%d", r.ID), ConversationID: r.ConversationID, Type: typ, FromID: r.FromID,
			Nick: "", Kind: r.Kind, Content: r.Content, Ts: r.CreatedAt.UnixMilli(),
		})
	}
	return out, rows.Err()
}

// GetMessage 取单条消息（含已删除）。
func (s *MySqlStore) GetMessage(ctx context.Context, conversationID int64, messageID string) (*types.ChatMessage, error) {
	var r types.MessageRow
	err := s.db.QueryRowContext(ctx,
		`SELECT id, conversation_id, from_id, kind, content, created_at FROM messages WHERE id=? AND conversation_id=?`, messageID, conversationID).
		Scan(&r.ID, &r.ConversationID, &r.FromID, &r.Kind, &r.Content, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
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
	err := s.db.QueryRowContext(ctx,
		`SELECT id, conversation_id, from_id, kind, content, created_at FROM messages WHERE conversation_id=? AND from_id=? AND content=? AND deleted_at IS NULL ORDER BY created_at DESC LIMIT 1`, conversationID, userID, content).
		Scan(&r.ID, &r.ConversationID, &r.FromID, &r.Kind, &r.Content, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
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
	err := s.db.QueryRowContext(ctx,
		`SELECT id, conversation_id, from_id, kind, content, created_at FROM messages WHERE from_id=? AND content=? AND deleted_at IS NULL ORDER BY created_at DESC LIMIT 1`, userID, content).
		Scan(&r.ID, &r.ConversationID, &r.FromID, &r.Kind, &r.Content, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
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
	res, err := s.db.ExecContext(ctx, `UPDATE messages SET deleted_at=NOW(3) WHERE id=? AND conversation_id=? AND deleted_at IS NULL`, messageID, conversationID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// HardDeleteMessage 硬删除。
func (s *MySqlStore) HardDeleteMessage(ctx context.Context, conversationID int64, messageID string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM messages WHERE id=? AND conversation_id=?`, messageID, conversationID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// TouchConversation 更新某会话成员的最新消息/预览/未读（fromId 除外）。
func (s *MySqlStore) TouchConversation(ctx context.Context, conversationID, userID int64, preview string, fromID int64) error {
	if len(preview) > 200 {
		preview = preview[:200]
	}
	unreadExpr := "unread"
	if fromID != 0 && fromID != userID {
		unreadExpr = "unread + 1"
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE conversation_members SET last_message_at=NOW(3), last_preview=?, unread=`+unreadExpr+` WHERE conversation_id=? AND user_id=?`, preview, conversationID, userID)
	return err
}

// MarkRead 清未读并记录已读位置。
func (s *MySqlStore) MarkRead(ctx context.Context, conversationID, userID int64, lastReadMessageID *int64) error {
	if lastReadMessageID != nil && *lastReadMessageID > 0 {
		_, err := s.db.ExecContext(ctx, `UPDATE conversation_members SET unread=0, last_read_message_id=? WHERE conversation_id=? AND user_id=?`, *lastReadMessageID, conversationID, userID)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE conversation_members SET unread=0 WHERE conversation_id=? AND user_id=?`, conversationID, userID)
	return err
}

// GetReadReceipts 会话内各成员已读位置。
func (s *MySqlStore) GetReadReceipts(ctx context.Context, conversationID int64) ([]types.ReadReceipt, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT user_id, last_read_message_id FROM conversation_members WHERE conversation_id=?`, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.ReadReceipt{}
	for rows.Next() {
		var rr types.ReadReceipt
		if err := rows.Scan(&rr.UserID, &rr.LastReadMessageID); err != nil {
			return nil, err
		}
		out = append(out, rr)
	}
	return out, rows.Err()
}

// ==================== 文件 ====================

// SaveFile 落库文件记录。
func (s *MySqlStore) SaveFile(ctx context.Context, in SaveFileInput) (*types.FileRecord, error) {
	storage := in.Storage
	if storage == "" {
		storage = "local"
	}
	var created string
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO files (uuid, filename, mime, size, storage, url, created_at) VALUES (?,?,?,?,?,?,NOW(3)); SELECT created_at FROM files WHERE uuid=?`,
		in.UUID, in.Filename, in.Mime, in.Size, storage, in.URL, in.UUID).Scan(&created)
	if err != nil {
		return nil, err
	}
	return &types.FileRecord{UUID: in.UUID, Filename: in.Filename, Mime: in.Mime, Size: in.Size, Storage: storage, URL: in.URL, CreatedAt: created}, nil
}

// GetFileByUUID 按 UUID 取文件。
func (s *MySqlStore) GetFileByUUID(ctx context.Context, uuid string) (*types.FileRecord, error) {
	var r types.FileRow
	err := s.db.QueryRowContext(ctx, `SELECT uuid, filename, mime, size, storage, url, created_at FROM files WHERE uuid=?`, uuid).
		Scan(&r.UUID, &r.Filename, &r.Mime, &r.Size, &r.Storage, &r.URL, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
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
	_, err := s.db.ExecContext(ctx, `INSERT INTO friends (user_a, user_b, created_at) VALUES (?,?,NOW(3))`, x, y)
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
	res, err := s.db.ExecContext(ctx, `DELETE FROM friends WHERE user_a=? AND user_b=?`, x, y)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{Msg: "not friends"}
	}
	return nil
}

// ListFriends 列好友（联对方用户）。
func (s *MySqlStore) ListFriends(ctx context.Context, userID int64) ([]types.Friend, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT f.user_a, f.user_b, f.created_at FROM friends f WHERE f.user_a=? OR f.user_b=? ORDER BY f.created_at ASC`, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.Friend{}
	for rows.Next() {
		var ua, ub, created int64
		if err := rows.Scan(&ua, &ub, &created); err != nil {
			return nil, err
		}
		other := ua
		if other == userID {
			other = ub
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
		out = append(out, types.Friend{UserID: u.ID, Username: u.Username, Nick: nick, Avatar: u.Avatar, AddedAt: toIso(time.UnixMilli(created))})
	}
	return out, rows.Err()
}
