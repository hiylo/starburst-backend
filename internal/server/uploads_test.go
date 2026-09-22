package server

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildUploadRequest encodes directory + file bytes as multipart/form-data.
func buildUploadRequest(t *testing.T, dir, name string, data []byte) (path, body string, ct string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if dir != "" {
		_ = w.WriteField("directory", dir)
	}
	fw, err := w.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(data)
	_ = w.Close()
	return "/api/opencode/upload", buf.String(), w.FormDataContentType()
}

func TestUploadFileWritesToWorkspace(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)
	ws := t.TempDir() // 当作会话工作目录（绝对路径、非系统目录）

	_, body, ct := buildUploadRequest(t, ws, "时序表.csv", []byte("型号,价格\nX,99\n"))
	req := s.do(t, http.MethodPost, "/api/opencode/upload", body, mergeWith(map[string]string{"Content-Type": ct}, wh))
	if req.Code != http.StatusOK {
		t.Fatalf("upload status %d: %s", req.Code, req.Body.String())
	}

	// 文件必须真实落到 <ws>/uploads/时序表.csv。
	onDisk := filepath.Join(ws, "uploads", "时序表.csv")
	raw, err := os.ReadFile(onDisk)
	if err != nil {
		t.Fatalf("uploaded file missing: %v", err)
	}
	if !strings.Contains(string(raw), "X,99") {
		t.Fatalf("uploaded content wrong: %q", string(raw))
	}
}

func TestUploadFileNoClobberOnReupload(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)
	ws := t.TempDir()

	_, body, ct := buildUploadRequest(t, ws, "a.txt", []byte("v1"))
	r1 := s.do(t, http.MethodPost, "/api/opencode/upload", body, mergeWith(map[string]string{"Content-Type": ct}, wh))
	if r1.Code != http.StatusOK {
		t.Fatalf("first upload status %d", r1.Code)
	}
	_, body2, ct2 := buildUploadRequest(t, ws, "a.txt", []byte("v2"))
	r2 := s.do(t, http.MethodPost, "/api/opencode/upload", body2, mergeWith(map[string]string{"Content-Type": ct2}, wh))
	if r2.Code != http.StatusOK {
		t.Fatalf("second upload status %d: %s", r2.Code, r2.Body.String())
	}
	// 两次上传都保留：a.txt 与 a-1.txt。
	if _, err := os.Stat(filepath.Join(ws, "uploads", "a.txt")); err != nil {
		t.Fatalf("first file lost: %v", err)
	}
	second, err := os.ReadFile(filepath.Join(ws, "uploads", "a-1.txt"))
	if err != nil || string(second) != "v2" {
		t.Fatalf("second file: %q, %v", string(second), err)
	}
}

func TestUploadFilePathSafety(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	// 系统目录被拒。
	_, body, ct := buildUploadRequest(t, "/etc", "x.txt", []byte("x"))
	if rec := s.do(t, http.MethodPost, "/api/opencode/upload", body, mergeWith(map[string]string{"Content-Type": ct}, wh)); rec.Code != http.StatusForbidden {
		t.Fatalf("system dir upload status = %d, want 403", rec.Code)
	}
	// 相对目录被拒（写入需要绝对路径）。
	_, body2, ct2 := buildUploadRequest(t, "relative/dir", "x.txt", []byte("x"))
	if rec := s.do(t, http.MethodPost, "/api/opencode/upload", body2, mergeWith(map[string]string{"Content-Type": ct2}, wh)); rec.Code != http.StatusBadRequest {
		t.Fatalf("relative dir upload status = %d, want 400", rec.Code)
	}
	// 文件名为空/非法 → 400（缺 name 且文件名被清洗为空）。
	_, body3, ct3 := buildUploadRequest(t, t.TempDir(), "..", []byte("x"))
	if rec := s.do(t, http.MethodPost, "/api/opencode/upload", body3, mergeWith(map[string]string{"Content-Type": ct3}, wh)); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad filename upload status = %d, want 400", rec.Code)
	}
	// 未鉴权 → 401。
	_, body4, ct4 := buildUploadRequest(t, t.TempDir(), "x.txt", []byte("x"))
	if rec := s.do(t, http.MethodPost, "/api/opencode/upload", body4, map[string]string{"Content-Type": ct4}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated upload status = %d, want 401", rec.Code)
	}
}

func TestSanitizeUploadName(t *testing.T) {
	cases := map[string]string{
		"safe.txt":        "safe.txt",
		"../evil.txt":     "evil.txt",
		"a/b/c.md":        "c.md",
		"..":              "",
		".":               "",
		"带空格 目录.xlsx": "带空格 目录.xlsx",
	}
	for in, want := range cases {
		if got := sanitizeUploadName(in); got != want {
			t.Errorf("sanitizeUploadName(%q) = %q, want %q", in, got, want)
		}
	}
}

// mergeWith merges two header maps for tests.
func mergeWith(a, b map[string]string) map[string]string {
	for k, v := range b {
		a[k] = v
	}
	return a
}