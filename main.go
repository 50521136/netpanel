package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed all:static
var staticFS embed.FS

// === 数据模型 ===

// Source 是一个测速源。前端只拿到 name/url/bytes，不暴露任何管理字段。
type Source struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Category string `json:"category"`
	Enabled  bool   `json:"enabled"`
	// Bytes 是每线程单次请求最多下载多少字节。0 表示由前端决定默认值。
	Bytes int64 `json:"bytes"`
}

// Config 是全部持久化状态。密码只存 salt:sha256，不存明文。
type Config struct {
	Title      string   `json:"title"`
	Footer     string   `json:"footer"`
	SourceLink string   `json:"source_link"`
	Salt       string   `json:"salt"`
	PwHash     string   `json:"pw_hash"`
	Sources    []Source `json:"sources"`
}

// Store 负责配置的读写，带互斥锁（后台改、前台读）。
type Store struct {
	mu   sync.RWMutex
	path string
	cfg  Config
}

func NewStore(path string) (*Store, error) {
	s := &Store{path: path}
	b, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		// 首次启动：用默认配置 + 环境变量里的初始密码。
		pw := os.Getenv("ADMIN_PASSWORD")
		if pw == "" {
			pw = "admin123"
			log.Println("警告: 未设置 ADMIN_PASSWORD，后台密码使用默认值 admin123，请尽快在后台修改")
		}
		s.cfg = defaultConfig()
		s.setPassword(pw)
		if err := s.save(); err != nil {
			return nil, err
		}
		return s, nil
	}
	if err := json.Unmarshal(b, &s.cfg); err != nil {
		return nil, err
	}
	if s.cfg.Title == "" {
		s.cfg.Title = "网络面板"
	}
	return s, nil
}

func defaultConfig() Config {
	return Config{
		Title:      "网络面板",
		Footer:     "本工具由自建服务器提供加速",
		SourceLink: "",
		// 种子源全部是实测可用的（2026-11 验证：200/206 + 支持 Range + 有真实字节）。
		// 踩过的坑：Cloudflare 的 __down 超过 1e8 会直接 403；cachefly 的 *.test
		// 已经失效只回 20 多字节；各镜像站的 ubuntu ISO 路径普遍 404/302。
		// 新增源时务必先 curl -r 0-2097151 验一遍，别凭印象填。
		Sources: []Source{
			{ID: "s1", Name: "Cloudflare 10MB", Category: "全球", URL: "https://speed.cloudflare.com/__down?bytes=10000000", Enabled: true, Bytes: 10000000},
			{ID: "s2", Name: "Cloudflare 50MB", Category: "全球", URL: "https://speed.cloudflare.com/__down?bytes=50000000", Enabled: true, Bytes: 50000000},
			{ID: "s3", Name: "Vultr 新加坡", Category: "全球", URL: "https://sgp-ping.vultr.com/vultr.com.100MB.bin", Enabled: true, Bytes: 33554432},
			{ID: "s4", Name: "ThinkBroadband", Category: "全球", URL: "http://ipv4.download.thinkbroadband.com/100MB.zip", Enabled: true, Bytes: 33554432},
			{ID: "s5", Name: "清华 TUNA", Category: "国内", URL: "https://mirrors.tuna.tsinghua.edu.cn/debian/dists/stable/main/binary-amd64/Packages.gz", Enabled: true, Bytes: 33554432},
			{ID: "s6", Name: "阿里云", Category: "国内", URL: "https://mirrors.aliyun.com/debian/dists/stable/main/binary-amd64/Packages.gz", Enabled: true, Bytes: 33554432},
			{ID: "s7", Name: "中科大 USTC", Category: "国内", URL: "https://mirrors.ustc.edu.cn/debian/dists/stable/main/binary-amd64/Packages.gz", Enabled: true, Bytes: 33554432},
			{ID: "s8", Name: "华为云", Category: "国内", URL: "https://mirrors.huaweicloud.com/debian/dists/stable/main/binary-amd64/Packages.gz", Enabled: true, Bytes: 33554432},
			{ID: "s9", Name: "网易 163", Category: "国内", URL: "https://mirrors.163.com/debian/dists/stable/main/binary-amd64/Packages.gz", Enabled: true, Bytes: 33554432},
		},
	}
}

func (s *Store) snapshot() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *Store) save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) setPassword(pw string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.Salt == "" {
		s.cfg.Salt = randHex(16)
	}
	s.cfg.PwHash = hashPassword(s.cfg.Salt, pw)
}

func hashPassword(salt, pw string) string {
	sum := sha256.Sum256([]byte(salt + "|" + pw))
	return hex.EncodeToString(sum[:])
}

func (s *Store) checkPassword(pw string) bool {
	cfg := s.snapshot()
	got := hashPassword(cfg.Salt, pw)
	return subtle.ConstantTimeCompare([]byte(got), []byte(cfg.PwHash)) == 1
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败属于系统级异常，退回时间戳但记日志。
		log.Printf("crypto/rand 失败: %v", err)
		return hex.EncodeToString([]byte(time.Now().String()))
	}
	return hex.EncodeToString(b)
}

