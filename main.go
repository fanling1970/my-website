// LAN-CMS：局域网轻量内容管理系统
// v2：树形结构（文件夹/页面/文件），富文本编辑，图片上传/裁剪，附件下载
// 管理后台: /admin    前台: /
// 认证: Cookie 会话登录（POST /api/login → lancms_session），退出 GET /logout
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

const (
	TypeFolder = "folder"
	TypePage   = "page"
	TypeFile   = "file"
)

// Node 树节点：文件夹 / 页面 / 文件
type Node struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Parent    string    `json:"parent"` // 父节点 ID，空 = 根节点
	Title     string    `json:"title"`
	Content   string    `json:"content"`           // page: 富文本 HTML；file: 文件访问路径
	FileName  string    `json:"fileName,omitempty"` // file: 原始文件名
	Size      int64     `json:"size,omitempty"`    // file: 字节数
	Hidden    bool      `json:"hidden,omitempty"`  // 仅登录可见（前台未登录时不显示/不可访问）
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Store 简单 JSON 文件存储
type Store struct {
	mu    sync.Mutex
	dir   string
	path  string
	nodes map[string]Node
}

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "uploads"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "files"), 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, path: filepath.Join(dir, "nodes.json"), nodes: map[string]Node{}}
	if _, err := os.Stat(s.path); os.IsNotExist(err) {
		s.migrateLegacy()
	}
	if b, err := os.ReadFile(s.path); err == nil {
		var list []Node
		if json.Unmarshal(b, &list) == nil {
			for _, n := range list {
				s.nodes[n.ID] = n
			}
		}
	}
	return s, nil
}

// migrateLegacy 兼容旧版 pages.json：全部转为根级 page 节点
func (s *Store) migrateLegacy() {
	b, err := os.ReadFile(filepath.Join(s.dir, "pages.json"))
	if err != nil {
		return
	}
	// 兼容带 UTF-8 BOM 的文件
	b = []byte(strings.TrimPrefix(string(b), "\uFEFF"))
	var list []Node
	if json.Unmarshal(b, &list) != nil {
		return
	}
	for _, n := range list {
		if n.ID == "" {
			continue
		}
		n.Type = TypePage
		n.Parent = ""
		s.nodes[n.ID] = n
	}
	_ = s.persistLocked()
}

func (s *Store) persistLocked() error {
	var list []Node
	for _, n := range s.nodes {
		list = append(list, n)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) List() []Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := make([]Node, 0, len(s.nodes))
	for _, n := range s.nodes {
		list = append(list, n)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].UpdatedAt.After(list[j].UpdatedAt) })
	return list
}

func (s *Store) Get(id string) (Node, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[id]
	return n, ok
}

func (s *Store) Create(n Node) (Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	n.ID = newID()
	n.CreatedAt = now
	n.UpdatedAt = now
	s.nodes[n.ID] = n
	if err := s.persistLocked(); err != nil {
		delete(s.nodes, n.ID)
		return Node{}, err
	}
	return n, nil
}

// Update 更新节点。parent 为 nil 时保持原父节点（防止保存时误移位置）
func (s *Store) Update(id string, n Node, parent *string) (Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.nodes[id]
	if !ok {
		return Node{}, os.ErrNotExist
	}
	old.Title = n.Title
	old.Content = n.Content
	if parent != nil {
		old.Parent = *parent
	}
	old.FileName = n.FileName
	old.Size = n.Size
	old.Hidden = n.Hidden
	old.UpdatedAt = time.Now()
	s.nodes[id] = old
	if err := s.persistLocked(); err != nil {
		return Node{}, err
	}
	return old, nil
}

// Delete 级联删除（文件夹连同子孙节点），返回删除数量
func (s *Store) Delete(id string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.nodes[id]; !ok {
		return 0, os.ErrNotExist
	}
	count := 0
	var collect func(string)
	collect = func(cur string) {
		if _, ok := s.nodes[cur]; ok {
			delete(s.nodes, cur)
			count++
		}
		for _, n := range s.nodes {
			if n.Parent == cur {
				collect(n.ID)
			}
		}
	}
	collect(id)
	if err := s.persistLocked(); err != nil {
		return 0, err
	}
	return count, nil
}

