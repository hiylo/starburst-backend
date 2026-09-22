package config

import (
	"os"
	"testing"
)

// TestParseRecordsAdminPasswordExplicitness pins the "是否显式传入" detection:
// only an explicit --default-admin-password flag or STARBURST_ADMIN_PASSWORD env
// marks DefaultAdminPasswordSet=true; the built-in default "admin" keeps it
// false so auth.Initialize can reject a weak default at startup.
func TestParseRecordsAdminPasswordExplicitness(t *testing.T) {
	os.Unsetenv("STARBURST_ADMIN_PASSWORD")
	t.Cleanup(func() { os.Unsetenv("STARBURST_ADMIN_PASSWORD") })

	cfg, err := Parse(nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.DefaultAdminPasswordSet {
		t.Error("neither flag nor env set: DefaultAdminPasswordSet must be false")
	}
	if cfg.DefaultAdminPassword != "admin" {
		t.Errorf("built-in default not applied: %q", cfg.DefaultAdminPassword)
	}

	cfg, err = Parse([]string{"--default-admin-password", "S3cureAdmin!"})
	if err != nil {
		t.Fatalf("parse with flag: %v", err)
	}
	if !cfg.DefaultAdminPasswordSet {
		t.Error("flag set: DefaultAdminPasswordSet must be true")
	}
	if cfg.DefaultAdminPassword != "S3cureAdmin!" {
		t.Errorf("flag value not honored: %q", cfg.DefaultAdminPassword)
	}

	os.Setenv("STARBURST_ADMIN_PASSWORD", "S3cureAdminEnv!")
	cfg, err = Parse([]string{"--listen", ":18880"})
	if err != nil {
		t.Fatalf("parse with env: %v", err)
	}
	if !cfg.DefaultAdminPasswordSet {
		t.Error("env set: DefaultAdminPasswordSet must be true")
	}
	if cfg.DefaultAdminPassword != "S3cureAdminEnv!" {
		t.Errorf("env value not honored: %q", cfg.DefaultAdminPassword)
	}
}
