// Package presence 维护在线状态登记表：userId → 当前连接的 socket id 集合。
// 1:1 移植自源仓库 server/src/presence.ts（addPresence/removePresence/isOnline）。
//
// 同用户可多端在线，因此一个 userId 对应一组连接 id。TS 用裸 Map（单线程事件循环天然安全）；
// Go 并发处理连接，包级注册表加读写锁：读多写少（群广播时高频遍历、连接建立/断开时写）。
package presence

import "sync"

var (
	mu       sync.RWMutex
	presence = map[int64]map[string]struct{}{}
)

// Add 登记一条连接在线（对标 addPresence(userId, socketId)）。
func Add(userID int64, connID string) {
	mu.Lock()
	defer mu.Unlock()
	set := presence[userID]
	if set == nil {
		set = map[string]struct{}{}
		presence[userID] = set
	}
	set[connID] = struct{}{}
}

// Remove 摘除一条连接；返回该用户是否仍在线（至少还有其它连接，对标 removePresence 的 boolean 返回）。
func Remove(userID int64, connID string) bool {
	mu.Lock()
	defer mu.Unlock()
	set := presence[userID]
	if set == nil {
		return false
	}
	delete(set, connID)
	if len(set) == 0 {
		delete(presence, userID)
		return false
	}
	return true
}

// IsOnline 判断用户当前是否在线（对标 isOnline）。
func IsOnline(userID int64) bool {
	mu.RLock()
	defer mu.RUnlock()
	set := presence[userID]
	return len(set) > 0
}
