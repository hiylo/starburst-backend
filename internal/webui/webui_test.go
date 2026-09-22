package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func servePath(f *FS, p string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, p, nil)
	rec := httptest.NewRecorder()
	f.Serve(rec, req, p)
	return rec
}

// TestDocPreviewPageServed ensures the document-preview page and its vendored
// renderer libraries are embedded and served with the right content types.
func TestDocPreviewPageServed(t *testing.T) {
	f := New()

	page := servePath(f, "/doc/preview.html")
	if page.Code != http.StatusOK {
		t.Fatalf("preview page status = %d", page.Code)
	}
	if !strings.Contains(page.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("preview content-type = %s", page.Header().Get("Content-Type"))
	}
	if !strings.Contains(page.Body.String(), `<div id="stage">`) {
		t.Fatal("preview page missing stage element")
	}
	// CSP 禁内联脚本：页面引用的所有脚本必须来自 /doc/vendor/ 且为外置文件。
	body := page.Body.String()
	for _, lib := range []string{"/doc/vendor/pdf.min.js", "/doc/vendor/mammoth.browser.min.js",
		"/doc/vendor/xlsx.full.min.js", "/doc/vendor/jszip.min.js", "/doc/preview.js"} {
		if !strings.Contains(body, `src="`+lib+`"`) {
			t.Errorf("preview page missing script tag %s", lib)
		}
	}
	if strings.Contains(body, "<script>") {
		t.Error("preview page contains an inline script, blocked by CSP")
	}
	if strings.Contains(body, `src="http`) || strings.Contains(body, `src="https`) {
		t.Error("preview page loads a third-party script, blocked by CSP")
	}
}

func TestDocVendorLibrariesEmbedded(t *testing.T) {
	f := New()
	for _, p := range []string{
		"/doc/vendor/pdf.min.js",
		"/doc/vendor/pdf.worker.min.js",
		"/doc/vendor/mammoth.browser.min.js",
		"/doc/vendor/xlsx.full.min.js",
		"/doc/vendor/jszip.min.js",
		"/doc/preview.js",
	} {
		rec := servePath(f, p)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d", p, rec.Code)
		}
		if !strings.Contains(rec.Header().Get("Content-Type"), "javascript") {
			t.Errorf("%s content-type = %s", p, rec.Header().Get("Content-Type"))
		}
		if len(rec.Body.Bytes()) < 1024 {
			t.Errorf("%s suspiciously small (%d bytes)", p, len(rec.Body.Bytes()))
		}
	}

	if rec := servePath(f, "/doc/../../../etc/passwd"); rec.Code != http.StatusNotFound {
		t.Fatalf("traversal should 404, got %d", rec.Code)
	}
}

// TestKbManagementPageServed verifies the standalone KB management page and its
// script are embedded, served with the right content type and free of inline
// scripts (CSP `script-src-elem 'self'`).
func TestKbManagementPageServed(t *testing.T) {
	f := New()

	page := servePath(f, "/kb.html")
	if page.Code != http.StatusOK || !strings.Contains(page.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("kb.html status=%d ct=%s", page.Code, page.Header().Get("Content-Type"))
	}
	body := page.Body.String()
	if !strings.Contains(body, `src="/assets/kb.js"`) {
		t.Fatal("kb.html missing kb.js script tag")
	}
	if strings.Contains(body, "<script>") {
		t.Fatal("kb.html must have no inline script (blocked by script-src-elem 'self')")
	}
	if strings.Contains(body, "onclick=") {
		t.Fatal("kb.html must have no inline event handlers (use addEventListener)")
	}

	js := servePath(f, "/assets/kb.js")
	if js.Code != http.StatusOK || !strings.Contains(js.Header().Get("Content-Type"), "javascript") {
		t.Fatalf("kb.js status=%d ct=%s", js.Code, js.Header().Get("Content-Type"))
	}
	if len(js.Body.Bytes()) < 1024 {
		t.Fatalf("kb.js suspiciously small (%d bytes)", len(js.Body.Bytes()))
	}
}

// TestCSPInlineScriptSplit pins the elem/attr CSP split that keeps inline
// <script> blocked while inline event handlers (onclick 等) keep working.
// 兜底 script-src 必须保留 'unsafe-inline'：不支持 elem/attr 拆分的旧浏览器
// 只认它，收紧到 'self' 会静默废掉 index.html 里 100+ 处内联事件属性。
func TestCSPInlineScriptSplit(t *testing.T) {
	f := New()
	rec := servePath(f, "/index.html")
	if rec.Code != http.StatusOK {
		t.Fatalf("index.html status = %d", rec.Code)
	}

	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{
		"script-src 'self' 'unsafe-inline'", // 旧浏览器兜底：内联事件属性靠它存活
		"script-src-elem 'self'",            // CSP3 浏览器：内联 <script> 仍被拦
		"script-src-attr 'unsafe-inline'",   // CSP3 浏览器：内联事件属性显式放行
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP 缺少 %q：%s", want, csp)
		}
	}
	if strings.Contains(csp, "script-src-elem 'self' 'unsafe-inline'") {
		t.Errorf("script-src-elem 不应放行内联 <script>：%s", csp)
	}

	body := rec.Body.String()
	if strings.Contains(body, "<script>") {
		t.Error("index.html 含内联 <script>，会被 script-src-elem 'self' 拦截")
	}
	for _, p := range []string{"/assets/theme.js", "/assets/chat.js", "/assets/app.js"} {
		if !strings.Contains(body, `src="`+p) {
			t.Errorf("index.html 缺少外置脚本 %s", p)
		}
	}
}

// TestCSPMobilePageServed covers the hidden /mobile entry with the same headers.
func TestCSPMobilePageServed(t *testing.T) {
	f := New()
	rec := servePath(f, "/mobile")
	if rec.Code != http.StatusOK {
		t.Fatalf("/mobile status = %d", rec.Code)
	}
	if rec := servePath(f, "/mobile.html"); rec.Code != http.StatusOK {
		t.Fatalf("/mobile.html status = %d", rec.Code)
	}
}
