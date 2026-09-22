package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLoginLimiterWindow(t *testing.T) {
	l := newLoginLimiter(3, time.Minute)
	for i := 0; i < 3; i++ {
		if !l.allow("ip1") {
			t.Fatalf("attempt %d within limit should be allowed", i+1)
		}
	}
	if l.allow("ip1") {
		t.Fatal("attempt beyond limit should be blocked")
	}
	l.clear("ip1")
	if !l.allow("ip1") {
		t.Fatal("clear() should reset the counter")
	}
}

// 限流器在不同 key 间互不影响，且过期尝试不再计数。
func TestLoginLimiterPerKeyAndExpiry(t *testing.T) {
	l := newLoginLimiter(2, time.Millisecond)
	if !l.allow("a") || !l.allow("b") {
		t.Fatal("different keys should not interfere")
	}
	if !l.allow("a") { // 第二次
		t.Fatal("second attempt for a should be allowed")
	}
	if l.allow("a") {
		t.Fatal("third attempt for a should be blocked")
	}
	time.Sleep(5 * time.Millisecond) // 窗口过期
	if !l.allow("a") {
		t.Fatal("attempt after window expiry should be allowed")
	}
}

// 回环 peer 时的限流 key 取 X-Forwarded-For 最右侧（最近一跳）条目并 Trim 空白，
// 让前置代理追加的最后一个真实来源生效；攻击者无法再用自控的首 IP 无限换 key。
func TestClientKeyUsesLastForwardedEntry(t *testing.T) {
	cases := []struct {
		name string
		addr string
		xff  string
		want string
	}{
		{"loopback multi-hop", "127.0.0.1:5555", "203.0.113.9, 198.51.100.7", "198.51.100.7"},
		{"loopback single hop", "127.0.0.1:5555", "192.0.2.9", "192.0.2.9"},
		{"loopback trailing spaces", "[::1]:5555", "192.0.2.9 , 203.0.113.8 ", "203.0.113.8"},
		{"loopback no xff", "127.0.0.1:5555", "", "127.0.0.1"},
		{"non-loopback ignores xff", "198.51.100.5:5555", "203.0.113.9, 198.51.100.7", "198.51.100.5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/web/session", nil)
			r.RemoteAddr = c.addr
			if c.xff != "" {
				r.Header.Set("X-Forwarded-For", c.xff)
			}
			if got := clientKey(r); got != c.want {
				t.Fatalf("clientKey = %q, want %q", got, c.want)
			}
		})
	}
}