func newID() string {
	return hex.EncodeToString(newIDBytes())
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

var allowedImageExt = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true}

// ---------- Cookie 会话登录 ----------
var (
	sessionMu  sync.Mutex
	sessions   = map[string]time.Time{} // token -> 过期时间
	sessionTTL = 7 * 24 * time.Hour
	authPath   string                   // 认证数据文件（data/auth.json），在 main 中初始化
)

// authInfo 注册制账号：用户名 + 加盐密码哈希
type authInfo struct {
	User     string `json:"user"`
	PassHash string `json:"passHash"`
	Salt     string `json:"salt"`
}

func hashPass(pass, salt string) string {
	h := sha256.Sum256([]byte(salt + ":" + pass))
	return hex.EncodeToString(h[:])
}

// parseFormCompat 兼容两种表单提交：
// 浏览器 FormData 是 multipart/form-data（必须 ParseMultipartForm 才能读到字段）；
// 普通 urlencoded 表单/curl 客户端用 ParseForm。ParseMultipartForm 对非 multipart 会返回错误，失败则回退 ParseForm。
func parseFormCompat(r *http.Request) {
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		_ = r.ParseForm()
	}
}

func loadAuth() (authInfo, bool) {
	b, err := os.ReadFile(authPath)
	if err != nil {
		return authInfo{}, false
	}
	var a authInfo
	if json.Unmarshal(b, &a) != nil || a.User == "" {
		return authInfo{}, false
	}
	return a, true
}

func saveAuth(a authInfo) error {
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	tmp := authPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, authPath)
}

// migrateAuthIfNeeded 兼容旧版环境变量账号：auth.json 不存在且设置了 LANCMS_USER/PASS 时迁移为注册制账号
func migrateAuthIfNeeded() {
	if _, err := os.Stat(authPath); err == nil {
		return
	}
	u := os.Getenv("LANCMS_USER")
	p := os.Getenv("LANCMS_PASS")
	if u == "" || p == "" {
		return
	}
	salt := hex.EncodeToString(newIDBytes())
	_ = saveAuth(authInfo{User: u, PassHash: hashPass(p, salt), Salt: salt})
}

// authMiddleware 保护后台页面与写 API：校验 lancms_session Cookie
func authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if validSession(r) {
			// no-store 防止后台页面被浏览器内存缓存(BFCache)：
			// 退出后按后退键会重新请求服务器，未登录则跳转登录页，而不是恢复缓存的后台页面
			w.Header().Set("Cache-Control", "no-store")
			next(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeJSON(w, 401, map[string]string{"error": "未登录"})
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	}
}

func validSession(r *http.Request) bool {
	c, err := r.Cookie("lancms_session")
	if err != nil || c.Value == "" {
		return false
	}
	sessionMu.Lock()
	exp, ok := sessions[c.Value]
	sessionMu.Unlock()
	return ok && time.Now().Before(exp)
}

