package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"github.com/hiylo/starburst-backend/internal/store"
)

// Settings keys used for web admin credentials.
const (
	SettingAdminPasswordHash = "admin.password_hash"
	SettingDefaultTokenSet   = "admin.default_token_set"
)

// Manager provides password and token verification against the store.
type Manager struct {
	store store.Store
}

// NewManager creates a Manager backed by the given store.
func NewManager(st store.Store) *Manager {
	return &Manager{store: st}
}

// Initialize ensures the admin password is set. If no password exists yet,
// it hashes the provided default and stores it. Returns whether it created one.
// explicit reports whether the password came from an explicit flag/env value
// rather than the built-in default "admin"; a weak default password is refused
// so a fresh install cannot boot on an easily guessed admin credential.
func (m *Manager) Initialize(ctx context.Context, defaultPassword string, explicit bool) (bool, error) {
	_, err := m.store.GetSetting(ctx, SettingAdminPasswordHash)
	if err == nil {
		return false, nil // already initialized
	}
	if err != store.ErrNotFound {
		return false, err
	}
	if strings.TrimSpace(defaultPassword) == "" {
		return false, fmt.Errorf("admin password must not be empty (set --default-admin-password or STARBURST_ADMIN_PASSWORD)")
	}
	// 未显式传入（flag/env 都没设置、用了内置默认 "admin"）且口令强度不足时拒绝
	// 启动：内置弱口令会让任何能访问机器的方直接登进管理台。显式传入弱口令仍放行
	//（用户明确选择），只留告警。
	if !explicit && !isStrongPassword(defaultPassword) {
		return false, fmt.Errorf("default admin password is too weak; set STARBURST_ADMIN_PASSWORD (or --default-admin-password) to a strong value")
	}
	if err := m.store.SetSetting(ctx, SettingAdminPasswordHash, mustHash(defaultPassword)); err != nil {
		return false, err
	}
	if !isStrongPassword(defaultPassword) {
		log.Printf("WARNING: initialized admin password is weak (%d chars); change it via the config UI", len(defaultPassword))
	}
	return true, nil
}

// mustHash bcrypt-hashes a password without enforcing strength rules. It is used
// only on first initialization so a fresh install can boot with the configured
// default; SetPassword keeps enforcing strength for user-set passwords.
func mustHash(plain string) string {
	hash, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		panic("auth: bcrypt hash failed: " + err.Error())
	}
	return string(hash)
}

// EnsureDefaultToken registers the configured default token on first run so
// clients can connect without first creating a token via the web UI. Idempotent:
// returns the raw token if it provisioned one, or "" when not configured/already set.
func (m *Manager) EnsureDefaultToken(ctx context.Context, defaultToken string) (string, error) {
	if strings.TrimSpace(defaultToken) == "" {
		return "", nil
	}
	if _, err := m.store.GetSetting(ctx, SettingDefaultTokenSet); err == nil {
		return "", nil // already provisioned
	} else if err != store.ErrNotFound {
		return "", err
	}
	rec := &store.Token{
		ID:        "default",
		Name:      "default",
		TokenHash: HashToken(defaultToken),
		// STARBURST_DEFAULT_TOKEN 是 App 设备 token：显式 device scope，
		// 不能做敏感代理操作（shell/command/share/config 等由 adminAccess 拦截）。
		Scope: "device",
	}
	if err := m.store.CreateToken(ctx, rec); err != nil {
		return "", err
	}
	if err := m.store.SetSetting(ctx, SettingDefaultTokenSet, "1"); err != nil {
		return "", err
	}
	return defaultToken, nil
}

// SetPassword stores a new bcrypt hash for the admin password.
func (m *Manager) SetPassword(ctx context.Context, plain string) error {
	if !isStrongPassword(plain) {
		return fmt.Errorf("password must be at least 8 characters and not a common weak password")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	return m.store.SetSetting(ctx, SettingAdminPasswordHash, string(hash))
}

// weakPasswords are common defaults rejected by isStrongPassword even when
// they meet the length requirement, so a fresh install can't sit on an easily
// guessed admin credential.
var weakPasswords = map[string]bool{
	"admin": true, "password": true, "12345678": true, "123456789": true,
	"1234567890": true, "qwertyui": true, "qwerty123": true, "letmein1": true,
	"00000000": true, "admin123": true, "root1234": true, "12345678a": true,
}

// isStrongPassword enforces a minimum length of 8 and rejects common weak
// passwords. Password rules are deliberately simple: length is the dominant
// factor, the weak-list only blocks obviously guessable values.
func isStrongPassword(plain string) bool {
	if len(plain) < 8 {
		return false
	}
	return !weakPasswords[strings.ToLower(strings.TrimSpace(plain))]
}

// VerifyPassword checks a plaintext password against the stored hash.
// It returns false (not an error) on mismatch or missing hash.
func (m *Manager) VerifyPassword(ctx context.Context, plain string) (bool, error) {
	hash, err := m.store.GetSetting(ctx, SettingAdminPasswordHash)
	if err == store.ErrNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)); err != nil {
		return false, nil
	}
	return true, nil
}

// CreateToken issues a new API token with the given scope and stores only its
// hash. The raw token is returned once and must be shown to the user; it is not
// recoverable. 默认 device scope：能通过普通端点鉴权，但敏感操作（任意命令执行、
// 内网 SSRF 探测等）由 server.adminAccess 挡在门外；scope 仅允许 admin 由
// requireWeb 通道显式创建（见 handleTokens）。
func (m *Manager) CreateToken(ctx context.Context, name string, scope string) (string, error) {
	if !validScope(scope) {
		scope = "device"
	}
	raw, err := randomToken()
	if err != nil {
		return "", err
	}
	id, err := randomID()
	if err != nil {
		return "", err
	}
	rec := &store.Token{
		ID:        id,
		Name:      name,
		TokenHash: HashToken(raw),
		Scope:     scope,
	}
	if err := m.store.CreateToken(ctx, rec); err != nil {
		return "", err
	}
	return raw, nil
}

// validScope reports whether scope is a supported token scope.
func validScope(scope string) bool {
	return scope == "admin" || scope == "device"
}

// VerifyToken returns the matching token record if raw is a valid, non-revoked token.
func (m *Manager) VerifyToken(ctx context.Context, raw string) (*store.Token, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, store.ErrNotFound
	}
	return m.store.GetTokenByHash(ctx, HashToken(raw))
}

// HashToken produces the stable sha256 hex used to look up and store tokens.
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// ListTokens delegates to the store.
func (m *Manager) ListTokens(ctx context.Context) ([]*store.Token, error) {
	return m.store.ListTokens(ctx)
}

// RevokeToken delegates to the store.
func (m *Manager) RevokeToken(ctx context.Context, id string) error {
	return m.store.RevokeToken(ctx, id)
}

// TouchToken marks a token as recently used.
func (m *Manager) TouchToken(ctx context.Context, id string) error {
	return m.store.TouchToken(ctx, id)
}

const tokenCharset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, b := range buf {
		sb.WriteByte(tokenCharset[int(b)%len(tokenCharset)])
	}
	return "ocb_" + sb.String(), nil
}

func randomID() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
