// Package db 的 MemoryStore 方法（群、对话、消息、文件、好友、邀请、人事、会话、邮件、假期、任务、TAPD）。
package db

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"mochi-oa-server-go/internal/types"
)

// ==================== 群 ====================

func (m *MemoryStore) CreateGroup(_ context.Context, in CreateGroupInput) (*types.Group, error) {
	for _, g := range m.groups {
		if g.Code == in.Code {
			return nil, &AlreadyExistsError{Msg: "code taken"}
		}
	}
	id := m.nextId()
	var cid *int64
	if in.CompanyID > 0 {
		v := in.CompanyID
		cid = &v
	}
	m.groups[id] = &types.GroupRow{ID: id, CompanyID: cid, DepartmentID: in.DepartmentID, Name: in.Name, Code: in.Code, OwnerID: in.OwnerID, CreatedAt: time.Now().UTC()}
	m.groupMem[id] = map[int64]*types.GroupMemberRow{in.OwnerID: {GroupID: id, UserID: in.OwnerID, Role: "owner", JoinedAt: time.Now().UTC()}}
	g := rowToGroup(m.groups[id])
	return &g, nil
}

func (m *MemoryStore) GetGroupByID(_ context.Context, id int64) (*types.Group, error) {
	if r, ok := m.groups[id]; ok {
		g := rowToGroup(r)
		return &g, nil
	}
	return nil, nil
}

func (m *MemoryStore) GetGroupByName(_ context.Context, companyID int64, name string) (*types.Group, error) {
	for _, r := range m.groups {
		cid := int64(0)
		if r.CompanyID != nil {
			cid = *r.CompanyID
		}
		if r.Name == name && cid == companyID {
			g := rowToGroup(r)
			return &g, nil
		}
	}
	return nil, nil
}

func (m *MemoryStore) GetGroupByCode(_ context.Context, code string) (*types.Group, error) {
	for _, r := range m.groups {
		if r.Code == code {
			g := rowToGroup(r)
			return &g, nil
		}
	}
	return nil, nil
}

