package gateway

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverGatewayPathsAndStaticRoutes(t *testing.T) {
	root := t.TempDir()
	write(t, root, "activity-provider/src/main/resources/bootstrap.yml", `
spring:
  application:
    name: activity-provider
  cloud:
    nacos:
      discovery:
        metadata:
          gateway.paths: /activity/**,/order/**,/signup/**
`)
	write(t, root, "nacos/gateway-route.yml", `
spring:
  cloud:
    gateway:
      routes:
        - id: echo-pay-callback
          uri: lb://payment-provider
          predicates:
            - Path=/pay/callback/{channel}
          filters:
            - RewritePath=/pay/callback/(?<channel>.*), /payment/callback/${channel}
`)
	write(t, root, "target/ignored.yml", `
spring:
  cloud:
    nacos:
      discovery:
        metadata:
          gateway.paths: /ignored/**
`)

	routes, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}

	var activity *Route
	var payment *Route
	for _, r := range routes {
		switch r.Service {
		case "activity-provider":
			activity = r
		case "payment-provider":
			payment = r
		}
	}
	if activity == nil {
		t.Fatal("activity-provider route not discovered")
	}
	if len(activity.Paths) != 3 || activity.Paths[0] != "/activity/**" {
		t.Fatalf("activity paths = %v, want [/activity/** /order/** /signup/**]", activity.Paths)
	}
	if payment == nil {
		t.Fatal("payment-provider static route not discovered")
	}
	if len(payment.Paths) != 1 || payment.Paths[0] != "/pay/callback/{channel}" {
		t.Fatalf("payment paths = %v", payment.Paths)
	}
	if payment.URI != "lb://payment-provider" {
		t.Fatalf("payment uri = %q", payment.URI)
	}
	// target/ must be skipped.
	for _, r := range routes {
		if r.Source != "" && filepath.Base(filepath.Dir(r.Source)) == "target" {
			t.Fatalf("target dir must be skipped, got source %q", r.Source)
		}
	}
}

func TestMatch(t *testing.T) {
	routes := []*Route{
		{Service: "activity-provider", Paths: []string{"/activity/**", "/order/**"}},
		{Service: "user-provider", Paths: []string{"/auth/**"}},
	}
	cases := []struct {
		path string
		want []string
	}{
		{"/activity/list", []string{"/activity/**"}},
		{"/activity", []string{"/activity/**"}},
		{"/order/123", []string{"/order/**"}},
		{"/auth/login", []string{"/auth/**"}},
		{"/nothing", nil},
	}
	for _, c := range cases {
		got := Match(routes, c.path)
		if len(got) != len(c.want) {
			t.Fatalf("Match(%q) = %v, want %v", c.path, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("Match(%q) = %v, want %v", c.path, got, c.want)
			}
		}
	}
}

func TestServiceFromURI(t *testing.T) {
	cases := map[string]string{
		"lb://payment-provider": "payment-provider",
		"http://localhost:8080": "localhost",
		"lb://admin":            "admin",
		"lb://frontend/":        "frontend",
		"":                      "",
	}
	for in, want := range cases {
		if got := serviceFromURI(in); got != want {
			t.Fatalf("serviceFromURI(%q) = %q, want %q", in, got, want)
		}
	}
}
