package server

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// loginLimiter is a small fixed-window counter that throttles web admin login
// attempts per client IP. It exists so a LAN-exposed backend is not trivially
// brute-forced; bcrypt already raises the cost per guess, the limiter bounds
// the total guesses. Attempts older than the window are discarded.
type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
	limit    int
	window   time.Duration
}

func newLoginLimiter(limit int, window time.Duration) *loginLimiter {
	return &loginLimiter{
		attempts: make(map[string][]time.Time),
		limit:    limit,
		window:   window,
	}
}

// allow reports whether key may make another attempt now, recording the
// attempt either way.
func (l *loginLimiter) allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	recent := now.Add(-l.window)
	hits := l.attempts[key][:0]
	for _, ts := range l.attempts[key] {
		if ts.After(recent) {
			hits = append(hits, ts)
		}
	}
	if len(hits) >= l.limit {
		l.attempts[key] = hits
		return false
	}
	l.attempts[key] = append(hits, now)
	// 周期性清理已失去价值的 key，防止 attempts 无界增长（配合 clientKey
	// 可被攻击者用大量 X-Forwarded-For 注入不同 key）。
	l.prune(recent)
	return true
}

// clear drops the recorded attempts for key after a successful login.
func (l *loginLimiter) clear(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, key)
}

// prune removes keys whose attempts have all aged out. Called opportunistically
// from allow so the map cannot grow without bound as clients churn.
func (l *loginLimiter) prune(recent time.Time) {
	for k, ts := range l.attempts {
		alive := ts[:0]
		for _, t := range ts {
			if t.After(recent) {
				alive = append(alive, t)
			}
		}
		if len(alive) == 0 {
			delete(l.attempts, k)
		} else {
			l.attempts[k] = alive
		}
	}
}

// clientKey derives a stable rate-limit key from the request: the peer IP with
// the port stripped, falling back to the first X-Forwarded-For entry when the
// peer is loopback (i.e. a reverse proxy is in front).
func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if host == "127.0.0.1" || host == "::1" || host == "localhost" {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if first := strings.TrimSpace(strings.Split(xff, ",")[0]); first != "" {
				return first
			}
		}
	}
	return host
}