// === 会话 ===

type session struct {
	exp time.Time
}

type sessionStore struct {
	mu sync.Mutex
	m  map[string]session
}

func newSessionStore() *sessionStore {
	ss := &sessionStore{m: map[string]session{}}
	go func() {
		for range time.Tick(10 * time.Minute) {
			ss.gc()
		}
	}()
	return ss
}

func (ss *sessionStore) gc() {
	now := time.Now()
	ss.mu.Lock()
	defer ss.mu.Unlock()
	for k, v := range ss.m {
		if now.After(v.exp) {
			delete(ss.m, k)
		}
	}
}

func (ss *sessionStore) create() string {
	tok := randHex(32)
	ss.mu.Lock()
	ss.m[tok] = session{exp: time.Now().Add(7 * 24 * time.Hour)}
	ss.mu.Unlock()
	return tok
}

func (ss *sessionStore) valid(tok string) bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	v, ok := ss.m[tok]
	if !ok {
		return false
	}
	if time.Now().After(v.exp) {
		delete(ss.m, tok)
		return false
	}
	return true
}

func (ss *sessionStore) drop(tok string) {
	ss.mu.Lock()
	delete(ss.m, tok)
	ss.mu.Unlock()
}

// === 服务器 ===

type Server struct {
	store    *Store
	sessions *sessionStore
	proxy    *Proxy
}

func main() {
	addr := flag.String("addr", ":8091", "监听地址")
	dataPath := flag.String("data", "data.json", "配置文件路径")
	flag.Parse()

	store, err := NewStore(*dataPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	srv := &Server{
		store:    store,
		sessions: newSessionStore(),
		proxy:    NewProxy(),
	}

	mux := http.NewServeMux()
	// 公开接口
	mux.HandleFunc("/api/config", srv.handlePublicConfig)
	mux.HandleFunc("/api/sources", srv.handleSources)
	mux.HandleFunc("/api/proxy", srv.proxy.Handle)
	mux.HandleFunc("/api/whoami", srv.handleWhoami)
	mux.HandleFunc("/api/ipinfo", srv.handleIPInfo)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})

	// 登录
	mux.HandleFunc("/api/login", srv.handleLogin)
	mux.HandleFunc("/api/logout", srv.handleLogout)
	mux.HandleFunc("/api/me", srv.handleMe)
	mux.HandleFunc("/api/admin/password", srv.requireAdmin(srv.handleChangePassword))
	mux.HandleFunc("/api/admin/settings", srv.requireAdmin(srv.handleAdminSettings))

	// 测速源 CRUD
	mux.HandleFunc("/api/admin/sources", srv.requireAdmin(srv.handleAdminSources))
	mux.HandleFunc("/api/admin/sources/", srv.requireAdmin(srv.handleAdminSourceByID))

	// 静态资源（内嵌）
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatalf("静态资源挂载失败: %v", err)
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))

	h := withCommonHeaders(mux)
	log.Printf("网络面板启动于 %s，配置文件 %s", *addr, *dataPath)
	s := &http.Server{
		Addr:              *addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		// 测速代理是长连接流式传输，不能设 WriteTimeout。
	}
	if err := s.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

// withCommonHeaders 给所有响应加基础安全头。
func withCommonHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// === 公开接口 ===

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *Server) handlePublicConfig(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"title":       cfg.Title,
		"footer":      cfg.Footer,
		"source_link": cfg.SourceLink,
	})
}

// publicSource 是暴露给前端的投影。刻意与 Source 分开：
// 内部字段（enabled 等）不该出现在公开响应里，也免得以后加字段时误泄漏。
type publicSource struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Category string `json:"category"`
	Bytes    int64  `json:"bytes"`
}

// handleSources 只返回启用的测速源，按 category 分组。
func (s *Server) handleSources(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.snapshot()
	groups := []map[string]any{}
	idx := map[string]int{}
	for _, src := range cfg.Sources {
		if !src.Enabled {
			continue
		}
		cat := src.Category
		if cat == "" {
			cat = "其他"
		}
		i, ok := idx[cat]
		if !ok {
			i = len(groups)
			idx[cat] = i
			groups = append(groups, map[string]any{"name": cat, "items": []publicSource{}})
		}
		items := groups[i]["items"].([]publicSource)
		groups[i]["items"] = append(items, publicSource{
			ID: src.ID, Name: src.Name, URL: src.URL, Category: cat, Bytes: src.Bytes,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": groups})
}

func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	ips := []string{}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for _, p := range strings.Split(xff, ",") {
			if v := strings.TrimSpace(p); v != "" {
				ips = append(ips, v)
			}
		}
	}
	hdrs := map[string]string{}
	for k := range r.Header {
		// 只回显一部分，避免把 Cookie 之类带回去。
		lk := strings.ToLower(k)
		if lk == "cookie" || lk == "authorization" {
			continue
		}
		hdrs[k] = r.Header.Get(k)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"remote_addr":      r.RemoteAddr,
		"x_forwarded_for":  ips,
		"user_agent":       r.UserAgent(),
		"headers":          hdrs,
		"server_time":      time.Now().Format(time.RFC3339),
	})
}

