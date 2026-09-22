package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hiylo/starburst-backend/internal/auth"
	"github.com/hiylo/starburst-backend/internal/opencode"
	"github.com/hiylo/starburst-backend/internal/store"
)

// newProxyEnv builds a Server whose openCode client points at the given fake
// upstream, registers only the mirror proxy route, and provisions an APP token.
func newProxyEnv(t *testing.T, upstream http.HandlerFunc) (*Server, string) {
	t.Helper()
	s := newTestServer(t)
	us := httptest.NewServer(upstream)
	t.Cleanup(us.Close)
	s.openCode = opencode.New(us.URL)

	raw, err := s.auth.CreateToken(context.Background(), "proxy-test", "device")
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(OpenCodeProxyPrefix+"/", s.handleOpenCodeProxy)
	mux.HandleFunc(OpenCodeProxyPrefix, s.handleOpenCodeProxy)
	s.testMux = mux
	return s, raw
}

func TestOpenCodeProxyRequiresToken(t *testing.T) {
	s, _ := newProxyEnv(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream must not be reached without APP token")
	})
	rec := s.do(t, http.MethodGet, OpenCodeProxyPrefix+"/session", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without token, got %d", rec.Code)
	}
}

func TestOpenCodeProxyPassthrough(t *testing.T) {
	var gotMethod, gotPath, gotQuery, gotDirHdr, gotAuth string
	var gotBody string
	upstream := func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotDirHdr = r.Header.Get("x-starburst-directory")
		gotAuth = r.Header.Get("Authorization")
		buf := make([]byte, 64)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Header().Set("X-Upstream", "yes")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"mirrored":true}`))
	}
	s, tok := newProxyEnv(t, upstream)

	rec := s.do(t, http.MethodPost, OpenCodeProxyPrefix+"/session/ses_1/prompt_async?x=1",
		`{"messageID":"msg_1","parts":[]}`,
		map[string]string{
			"Authorization":         "Bearer " + tok,
			"Content-Type":          "application/json",
			"X-Starburst-Directory": "/w/proj",
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/session/ses_1/prompt_async" {
		t.Fatalf("upstream path = %q", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("upstream method = %q", gotMethod)
	}
	if gotQuery != "x=1" {
		t.Fatalf("upstream query = %q", gotQuery)
	}
	if gotDirHdr != "/w/proj" {
		t.Fatalf("directory header not forwarded: %q", gotDirHdr)
	}
	if gotAuth != "" {
		t.Fatalf("APP token must not leak upstream, got %q", gotAuth)
	}
	if !strings.Contains(gotBody, "msg_1") {
		t.Fatalf("request body not forwarded: %q", gotBody)
	}
	if rec.Header().Get("X-Upstream") != "yes" {
		t.Fatalf("upstream response header not relayed")
	}
	if !strings.Contains(rec.Body.String(), `"mirrored":true`) {
		t.Fatalf("upstream body not relayed: %s", rec.Body.String())
	}
}

func TestOpenCodeProxyUpstreamErrorRelayed(t *testing.T) {
	s, tok := newProxyEnv(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	rec := s.do(t, http.MethodGet, OpenCodeProxyPrefix+"/session", "",
		map[string]string{"Authorization": "Bearer " + tok})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want upstream 500 relayed, got %d", rec.Code)
	}
}

func TestOpenCodeProxySSEStream(t *testing.T) {
	upstream := func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("Accept header not forwarded: %q", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"type\":\"a\"}\n\n"))
		fl.Flush()
		_, _ = w.Write([]byte("data: {\"type\":\"b\"}\n\n"))
		fl.Flush()
	}
	s, tok := newProxyEnv(t, upstream)
	rec := s.do(t, http.MethodGet, OpenCodeProxyPrefix+"/global/event", "",
		map[string]string{
			"Authorization": "Bearer " + tok,
			"Accept":        "text/event-stream",
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type not relayed: %q", ct)
	}
	if !strings.Contains(rec.Body.String(), `{"type":"a"}`) || !strings.Contains(rec.Body.String(), `{"type":"b"}`) {
		t.Fatalf("SSE events not relayed: %q", rec.Body.String())
	}
}

func TestOpenCodeProxyBadPath(t *testing.T) {
	s, tok := newProxyEnv(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream must not be reached for non-proxy path")
	})
	rec := s.do(t, http.MethodGet, "/api/not-opencode", "",
		map[string]string{"Authorization": "Bearer " + tok})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 for non-proxy path, got %d", rec.Code)
	}
}

// 凭据载荷里的假值刻意不含 sk-/glpat- 等真实前缀，避免测试自身触发密钥扫描。
const proxyFakeKey = "PLAINTEXT-PROVIDER-KEY-MUST-NOT-RELAY"

func TestOpenCodeProxyRedactsProviderCredentials(t *testing.T) {
	upstream := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"providers":[{"id":"p1","name":"P1","key":"` + proxyFakeKey +
			`","options":{"api_key":"` + proxyFakeKey + `-nested","baseURL":"https://198.51.100.7:18090/v1"},` +
			`"models":{"m1":{"id":"m1","name":"M1","limit":{"context":200000}}}}],"default":{"p1":"m1"}}`))
	}
	s, tok := newProxyEnv(t, upstream)
	rec := s.do(t, http.MethodGet, OpenCodeProxyPrefix+"/config/providers", "",
		map[string]string{"Authorization": "Bearer " + tok})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, proxyFakeKey) {
		t.Fatalf("plaintext provider key relayed to client: %s", body)
	}
	// 下拉框与上下文预算依赖的非凭据字段不能一起抹掉。
	var decoded struct {
		Providers []struct {
			ID      string `json:"id"`
			Options struct {
				BaseURL string `json:"baseURL"`
			} `json:"options"`
			Models map[string]struct {
				Limit struct {
					Context int `json:"context"`
				} `json:"limit"`
			} `json:"models"`
		} `json:"providers"`
		Default map[string]string `json:"default"`
	}
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("redacted body is not valid JSON: %v (%s)", err, body)
	}
	if len(decoded.Providers) != 1 || decoded.Providers[0].ID != "p1" {
		t.Fatalf("provider list lost in redaction: %s", body)
	}
	if got := decoded.Providers[0].Models["m1"].Limit.Context; got != 200000 {
		t.Fatalf("model context limit lost: %d", got)
	}
	if got := decoded.Providers[0].Options.BaseURL; got == "" {
		t.Fatalf("non-credential option blanked: %s", body)
	}
	if decoded.Default["p1"] != "m1" {
		t.Fatalf("default model map lost: %s", body)
	}
}

