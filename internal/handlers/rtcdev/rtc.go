// Package rtcdev 是 WebRTC 通话/远程设备/系统监控三组 socket 事件 handler，
// 1:1 复刻自源仓库 server/src/handlers/rtc.ts、dev.ts、sysmon.ts。
//
// 本文件（rtc.go）对应 rtc.ts：WebRTC 信令房间转发层。
//
// 房间命名规则（照 TS 内联字面量，不臆造）：
//   - 信令房间：rtc:{rtcId}（rtcRoom 内联拼前缀）；
//   - conf（会议）：rtcId == 6 位会议号，预约创建/直接开始，开始+结束后删除；
//   - dm（私聊通话）：rtcId = dm:{小userId}:{大userId}；
//   - group（群视频/语音）：rtcId = group:{groupId}（复用 util.GroupRoom 的字面量）；
//   - perm（永久房间，K歌房等）：rtcId = perm:{ts}:{rand}，空房不删。
//
// 会话表 rooms 为包级进程内状态（对标 TS 的 new Map）；Go 多协程并发访问，
// 一律持 roomsMu 互斥锁保护。密码为临时会话口令，只存内存、绝不进日志。
package rtcdev

import (
	"context"
	"encoding/json"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"mochi-oa-server-go/internal/config"
	"mochi-oa-server-go/internal/db"
	"mochi-oa-server-go/internal/ice"
	"mochi-oa-server-go/internal/logger"
	"mochi-oa-server-go/internal/socketapi"
	"mochi-oa-server-go/internal/types"
	"mochi-oa-server-go/internal/util"
)

// module 是日志模块前缀（全组共用）。
const module = "handlers/rtcdev"

// Ctx 是 handler 注册所需的依赖集合（对标 TS 的 {io, store, config}）。
type Ctx struct {
	Srv    socketapi.Server
	Store  db.Store
	Config *config.ServerConfig
}

// ---- 连接态键（对标 TS socket.data.*） ----
const (
	keyAuth     = "auth"     // *types.Auth，认证中间件写入
	keyRtc      = "rtc"      // map{userId,nick,avatar}，加入 RTC 房间后写入
	keyRtcRooms = "rtcRooms" // map[string]bool，本连接所在的 RTC 房间集合
	keyDeviceID = "deviceId" // 被控端设备号（dev.go 使用）
	keyIsSlave  = "isSlave"  // bool，被控端连接标记（dev.go 使用）
)

// scheduleMaxMs 是预约会议未开始的最长保留时长：356 天（照 TS 常量）。
const scheduleMaxMs = int64(356 * 86400000)

// rtcSession 是一次会议/通话的内存会话（对标 RtcSession）。
type rtcSession struct {
	rtcID     string
	typ       string // conf|dm|group|perm
	kind      string // video|voice|remote
	meetingNo string // 仅 conf
	title     string // 空串=未设置（JSON 省略该键）
	password  string // 空串=无密码（绝不进日志）
	hostID    int64
	groupID   int64 // 0=无
	dmA       int64 // 仅 dm
	dmB       int64 // 仅 dm
	createdAt int64
	startedAt *int64 // nil=未开始
	ended     bool
}

// rtcPeer 是房间内在线参与者视图（含头像，供语音通话头像列表）。
type rtcPeer struct {
	SocketID string `json:"socketId"`
	UserID   int64  `json:"userId"`
	Nick     string `json:"nick"`
	Avatar   string `json:"avatar"`
}

// RTC 会话表：rtcId -> *rtcSession（对标 TS 的 rooms Map）。
// 包级进程内注册表；Go 多协程并发，统一由 roomsMu 保护。
var (
	roomsMu sync.Mutex
	rooms   = map[string]*rtcSession{}
)

var reMeetingNo = regexp.MustCompile(`^\d{6}$`)

// ==================== 共享小工具（三组 handler 共用） ====================

// authed 从连接取出认证用户（对标 socket.data.auth）。未登录返回 nil。
func authed(c socketapi.Conn) *types.Auth {
	v := c.Get(keyAuth)
	if v == nil {
		return nil
	}
	switch a := v.(type) {
	case *types.Auth:
		return a
	case types.Auth:
		return &a
	}
	return nil
}

// isServerAdmin 判定 SERVER_ADMIN：用户名大小写不敏感匹配 config.ServerAdmins。
// （TS mail.ts 另有运行时 serverAdminOverrides 集合，属 admin handler 移植范围，此处不实现。）
func isServerAdmin(a *types.Auth, cfg *config.ServerConfig) bool {
	if a == nil || cfg == nil {
		return false
	}
	n := strings.ToLower(a.Username)
	for _, u := range cfg.ServerAdmins {
		if strings.ToLower(u) == n {
			return true
		}
	}
	return false
}

