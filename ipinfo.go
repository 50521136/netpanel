package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// IP 离线库查询。
//
// 纯前端做不了这件事：查询接口基本都不给 CORS 头，浏览器直接 fetch 会被拦。
// 所以走服务端代查 + 5 分钟内存缓存，避免把配额打光。

type ipInfoEntry struct {
	Data map[string]any
	At   time.Time
}

var (
	ipCacheMu sync.Mutex
	ipCache   = map[string]ipInfoEntry{}
)

func (s *Server) handleIPInfo(w http.ResponseWriter, r *http.Request) {
	ip := strings.TrimSpace(r.URL.Query().Get("ip"))
	if ip == "" {
		// 不传就查访问者自己的 IP。
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err == nil {
			ip = host
		} else {
			ip = r.RemoteAddr
		}
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			ip = strings.TrimSpace(strings.Split(xff, ",")[0])
		}
	}
	if net.ParseIP(ip) == nil {
		writeErr(w, http.StatusBadRequest, "IP 格式不正确")
		return
	}

	// 缓存命中直接返回。
	ipCacheMu.Lock()
	if e, ok := ipCache[ip]; ok && time.Since(e.At) < 5*time.Minute {
		ipCacheMu.Unlock()
		writeJSON(w, http.StatusOK, e.Data)
		return
	}
	ipCacheMu.Unlock()

	data := lookupIP(ip)

	ipCacheMu.Lock()
	// 简单限长，避免内存无限增长。
	if len(ipCache) > 5000 {
		ipCache = map[string]ipInfoEntry{}
	}
	ipCache[ip] = ipInfoEntry{Data: data, At: time.Now()}
	ipCacheMu.Unlock()

	writeJSON(w, http.StatusOK, data)
}

// lookupIP 依次尝试几个公开接口，任一成功即返回。
func lookupIP(ip string) map[string]any {
	// 先判保留地址，省一次外网请求。
	if parsed := net.ParseIP(ip); parsed != nil && badIP(parsed) {
		return map[string]any{
			"ip": ip, "ok": true, "private": true,
			"country": "内网/保留地址", "region": "", "city": "", "isp": "",
		}
	}

	client := &http.Client{Timeout: 6 * time.Second}

	// 接口一：ip-api.com（免费、无需 key，限制 45 次/分钟，服务端有缓存够用）
	if m, ok := fetchJSON(client, "http://ip-api.com/json/"+ip+"?fields=status,country,regionName,city,isp,as,query,timezone"); ok {
		if st, _ := m["status"].(string); st == "success" {
			return map[string]any{
				"ip": ip, "ok": true,
				"country": m["country"], "region": m["regionName"],
				"city": m["city"], "isp": m["isp"], "as": m["as"],
				"timezone": m["timezone"], "source": "ip-api.com",
			}
		}
	}

	// 接口二：ipinfo.io（兜底）
	if m, ok := fetchJSON(client, "https://ipinfo.io/"+ip+"/json"); ok {
		return map[string]any{
			"ip": ip, "ok": true,
			"country": m["country"], "region": m["region"],
			"city": m["city"], "isp": m["org"],
			"source": "ipinfo.io",
		}
	}

	return map[string]any{"ip": ip, "ok": false, "error": "查询失败，接口均不可达"}
}

func fetchJSON(c *http.Client, url string) (map[string]any, bool) {
	resp, err := c.Get(url)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	var m map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&m); err != nil {
		return nil, false
	}
	return m, true
}
