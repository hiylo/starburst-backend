package web

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanWeb(t *testing.T) {
	src := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(src, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 跳过目录：node_modules / dist / build 不参与扫描。
	write("node_modules/vendor.js", `axios.get('/ignored')`)
	write("dist/bundle.js", `axios.post('/ignored-dist')`)
	write("build/out.js", `axios.put('/ignored-build')`)
	// 动词写法：axios.get/post、request.put，字符串字面量路径。
	write("src/api/user.ts", `export const api = {
  fetchUser() { return axios.get('/api/users'); },
  createUser() { return axios.post('/api/users'); },
  updateUser() { return request.put('/api/users/1'); },
};`)
	// axios 对象写法（method+url）：单行与跨行各一例，method 优先。
	write("src/api/detail.ts", `export function loadDetail() {
  return axios({ method: 'get', url: '/api/detail' });
}
export function updateDetail() {
  return axios({
    method: 'put',
    url: '/api/detail',
  });
}`)
	// fetch：带 method 与缺省 method（默认 GET）。
	write("src/api/fetch.ts", `export async function save() {
  const r = await fetch('/api/save', { method: 'POST', headers: {} });
}
export async function load() {
  return fetch('/api/load');
}`)
	// 去重：同一 (method, path) 只保留首个。zzdup 排在 user.ts 之后（walk 为
	// 字典序），因此 GET /api/users 的首个来源仍是 src/api/user.ts:2。
	write("src/api/zzdup.ts", `const a = axios.get('/api/users');
const b = axios.get('/api/users');`)

	eps, err := ScanWeb(src)
	if err != nil {
		t.Fatal(err)
	}

	type want struct {
		method, path, file string
		line               int
	}
	wants := []want{
		{"GET", "/api/users", "src/api/user.ts", 2},
		{"POST", "/api/users", "src/api/user.ts", 3},
		{"PUT", "/api/users/1", "src/api/user.ts", 4},
		{"GET", "/api/detail", "src/api/detail.ts", 2},
		{"PUT", "/api/detail", "src/api/detail.ts", 5},
		{"POST", "/api/save", "src/api/fetch.ts", 2},
		{"GET", "/api/load", "src/api/fetch.ts", 5},
	}
	got := map[string]want{}
	for _, ep := range eps {
		key := ep.Method + " " + ep.Path
		if _, dup := got[key]; dup {
			t.Errorf("duplicate endpoint %q not deduplicated", key)
		}
		got[key] = want{ep.Method, ep.Path, ep.SourceFile, ep.SourceLine}
	}
	if len(got) != len(wants) {
		t.Fatalf("got %d endpoints, want %d: %+v", len(got), len(wants), eps)
	}
	for _, w := range wants {
		key := w.method + " " + w.path
		g, ok := got[key]
		if !ok {
			t.Errorf("missing endpoint %q", key)
			continue
		}
		if g.file != w.file {
			t.Errorf("endpoint %q SourceFile = %q, want %q", key, g.file, w.file)
		}
		if g.line != w.line {
			t.Errorf("endpoint %q SourceLine = %d, want %d", key, g.line, w.line)
		}
	}
}

func TestScanWebSkipsDynamicPath(t *testing.T) {
	src := t.TempDir()
	path := filepath.Join(src, "api.ts")
	content := `const url = '/api/dynamic';
export function get(id) {
  return axios.get('/api/users/' + id);
  return axios.get(\` + "`" + `/api/tpl/${id}` + "`" + `);
  return fetch(url);
}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	eps, err := ScanWeb(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 0 {
		t.Errorf("got %d endpoints for dynamic/variable paths, want 0: %+v", len(eps), eps)
	}
}
