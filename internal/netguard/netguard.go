// Package netguard validates the destinations of outbound requests whose target
// comes from a client rather than from server config.
//
// 测试智能的全部用途就是去访问开发者本机和内网里跑着的服务，所以这里刻意**不**封
// 私网段；要封的是"任何拿到设备 token 的人都不该借后端摸到"的地址：链路本地
// （云元数据 169.254.169.254 / fe80::/10）、未指定地址、组播与保留段，以及各家云
// 的元数据常量。两条必要的配套：
//
//   - 先把域名解析成 IP 再判，否则一条指向 169.254.169.254 的 A 记录、或
//     metadata.google.internal 这类名字就能绕过字符串黑名单；
//   - 校验之后**按已校验的 IP 拨号**，并且不跟随任何重定向——否则 DNS
//     rebinding 和一跳 302 都能把合法目标换成被禁目标（TOCTOU）。
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// ErrBlockedTarget is returned when a destination resolves to an address this
// package refuses to reach.
var ErrBlockedTarget = errors.New("target address is not allowed")

// dialTimeout bounds only the connect phase; callers set their own overall
// context/timeout for the request.
const dialTimeout = 5 * time.Second

// blockedExact lists cloud metadata addresses that do not fall out of the
// generic rules below (they are routable unicast rather than link-local).
var blockedExact = []string{
	"100.100.100.200", // Alibaba Cloud metadata
	"fd00:ec2::254",   // AWS IPv6 metadata (ULA, not link-local)
}

// hostResolver is net.DefaultResolver's lookup surface, kept as an interface so
// tests can drive the "name resolves to a blocked address" path without DNS.
type hostResolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

var resolver hostResolver = net.DefaultResolver

// CheckIP reports whether ip may be used as an outbound destination.
func CheckIP(ip net.IP) error {
	if ip == nil {
		return ErrBlockedTarget
	}
	if ip.IsLoopback() {
		return nil // 本机服务正是这套接口的用途
	}
	if ip.IsUnspecified() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return ErrBlockedTarget
	}
	for _, s := range blockedExact {
		if e := net.ParseIP(s); e != nil && e.Equal(ip) {
			return ErrBlockedTarget
		}
	}
	return nil
}

// CheckHost validates a bare host (IP literal or name). Names are resolved and
// **every** returned address must pass: a hostname that points partly at a
// blocked address is exactly the rebinding case this guards against.
func CheckHost(ctx context.Context, host string) error {
	if host == "" {
		return ErrBlockedTarget
	}
	if ip := parseHostIP(host); ip != nil {
		return CheckIP(ip)
	}
	_, err := resolveAllowed(ctx, host)
	return err
}

// parseHostIP accepts an IPv6 literal with or without a %zone, which
// net.ParseIP alone rejects.
func parseHostIP(host string) net.IP {
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	return net.ParseIP(host)
}

// resolveAllowed returns the addresses of host that pass CheckIP.
func resolveAllowed(ctx context.Context, host string) ([]net.IP, error) {
	addrs, err := resolver.LookupHost(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", host, err)
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		ip := parseHostIP(a)
		if ip == nil {
			return nil, ErrBlockedTarget
		}
		if err := CheckIP(ip); err != nil {
			return nil, fmt.Errorf("%w: %s resolves to %s", err, host, a)
		}
		ips = append(ips, ip)
	}
	if len(ips) == 0 {
		return nil, ErrBlockedTarget
	}
	return ips, nil
}

// Dial connects to addr after validating its destination, dialing the resolved
// IPs directly so the checked address is the one actually contacted.
func Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := resolveAllowed(ctx, host)
	if err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: dialTimeout}
	var lastErr error
	for _, ip := range ips {
		conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = ErrBlockedTarget
	}
	return nil, lastErr
}

// Reachable reports whether a guarded TCP connect to host:port succeeds. It is
// the probe form used for "is this middleware / node up?" checks, where the
// caller only cares about a boolean.
func Reachable(ctx context.Context, host string, port int) bool {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	conn, err := Dial(ctx, "tcp", net.JoinHostPort(host, fmt.Sprint(port)))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// Client returns an http.Client for requests whose URL came from a client: the
// transport validates and pins the destination IP, and redirects are never
// followed (a 3xx is returned to the caller as-is, so a redirect can't smuggle
// the request to a blocked address).
func Client(timeout time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = Dial
	return &http.Client{
		Timeout:   timeout,
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
