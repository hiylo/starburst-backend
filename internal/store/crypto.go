package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
)

// Secret-at-rest encryption for credentials such as test-middleware passwords
// and remote-node auth. The AES-256-GCM key is a per-install random value
// persisted in settings (intel.secrets_key), so ciphertext survives restarts
// without any config dependency.
//
// Values written before encryption (legacy plaintext) are read back as-is, so
// enabling this never breaks existing rows; new writes are prefixed "enc:" to
// distinguish ciphertext from legacy values.

const (
	secretKeySetting = "intel.secrets_key"
	encPrefix        = "enc:"
)

var (
	secretKeyMu     sync.Mutex
	cachedSecretKey []byte
	errBadSecret    = errors.New("invalid encrypted secret")
)

// secretKey returns (and lazily creates) the per-install AES-256 key.
func (s *sqlStore) secretKey(ctx context.Context) ([]byte, error) {
	secretKeyMu.Lock()
	defer secretKeyMu.Unlock()
	if len(cachedSecretKey) == 32 {
		return cachedSecretKey, nil
	}
	if v, err := s.GetSetting(ctx, secretKeySetting); err == nil {
		if k, derr := base64.StdEncoding.DecodeString(v); derr == nil && len(k) == 32 {
			cachedSecretKey = k
			return k, nil
		}
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	if err := s.SetSetting(ctx, secretKeySetting, base64.StdEncoding.EncodeToString(k)); err != nil {
		return nil, err
	}
	cachedSecretKey = k
	return k, nil
}

// encryptSecret encrypts plaintext with AES-256-GCM. Empty stays empty so the
// "no secret set" signal is preserved.
func (s *sqlStore) encryptSecret(ctx context.Context, plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	key, err := s.secretKey(ctx)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return encPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// decryptSecret decrypts an "enc:" value, returning legacy plaintext as-is.
func (s *sqlStore) decryptSecret(ctx context.Context, ct string) (string, error) {
	if ct == "" {
		return "", nil
	}
	if len(ct) < len(encPrefix) || ct[:len(encPrefix)] != encPrefix {
		return ct, nil // legacy plaintext row
	}
	key, err := s.secretKey(ctx)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(ct[len(encPrefix):])
	if err != nil {
		return "", errBadSecret
	}
	nonceSize := gcm.NonceSize()
	if len(raw) < nonceSize {
		return "", errBadSecret
	}
	pt, err := gcm.Open(nil, raw[:nonceSize], raw[nonceSize:], nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}