func (m *MemoryStore) ListGroups(_ context.Context, companyID int64, keyword string) ([]types.Group, error) {
	ids := []int64{}
	for id := range m.groups {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := []types.Group{}
	for _, id := range ids {
		r := m.groups[id]
		if companyID != 0 {
			cid := int64(0)
			if r.CompanyID != nil {
				cid = *r.CompanyID
			}
			if cid != companyID {
				continue
			}
		}
		g := rowToGroup(r)
		if kw := strings.TrimSpace(keyword); kw != "" && !strings.Contains(g.Name, kw) {
			continue
		}
		out = append(out, g)
	}
	return out, nil
}

func (m *MemoryStore) AddGroupMember(_ context.Context, groupID, userID int64, role string) error {
	set := m.groupMem[groupID]
	if set == nil {
		set = map[int64]*types.GroupMemberRow{}
		m.groupMem[groupID] = set
	}
	if _, ok := set[userID]; ok {
		return &AlreadyExistsError{Msg: "already in group"}
	}
	set[userID] = &types.GroupMemberRow{GroupID: groupID, UserID: userID, Role: role, JoinedAt: time.Now().UTC()}
	return nil
}

func (m *MemoryStore) RemoveGroupMember(_ context.Context, groupID, userID int64) error {
	if set := m.groupMem[groupID]; set != nil {
		if _, ok := set[userID]; ok {
			delete(set, userID)
			return nil
		}
	}
	return &NotFoundError{Msg: "not in group"}
}

func (m *MemoryStore) GetGroupMemberRole(_ context.Context, groupID, userID int64) (string, error) {
	if set := m.groupMem[groupID]; set != nil {
		if r, ok := set[userID]; ok {
			return r.Role, nil
		}
	}
	return "", nil
}

func (m *MemoryStore) GetGroupMember(_ context.Context, groupID, userID int64) (string, *string, error) {
	if set := m.groupMem[groupID]; set != nil {
		if r, ok := set[userID]; ok {
			var muted *string
			if r.MutedUntil != nil && !r.MutedUntil.IsZero() {
				s := toIso(*r.MutedUntil)
				muted = &s
			}
			return r.Role, muted, nil
		}
	}
	return "", nil, nil
}

func (m *MemoryStore) SetGroupMemberRole(_ context.Context, groupID, userID int64, role string) error {
	if set := m.groupMem[groupID]; set != nil {
		if r, ok := set[userID]; ok {
			r.Role = role
		}
	}
	return nil
}

func (m *MemoryStore) SetGroupMemberMuted(_ context.Context, groupID, userID int64, mutedUntil *string) error {
	if set := m.groupMem[groupID]; set != nil {
		if r, ok := set[userID]; ok {
			if mutedUntil == nil || *mutedUntil == "" {
				r.MutedUntil = nil
			} else {
				t, err := parseTimeFlex(*mutedUntil)
				if err == nil {
					r.MutedUntil = &t
				}
			}
		}
	}
	return nil
}

func (m *MemoryStore) SetGroupOwner(_ context.Context, groupID, ownerID int64) error {
	if g, ok := m.groups[groupID]; ok {
		g.OwnerID = ownerID
		return nil
	}
	return &NotFoundError{Msg: "group not found"}
}

func (m *MemoryStore) DeleteGroup(_ context.Context, groupID int64) error {
	if cid, ok := m.groupConv[groupID]; ok {
		delete(m.messages, cid)
		delete(m.convMem, cid)
		delete(m.conversations, cid)
		delete(m.groupConv, groupID)
	}
	delete(m.groupMem, groupID)
	delete(m.groups, groupID)
	return nil
}

func (m *MemoryStore) GetGroupMembers(_ context.Context, groupID int64) ([]types.GroupMember, error) {
	out := []types.GroupMember{}
	if set := m.groupMem[groupID]; set != nil {
		for _, r := range set {
			gm := types.GroupMember{GroupID: r.GroupID, UserID: r.UserID, Role: r.Role, JoinedAt: toIso(r.JoinedAt)}
			if r.MutedUntil != nil && !r.MutedUntil.IsZero() {
				s := toIso(*r.MutedUntil)
				gm.MutedUntil = &s
			}
			if u, _ := m.GetUserByID(context.Background(), r.UserID); u != nil {
				un, nk := u.Username, u.Nick
				gm.Username, gm.Nick = &un, &nk
			}
			out = append(out, gm)
		}
	}
	return out, nil
}

func (m *MemoryStore) GetUserGroups(_ context.Context, userID int64) ([]types.Group, error) {
	out := []types.Group{}
	for gid, set := range m.groupMem {
		if _, ok := set[userID]; ok {
			if r, ok := m.groups[gid]; ok {
				g := rowToGroup(r)
				out = append(out, g)
			}
		}
	}
	return out, nil
}

// ==================== 对话 ====================

func (m *MemoryStore) convType(id int64) string {
	if r, ok := m.conversations[id]; ok {
		return r.Type
	}
	return "group"
}

func (m *MemoryStore) GetOrCreateGroupConversation(_ context.Context, groupID, ownerID int64) (*types.Conversation, error) {
	if cid, ok := m.groupConv[groupID]; ok {
		r := m.conversations[cid]
		c := rowToConversation(r)
		return &c, nil
	}
	id := m.nextId()
	m.conversations[id] = &types.ConversationRow{ID: id, Type: "group", GroupID: &groupID, CreatedAt: time.Now().UTC()}
	m.groupConv[groupID] = id
	if ownerID != 0 {
		_ = m.AddConversationMember(context.Background(), id, ownerID)
	}
	r := m.conversations[id]
	c := rowToConversation(r)
	return &c, nil
}

func (m *MemoryStore) GetOrCreateDmConversation(_ context.Context, a, b int64) (*types.Conversation, error) {
	x, y := normPair(a, b)
	if cid, ok := m.dmConv[pairKey(x, y)]; ok {
		r := m.conversations[cid]
		c := rowToConversation(r)
		return &c, nil
	}
	id := m.nextId()
	m.conversations[id] = &types.ConversationRow{ID: id, Type: "dm", DmUserA: &x, DmUserB: &y, CreatedAt: time.Now().UTC()}
	m.dmConv[pairKey(x, y)] = id
	_ = m.AddConversationMember(context.Background(), id, x)
	_ = m.AddConversationMember(context.Background(), id, y)
	r := m.conversations[id]
	c := rowToConversation(r)
	return &c, nil
}

func (m *MemoryStore) GetConversationByID(_ context.Context, id int64) (*types.Conversation, error) {
	if r, ok := m.conversations[id]; ok {
		c := rowToConversation(r)
		return &c, nil
	}
	return nil, nil
}

func (m *MemoryStore) AddConversationMember(_ context.Context, conversationID, userID int64) error {
	set := m.convMem[conversationID]
	if set == nil {
		set = map[int64]*types.ConversationMemberRow{}
		m.convMem[conversationID] = set
	}
	if _, ok := set[userID]; ok {
		return &AlreadyExistsError{Msg: "already in conversation"}
	}
	set[userID] = &types.ConversationMemberRow{ConversationID: conversationID, UserID: userID}
	return nil
}

func (m *MemoryStore) RemoveConversationMember(_ context.Context, conversationID, userID int64) error {
	if set := m.convMem[conversationID]; set != nil {
		if _, ok := set[userID]; ok {
			delete(set, userID)
			return nil
		}
	}
	return &NotFoundError{Msg: "not in conversation"}
}

func (m *MemoryStore) SetConversationPinned(_ context.Context, conversationID, userID int64, pinned bool) error {
	if set := m.convMem[conversationID]; set != nil {
		if r, ok := set[userID]; ok {
			r.Pinned = pinned
		}
	}
	return nil
}

func (m *MemoryStore) GetConversationMember(_ context.Context, conversationID, userID int64) (*types.ConversationMember, error) {
	if set := m.convMem[conversationID]; set != nil {
		if r, ok := set[userID]; ok {
			out := &types.ConversationMember{ConversationID: r.ConversationID, UserID: r.UserID, Pinned: r.Pinned, Unread: r.Unread}
			if r.LastMessageAt != nil {
				s := toIso(*r.LastMessageAt)
				out.LastMessageAt = &s
			}
			out.LastPreview = r.LastPreview
			return out, nil
		}
	}
	return nil, nil
}

func (m *MemoryStore) ListConversations(_ context.Context, userID int64, typ string) ([]types.ConversationItem, error) {
	out := []types.ConversationItem{}
	for cid, set := range m.convMem {
		r, ok := set[userID]
		if !ok {
			continue
		}
		c := m.conversations[cid]
		if typ != "" && c.Type != typ {
			continue
		}
		var dmUserID *int64
		if c.Type == "dm" && c.DmUserA != nil && c.DmUserB != nil {
			other := *c.DmUserA
			if other == userID {
				other = *c.DmUserB
			}
			dmUserID = &other
		}
		var last *string
		if r.LastMessageAt != nil {
			s := toIso(*r.LastMessageAt)
			last = &s
		}
		receipts, _ := m.GetReadReceipts(context.Background(), cid)
		out = append(out, types.ConversationItem{
			ConversationID: cid, Type: c.Type, GroupID: c.GroupID, DmUserID: dmUserID,
			Pinned: r.Pinned, LastMessageAt: last, LastPreview: r.LastPreview, Unread: r.Unread, ReadReceipts: receipts,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pinned != out[j].Pinned {
			return out[i].Pinned
		}
		return false
	})
	return out, nil
}

// ==================== 消息 ====================

func (m *MemoryStore) SaveMessage(_ context.Context, conversationID, fromID int64, kind, content string) (*types.ChatMessage, error) {
	id := m.nextId()
	m.messages[id] = &types.MessageRow{ID: id, ConversationID: conversationID, FromID: fromID, Kind: kind, Content: content, CreatedAt: time.Now().UTC()}
	return m.GetMessage(context.Background(), conversationID, fmt.Sprintf("%d", id))
}

func (m *MemoryStore) ListMessages(_ context.Context, conversationID, beforeTs, limit int64) ([]types.ChatMessage, error) {
	if limit <= 0 {
		limit = 50
	}
	type kv struct {
		id int64
		r  *types.MessageRow
	}
	arr := []kv{}
	for _, r := range m.messages {
		if r.ConversationID != conversationID || r.DeletedAt != nil {
			continue
		}
		if beforeTs > 0 && r.CreatedAt.UnixMilli() >= beforeTs {
			continue
		}
		arr = append(arr, kv{r.ID, r})
	}
	sort.Slice(arr, func(i, j int) bool { return arr[i].r.CreatedAt.After(arr[j].r.CreatedAt) })
	out := []types.ChatMessage{}
	for i, e := range arr {
		if int64(i) >= limit {
			break
		}
		out = append(out, types.ChatMessage{ID: fmt.Sprintf("%d", e.r.ID), ConversationID: e.r.ConversationID, Type: m.convType(conversationID), FromID: e.r.FromID, Kind: e.r.Kind, Content: e.r.Content, Ts: e.r.CreatedAt.UnixMilli()})
	}
	return out, nil
}

func (m *MemoryStore) GetMessage(_ context.Context, conversationID int64, messageID string) (*types.ChatMessage, error) {
	var id int64
	fmt.Sscanf(messageID, "%d", &id)
	if r, ok := m.messages[id]; ok && r.ConversationID == conversationID {
		return &types.ChatMessage{ID: messageID, ConversationID: r.ConversationID, Type: m.convType(conversationID), FromID: r.FromID, Kind: r.Kind, Content: r.Content, Ts: r.CreatedAt.UnixMilli()}, nil
	}
	return nil, nil
}

func (m *MemoryStore) FindOwnMessageByContent(_ context.Context, conversationID, userID int64, content string) (*types.ChatMessage, error) {
	return m.findOwnMsg(conversationID, userID, content)
}

func (m *MemoryStore) FindOwnMessageByContentGlobal(_ context.Context, userID int64, content string) (*types.ChatMessage, error) {
	return m.findOwnMsg(0, userID, content)
}

func (m *MemoryStore) findOwnMsg(conversationID, userID int64, content string) (*types.ChatMessage, error) {
	var best *types.MessageRow
	for _, r := range m.messages {
		if r.FromID != userID || r.Content != content || r.DeletedAt != nil {
			continue
		}
		if conversationID != 0 && r.ConversationID != conversationID {
			continue
		}
		if best == nil || r.CreatedAt.After(best.CreatedAt) {
			best = r
		}
	}
	if best == nil {
		return nil, nil
	}
	return &types.ChatMessage{ID: fmt.Sprintf("%d", best.ID), ConversationID: best.ConversationID, Type: m.convType(best.ConversationID), FromID: best.FromID, Kind: best.Kind, Content: best.Content, Ts: best.CreatedAt.UnixMilli()}, nil
}

func (m *MemoryStore) SoftDeleteMessage(_ context.Context, conversationID int64, messageID string) (bool, error) {
	var id int64
	fmt.Sscanf(messageID, "%d", &id)
	if r, ok := m.messages[id]; ok && r.ConversationID == conversationID && r.DeletedAt == nil {
		now := time.Now().UTC()
		r.DeletedAt = &now
		return true, nil
	}
	return false, nil
}

func (m *MemoryStore) HardDeleteMessage(_ context.Context, conversationID int64, messageID string) (bool, error) {
	var id int64
	fmt.Sscanf(messageID, "%d", &id)
	if r, ok := m.messages[id]; ok && r.ConversationID == conversationID {
		delete(m.messages, id)
		return true, nil
	}
	return false, nil
}

func (m *MemoryStore) TouchConversation(_ context.Context, conversationID, userID int64, preview string, fromID int64) error {
	if set := m.convMem[conversationID]; set != nil {
		if r, ok := set[userID]; ok {
			now := time.Now().UTC()
			r.LastMessageAt = &now
			p := preview
			if len(p) > 200 {
				p = p[:200]
			}
			r.LastPreview = &p
			if fromID != 0 && fromID != userID {
				r.Unread++
			}
		}
	}
	return nil
}

func (m *MemoryStore) MarkRead(_ context.Context, conversationID, userID int64, lastReadMessageID *int64) error {
	if set := m.convMem[conversationID]; set != nil {
		if r, ok := set[userID]; ok {
			r.Unread = 0
			r.LastReadMessageID = lastReadMessageID
		}
	}
	return nil
}

func (m *MemoryStore) GetReadReceipts(_ context.Context, conversationID int64) ([]types.ReadReceipt, error) {
	out := []types.ReadReceipt{}
	if set := m.convMem[conversationID]; set != nil {
		for _, r := range set {
			out = append(out, types.ReadReceipt{UserID: r.UserID, LastReadMessageID: r.LastReadMessageID})
		}
	}
	return out, nil
}

// ==================== 文件 ====================

func (m *MemoryStore) SaveFile(_ context.Context, in SaveFileInput) (*types.FileRecord, error) {
	storage := in.Storage
	if storage == "" {
		storage = "local"
	}
	m.files[in.UUID] = &types.FileRow{UUID: in.UUID, Filename: in.Filename, Mime: in.Mime, Size: in.Size, Storage: storage, URL: in.URL, CreatedAt: time.Now().UTC()}
	return &types.FileRecord{UUID: in.UUID, Filename: in.Filename, Mime: in.Mime, Size: in.Size, Storage: storage, URL: in.URL, CreatedAt: toIso(m.files[in.UUID].CreatedAt)}, nil
}

func (m *MemoryStore) GetFileByUUID(_ context.Context, uuid string) (*types.FileRecord, error) {
	if r, ok := m.files[uuid]; ok {
		return &types.FileRecord{UUID: r.UUID, Filename: r.Filename, Mime: r.Mime, Size: r.Size, Storage: r.Storage, URL: r.URL, CreatedAt: toIso(r.CreatedAt)}, nil
	}
	return nil, nil
}

// ==================== 好友 ====================

func (m *MemoryStore) AddFriend(_ context.Context, a, b int64) error {
	x, y := normPair(a, b)
	k := pairKey(x, y)
	if _, ok := m.friends[k]; ok {
		return &AlreadyExistsError{Msg: "already friends"}
	}
	m.friends[k] = &types.FriendRow{UserA: x, UserB: y, CreatedAt: time.Now().UTC()}
	return nil
}

func (m *MemoryStore) RemoveFriend(_ context.Context, a, b int64) error {
	x, y := normPair(a, b)
	k := pairKey(x, y)
	if _, ok := m.friends[k]; ok {
		delete(m.friends, k)
		return nil
	}
	return &NotFoundError{Msg: "not friends"}
}

func (m *MemoryStore) ListFriends(_ context.Context, userID int64) ([]types.Friend, error) {
	out := []types.Friend{}
	for _, r := range m.friends {
		other := r.UserA
		if other == userID {
			other = r.UserB
		}
		u, err := m.GetUserByID(context.Background(), other)
		if err != nil || u == nil {
			continue
		}
		nick := u.Nick
		if nick == "" {
			nick = u.Username
		}
		out = append(out, types.Friend{UserID: u.ID, Username: u.Username, Nick: nick, Avatar: u.Avatar, AddedAt: toIso(r.CreatedAt)})
	}
	return out, nil
}
