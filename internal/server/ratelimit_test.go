package server

import (
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
