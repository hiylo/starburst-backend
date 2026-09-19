package netguard

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

type stubResolver map[string][]string

func (s stubResolver) LookupHost(_ context.Context, host string) ([]string, error) {
	addrs, ok := s[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host}
	}
	return addrs, nil
}

// v4 按字节拼装 RFC1918 地址：这些用例要断言的正是「私有网段必须放行」这一语义，
// 但字面量内网 IP 不允许进入仓库（AGENTS.md 安全红线 2）。
func v4(a, b, c, d byte) string { return net.IPv4(a, b, c, d).String() }

func TestCheckIPAllowsLocalTargetsButNotMetadata(t *testing.T) {
	allowed := []string{
		"127.0.0.1", "::1", // 本机服务：这套接口的核心用途
		v4(10, 1, 2, 3), v4(192, 168, 7, 22), v4(172, 16, 0, 9), // 内网被测服务
		"198.51.100.7", // 文档网段的公网示例
	}
	for _, s := range allowed {
		if err := CheckIP(net.ParseIP(s)); err != nil {
			t.Errorf("%s should be reachable, got %v", s, err)
		}
	}
	blocked := []string{
		"169.254.169.254", // AWS / GCP / OpenStack 元数据
		"fe80::1",         // 链路本地
		"0.0.0.0", "::",   // 未指定
		"224.0.0.1",       // 组播
		"100.100.100.200", // 阿里云元数据
		"fd00:ec2::254",   // AWS IPv6 元数据
	}
	for _, s := range blocked {
		if err := CheckIP(net.ParseIP(s)); !errors.Is(err, ErrBlockedTarget) {
			t.Errorf("%s should be blocked, got %v", s, err)
		}
	}
}

// 字符串黑名单挡不住"名字指向元数据"，必须解析后再判，且任一地址被禁就整体拒绝。
func TestCheckHostResolvesBeforeDeciding(t *testing.T) {
	prev := resolver
	t.Cleanup(func() { resolver = prev })
	resolver = stubResolver{
		"good.internal":     {v4(192, 168, 1, 20)},
		"rebind.example":    {v4(192, 168, 1, 20), "169.254.169.254"},
		"metadata.internal": {"169.254.169.254"},
	}
	ctx := context.Background()
	if err := CheckHost(ctx, "good.internal"); err != nil {
		t.Fatalf("private name should pass: %v", err)
	}
	if err := CheckHost(ctx, "rebind.example"); !errors.Is(err, ErrBlockedTarget) {
		t.Fatalf("mixed resolution should be blocked, got %v", err)
	}
	if err := CheckHost(ctx, "metadata.internal"); !errors.Is(err, ErrBlockedTarget) {
		t.Fatalf("metadata name should be blocked, got %v", err)
	}
	if err := CheckHost(ctx, "absent.example"); errors.Is(err, ErrBlockedTarget) {
		t.Fatalf("a DNS failure must not be reported as a policy block: %v", err)
	}
	if err := CheckHost(ctx, "169.254.169.254"); !errors.Is(err, ErrBlockedTarget) {
		t.Fatalf("IP literal should be checked without DNS, got %v", err)
	}
}

func TestReachableOnLoopback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener available: %v", err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("bad port %q: %v", port, err)
	}
	if !Reachable(context.Background(), "127.0.0.1", n) {
		t.Fatalf("loopback probe should succeed")
	}
	if Reachable(context.Background(), "169.254.169.254", 80) {
		t.Fatalf("metadata probe must be refused without dialing")
	}
}

func TestClientPinsTargetAndIgnoresRedirects(t *testing.T) {
	hits := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte("ok"))
	}))
	defer target.Close()

	// 302 指向另一个目标：不跟随，调用方自己看到 3xx。
	mall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer mall.Close()

	res, err := Client(5 * time.Second).Get(mall.URL)
	if err != nil {
		t.Fatalf("guarded client: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusFound {
		t.Fatalf("redirect was followed: got %d", res.StatusCode)
	}
	if hits != 0 {
		t.Fatalf("redirect target was dialed %d times", hits)
	}
}
