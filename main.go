// LAN-CMS：局域网轻量内容管理系统
// v2：树形结构（文件夹/页面/文件），富文本编辑，图片上传/裁剪，附件下载
// 管理后台: /admin    前台: /
package main

import (
	"crypto/rand"
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
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return hex.EncodeToString([]byte(time.Now().String()))
	}
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

var allowedImageExt = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true}

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

	mux := http.NewServeMux()

	// ---------- 节点 API ----------
	mux.HandleFunc("GET /api/nodes", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, store.List())
	})
	mux.HandleFunc("POST /api/nodes", basicAuth(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Type    string `json:"type"`
			Parent  string `json:"parent"`
			Title   string `json:"title"`
			Content string `json:"content"`
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
		n, err := store.Create(Node{Type: req.Type, Parent: req.Parent, Title: req.Title, Content: req.Content})
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
		writeJSON(w, 200, n)
	})
	mux.HandleFunc("PUT /api/nodes/{id}", basicAuth(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Title   string  `json:"title"`
			Content string  `json:"content"`
			Parent  *string `json:"parent"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]string{"error": "请求格式错误"})
			return
		}
		n, err := store.Update(r.PathValue("id"), Node{Title: req.Title, Content: req.Content}, req.Parent)
		if err != nil {
			writeJSON(w, 404, map[string]string{"error": "not found"})
			return
		}
		writeJSON(w, 200, n)
	}))
	mux.HandleFunc("DELETE /api/nodes/{id}", basicAuth(func(w http.ResponseWriter, r *http.Request) {
		count, err := store.Delete(r.PathValue("id"))
		if err != nil {
			writeJSON(w, 404, map[string]string{"error": "not found"})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "deleted": count})
	}))

	// ---------- 文件上传（图片 + 附件）----------
	mux.HandleFunc("POST /api/upload", basicAuth(func(w http.ResponseWriter, r *http.Request) {
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
	servePage := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			b, err := fs.ReadFile(webSub, name)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache") // 防止浏览器缓存旧版页面，升级后强制刷新
			_, _ = w.Write(b)
		}
	}
	mux.HandleFunc("GET /admin", basicAuth(servePage("admin.html")))
	mux.HandleFunc("GET /", servePage("index.html"))

	log.Printf("LAN-CMS v2 已启动: http://0.0.0.0:%s   管理后台: /admin", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}

// basicAuth 用 HTTP Basic Auth 保护后台页面与写操作。
// 用户名/密码来自环境变量 LANCMS_USER / LANCMS_PASS，缺省为 admin / admin123。
func basicAuth(next http.HandlerFunc) http.HandlerFunc {
	user := os.Getenv("LANCMS_USER")
	if user == "" {
		user = "admin"
	}
	pass := os.Getenv("LANCMS_PASS")
	if pass == "" {
		pass = "admin123"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != user || p != pass {
			w.Header().Set("WWW-Authenticate", `Basic realm="秋风小站管理后台"`)
			http.Error(w, "未授权，请输入用户名和密码", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}