// === 登录与会话 ===

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if !s.store.checkPassword(body.Password) {
		// 固定延迟，削弱暴力破解的时序与速率优势。
		time.Sleep(300 * time.Millisecond)
		writeErr(w, http.StatusUnauthorized, "密码错误")
		return
	}
	tok := s.sessions.create()
	http.SetCookie(w, &http.Cookie{
		Name:     "np_session",
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   7 * 24 * 3600,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("np_session"); err == nil {
		s.sessions.drop(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: "np_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie("np_session")
	authed := err == nil && s.sessions.valid(c.Value)
	writeJSON(w, http.StatusOK, map[string]bool{"authed": authed})
}

func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("np_session")
		if err != nil || !s.sessions.valid(c.Value) {
			writeErr(w, http.StatusUnauthorized, "未登录")
			return
		}
		next(w, r)
	}
}

// === 后台接口 ===

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}
	var body struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if !s.store.checkPassword(body.Old) {
		writeErr(w, http.StatusUnauthorized, "原密码错误")
		return
	}
	if len(body.New) < 6 {
		writeErr(w, http.StatusBadRequest, "新密码至少 6 位")
		return
	}
	s.store.setPassword(body.New)
	if err := s.store.save(); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleAdminSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.store.snapshot()
		writeJSON(w, http.StatusOK, map[string]any{
			"title":       cfg.Title,
			"footer":      cfg.Footer,
			"source_link": cfg.SourceLink,
		})
	case http.MethodPost:
		var body struct {
			Title      string `json:"title"`
			Footer     string `json:"footer"`
			SourceLink string `json:"source_link"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "请求格式错误")
			return
		}
		s.store.mu.Lock()
		if body.Title != "" {
			s.store.cfg.Title = body.Title
		}
		s.store.cfg.Footer = body.Footer
		s.store.cfg.SourceLink = body.SourceLink
		err := s.store.saveLocked()
		s.store.mu.Unlock()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "方法不允许")
	}
}

// handleAdminSources 处理列表与新增。
func (s *Server) handleAdminSources(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.store.snapshot()
		writeJSON(w, http.StatusOK, map[string]any{"sources": cfg.Sources})
	case http.MethodPost:
		var src Source
		if err := json.NewDecoder(io.LimitReader(r.Body, 16384)).Decode(&src); err != nil {
			writeErr(w, http.StatusBadRequest, "请求格式错误")
			return
		}
		if msg := validateSource(src); msg != "" {
			writeErr(w, http.StatusBadRequest, msg)
			return
		}
		src.ID = randHex(8)
		s.store.mu.Lock()
		s.store.cfg.Sources = append(s.store.cfg.Sources, src)
		err := s.store.saveLocked()
		s.store.mu.Unlock()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, src)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "方法不允许")
	}
}

// handleAdminSourceByID 处理单个源的改与删。
func (s *Server) handleAdminSourceByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/admin/sources/")
	if id == "" || strings.Contains(id, "/") {
		writeErr(w, http.StatusBadRequest, "缺少 id")
		return
	}
	switch r.Method {
	case http.MethodPut:
		var src Source
		if err := json.NewDecoder(io.LimitReader(r.Body, 16384)).Decode(&src); err != nil {
			writeErr(w, http.StatusBadRequest, "请求格式错误")
			return
		}
		if msg := validateSource(src); msg != "" {
			writeErr(w, http.StatusBadRequest, msg)
			return
		}
		s.store.mu.Lock()
		found := false
		for i := range s.store.cfg.Sources {
			if s.store.cfg.Sources[i].ID == id {
				src.ID = id
				s.store.cfg.Sources[i] = src
				found = true
				break
			}
		}
		var err error
		if found {
			err = s.store.saveLocked()
		}
		s.store.mu.Unlock()
		if !found {
			writeErr(w, http.StatusNotFound, "测速源不存在")
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, src)
	case http.MethodDelete:
		s.store.mu.Lock()
		out := s.store.cfg.Sources[:0]
		found := false
		for _, src := range s.store.cfg.Sources {
			if src.ID == id {
				found = true
				continue
			}
			out = append(out, src)
		}
		s.store.cfg.Sources = out
		var err error
		if found {
			err = s.store.saveLocked()
		}
		s.store.mu.Unlock()
		if !found {
			writeErr(w, http.StatusNotFound, "测速源不存在")
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "方法不允许")
	}
}

func validateSource(src Source) string {
	if strings.TrimSpace(src.Name) == "" {
		return "名称不能为空"
	}
	u := strings.TrimSpace(src.URL)
	if u == "" {
		return "地址不能为空"
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return "地址必须以 http:// 或 https:// 开头"
	}
	if src.Bytes < 0 {
		return "单次上限不能为负"
	}
	return ""
}
