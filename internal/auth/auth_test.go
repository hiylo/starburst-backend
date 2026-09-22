package auth

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hiylo/starburst-backend/internal/store"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	dsn := store.SQLiteDSN(filepath.Join(t.TempDir(), "test.db"))
	st, err := store.OpenFromConfig(context.Background(), "sqlite", dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return NewManager(st)
}

func TestInitializeAndVerify(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)

	created, err := m.Initialize(ctx, "Str0ngPass", true)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if !created {
		t.Fatalf("expected first-run create")
	}

	ok, err := m.VerifyPassword(ctx, "Str0ngPass")
	if err != nil || !ok {
		t.Fatalf("verify correct password: ok=%v err=%v", ok, err)
	}
	ok, _ = m.VerifyPassword(ctx, "wrong")
	if ok {
		t.Fatalf("wrong password accepted")
	}

	// Second initialize is a no-op.
	created, err = m.Initialize(ctx, "OtherPass2", true)
	if err != nil {
		t.Fatalf("re-init: %v", err)
	}
	if created {
		t.Fatalf("expected second init to be no-op")
	}
	ok, _ = m.VerifyPassword(ctx, "OtherPass2")
	if ok {
		t.Fatalf("re-init overwrote password")
	}
}

func TestSetPasswordTooShort(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)
	if err := m.SetPassword(ctx, "ab"); err == nil {
		t.Fatalf("expected error for short password")
	}
}

// 内置默认 "admin"（未显式传入）且强度不足时必须拒绝启动；显式传入弱口令仍
// 放行（用户明确选择），只留告警。
func TestInitializeRejectsWeakImplicitDefault(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)
	if _, err := m.Initialize(ctx, "admin", false); err == nil {
		t.Fatal("built-in default weak password must be rejected")
	}
	// 先用强口令完成初始化。
	if _, err := m.Initialize(ctx, "S3cureAdmin!", false); err != nil {
		t.Fatalf("init with strong default: %v", err)
	}
	// 已初始化后重复 Initialize（哪怕弱口令）是 no-op，不再报错。
	if created, err := m.Initialize(ctx, "admin", false); err != nil || created {
		t.Fatalf("re-init after existing hash must be a no-op: created=%v err=%v", created, err)
	}

	m2 := newTestManager(t)
	created, err := m2.Initialize(ctx, "admin", true)
	if err != nil || !created {
		t.Fatalf("explicit weak password rejected: created=%v err=%v", created, err)
	}
	ok, err := m2.VerifyPassword(ctx, "admin")
	if err != nil || !ok {
		t.Fatalf("explicit weak password not stored: ok=%v err=%v", ok, err)
	}
}

func TestTokenRoundTrip(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)

	raw, err := m.CreateToken(ctx, "my-phone", "device")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(raw) < 20 {
		t.Fatalf("token too short: %q", raw)
	}

	rec, err := m.VerifyToken(ctx, raw)
	if err != nil || rec == nil {
		t.Fatalf("verify valid token: %v", err)
	}
	if rec.Name != "my-phone" {
		t.Fatalf("unexpected name %q", rec.Name)
	}

	// Raw token is never stored.
	if rec.TokenHash == raw {
		t.Fatalf("raw token leaked into store")
	}

	// Wrong token rejected.
	if _, err := m.VerifyToken(ctx, "ocb_wrong"); err != store.ErrNotFound {
		t.Fatalf("expected ErrNotFound for bad token, got %v", err)
	}

	// Revoked token rejected.
	if err := m.RevokeToken(ctx, rec.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := m.VerifyToken(ctx, raw); err != store.ErrNotFound {
		t.Fatalf("expected ErrNotFound after revoke, got %v", err)
	}
}

func TestEnsureDefaultToken(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t)

	// 未配置默认 token：不做任何事。
	raw, err := m.EnsureDefaultToken(ctx, "")
	if err != nil || raw != "" {
		t.Fatalf("unconfigured: raw=%q err=%v", raw, err)
	}
	if n, err := m.ListTokens(ctx); err != nil || len(n) != 0 {
		t.Fatalf("expected no tokens, got %d (%v)", len(n), err)
	}

	// 首次运行：预置一个可验证的 token，且只落哈希不落原文。
	raw, err = m.EnsureDefaultToken(ctx, "ocb_fixed_dev_token")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if raw != "ocb_fixed_dev_token" {
		t.Fatalf("unexpected raw token %q", raw)
	}
	rec, err := m.VerifyToken(ctx, raw)
	if err != nil || rec == nil {
		t.Fatalf("verify default token: %v", err)
	}
	if rec.ID != "default" || rec.Name != "default" {
		t.Fatalf("unexpected record id=%q name=%q", rec.ID, rec.Name)
	}
	if rec.TokenHash == raw {
		t.Fatalf("raw token leaked into store")
	}

	// 幂等：已预置过就不再创建，也不会被新值覆盖。
	again, err := m.EnsureDefaultToken(ctx, "ocb_other")
	if err != nil || again != "" {
		t.Fatalf("second run: raw=%q err=%v", again, err)
	}
	if n, err := m.ListTokens(ctx); err != nil || len(n) != 1 {
		t.Fatalf("expected 1 token, got %d (%v)", len(n), err)
	}
	if _, err := m.VerifyToken(ctx, "ocb_other"); err != store.ErrNotFound {
		t.Fatalf("expected old value to win, got %v", err)
	}
}

func TestTokenPrefix(t *testing.T) {
	m := newTestManager(t)
	raw, err := m.CreateToken(context.Background(), "x", "device")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if raw[:4] != "ocb_" {
		t.Fatalf("expected ocb_ prefix, got %q", raw)
	}
}
