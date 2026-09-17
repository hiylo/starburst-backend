package store

import (
	"context"
	"strings"
	"testing"
)

func TestSecretEncryptDecryptRoundTrip(t *testing.T) {
	st := newTestStore(t).(*sqlStore)
	ctx := context.Background()

	enc, err := st.encryptSecret(ctx, "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, encPrefix) || enc == "hunter2" {
		t.Fatalf("encrypted value should be prefixed and not plaintext: %q", enc)
	}
	dec, err := st.decryptSecret(ctx, enc)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if dec != "hunter2" {
		t.Errorf("round trip = %q, want hunter2", dec)
	}
	// Non-determinism: two encryptions differ (random nonce) but both decrypt.
	enc2, _ := st.encryptSecret(ctx, "hunter2")
	if enc == enc2 {
		t.Error("same plaintext should produce different ciphertext (random nonce)")
	}
}

func TestSecretLegacyPlaintextFallback(t *testing.T) {
	st := newTestStore(t).(*sqlStore)
	ctx := context.Background()
	// A row written before encryption stores plaintext: read back as-is.
	dec, err := st.decryptSecret(ctx, "legacy-password")
	if err != nil || dec != "legacy-password" {
		t.Errorf("legacy fallback = %q/%v, want legacy-password/nil", dec, err)
	}
	// Empty stays empty both ways.
	e, _ := st.encryptSecret(ctx, "")
	if e != "" {
		t.Errorf("encrypt empty = %q, want empty", e)
	}
	d, _ := st.decryptSecret(ctx, "")
	if d != "" {
		t.Errorf("decrypt empty = %q, want empty", d)
	}
}

func TestSecretKeyStableAcrossInstances(t *testing.T) {
	// Two stores over the same database file must share the persisted key so
	// existing ciphertext decrypts after a restart.
	dir := t.TempDir()
	dsn := SQLiteDSN(dir + "/crypto.db")
	st1, err := OpenFromConfig(context.Background(), "sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st1.Close()
	st2, err := OpenFromConfig(context.Background(), "sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	ctx := context.Background()
	enc, err := st1.(*sqlStore).encryptSecret(ctx, "secret-1")
	if err != nil {
		t.Fatal(err)
	}
	dec, err := st2.(*sqlStore).decryptSecret(ctx, enc)
	if err != nil || dec != "secret-1" {
		t.Errorf("cross-instance decrypt = %q/%v, want secret-1/nil", dec, err)
	}
}
