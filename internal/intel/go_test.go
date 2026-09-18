package intel

import (
	"os"
	"path/filepath"
	"testing"
)

// TestScanGoFilesExports extracts exported functions, receiver methods and HTTP
// registrations from a Go module, proving the RAG 盲区 for non-HTTP Go
// projects is closed (pure-function projects now get a retrievable contract
// surface).
func TestScanGoFilesExports(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "api.go", `package svc

// User 用户实体
type User struct {
	ID   int64  `+"`json:\"id\"`"+`
	Name string `+"`json:\"name\"`"+`
}

// Add 求和
func Add(a, b int) int { return a + b }

func NewServer() *Server { return nil }

type Server struct{}

func (s *Server) Handle() string { return "ok" }
`)
	writeGoFile(t, dir, "routes.go", `package svc

import "net/http"

func Routes(mux *http.ServeMux) {
	mux.HandleFunc("/api/users", getUser)
	mux.HandleFunc("/healthz", health)
}

func getUser(w http.ResponseWriter, r *http.Request) {}
func health(w http.ResponseWriter, r *http.Request) {}
`)
	ents, eps := scanGoFiles([]string{filepath.Join(dir, "api.go"), filepath.Join(dir, "routes.go")})

	// 实体：User 的 id/name 两个字段。
	if len(ents) < 2 {
		t.Fatalf("entities = %d, want >=2: %+v", len(ents), ents)
	}
	byCol := map[string]string{}
	for _, e := range ents {
		byCol[e.ColumnName] = e.Entity
	}
	if byCol["id"] != "User" || byCol["name"] != "User" {
		t.Fatalf("struct fields not extracted: %+v", byCol)
	}

	paths := map[string]bool{}
	methods := map[string]bool{}
	for _, e := range eps {
		paths[e.Path] = true
		methods[e.Method] = true
	}
	// 标准库路由注册 → 具体端点。
	if !paths["/api/users"] || !paths["/healthz"] {
		t.Fatalf("http routes missing: %+v", paths)
	}
	// 导出函数与方法 → 伪端点。
	if !paths["/func/Add"] || !paths["/method/server/Handle"] {
		t.Fatalf("exported funcs missing: %+v", paths)
	}
	// 未导出函数不产出（getUser/health 是路由 handler，不应作为伪端点）。
	if paths["/func/getUser"] {
		t.Fatalf("unexported func leaked: %+v", paths)
	}
	if !methods["FUNC"] || !methods["METHOD"] || !methods["GET"] {
		t.Fatalf("expected FUNC/METHOD/GET methods, got %v", methods)
	}
}

// TestScanGoFilesSkipsTestFiles verifies _test.go files are never treated as
// contract sources.
func TestScanGoFilesSkipsTestFiles(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "calc.go", "package c\n\n// Sum 求和\nfunc Sum(a, b int) int { return a + b }\n")
	writeGoFile(t, dir, "calc_test.go", "package c\n\nfunc TestSum(t *testing.T) {}\n")
	_, eps := scanGoFiles([]string{filepath.Join(dir, "calc.go"), filepath.Join(dir, "calc_test.go")})
	for _, e := range eps {
		if filepath.Base(e.SourceFile) == "calc_test.go" {
			t.Fatalf("test file leaked into contracts: %+v", e)
		}
	}
	if len(eps) != 1 || eps[0].Path != "/func/Sum" {
		t.Fatalf("expected only Sum func, got %+v", eps)
	}
}

func writeGoFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
