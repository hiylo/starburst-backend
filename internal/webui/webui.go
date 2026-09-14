package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed static
var staticFS embed.FS

// FS implements the server.webUIFSProvider contract over the embedded assets.
type FS struct {
	sub fs.FS
}

// New returns a webUI provider serving the embedded static assets.
func New() *FS {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	return &FS{sub: sub}
}

// Open reads a file from the embedded assets.
func (f *FS) Open(name string) ([]byte, error) {
	return fs.ReadFile(f.sub, path.Clean("/"+name))
}

// Serve writes an embedded asset (or 404) to the response.
func (f *FS) Serve(w http.ResponseWriter, r *http.Request, p string) {
	p = path.Clean("/" + p)
	if p == "/" || p == "" {
		p = "/index.html"
	}
	// fs.FS paths must be relative (no leading slash).
	name := strings.TrimPrefix(p, "/")
	data, err := fs.ReadFile(f.sub, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType(name))
	// 禁止缓存：前端 JS 与 HTML 随后端版本更新，浏览器缓存的旧资产会导致
	// 登录/鉴权行为与新后端不一致（例如旧的 api() 会在任意 401 时清会话）。
	// 资产总量很小，no-cache 无性能负担。
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func contentType(name string) string {
	switch path.Ext(name) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js":
		return "application/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	default:
		return "text/plain; charset=utf-8"
	}
}
