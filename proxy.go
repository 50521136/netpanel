package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Proxy 是跨域测速代理。
//
// 为什么必须有它：浏览器对跨域资源做 fetch 时，如果对方没给 CORS 头，
// 拿到的就是 opaque response —— response.body 读不了、Content-Length 也拿不到，
// 于是「测速」根本无从谈起。想在前端精确统计字节数，只能让同源的服务端
// 转一手：服务端去取上游，再把字节流原样吐给前端，并补上 CORS 头。
//
// 顺带解决三件事：
//  1. HTTPS 页面测 HTTP 源（混合内容会被浏览器直接拦掉）；
//  2. 上游没有 Content-Length / 不支持 Range 时由代理兜底；
//  3. 上游限制 Referer / UA 时由代理统一伪装。
//
// 安全：这是一个「让浏览器指挥服务器发请求」的端点，天然是 SSRF 面。
// 因此必须做两件事 —— 解析域名后校验 IP 是否落在内网，以及把已解析的 IP
// 直接交给拨号器（否则攻击者可以用 DNS rebinding 先给公网 IP 再换成 127.0.0.1）。
type Proxy struct {
	client *http.Client
}

func NewProxy() *Proxy {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}

	transport := &http.Transport{
		// 关键：自己做 DNS 解析与 IP 校验，然后直接拨 IP，杜绝 rebinding。
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			var lastErr error
			for _, ipa := range ips {
				if badIP(ipa.IP) {
					lastErr = errors.New("目标地址不允许访问（内网/保留地址）")
					continue
				}
				conn, derr := dialer.DialContext(ctx, network, net.JoinHostPort(ipa.IP.String(), port))
				if derr == nil {
					return conn, nil
				}
				lastErr = derr
			}
			if lastErr == nil {
				lastErr = errors.New("域名解析不到可用地址")
			}
			return nil, lastErr
		},
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		DisableCompression:    true, // 原样透传，压缩会让字节数失真
	}

	return &Proxy{
		client: &http.Client{
			Transport: transport,
			// 不设 Timeout：测速是长流，靠前端 abort 结束。
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return errors.New("重定向次数过多")
				}
				// 重定向目标同样要过内网校验（拨号器已兜住，这里再拦一层明文协议）。
				if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
					return errors.New("重定向到不支持的协议")
				}
				return nil
			},
		},
	}
}

// badIP 判断 IP 是否属于不该被代理访问的地址段。
func badIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	// 云厂商元数据服务（169.254.169.254 已被 LinkLocalUnicast 覆盖，
	// 这里额外挡 100.64/10 运营商级 NAT 与 0.0.0.0/8）。
	if ip4 := ip.To4(); ip4 != nil {
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return true
		}
		if ip4[0] == 0 {
			return true
		}
	}
	// IPv6 唯一本地地址 fc00::/7
	if len(ip) == net.IPv6len && (ip[0]&0xfe) == 0xfc {
		return true
	}
	return false
}

// cors 给响应补上跨域头。测速是公开只读接口，允许任意来源。
func cors(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = "*"
	}
	h.Set("Access-Control-Allow-Origin", origin)
	// 前端要读这些头才能算准速度，必须显式 expose。
	h.Set("Access-Control-Expose-Headers", "Content-Length,Content-Range,Accept-Ranges,X-Upstream-Status,X-Upstream-Url")
	// 让 Resource Timing API 能拿到跨域耗时明细。
	h.Set("Timing-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET,HEAD,OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Range,Content-Type")
	h.Set("Vary", "Origin")
	// 代理响应绝不能被任何缓存留下。
	h.Set("Cache-Control", "no-store, no-cache, must-revalidate, private, max-age=0")
}

// Handle 处理 /api/proxy?u=<url>&limit=<bytes>
func (p *Proxy) Handle(w http.ResponseWriter, r *http.Request) {
	cors(w, r)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeErr(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}

	raw := r.URL.Query().Get("u")
	if raw == "" {
		writeErr(w, http.StatusBadRequest, "缺少参数 u")
		return
	}
	target, err := url.Parse(raw)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		writeErr(w, http.StatusBadRequest, "地址非法，只支持 http/https")
		return
	}

	// 提前挡掉字面量内网地址，省一次 DNS。
	if host := target.Hostname(); host != "" {
		if ip := net.ParseIP(host); ip != nil && badIP(ip) {
			writeErr(w, http.StatusForbidden, "目标地址不允许访问（内网/保留地址）")
			return
		}
		if strings.EqualFold(host, "localhost") {
			writeErr(w, http.StatusForbidden, "目标地址不允许访问（内网/保留地址）")
			return
		}
	}

	var limit int64
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, e := strconv.ParseInt(v, 10, 64); e == nil && n > 0 {
			limit = n
		}
	}
	if limit == 0 {
		limit = 32 << 20 // 默认 32MiB，防止单请求无限拉
	}
	if limit > 512<<20 {
		limit = 512 << 20
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "构造请求失败")
		return
	}
	// 透传 Range：多线程测速时前端可以按固定大小切片，避免总在拉文件头。
	if rg := r.Header.Get("Range"); rg != "" {
		req.Header.Set("Range", rg)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; NetPanel/1.0)")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := p.client.Do(req)
	if err != nil {
		if r.Context().Err() != nil {
			return // 前端主动 abort，正常路径，不刷日志
		}
		log.Printf("代理失败 %s: %v", target.Redacted(), err)
		writeErr(w, http.StatusBadGateway, "上游请求失败: "+err.Error())
		return
	}
	defer resp.Body.Close()

	// 把上游状态码原样带回，但限制成「客户端能理解的」范围。
	code := resp.StatusCode
	if code < 200 || code > 599 {
		code = http.StatusBadGateway
	}

	h := w.Header()
	h.Set("Content-Type", resp.Header.Get("Content-Type"))
	h.Set("X-Upstream-Status", strconv.Itoa(resp.StatusCode))
	h.Set("X-Upstream-Url", target.Redacted())
	// 这两个头决定前端能不能算「进度」和「总量」，必须透传。
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		h.Set("Content-Length", cl)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		h.Set("Content-Range", cr)
	}
	if ar := resp.Header.Get("Accept-Ranges"); ar != "" {
		h.Set("Accept-Ranges", ar)
	}

	w.WriteHeader(code)
	if r.Method == http.MethodHead {
		return
	}

	// 边读边写，不缓冲：缓冲会让前端看到的速率曲线完全失真。
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	var written int64
	for {
		if written >= limit {
			break
		}
		want := int64(len(buf))
		if remain := limit - written; remain < want {
			want = remain
		}
		n, rerr := resp.Body.Read(buf[:want])
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return // 客户端断开
			}
			written += int64(n)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			if rerr != io.EOF && r.Context().Err() == nil {
				// 上游中途断开不算服务端错误，只是本次测速提前结束。
				log.Printf("上游读取结束 %s: %v (已传 %d 字节)", target.Redacted(), rerr, written)
			}
			break
		}
	}
}