// connByID 按连接 id 找连接（对标 io.sockets.sockets.get(sid)）。找不到返回 nil。
// socketapi 无连接表查询接口，遍历 Sockets() 等价实现。
func connByID(srv socketapi.Server, id string) socketapi.Conn {
	if id == "" {
		return nil
	}
	for _, c := range srv.Sockets() {
		if c.ID() == id {
			return c
		}
	}
	return nil
}

// nowMs 当前毫秒时间戳（对标 Date.now()）。
func nowMs() int64 { return time.Now().UnixMilli() }

// asInt64 把 JSON 数字（float64）等宽转 int64；非数字返回 0。
func asInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	}
	return 0
}

// reqInt64Ptr 检查 *float64 是否为合法正整数 id（对标 Number.isInteger(x) && x>0）。
func validPosInt(v *float64) (int64, bool) {
	if v == nil {
		return 0, false
	}
	n := *v
	if n <= 0 || n != float64(int64(n)) {
		return 0, false
	}
	return int64(n), true
}

// ==================== RTC 内部函数（对标 rtc.ts 闭包内函数） ====================

// rtcRoom 拼信令房间名：rtc:{rtcId}（照 TS 内联规则）。
func rtcRoom(rtcID string) string { return "rtc:" + rtcID }

// makeMeetingNo 生成不冲突的 6 位会议号（100000-999999）。
func makeMeetingNo() string {
	roomsMu.Lock()
	defer roomsMu.Unlock()
	for {
		n := 100000 + rand.Intn(900000)
		id := itoa6(n)
		if _, taken := rooms[id]; !taken {
			return id
		}
	}
}

// itoa6 把会议号转定长 6 位字符串（rand 范围保证 6 位）。
func itoa6(n int) string {
	if n < 100000 || n > 999999 {
		return ""
	}
	b := [6]byte{}
	for i := 5; i >= 0; i-- {
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[:])
}

// expireSweep 清除超 356 天仍未开始的预约会议（会议号作废）。
func expireSweep() {
	cutoff := nowMs() - scheduleMaxMs
	roomsMu.Lock()
	for id, r := range rooms {
		if r.typ == "conf" && r.startedAt == nil && r.createdAt < cutoff {
			delete(rooms, id)
		}
	}
	roomsMu.Unlock()
}

// peerState 从连接读 RTC 参与者状态（对标 socket.data.rtc）。
func peerState(c socketapi.Conn) (userID int64, nick, avatar string, ok bool) {
	v := c.Get(keyRtc)
	m, _ := v.(map[string]any)
	if m == nil {
		return 0, "", "", false
	}
	return asInt64(m["userId"]), strOf(m["nick"]), strOf(m["avatar"]), true
}

// strOf any→string 兜底。
func strOf(v any) string {
	if s, _ := v.(string); s != "" {
		return s
	}
	return ""
}

// peerOf 取该连接的参与者信息（含头像；无 rtc 态回退 auth）。
func peerOf(c socketapi.Conn) rtcPeer {
	if uid, nick, avatar, ok := peerState(c); ok {
		return rtcPeer{SocketID: c.ID(), UserID: uid, Nick: nick, Avatar: avatar}
	}
	a := authed(c)
	if a != nil {
		return rtcPeer{SocketID: c.ID(), UserID: a.ID, Nick: a.Nick}
	}
	return rtcPeer{SocketID: c.ID()}
}

// peersInfo 列出某 RTC 房间内全部在线参与者（socketId 维度，支持多端同时在线）。
func peersInfo(srv socketapi.Server, rtcID string) []rtcPeer {
	room := rtcRoom(rtcID)
	out := []rtcPeer{}
	for _, c := range srv.Sockets() {
		inRoom := false
		for _, r := range c.Rooms() {
			if r == room {
				inRoom = true
				break
			}
		}
		if !inRoom {
			continue
		}
		out = append(out, peerOf(c))
	}
	return out
}

// view 输出会话视图（对标 view()；可选键未设置时省略，与 TS undefined 一致）。
func (r *rtcSession) view() map[string]any {
	m := map[string]any{
		"id":          r.rtcID,
		"type":        r.typ,
		"kind":        r.kind,
		"hasPassword": r.password != "",
		"hostId":      r.hostID,
		"started":     r.startedAt != nil,
		"createdAt":   r.createdAt,
	}
	if r.meetingNo != "" {
		m["meetingNo"] = r.meetingNo
	}
	if r.title != "" {
		m["title"] = r.title
	}
	if r.groupID != 0 {
		m["groupId"] = r.groupID
	}
	return m
}