func TestOpenCodeProxyLeavesOtherPayloadsAlone(t *testing.T) {
	// 会话消息里出现名为 key 的字段（工具入参、快捷键配置等）属正常数据，
	// 只有 provider/config 族才做凭据剥离。
	const body = `{"parts":[{"type":"tool","state":{"input":{"key":"value"}}}]}`
	s, tok := newProxyEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	rec := s.do(t, http.MethodGet, OpenCodeProxyPrefix+"/session/ses_1/message", "",
		map[string]string{"Authorization": "Bearer " + tok})
	if got := rec.Body.String(); got != body {
		t.Fatalf("non-provider payload altered: %s", got)
	}
}

func TestProviderCredentialPath(t *testing.T) {
	open := []string{"/config", "/config/providers", "/provider", "/provider/auth", "/provider/x/oauth/authorize"}
	closed := []string{"/", "/session", "/configx", "/providers", "/permission"}
	for _, p := range open {
		if !providerCredentialPath(p) {
			t.Errorf("%s should be credential-scanned", p)
		}
	}
	for _, p := range closed {
		if providerCredentialPath(p) {
			t.Errorf("%s should not be credential-scanned", p)
		}
	}
}

// 任意命令执行（/shell、/command）与外部分发公链写操作（/share 的 POST/DELETE）
// 必须对设备 token 判为敏感；发消息（/prompt）与 share 读取保持开放。
func TestProxyIsSensitiveSessionRCEAndShare(t *testing.T) {
	sensitive := []struct{ method, path string }{
		{http.MethodPost, "/session/ses_1/shell"},
		{http.MethodPost, "/session/ses_1/command"},
		{http.MethodPost, "/session/ses_1/share"},
		{http.MethodDelete, "/session/ses_1/share"},
		{http.MethodPost, "/auth/anthropic"},
		{http.MethodPatch, "/config/providers"},
		{http.MethodPost, "/global/dispose"},
	}
	for _, c := range sensitive {
		if !proxyIsSensitive(c.method, c.path) {
			t.Errorf("%s %s should be sensitive", c.method, c.path)
		}
	}
	open := []struct{ method, path string }{
		{http.MethodGet, "/session/ses_1/share"},   // 读取已有共享链接
		{http.MethodPost, "/session/ses_1/prompt"}, // App 核心：发消息
		{http.MethodPost, "/session/ses_1/prompt_async"},
		{http.MethodGet, "/session/ses_1/message"},
		{http.MethodGet, "/session/ses_1/shell"}, // 非 POST 不触发
		{http.MethodGet, "/config/providers"},    // 模型下拉框数据源豁免
	}
	for _, c := range open {
		if proxyIsSensitive(c.method, c.path) {
			t.Errorf("%s %s should stay open", c.method, c.path)
		}
	}
}

