package server

// 测试智能的三类入参收敛：本地源码目录（之后会被 walk + ReadFile 读内容）、
// 项目写操作（管理员动作）、以及 baseUrl 出网目标（元数据地址必须先拒）。

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIntelProjectLocalPathMustStayOutsideSystemDirs(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	for _, bad := range []string{
		"relative/dir",       // 相对路径落在后端 cwd，语义不确定
		"/no/such/dir-xyz",   // 不存在
		"/tmp",               // 一级目录：整棵顶层目录不可能是一个仓库
		"/etc",               // 敏感根目录
		"/etc/ssh",           // 敏感目录子树
		"/proc/self/environ", // 伪文件系统内的任意可读文件
	} {
		rec := s.do(t, http.MethodPost, "/api/intel/projects",
			`{"name":"demo","source":"local","localPath":"`+bad+`"}`, wh)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("localPath %q: want 400, got %d %s", bad, rec.Code, rec.Body.String())
		}
	}

	// 软链接不能当旁路：链接名看着普通，解析后落在 /etc 下同样拒绝。
	link := filepath.Join(t.TempDir(), "repo")
	if err := os.Symlink("/etc/ssh", link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	rec := s.do(t, http.MethodPost, "/api/intel/projects",
		`{"name":"demo","source":"local","localPath":"`+filepath.ToSlash(link)+`"}`, wh)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("symlinked system dir: want 400, got %d %s", rec.Code, rec.Body.String())
	}

	// 正常目录仍然可用（回归保护），并验证 sources 走同一套校验。
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "pom.xml"), `<project></project>`)
	rec = s.do(t, http.MethodPost, "/api/intel/projects",
		`{"name":"demo","source":"local","localPath":"`+filepath.ToSlash(root)+`"}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid local path rejected: %d %s", rec.Code, rec.Body.String())
	}
	var proj struct {
		ID        int64  `json:"id"`
		LocalPath string `json:"localPath"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &proj); err != nil || proj.ID == 0 {
		t.Fatalf("create response: %s", rec.Body.String())
	}
	if proj.LocalPath != filepath.ToSlash(root) {
		t.Errorf("stored localPath mismatch: %q want %q", proj.LocalPath, filepath.ToSlash(root))
	}

	rec = s.do(t, http.MethodPut, "/api/intel/projects/"+jsonInt(proj.ID)+"/sources",
		`{"sources":[{"endName":"svc","source":"local","localPath":"/etc/ssh"}]}`, wh)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("sources localPath /etc/ssh: want 400, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestIntelProjectWritesRequireWebSession(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)
	th := sttTestToken(t, s)

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "pom.xml"), `<project></project>`)
	body := `{"name":"demo","source":"local","localPath":"` + filepath.ToSlash(root) + `"}`

	rec := s.do(t, http.MethodPost, "/api/intel/projects", body, th)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("create with APP token: want 403, got %d %s", rec.Code, rec.Body.String())
	}
	// 鉴权判定在管理员判定之前：完全没凭据仍是 401，不泄露「该接口存在」。
	rec = s.do(t, http.MethodPost, "/api/intel/projects", body, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("create without credentials: want 401, got %d %s", rec.Code, rec.Body.String())
	}

	rec = s.do(t, http.MethodPost, "/api/intel/projects", body, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("create with web session: %d %s", rec.Code, rec.Body.String())
	}
	var proj struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &proj); err != nil || proj.ID == 0 {
		t.Fatalf("create response: %s", rec.Body.String())
	}
	id := jsonInt(proj.ID)

	for _, c := range []struct{ method, path, body string }{
		{http.MethodPut, "/api/intel/projects/" + id, `{"description":"x"}`},
		{http.MethodPut, "/api/intel/projects/" + id + "/sources", `{"sources":[]}`},
		{http.MethodDelete, "/api/intel/projects/" + id, ""},
	} {
		rec := s.do(t, c.method, c.path, c.body, th)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s with APP token: want 403, got %d %s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}

	// 读侧保持对 APP token 开放。
	rec = s.do(t, http.MethodGet, "/api/intel/projects", "", th)
	if rec.Code != http.StatusOK {
		t.Errorf("list with APP token: want 200, got %d %s", rec.Code, rec.Body.String())
	}
	rec = s.do(t, http.MethodGet, "/api/intel/projects/"+id, "", th)
	if rec.Code != http.StatusOK {
		t.Errorf("detail with APP token: want 200, got %d %s", rec.Code, rec.Body.String())
	}
	// 项目仍在（写操作被拒后不应有副作用）。
	rec = s.do(t, http.MethodPut, "/api/intel/projects/"+id, `{"description":"x"}`, wh)
	if rec.Code != http.StatusOK {
		t.Errorf("PUT with web session: want 200, got %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"description":"x"`) {
		t.Errorf("description not saved: %s", rec.Body.String())
	}
}

// baseUrl 由客户端给出、后端代为请求：元数据地址必须在发出任何请求之前拒绝。
func TestIntelBaseURLBlockedBeforeRequest(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	for _, bad := range []string{
		"http://169.254.169.254/latest/meta-data/", // 链路本地：云元数据
		"http://100.100.100.200/latest/meta-data/", // 阿里云元数据
		"http://[fd00:ec2::254]/",                  // AWS IPv6 元数据
		"http://0.0.0.0:8080/",                     // 未指定地址
		"file:///etc/passwd",                       // 非 http(s)
		"not a url",
	} {
		rec := s.do(t, http.MethodPost, "/api/intel/contracts/check-batch",
			`{"projectId":1,"baseUrl":"`+bad+`"}`, wh)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("baseUrl %q: want 400, got %d %s", bad, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "baseUrl") {
			t.Errorf("baseUrl %q: 期望指向入参的报错，实际 %s", bad, rec.Body.String())
		}
	}

	rec := s.do(t, http.MethodPost, "/api/intel/features/test",
		`{"projectId":1,"featureId":1,"baseUrl":"http://169.254.169.254/"}`, wh)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("features/test metadata baseUrl: want 400, got %d %s", rec.Code, rec.Body.String())
	}
}