// rtcRoomsOf 读连接所在的 RTC 房间集合（对标 socket.data.rtcRooms Set）。
func rtcRoomsOf(c socketapi.Conn) map[string]bool {
	m, _ := c.Get(keyRtcRooms).(map[string]bool)
	return m
}

// addSocket 把连接挂入信令房间、写入参与者态（对标 addSocket）。
func addSocket(srv socketapi.Server, st db.Store, c socketapi.Conn, rtcID string, u *types.Auth) {
	_ = c.Join(rtcRoom(rtcID))
	avatar := ""
	if st != nil {
		if user, err := st.GetUserByID(context.Background(), u.ID); err == nil && user != nil {
			avatar = user.Avatar
		} else if err != nil {
			logger.Error(module, "addSocket 取头像失败", "user", u.ID, "error", err.Error())
		}
	}
	c.Set(keyRtc, map[string]any{"userId": u.ID, "nick": u.Nick, "avatar": avatar})
	set := rtcRoomsOf(c)
	if set == nil {
		set = map[string]bool{}
	}
	set[rtcID] = true
	c.Set(keyRtcRooms, set)
}

// removeSocket 连接退出信令房间并清参与者态（对标 removeSocket）。
func removeSocket(c socketapi.Conn, rtcID string) {
	_ = c.Leave(rtcRoom(rtcID))
	if set := rtcRoomsOf(c); set != nil {
		delete(set, rtcID)
		c.Set(keyRtcRooms, set)
	}
	if _, _, _, ok := peerState(c); ok {
		c.Set(keyRtc, nil)
	}
}

// broadcastRoomsChanged 进行中房间列表变化 → 通知所有在线端（/view/room 实时刷新）。
func broadcastRoomsChanged(srv socketapi.Server) {
	_ = srv.Broadcast("rtc:roomsChanged", map[string]any{"ts": nowMs()})
}

// cleanupIfEmpty 房间空 → 结束会话：conf/dm/group 空房即删；perm 空房常驻。
func cleanupIfEmpty(srv socketapi.Server, rtcID string) {
	roomsMu.Lock()
	r := rooms[rtcID]
	roomsMu.Unlock()
	if r == nil {
		return
	}
	if len(peersInfo(srv, rtcID)) > 0 {
		return
	}
	if r.typ == "perm" {
		broadcastRoomsChanged(srv) // 永久房间：没人也常驻
		return
	}
	roomsMu.Lock()
	r.ended = true
	delete(rooms, rtcID)
	roomsMu.Unlock()
	broadcastRoomsChanged(srv)
}

// forceEnd 强制结束：通知所有参与者、踢出房间、删除会话（会议号作废）。
func forceEnd(srv socketapi.Server, rtcID string, by map[string]any) {
	roomsMu.Lock()
	r := rooms[rtcID]
	if r != nil {
		r.ended = true
		delete(rooms, rtcID)
	}
	roomsMu.Unlock()
	if r == nil {
		return
	}
	_ = srv.EmitTo(rtcRoom(rtcID), "rtc:ended", map[string]any{
		"room":   rtcID,
		"reason": "ended",
		"by":     by,
	})
	broadcastRoomsChanged(srv)
	// 把房间内所有连接踢出（对标 for sid of rooms.get(rtcRoom) → leave）
	room := rtcRoom(rtcID)
	for _, c := range srv.Sockets() {
		for _, rm := range c.Rooms() {
			if rm == room {
				if set := rtcRoomsOf(c); set != nil {
					delete(set, rtcID)
					c.Set(keyRtcRooms, set)
				}
				_ = c.Leave(room)
				break
			}
		}
	}
}

// liveRooms 进行中的房间列表（网页端 /view/room 拉取）。
func liveRooms(srv socketapi.Server) []map[string]any {
	roomsMu.Lock()
	out := []map[string]any{}
	for _, r := range rooms {
		if r.ended || r.startedAt == nil {
			continue
		}
		e := map[string]any{
			"id":          r.rtcID,
			"type":        r.typ,
			"kind":        r.kind,
			"hasPassword": r.password != "",
			"hostId":      r.hostID,
			"peerCount":   len(peersInfo(srv, r.rtcID)),
			"startedAt":   *r.startedAt,
		}
		if r.meetingNo != "" {
			e["meetingNo"] = r.meetingNo
		}
		if r.title != "" {
			e["title"] = r.title
		}
		if r.groupID != 0 {
			e["groupId"] = r.groupID
		}
		out = append(out, e)
	}
	roomsMu.Unlock()
	return out
}

