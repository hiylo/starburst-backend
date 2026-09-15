package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
func (m *Manager) Initialize(ctx context.Context, defaultPassword string) (bool, error) {
	_, err := m.store.GetSetting(ctx, SettingAdminPasswordHash)
	if err == nil {
		return false, nil // already initialized
	}
	if err != store.ErrNotFound {
		return false, err
	}
	if err := m.SetPassword(ctx, defaultPassword); err != nil {
		return false, err
	}
	return true, nil
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

// CreateToken issues a new API token for a named device and stores only its hash.
// The raw token is returned once and must be shown to the user; it is not recoverable.
func (m *Manager) CreateToken(ctx context.Context, name string) (string, error) {
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
	}
	if err := m.store.CreateToken(ctx, rec); err != nil {
		return "", err
	}
	return raw, nil
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
