package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
)

//go:embed static
var staticFS embed.FS

// FS implements the server.webUIFSProvider contract over the embedded assets.
type FS struct {
	sub fs.FS
	// buildStamp 每次进程启动生成，用于 index.html 里静态资源 URL 的 ?v= 缓存破坏：
	// 前端 JS/HTML 随后端版本更新，浏览器（含 App WebView / 强缓存）可能拿旧资产导致
	// 行为与新后端不一致，带版本号的 query 保证部署后必定加载新文件。
	buildStamp string
}

// New returns a webUI provider serving the embedded static assets.
func New() *FS {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	return &FS{sub: sub, buildStamp: strconv.FormatInt(time.Now().Unix(), 36)}
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
	// 隐藏的移动端入口：/mobile 映射到移动端壳页，独立于后台控制台导航。
	if p == "/mobile" {
		p = "/mobile.html"
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
	// 安全头：防 MIME 嗅探执行（页面公开可访问，兜一层）。
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	// CSP：脚本按 CSP3 拆成 elem / attr 两级，兼顾安全与可用性——
	// script-src-elem 'self' 阻断 XSS 注入的内联 <script>（页面脚本已全部外置
	// 到 /assets/*.js，doc 预览与知识库页同样零内联脚本），阻断数据外传；
	// script-src-attr 'unsafe-inline' 放行 index.html / app.js 里的内联事件属性
	//（onclick 等 100+ 处），否则整站按钮点击全被拦截失效。
	// 兜底的 script-src 保留 'unsafe-inline'：不支持 elem/attr 拆分的旧浏览器
	// （以及 Chromium 把 script-src 当额外约束而非回退的版本）只认它，若收紧到
	// 'self' 会在这些浏览器里静默废掉全部内联事件属性。代价是旧浏览器放行内联
	// <script>（与拆分前的基线一致）；支持 CSP3 的浏览器仍由 elem 'self' 拦住。
	// 样式保留 'unsafe-inline'（前端 100+ 处内联 style）+ Google Fonts。
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; script-src 'self' 'unsafe-inline'; "+
			"script-src-elem 'self'; script-src-attr 'unsafe-inline'; "+
			"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; "+
			"font-src 'self' https://fonts.gstatic.com data:; "+
			"img-src 'self' data: blob:; connect-src 'self' ws: wss:; "+
			"object-src 'none'; base-uri 'self'")
	// index.html / mobile.html 里的 ?v=__BUILD__ 换成进程构建戳，强制静态资源走新版本。
	if (name == "index.html" || name == "mobile.html") && strings.Contains(string(data), "__BUILD__") {
		data = []byte(strings.ReplaceAll(string(data), "__BUILD__", f.buildStamp))
	}
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