// ==================== 请求体结构 ====================

type rtcCreateMeetingReq struct {
	Title    *string `json:"title"`
	StartAt  any     `json:"startAt"`
	Password *string `json:"password"`
	Kind     *string `json:"kind"`
}

type rtcCreateRoomReq struct {
	Type     *string  `json:"type"`
	Title    *string  `json:"title"`
	Kind     *string  `json:"kind"`
	GroupID  *float64 `json:"groupId"`
	Password *string  `json:"password"`
}

type rtcGetMeetingReq struct {
	MeetingNo *string `json:"meetingNo"`
}

type rtcJoinReq struct {
	RoomID   *string `json:"roomId"`
	Password *string `json:"password"`
	Kind     *string `json:"kind"`
}

type rtcRoomIDReq struct {
	RoomID *string `json:"roomId"`
}

type rtcChatReq struct {
	RoomID  *string `json:"roomId"`
	Content *string `json:"content"`
}

type rtcDmCallReq struct {
	UserID   *float64 `json:"userId"`
	Kind     *string  `json:"kind"`
	Password *string  `json:"password"`
}

type rtcDmAnswerReq struct {
	RoomID *string `json:"roomId"`
	Accept *bool   `json:"accept"`
}

type rtcGroupCallReq struct {
	GroupID *float64 `json:"groupId"`
	Kind    *string  `json:"kind"`
}