func newIDBytes() []byte {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return []byte(time.Now().String())
	}
	return b
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8090"
	}
	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "/data"
	}

	store, err := NewStore(dataDir)
	if err != nil {
		log.Fatalf("初始化数据目录失败: %v", err)
	}
	uploadsDir := filepath.Join(dataDir, "uploads")
	filesDir := filepath.Join(dataDir, "files")
	authPath = filepath.Join(dataDir, "auth.json")
	migrateAuthIfNeeded()

	mux := http.NewServeMux()

	// ---------- 节点 API ----------
	mux.HandleFunc("GET /api/nodes", func(w http.ResponseWriter, r *http.Request) {
		list := store.List()
		// 未登录时过滤"仅登录可见"页面，登录后显示全部
		if !validSession(r) {
			filtered := make([]Node, 0, len(list))
			for _, n := range list {
				if !n.Hidden {
					filtered = append(filtered, n)
				}
			}
			list = filtered
		}
		writeJSON(w, 200, list)
	})
	mux.HandleFunc("POST /api/nodes", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Type    string `json:"type"`
			Parent  string `json:"parent"`
			Title   string `json:"title"`
			Content string `json:"content"`
			Hidden  bool   `json:"hidden"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]string{"error": "请求格式错误"})
			return
		}
		if req.Type != TypeFolder && req.Type != TypePage && req.Type != TypeFile {
			writeJSON(w, 400, map[string]string{"error": "type 必须是 folder/page/file"})
			return
		}
		if req.Title == "" {
			writeJSON(w, 400, map[string]string{"error": "标题不能为空"})
			return
		}
		n, err := store.Create(Node{Type: req.Type, Parent: req.Parent, Title: req.Title, Content: req.Content, Hidden: req.Hidden})
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 201, n)
	}))
	mux.HandleFunc("GET /api/nodes/{id}", func(w http.ResponseWriter, r *http.Request) {
		n, ok := store.Get(r.PathValue("id"))
		if !ok {
			writeJSON(w, 404, map[string]string{"error": "not found"})
			return
		}
		if n.Hidden && !validSession(r) {
			writeJSON(w, 401, map[string]string{"error": "未登录"})
			return
		}
		writeJSON(w, 200, n)
	})
	mux.HandleFunc("PUT /api/nodes/{id}", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Title   string  `json:"title"`
			Content string  `json:"content"`
			Parent  *string `json:"parent"`
			Hidden  bool    `json:"hidden"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]string{"error": "请求格式错误"})
			return
		}
		n, err := store.Update(r.PathValue("id"), Node{Title: req.Title, Content: req.Content, Hidden: req.Hidden}, req.Parent)
		if err != nil {
			writeJSON(w, 404, map[string]string{"error": "not found"})
			return
		}
		writeJSON(w, 200, n)
	}))
	mux.HandleFunc("DELETE /api/nodes/{id}", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		count, err := store.Delete(r.PathValue("id"))
		if err != nil {
			writeJSON(w, 404, map[string]string{"error": "not found"})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "deleted": count})
	}))

	// ---------- 文件上传（图片 + 附件）----------
	mux.HandleFunc("POST /api/upload", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 200<<20)
		if err := r.ParseMultipartForm(200 << 20); err != nil {
			writeJSON(w, 400, map[string]string{"error": "文件太大或格式错误（最大 200MB）"})
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": "缺少 file 字段"})
			return
		}
		defer file.Close()

		ext := strings.ToLower(filepath.Ext(header.Filename))
		kind := "file"
		subdir := "files"
		if allowedImageExt[ext] {
			kind = "image"
			subdir = "uploads"
		}
		dir := filesDir
		if subdir == "uploads" {
			dir = uploadsDir
		}
		name := filepath.Base(header.Filename) // 保留原始文件名（用户要求上传软件不改名）
		if name == "." || name == string(filepath.Separator) || name == "" {
			name = "file" + ext
		}
		// 重名自动加序号：foo.exe -> foo(1).exe
		stem := strings.TrimSuffix(name, ext)
		for i := 1; ; i++ {
			if _, err := os.Stat(filepath.Join(dir, name)); os.IsNotExist(err) {
				break
			}
			name = fmt.Sprintf("%s(%d)%s", stem, i, ext)
		}
		dst, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "保存失败"})
			return
		}
		size, err := io.Copy(dst, file)
		if err != nil {
			dst.Close()
			_ = os.Remove(filepath.Join(dir, name))
			writeJSON(w, 500, map[string]string{"error": "写入失败"})
			return
		}
		dst.Close()

		writeJSON(w, 200, map[string]any{
			"errno": 0,
			"data": map[string]any{
				"url":  "/" + subdir + "/" + url.PathEscape(name),
				"alt":  header.Filename,
				"href": "",
				"kind": kind,
				"name": header.Filename,
				"size": size,
			},
		})
	}))

	// 图片与附件静态服务
	mux.Handle("GET /uploads/", http.StripPrefix("/uploads/", http.FileServer(http.Dir(uploadsDir))))
	mux.Handle("GET /files/", http.StripPrefix("/files/", http.FileServer(http.Dir(filesDir))))

	// 前端页面（/ → index.html，/admin → admin.html）
	webSub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("加载前端资源失败: %v", err)
	}
	servePage := func(name string, cache string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			b, err := fs.ReadFile(webSub, name)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if cache != "" {
				w.Header().Set("Cache-Control", cache) // 后台页 no-store 防 BFCache；前台 no-cache 防旧版缓存
			}
			_, _ = w.Write(b)
		}
	}

	// ---------- 登录 / 注册 / 退出 / 会话 / 改密 ----------
	mux.HandleFunc("GET /login", servePage("login.html", "no-cache"))
	mux.HandleFunc("GET /api/auth-status", func(w http.ResponseWriter, r *http.Request) {
		_, ok := loadAuth()
		writeJSON(w, 200, map[string]bool{"registered": ok, "loggedIn": validSession(r)})
	})
	mux.HandleFunc("POST /api/login", func(w http.ResponseWriter, r *http.Request) {
		parseFormCompat(r)
		a, ok := loadAuth()
		if !ok {
			writeJSON(w, 200, map[string]bool{"ok": false, "needRegister": true})
			return
		}
		if r.FormValue("username") != a.User || hashPass(r.FormValue("password"), a.Salt) != a.PassHash {
			writeJSON(w, 200, map[string]bool{"ok": false})
			return
		}
		token := newID()
		sessionMu.Lock()
		sessions[token] = time.Now().Add(sessionTTL)
		sessionMu.Unlock()
		http.SetCookie(w, &http.Cookie{
			Name: "lancms_session", Value: token, Path: "/",
			HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL.Seconds()),
		})
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/register", func(w http.ResponseWriter, r *http.Request) {
		parseFormCompat(r)
		if _, ok := loadAuth(); ok {
			writeJSON(w, 200, map[string]any{"ok": false, "err": "已注册，请直接登录"})
			return
		}
		u := strings.TrimSpace(r.FormValue("username"))
		p := r.FormValue("password")
		p2 := r.FormValue("confirm")
		if u == "" || p == "" || p != p2 {
			writeJSON(w, 200, map[string]any{"ok": false, "err": "用户名不能为空或两次密码不一致"})
			return
		}
		salt := hex.EncodeToString(newIDBytes())
		if err := saveAuth(authInfo{User: u, PassHash: hashPass(p, salt), Salt: salt}); err != nil {
			writeJSON(w, 200, map[string]any{"ok": false, "err": "保存失败"})
			return
		}
		token := newID()
		sessionMu.Lock()
		sessions[token] = time.Now().Add(sessionTTL)
		sessionMu.Unlock()
		http.SetCookie(w, &http.Cookie{
			Name: "lancms_session", Value: token, Path: "/",
			HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL.Seconds()),
		})
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/change-password", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		parseFormCompat(r)
		a, ok := loadAuth()
		if !ok {
			writeJSON(w, 200, map[string]any{"ok": false, "err": "尚未注册"})
			return
		}
		if hashPass(r.FormValue("old"), a.Salt) != a.PassHash {
			writeJSON(w, 200, map[string]any{"ok": false, "err": "当前密码错误"})
			return
		}
		np := r.FormValue("new")
		if np == "" || np != r.FormValue("confirm") {
			writeJSON(w, 200, map[string]any{"ok": false, "err": "新密码不能为空或两次输入不一致"})
			return
		}
		if err := saveAuth(authInfo{User: a.User, PassHash: hashPass(np, a.Salt), Salt: a.Salt}); err != nil {
			writeJSON(w, 200, map[string]any{"ok": false, "err": "保存失败"})
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))
	mux.HandleFunc("GET /logout", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("lancms_session"); err == nil {
			sessionMu.Lock()
			delete(sessions, c.Value)
			sessionMu.Unlock()
		}
		http.SetCookie(w, &http.Cookie{Name: "lancms_session", Value: "", Path: "/", MaxAge: -1})
		http.Redirect(w, r, "/", http.StatusFound)
	})
	mux.HandleFunc("GET /api/session", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))
	mux.HandleFunc("GET /admin", authMiddleware(servePage("admin.html", "no-store")))
	mux.HandleFunc("GET /", servePage("index.html", "no-cache"))

	log.Printf("LAN-CMS v2 已启动: http://0.0.0.0:%s   管理后台: /admin (登录: /login)", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}