// 设备 scope 的 token 不能触发 shell / command / share 写等敏感代理操作，admin
// scope 的 token 与 web session 可以；开放路径（GET share、POST prompt）不受挡。
func TestOpenCodeProxyScopeGate(t *testing.T) {
	s, _ := newProxyEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	ctx := context.Background()
	mk := func(scope, name string) string {
		raw := "ocb_scope_" + name
		if err := s.store.CreateToken(ctx, &store.Token{
			ID:        "tok-" + name,
			Name:      name,
			TokenHash: auth.HashToken(raw),
			Scope:     scope,
		}); err != nil {
			t.Fatalf("create %s token: %v", scope, err)
		}
		return raw
	}
	deviceH := map[string]string{"Authorization": "Bearer " + mk("device", "dev")}
	adminH := map[string]string{"Authorization": "Bearer " + mk("admin", "adm")}

	sensitive := []struct{ method, path string }{
		{http.MethodPost, OpenCodeProxyPrefix + "/session/ses_1/shell"},
		{http.MethodPost, OpenCodeProxyPrefix + "/session/ses_1/command"},
		{http.MethodPost, OpenCodeProxyPrefix + "/session/ses_1/share"},
		{http.MethodDelete, OpenCodeProxyPrefix + "/session/ses_1/share"},
		{http.MethodPatch, OpenCodeProxyPrefix + "/config"},
	}
	for _, c := range sensitive {
		if rec := s.do(t, c.method, c.path, "{}", deviceH); rec.Code != http.StatusForbidden {
			t.Errorf("device %s %s: want 403, got %d %s", c.method, c.path, rec.Code, rec.Body.String())
		}
		if rec := s.do(t, c.method, c.path, "{}", adminH); rec.Code != http.StatusOK {
			t.Errorf("admin %s %s: want 200, got %d %s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
	open := []struct{ method, path string }{
		{http.MethodGet, OpenCodeProxyPrefix + "/session/ses_1/share"},
		{http.MethodPost, OpenCodeProxyPrefix + "/session/ses_1/prompt"},
		{http.MethodPost, OpenCodeProxyPrefix + "/session/ses_1/prompt_async"},
	}
	for _, c := range open {
		if rec := s.do(t, c.method, c.path, "{}", deviceH); rec.Code != http.StatusOK {
			t.Errorf("device %s %s open path: want 200, got %d %s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
}

// 模型下拉框的数据源必须对 APP token 开放，否则安卓端列不出可选模型；
// 但它仍属 /config 一族，其它方法/路径继续要求管理员 web session。
func TestOpenCodeProxyProviderListingOpenToAppToken(t *testing.T) {
	s, tok := newProxyEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"providers":[],"default":{}}`))
	})
	auth := map[string]string{"Authorization": "Bearer " + tok}

	rec := s.do(t, http.MethodGet, OpenCodeProxyPrefix+"/config/providers", "", auth)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /config/providers with APP token: got %d %s", rec.Code, rec.Body.String())
	}
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, OpenCodeProxyPrefix + "/config"},
		{http.MethodPatch, OpenCodeProxyPrefix + "/config"},
		{http.MethodPatch, OpenCodeProxyPrefix + "/config/providers"},
		{http.MethodPut, OpenCodeProxyPrefix + "/auth/anthropic"},
		{http.MethodPost, OpenCodeProxyPrefix + "/global/dispose"},
	} {
		rec := s.do(t, c.method, c.path, "{}", auth)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: want 403 for APP token, got %d", c.method, c.path, rec.Code)
		}
	}
}

// 上游把凭据藏在 env 变量风格的字段名里（ANTHROPIC_API_KEY、
// aws_secret_access_key）也必须被抹掉——只比全名会漏。
func TestOpenCodeProxyRedactsEnvStyleCredentialNames(t *testing.T) {
	s, tok := newProxyEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"providers":[{"id":"p1","name":"P1","ANTHROPIC_API_KEY":"` + proxyFakeKey +
			`","aws_secret_access_key":"` + proxyFakeKey + `","headers":{"authorization":"Bearer ` + proxyFakeKey + `"}}]}`))
	})
	rec := s.do(t, http.MethodGet, OpenCodeProxyPrefix+"/config/providers", "",
		map[string]string{"Authorization": "Bearer " + tok})
	if body := rec.Body.String(); strings.Contains(body, proxyFakeKey) {
		t.Fatalf("env-style credential relayed: %s", body)
	}
	if !strings.Contains(rec.Body.String(), `"id":"p1"`) {
		t.Fatalf("over-redacted: %s", rec.Body.String())
	}
}

// Content-Type 缺失或不是 JSON 时不能按类型放行（等于把密钥原文透出去），
// 仍然是缓冲后剥离；剥不动就 502，不做透传。
func TestOpenCodeProxyRedactsRegardlessOfContentType(t *testing.T) {
	s, tok := newProxyEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(`{"providers":[{"id":"p1","key":"` + proxyFakeKey + `"}]}`))
	})
	rec := s.do(t, http.MethodGet, OpenCodeProxyPrefix+"/config/providers", "",
		map[string]string{"Authorization": "Bearer " + tok})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, proxyFakeKey) {
		t.Fatalf("credential relayed on non-JSON content type: %s", body)
	}

	// 非 JSON 正文（上游异常页）失败关闭。
	bad, badTok := newProxyEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html>gateway error</html>`))
	})
	rec = bad.do(t, http.MethodGet, OpenCodeProxyPrefix+"/config/providers", "",
		map[string]string{"Authorization": "Bearer " + badTok})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("unparseable credential payload: want 502, got %d %s", rec.Code, rec.Body.String())
	}
}

// 凭据路径上的空响应（204）只回状态码，不能被 502 掩盖成失败。
func TestOpenCodeProxyRelayEmptyCredentialPathResponse(t *testing.T) {
	s, _ := newProxyEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	if err := s.store.CreateWebSession(context.Background(), "proxy-admin-sid", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create web session: %v", err)
	}
	rec := s.do(t, http.MethodDelete, OpenCodeProxyPrefix+"/config/custom", "",
		map[string]string{"X-Web-Session": "proxy-admin-sid"})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestOpenCodeProxyRejectsOversizeRequestBody(t *testing.T) {
	s, tok := newProxyEnv(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream must not be reached for an oversize body")
	})
	req := httptest.NewRequest(http.MethodPost, OpenCodeProxyPrefix+"/session/ses_1/message", nil)
	req.ContentLength = maxProxyRequestBytes + 1
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	s.testMux.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413, got %d: %s", rec.Code, rec.Body.String())
	}
}
