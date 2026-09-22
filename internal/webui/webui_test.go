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