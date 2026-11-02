# 网络面板（Net Panel）

多线程下载测速面板，参考 net.netart.cn 的形态自己实现。单二进制、零外部依赖，
前端内嵌在 Go 程序里，测速链接在后台增删改。

## 为什么需要服务端代理（跨域问题）

浏览器对跨域资源做 `fetch` 时，如果对方没给 CORS 头，拿到的就是 **opaque response**：
`response.body` 读不了、`Content-Length` 也拿不到，于是「测速」根本无从谈起。

所以本项目提供一个同源的 `/api/proxy?u=<url>&limit=<bytes>` 端点，由服务端去取上游、
把字节流原样吐回，并补上：

- `Access-Control-Allow-Origin` / `Access-Control-Expose-Headers`（让前端能读 `Content-Length`）
- `Timing-Allow-Origin: *`
- `Cache-Control: no-store`

顺带解决三件事：HTTPS 页面测 HTTP 源（混合内容会被浏览器直接拦掉）、上游没有
`Content-Length` 或不支持 `Range` 时兜底、上游限制 Referer/UA 时统一伪装。

## 安全

`/api/proxy` 是「让浏览器指挥服务器发请求」的端点，天然是 SSRF 面。防护措施：

- 解析域名后校验 IP，落在 loopback / 私有 / link-local / 组播 / CGNAT(100.64/10) /
  IPv6 ULA(fc00::/7) 一律拒绝
- **把已解析的 IP 直接交给拨号器**，而不是让 http.Transport 自己再解析一次 ——
  否则攻击者可以用 DNS rebinding（第一次给公网 IP，第二次给 127.0.0.1）绕过校验
- 只允许 http/https，重定向最多 5 次且目标协议重新校验
- 单次请求默认上限 32MiB，最大 512MiB

## 用法

```bash
# 首次启动前设置后台密码（不设则用默认 admin123 并在日志里警告）
export ADMIN_PASSWORD='你的密码'

go build -o netpanel .
./netpanel -addr 0.0.0.0:8091 -data ./run/data.json
```

- 面板：`http://<ip>:8091/`
- 后台：`http://<ip>:8091/admin.html`

后台可以：增删改测速链接（名称/分类/地址/单次上限/启停）、内置「测试这个地址」
按钮先验证源是否可用、改站点标题与页脚、改密码。

## 测速源选择

种子源全部是实测可用的（200/206 + 支持 Range + 有真实字节）。新增源时**务必先验证**：

```bash
curl -sS -o /dev/null -r 0-2097151 -w 'code=%{http_code} got=%{size_download}\n' '<url>'
```

踩过的坑：

- `speed.cloudflare.com/__down?bytes=` 超过 1e8 会直接 **403**（用 1e7 / 5e7）
- `cachefly.cachefly.net/*.test` 已失效，只回 20 多字节
- 各镜像站的 ubuntu ISO 路径普遍 404 或 302；改用
  `debian/dists/stable/main/binary-amd64/Packages.gz` 稳定且支持 Range

## 目录

```
main.go      配置持久化、会话、登录、后台 CRUD
proxy.go     跨域测速代理（含 SSRF 防护）
ipinfo.go    IP 归属查询（服务端代查 + 5 分钟缓存）
static/      前端（index.html / app.js / admin.html / admin.js / style.css）
```
