// LAN-CMS：局域网轻量内容管理系统
// 单二进制，零数据库（JSON 文件存储），内置富文本编辑器
// 管理后台: /admin    前台: /
package main

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

// Page 一个页面（标题 + 富文本内容）
type Page struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Store 简单的 JSON 文件存储
type Store struct {
	mu    sync.Mutex
	dir   string
	path  string
	pages map[string]Page
}

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "uploads"), 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, path: filepath.Join(dir, "pages.json"), pages: map[string]Page{}}
	if b, err := os.ReadFile(s.path); err == nil {
		var list []Page
		if err := json.Unmarshal(b, &list); err == nil {
			for _, p := range list {
				s.pages[p.ID] = p
			}
		}
	}
	return s, nil
}

func (s *Store) persistLocked() error {
	var list []Page
	for _, p := range s.pages {
		list = append(list, p)
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

func (s *Store) List() []Page {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := make([]Page, 0, len(s.pages))
	for _, p := range s.pages {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].UpdatedAt.After(list[j].UpdatedAt) })
	return list
}

func (s *Store) Get(id string) (Page, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pages[id]
	return p, ok
}

func (s *Store) Create(title, content string) (Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	p := Page{ID: newID(), Title: title, Content: content, CreatedAt: now, UpdatedAt: now}
	s.pages[p.ID] = p
	if err := s.persistLocked(); err != nil {
		delete(s.pages, p.ID)
		return Page{}, err
	}
	return p, nil
}

func (s *Store) Update(id, title, content string) (Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pages[id]
	if !ok {
		return Page{}, os.ErrNotExist
	}
	p.Title = title
	p.Content = content
	p.UpdatedAt = time.Now()
	s.pages[id] = p
	if err := s.persistLocked(); err != nil {
		return Page{}, err
	}
	return p, nil
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pages[id]; !ok {
		return os.ErrNotExist
	}
	delete(s.pages, id)
	return s.persistLocked()
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

	mux := http.NewServeMux()

	// ---------- 页面 API ----------
	mux.HandleFunc("GET /api/pages", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, store.List())
	})
	mux.HandleFunc("POST /api/pages", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Title   string `json:"title"`
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]string{"error": "请求格式错误"})
			return
		}
		p, err := store.Create(req.Title, req.Content)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 201, p)
	})
	mux.HandleFunc("GET /api/pages/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := store.Get(r.PathValue("id"))
		if !ok {
			writeJSON(w, 404, map[string]string{"error": "not found"})
			return
		}
		writeJSON(w, 200, p)
	})
	mux.HandleFunc("PUT /api/pages/{id}", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Title   string `json:"title"`
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]string{"error": "请求格式错误"})
			return
		}
		p, err := store.Update(r.PathValue("id"), req.Title, req.Content)
		if err != nil {
			writeJSON(w, 404, map[string]string{"error": "not found"})
			return
		}
		writeJSON(w, 200, p)
	})
	mux.HandleFunc("DELETE /api/pages/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := store.Delete(r.PathValue("id")); err != nil {
			writeJSON(w, 404, map[string]string{"error": "not found"})
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	})

	// ---------- 图片上传 ----------
	mux.HandleFunc("POST /api/upload", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 20<<20)
		if err := r.ParseMultipartForm(20 << 20); err != nil {
			writeJSON(w, 400, map[string]string{"error": "文件太大或格式错误（最大 20MB）"})
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": "缺少 file 字段"})
			return
		}
		defer file.Close()

		ext := strings.ToLower(filepath.Ext(header.Filename))
		if !allowedImageExt[ext] {
			writeJSON(w, 400, map[string]string{"error": "仅支持 jpg / png / gif / webp"})
			return
		}
		name := time.Now().Format("20060102150405") + "-" + newID()[:6] + ext
		dst, err := os.Create(filepath.Join(uploadsDir, name))
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "保存失败"})
			return
		}
		if _, err := io.Copy(dst, file); err != nil {
			dst.Close()
			_ = os.Remove(filepath.Join(uploadsDir, name))
			writeJSON(w, 500, map[string]string{"error": "写入失败"})
			return
		}
		dst.Close()

		writeJSON(w, 200, map[string]any{
			"errno": 0,
			"data":  map[string]string{"url": "/uploads/" + name, "alt": header.Filename, "href": ""},
		})
	})

	// 已上传的图片
	mux.Handle("GET /uploads/", http.StripPrefix("/uploads/", http.FileServer(http.Dir(uploadsDir))))

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
			_, _ = w.Write(b)
		}
	}
	mux.HandleFunc("GET /admin", servePage("admin.html"))
	mux.HandleFunc("GET /", servePage("index.html"))

	log.Printf("LAN-CMS 已启动: http://0.0.0.0:%s   管理后台: /admin", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}