// trimStr 安全取串：nil→""，再 TrimSpace。
func trimStr(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

// ==================== 注册入口 ====================

// RegisterRtc 注册全部 RTC 通话事件（对标 registerRtcHandlers）。
func RegisterRtc(c Ctx) {
	srv, st, cfg := c.Srv, c.Store, c.Config

	// 每小时清理超期预约会议（对标 setInterval(expireSweep, 3600000)）。
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for range t.C {
			expireSweep()
		}
	}()

	// ---- 创建会议（会议号，可预约或直接开始） ----
	srv.On("rtc:createMeeting", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		u := authed(c)
		if u == nil {
			ack(util.Fail("未登录"))
			return
		}
		var d rtcCreateMeetingReq
		_ = json.Unmarshal(raw, &d)
		expireSweep()
		meetingNo := makeMeetingNo()
		kind := "video"
		if d.Kind != nil && *d.Kind == "voice" {
			kind = "voice"
		}
		title := ""
		if d.Title != nil {
			if t := strings.TrimSpace(*d.Title); t != "" {
				title = truncate(t, 64)
			}
		}
		password := ""
		if d.Password != nil && *d.Password != "" {
			password = truncate(*d.Password, 32)
		}
		sess := &rtcSession{
			rtcID: meetingNo, typ: "conf", kind: kind, meetingNo: meetingNo,
			title: title, password: password, hostID: u.ID,
			createdAt: nowMs(), ended: false,
		}
		roomsMu.Lock()
		rooms[meetingNo] = sess
		roomsMu.Unlock()
		// 预约会议：不立即开始，保留会议号，由首个加入者开始。
		scheduled := d.StartAt != nil && d.StartAt != ""
		if scheduled {
			res := map[string]any{"meeting": sess.view(), "scheduled": true}
			if liveKitEnabled(cfg.LiveKit) {
				// 预约不签发 token（token 有效期 2h 可能盖不住预约时长）；先下发 url，token 由 rtc:join 时签发。
				res["url"] = cfg.LiveKit.URL
				res["roomName"] = liveKitRoomName(cfg.LiveKit, meetingNo)
			}
			ack(util.OK(res))
			return
		}
		// 直接开始：主持人加入房间
		ts := nowMs()
		sess.startedAt = &ts
		addSocket(srv, st, c, meetingNo, u)
		broadcastRoomsChanged(srv)
		// SFU：主持人为首个加入者，一并下发连接信息。
		sfuURL, sfuToken, sfuRoomName := joinSfuFields(cfg, meetingNo, u.ID)
		ack(util.OK(map[string]any{
			"meeting":    sess.view(),
			"scheduled":  false,
			"peers":      filterPeers(srv, meetingNo, c.ID()),
			"iceServers": ice.GetIceServers(cfg),
			"url":        sfuURL,
			"token":      sfuToken,
			"roomName":   sfuRoomName,
		}))
	})

	// ---- 管理后台创建房间（SERVER_ADMIN）：会议/群聊/永久 ----
	srv.On("rtc:createRoom", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		u := authed(c)
		if u == nil {
			ack(util.Fail("未登录"))
			return
		}
		if !isServerAdmin(u, cfg) {
			ack(util.Fail("需要 SERVER_ADMIN 权限"))
			return
		}
		var d rtcCreateRoomReq
		_ = json.Unmarshal(raw, &d)
		typ := "conf"
		if d.Type != nil && (*d.Type == "group" || *d.Type == "perm") {
			typ = *d.Type
		}
		kind := "video"
		if d.Kind != nil && *d.Kind == "voice" {
			kind = "voice"
		}
		password := ""
		if d.Password != nil && *d.Password != "" {
			password = truncate(*d.Password, 32)
		}
		title := ""
		if d.Title != nil {
			if t := strings.TrimSpace(*d.Title); t != "" {
				title = truncate(t, 64)
			}
		}
		rtcID := ""
		var groupID int64
		switch typ {
		case "conf":
			rtcID = makeMeetingNo()
			if title == "" {
				title = rtcID // 会议默认标题=会议号
			}
		case "group":
			gid, ok := validPosInt(d.GroupID)
			if !ok {
				ack(util.Fail("群聊房间需选择群"))
				return
			}
			g, err := st.GetGroupByID(context.Background(), gid)
			if err != nil || g == nil {
				ack(util.Fail("群不存在"))
				return
			}
			groupID = gid
			rtcID = "group:" + itoaPositive(gid)
			roomsMu.Lock()
			_, taken := rooms[rtcID]
			roomsMu.Unlock()
			if taken {
				ack(util.Fail("该群已有进行中的群通话"))
				return
			}
			if title == "" {
				title = g.Name // 群聊默认标题=群名
			}
		default: // perm
			if title == "" {
				ack(util.Fail("永久房间需要填写标题"))
				return
			}
			rtcID = "perm:" + itoaPositive(nowMs()) + ":" + itoaPositive(int64(rand.Intn(1000)))
		}
		ts := nowMs()
		sess := &rtcSession{
			rtcID: rtcID, typ: typ, kind: kind, title: title, password: password,
			hostID: u.ID, groupID: groupID, createdAt: ts, startedAt: &ts, ended: false,
		}
		if typ == "conf" {
			sess.meetingNo = rtcID
		}
		roomsMu.Lock()
		rooms[rtcID] = sess
		roomsMu.Unlock()
		broadcastRoomsChanged(srv)
		logger.Info(module, "rtc:createRoom", "admin", u.ID, "type", typ, "room", rtcID, "kind", kind)
		ack(util.OK(map[string]any{"room": sess.view()}))
	})

	// ---- 查询会议信息（预约会议按会议号查） ----
	srv.On("rtc:getMeeting", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		u := authed(c)
		if u == nil {
			ack(util.Fail("未登录"))
			return
		}
		var d rtcGetMeetingReq
		_ = json.Unmarshal(raw, &d)
		meetingNo := trimStr(d.MeetingNo)
		if !reMeetingNo.MatchString(meetingNo) {
			ack(util.Fail("会议号格式不正确"))
			return
		}
		expireSweep()
		roomsMu.Lock()
		r := rooms[meetingNo]
		roomsMu.Unlock()
		if r == nil || r.typ != "conf" {
			ack(util.Fail("会议不存在或已结束"))
			return
		}
		ack(util.OK(map[string]any{"meeting": r.view()}))
	})

	// ---- 查询所有进行中的会议/通话（仅 SERVER_ADMIN） ----
	srv.On("rtc:listRooms", func(c socketapi.Conn, _ json.RawMessage, ack socketapi.Ack) {
		u := authed(c)
		if u == nil {
			ack(util.Fail("未登录"))
			return
		}
		if !isServerAdmin(u, cfg) {
			ack(util.Fail("需要 SERVER_ADMIN 权限"))
			return
		}
		expireSweep()
		ack(util.OK(map[string]any{"rooms": liveRooms(srv)}))
	})

	// ---- 加入房间（会议按会议号；dm/group 按 rtcId） ----
	srv.On("rtc:join", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		u := authed(c)
		if u == nil {
			ack(util.Fail("未登录"))
			return
		}
		var d rtcJoinReq
		_ = json.Unmarshal(raw, &d)
		roomID := trimStr(d.RoomID)
		if roomID == "" {
			ack(util.Fail("缺少房间号/会议号"))
			return
		}
		expireSweep()
		roomsMu.Lock()
		r := rooms[roomID]
		roomsMu.Unlock()
		if r == nil {
			ack(util.Fail("会议或房间不存在"))
			return
		}
		if r.ended {
			ack(util.Fail("会议已结束"))
			return
		}
		// 会议密码门控：文案含「密码」，客户端据此硬编码弹密码框，逐字保留。
		if r.typ == "conf" && r.password != "" && (d.Password == nil || *d.Password != r.password) {
			ack(util.Fail("会议密码错误"))
			return
		}
		wasStarted := r.startedAt != nil
		if !wasStarted {
			ts := nowMs()
			r.startedAt = &ts // 首个加入者即开始会议
		}
		addSocket(srv, st, c, roomID, u)
		_ = srv.EmitToSkip(rtcRoom(roomID), "rtc:peerJoined",
			map[string]any{"room": roomID, "peer": peerOf(c)}, c.ID())
		broadcastRoomsChanged(srv)
		logger.Info(module, "rtc:join", "user", u.ID, "room", roomID, "type", r.typ,
			"已有成员", len(peersInfo(srv, roomID))-1)
		// SFU：启用 LiveKit 时为加入者签发 access token（客户端据此连 SFU，不再走 rtc:signal P2P 媒体信令）。
		sfuURL, sfuToken, sfuRoomName := joinSfuFields(cfg, roomID, u.ID)
		ack(util.OK(map[string]any{
			"room":       r.view(),
			"peers":      filterPeers(srv, roomID, c.ID()),
			"iceServers": ice.GetIceServers(cfg),
			"started":    wasStarted,
			// SFU 化：启用 LiveKit 时下发连接信息，客户端据此连 SFU（不再走 rtc:signal P2P 媒体信令）。
			"url":      sfuURL,
			"token":    sfuToken,
			"roomName": sfuRoomName,
		}))
	})

	// ---- 房间内嵌聊天（与信令分离，广播给房间内其他参与者） ----
	srv.On("rtc:chatMessage", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		u := authed(c)
		if u == nil {
			ack(util.Fail("未登录"))
			return
		}
		var d rtcChatReq
		_ = json.Unmarshal(raw, &d)
		roomID := trimStr(d.RoomID)
		if roomID == "" {
			ack(util.Fail("缺少房间号"))
			return
		}
		content := trimStr(d.Content)
		if content == "" {
			ack(util.Fail("消息内容为空"))
			return
		}
		if len(content) > 2000 {
			ack(util.Fail("消息内容过长"))
			return
		}
		roomsMu.Lock()
		_, ok := rooms[roomID]
		roomsMu.Unlock()
		if !ok {
			ack(util.Fail("房间不存在"))
			return
		}
		inRoom := false
		if set := rtcRoomsOf(c); set != nil {
			inRoom = set[roomID]
		}
		if !inRoom {
			ack(util.Fail("不在该房间中"))
			return
		}
		_ = srv.EmitToSkip(rtcRoom(roomID), "rtc:chatMessage",
			map[string]any{"room": roomID, "from": peerOf(c), "content": content, "ts": nowMs()}, c.ID())
		ack(util.OK(nil))
	})

	// ---- 离开房间 ----
	srv.On("rtc:leave", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		var d rtcRoomIDReq
		_ = json.Unmarshal(raw, &d)
		roomID := ""
		if d.RoomID != nil {
			roomID = *d.RoomID
		}
		if roomID == "" {
			ack(util.Fail("缺少房间号"))
			return
		}
		removeSocket(c, roomID)
		_ = srv.EmitToSkip(rtcRoom(roomID), "rtc:peerLeft",
			map[string]any{"room": roomID, "peerId": c.ID()}, c.ID())
		cleanupIfEmpty(srv, roomID)
		broadcastRoomsChanged(srv)
		ack(util.OK(nil))
	})

	// ---- 结束会议/通话 ----
	srv.On("rtc:end", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		u := authed(c)
		var d rtcRoomIDReq
		_ = json.Unmarshal(raw, &d)
		roomID := ""
		if d.RoomID != nil {
			roomID = *d.RoomID
		}
		if roomID == "" {
			ack(util.Fail("缺少房间号"))
			return
		}
		roomsMu.Lock()
		_, ok := rooms[roomID]
		roomsMu.Unlock()
		if !ok {
			ack(util.Fail("房间不存在"))
			return
		}
		by := map[string]any{"userId": int64(0), "nick": ""}
		if u != nil {
			by["userId"] = u.ID
			by["nick"] = u.Nick
		}
		forceEnd(srv, roomID, by)
		ack(util.OK(nil))
	})

	// ---- 私聊视频/语音/远程控制：主叫方发起 ----
	srv.On("rtc:dmCall", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		u := authed(c)
		if u == nil {
			ack(util.Fail("未登录"))
			return
		}
		var d rtcDmCallReq
		_ = json.Unmarshal(raw, &d)
		targetID, ok := validPosInt(d.UserID)
		if !ok {
			ack(util.Fail("参数不合法"))
			return
		}
		if targetID == u.ID {
			ack(util.Fail("不能呼叫自己"))
			return
		}
		kind := "video"
		if d.Kind != nil {
			switch *d.Kind {
			case "voice":
				kind = "voice"
			case "remote":
				kind = "remote"
			}
		}
		// 远程控制：若被控方设了远程控制密码（extra.remotePassword），主叫必须输对。
		// 错误文案含「密码」，客户端据此弹密码框，逐字保留。
		if kind == "remote" {
			if t, err := st.GetUserByID(context.Background(), targetID); err == nil && t != nil && t.Extra != "" {
				var ex struct {
					RemotePassword string `json:"remotePassword"`
				}
				if json.Unmarshal([]byte(t.Extra), &ex) == nil && ex.RemotePassword != "" {
					if d.Password == nil || *d.Password != ex.RemotePassword {
						ack(util.Fail("对方设置了远程控制密码，密码错误"))
						return
					}
				}
			} else if err != nil {
				logger.Error(module, "rtc:dmCall 取对端失败", "user", targetID, "error", err.Error())
			}
		}
		lo, hi := u.ID, targetID
		if lo > hi {
			lo, hi = hi, lo
		}
		rtcID := "dm:" + itoaPositive(lo) + ":" + itoaPositive(hi)
		roomsMu.Lock()
		sess := rooms[rtcID]
		if sess == nil {
			ts := nowMs()
			sess = &rtcSession{
				rtcID: rtcID, typ: "dm", kind: kind, hostID: u.ID,
				dmA: lo, dmB: hi, createdAt: ts, startedAt: &ts, ended: false,
			}
			rooms[rtcID] = sess
		} else if sess.ended {
			roomsMu.Unlock()
			ack(util.Fail("通话已结束"))
			return
		}
		roomsMu.Unlock()
		addSocket(srv, st, c, rtcID, u)
		// SFU：为双方签发同一 room 的 token；控制端（发起方）从 ack 拿，被控端从 rtc:dmIncoming 拿。
		sfuURL, sfuToken, sfuRoomName := joinSfuFields(cfg, rtcID, u.ID)
		dmIncoming := map[string]any{"room": rtcID, "kind": kind, "from": peerOf(c), "ts": nowMs()}
		if sfuRoomName != "" {
			slaveTok, err := signLiveKitToken(cfg.LiveKit, strconv.FormatInt(targetID, 10), sfuRoomName)
			if err == nil {
				dmIncoming["url"] = sfuURL
				dmIncoming["token"] = slaveTok
				dmIncoming["roomName"] = sfuRoomName
			} else {
				logger.Warn(module, "rtc:dmCall 签被控端 LiveKit token 失败", "user", targetID, "room", rtcID, "error", err)
			}
		}
		// 通知对方所有在线端
		_ = srv.EmitTo(util.UserRoom(targetID), "rtc:dmIncoming", dmIncoming)
		broadcastRoomsChanged(srv)
		ack(util.OK(map[string]any{
			"room":       sess.view(),
			"kind":       kind,
			"iceServers": ice.GetIceServers(cfg),
			// SFU：发起方进入房间，一并下发连接信息。
			"url":      sfuURL,
			"token":    sfuToken,
			"roomName": sfuRoomName,
		}))
	})

	// ---- 私聊通话：被叫方应答（accept=true 加入房间） ----
	srv.On("rtc:dmAnswer", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		u := authed(c)
		if u == nil {
			ack(util.Fail("未登录"))
			return
		}
		var d rtcDmAnswerReq
		_ = json.Unmarshal(raw, &d)
		roomID := ""
		if d.RoomID != nil {
			roomID = *d.RoomID
		}
		roomsMu.Lock()
		sess := rooms[roomID]
		exists := sess != nil && sess.typ == "dm"
		roomsMu.Unlock()
		if !exists {
			ack(util.Fail("通话不存在"))
			return
		}
		if d.Accept != nil && *d.Accept {
			addSocket(srv, st, c, roomID, u)
			_ = srv.EmitToSkip(rtcRoom(roomID), "rtc:peerJoined",
				map[string]any{"room": roomID, "peer": peerOf(c)}, c.ID())
			broadcastRoomsChanged(srv)
			// SFU：被叫方接受并进入房间，一并下发连接信息。
			sfuURL, sfuToken, sfuRoomName := joinSfuFields(cfg, roomID, u.ID)
			ack(util.OK(map[string]any{
				"room":       sess.view(),
				"peers":      filterPeers(srv, roomID, c.ID()),
				"iceServers": ice.GetIceServers(cfg),
				"url":        sfuURL,
				"token":      sfuToken,
				"roomName":   sfuRoomName,
			}))
			return
		}
		_ = srv.EmitToSkip(rtcRoom(roomID), "rtc:dmRejected",
			map[string]any{"room": roomID, "by": peerOf(c)}, c.ID())
		ack(util.OK(nil))
	})

	// ---- 群视频/语音：无会议号，群里直接开始 ----
	srv.On("rtc:groupCall", func(c socketapi.Conn, raw json.RawMessage, ack socketapi.Ack) {
		u := authed(c)
		if u == nil {
			ack(util.Fail("未登录"))
			return
		}
		var d rtcGroupCallReq
		_ = json.Unmarshal(raw, &d)
		gid, ok := validPosInt(d.GroupID)
		if !ok {
			ack(util.Fail("参数不合法"))
			return
		}
		// 校验发起者是群成员
		groups, err := st.GetUserGroups(context.Background(), u.ID)
		member := false
		if err == nil {
			for _, g := range groups {
				if g.ID == gid {
					member = true
					break
				}
			}
		} else {
			logger.Error(module, "rtc:groupCall 取用户群失败", "user", u.ID, "error", err.Error())
		}
		if !member {
			ack(util.Fail("不是该群成员"))
			return
		}
		kind := "video"
		if d.Kind != nil && *d.Kind == "voice" {
			kind = "voice"
		}
		rtcID := "group:" + itoaPositive(gid)
		roomsMu.Lock()
		sess := rooms[rtcID]
		if sess == nil {
			ts := nowMs()
			sess = &rtcSession{
				rtcID: rtcID, typ: "group", kind: kind, groupID: gid,
				hostID: u.ID, createdAt: ts, startedAt: &ts, ended: false,
			}
			rooms[rtcID] = sess
		} else if sess.ended {
			roomsMu.Unlock()
			ack(util.Fail("通话已结束"))
			return
		}
		roomsMu.Unlock()
		addSocket(srv, st, c, rtcID, u)
		_ = srv.EmitToSkip(rtcRoom(rtcID), "rtc:peerJoined",
			map[string]any{"room": rtcID, "peer": peerOf(c)}, c.ID())
		// 通知在线群成员（排除发起人自己，避免发起人也弹接听窗口）
		_ = srv.EmitToSkip(util.GroupRoom(gid), "rtc:groupCall",
			map[string]any{"room": rtcID, "kind": kind, "groupId": gid, "from": peerOf(c), "ts": nowMs()}, c.ID())
		broadcastRoomsChanged(srv)
		// SFU：群通话发起方进入房间，一并下发连接信息。
		sfuURL, sfuToken, sfuRoomName := joinSfuFields(cfg, rtcID, u.ID)
		ack(util.OK(map[string]any{
			"room":       sess.view(),
			"peers":      filterPeers(srv, rtcID, c.ID()),
			"iceServers": ice.GetIceServers(cfg),
			"url":        sfuURL,
			"token":      sfuToken,
			"roomName":   sfuRoomName,
		}))
	})

	// ---- 断线清理：从所在的所有 rtc 房间退出并通知 ----
	srv.OnDisconnect(func(c socketapi.Conn, _ string) {
		set := rtcRoomsOf(c)
		if len(set) == 0 {
			return
		}
		for rtcID := range set {
			_ = srv.EmitToSkip(rtcRoom(rtcID), "rtc:peerLeft",
				map[string]any{"room": rtcID, "peerId": c.ID()}, c.ID())
			cleanupIfEmpty(srv, rtcID)
		}
	})
}

// filterPeers 返回房间内除 selfID 外的参与者列表（对标 peersInfo(...).filter(socketId!==self)）。
func filterPeers(srv socketapi.Server, rtcID, selfID string) []rtcPeer {
	all := peersInfo(srv, rtcID)
	out := make([]rtcPeer, 0, len(all))
	for _, p := range all {
		if p.SocketID != selfID {
			out = append(out, p)
		}
	}
	return out
}

// truncate 截断字符串到最大长度（对标 TS 的 .slice(0, n)）。
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// itoaPositive int64 → 十进制字符串。
func itoaPositive(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
