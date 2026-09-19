package server

// directory / localPath 这类「后端会切进去操作」的入参共用的路径规则（internal
// /server/paths.go）：系统目录、软链接逃逸、`..` 越界必须拒；正常仓库路径与留空
// 必须放行。

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestValidateWorkDirectoryRules(t *testing.T) {
	allowed := []string{
		"",     // 留空 = 用默认目录
		"   ",  // 全空白同义
		"/w",   // 测试里常见的顶层目录
		"data", // 相对路径由上游解析
		"./sub/proj",
		filepath.Join(t.TempDir(), "repo"),
		"/var/lib/builds/app", // /var 子树不是系统目录禁区
	}
	for _, p := range allowed {
		if err := validateWorkDirectory(p); err != nil {
			t.Errorf("validateWorkDirectory(%q) = %v, want nil", p, err)
		}
	}

	blocked := []string{
		"/",
		"/etc",
		"/etc/ssh",
		"/proc/self/environ",
		"/root/.ssh",
		"/sys/class",
		"/dev/null",
		"/var/../etc/ssh", // Clean 之后就是 /etc/ssh
		"../escape",
		"a/../../etc/ssh",
	}
	for _, p := range blocked {
		if err := validateWorkDirectory(p); err == nil {
			t.Errorf("validateWorkDirectory(%q) = nil, want error", p)
		}
	}

	// 软链接不能当旁路：链接名普通，解析后落在系统目录里同样拒绝。
	link := filepath.Join(t.TempDir(), "workspace")
	if err := os.Symlink("/etc/ssh", link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := validateWorkDirectory(link); err == nil {
		t.Errorf("symlinked system dir accepted: %s", link)
	}
}

func TestValidateIntelLocalPathRules(t *testing.T) {
	root := t.TempDir()
	if err := validateIntelLocalPath(root); err != nil {
		t.Fatalf("valid repo dir rejected: %v", err)
	}
	if err := validateIntelLocalPath(root + "/./"); err != nil {
		t.Fatalf("uncleaned valid dir rejected: %v", err)
	}
	// 比 directory 更严：顶层单段目录与不存在的目录都不收。
	for _, p := range []string{"/tmp", "/data", "/no/such/dir-xyz", "relative/repo"} {
		if err := validateIntelLocalPath(p); err == nil {
			t.Errorf("validateIntelLocalPath(%q) = nil, want error", p)
		}
	}
	writeTestFile(t, filepath.Join(root, "pom.xml"), "<project/>")
	if err := validateIntelLocalPath(filepath.Join(root, "pom.xml")); err == nil {
		t.Error("file instead of directory accepted")
	}
}

// 四个持久化入口都要挡住越界目录，合法目录保持可用（回归保护）。
func TestDirectoryParamRejectedOnWriteEndpoints(t *testing.T) {
	s := newTestServer(t)
	th := sttTestToken(t, s)

	for _, c := range []struct{ path, body string }{
		{"/api/tasks", `{"prompt":"x","directory":"/etc/ssh"}`},
		{"/api/batch", `{"prompt":"x","targets":[{"directory":"/etc"}]}`},
		{"/api/workflow", `{"name":"wf","steps":[{"prompt":"p","directory":"/proc/self/root"}]}`},
		{"/api/workflow", `{"name":"wf","directory":"/root/.ssh","steps":[{"prompt":"p"}]}`},
		{"/api/rules", `{"kind":"git","prompt":"p","schedule":"/etc/ssh"}`},
	} {
		rec := s.do(t, http.MethodPost, c.path, c.body, th)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST %s %s: want 400, got %d %s", c.path, c.body, rec.Code, rec.Body.String())
		}
	}

	// 合法目录：与上面同一批接口仍然可建。
	ok := filepath.ToSlash(t.TempDir())
	rec := s.do(t, http.MethodPost, "/api/tasks", `{"prompt":"x","directory":"`+ok+`"}`, th)
	if rec.Code != http.StatusCreated {
		t.Errorf("valid directory task: want 201, got %d %s", rec.Code, rec.Body.String())
	}
	rec = s.do(t, http.MethodPost, "/api/batch", `{"prompt":"x","targets":[{"directory":"`+ok+`"}]}`, th)
	if rec.Code != http.StatusCreated {
		t.Errorf("valid directory batch: want 201, got %d %s", rec.Code, rec.Body.String())
	}
	rec = s.do(t, http.MethodPost, "/api/rules", `{"kind":"git","prompt":"p","schedule":"`+ok+`"}`, th)
	if rec.Code != http.StatusCreated {
		t.Errorf("valid git rule: want 201, got %d %s", rec.Code, rec.Body.String())
	}
	// directory 留空同样可用（用默认目录）。
	rec = s.do(t, http.MethodPost, "/api/tasks", `{"prompt":"x"}`, th)
	if rec.Code != http.StatusCreated {
		t.Errorf("empty directory task: want 201, got %d %s", rec.Code, rec.Body.String())
	}
}
