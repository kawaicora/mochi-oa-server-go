// Package ice 负责 WebRTC ICE 服务器下发，移植自源仓库 server/src/ice.ts。
//
// 优先从 Cloudflare TURN API 动态获取凭证（缓存 ttl 秒，提前 1 分钟过期）；
// 未开启或获取失败时回退到静态 ICE_SERVERS 配置。
package ice

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"mochi-oa-server-go/internal/config"
	"mochi-oa-server-go/internal/logger"
)

// cfResponse 是 Cloudflare TURN API 返回体。
type cfResponse struct {
	IceServers []config.IceServer `json:"iceServers"`
}

// cfCache 是凭证缓存（提前 1 分钟过期，避免每次通话打 CF API）。
var (
	cfCacheMu sync.Mutex
	cfCache   struct {
		servers   []config.IceServer
		expiresAt int64
	}
	httpClient = &http.Client{Timeout: 10 * time.Second}
)

// FetchCloudflareIce 从 Cloudflare TURN API 动态获取 ICE 服务器。
// 未启用返回 nil；失败打日志并返回 nil（不抛错，由调用方回退静态配置）。
func FetchCloudflareIce(cfg *config.ServerConfig) []config.IceServer {
	cf := cfg.IceCloudflare
	if cf == nil || !cf.Enabled {
		return nil
	}
	url := "https://rtc.live.cloudflare.com/v1/turn/keys/" + cf.KeyID + "/credentials/generate-ice-servers"
	body, _ := json.Marshal(map[string]int{"ttl": cf.TTL})
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		logger.Error("ice", "Cloudflare ICE 请求构造失败", "error", err.Error())
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+cf.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		logger.Error("ice", "Cloudflare ICE 请求异常", "error", err.Error())
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		logger.Error("ice", "Cloudflare ICE 获取失败", "status", resp.StatusCode)
		return nil
	}
	var data cfResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil || len(data.IceServers) == 0 {
		logger.Error("ice", "Cloudflare ICE 返回空 iceServers")
		return nil
	}
	return data.IceServers
}

// GetIceServers 返回用于下发客户端的 ICE servers。
// CF 开启时动态取 CF TURN（带缓存）；失败/未开启回退静态配置。
func GetIceServers(cfg *config.ServerConfig) []config.IceServer {
	cf := cfg.IceCloudflare
	if cf != nil && cf.Enabled {
		now := time.Now().UnixMilli()
		cfCacheMu.Lock()
		cached := cfCache.servers != nil && cfCache.expiresAt > now
		servers := cfCache.servers
		cfCacheMu.Unlock()
		if cached {
			return servers
		}
		fetched := FetchCloudflareIce(cfg)
		if len(fetched) > 0 {
			cfCacheMu.Lock()
			cfCache.servers = fetched
			cfCache.expiresAt = now + int64(cf.TTL)*1000 - 60000
			cfCacheMu.Unlock()
			logger.Info("ice", "Cloudflare ICE 已下发", "count", len(fetched), "ttl", cf.TTL)
			return fetched
		}
		logger.Warn("ice", "Cloudflare ICE 不可用，回退静态 ICE_SERVERS")
	}
	return cfg.IceServers
}
