package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

// Credentials for one camera.
type Credentials struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// Secrets is the content of data.secrets_file.
type Secrets struct {
	Cameras map[string]Credentials `yaml:"cameras"`
}

// ErrInsecureSecrets is returned when the secrets file is readable by others.
var ErrInsecureSecrets = errors.New("secrets file must not be accessible by group or others (chmod 600)")

// LoadSecrets reads the secrets file. A missing path yields empty secrets.
// On Unix the file must be mode 0600 or stricter; on Windows protect it with
// an ACL limited to the service account and Administrators.
func LoadSecrets(path string) (Secrets, error) {
	var s Secrets
	if path == "" {
		return s, nil
	}
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return s, fmt.Errorf("%s: %w", path, ErrInsecureSecrets)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	if err := yaml.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("parse secrets: %w", err)
	}
	return s, nil
}

// SaveSecrets writes s to path atomically with mode 0600.
func SaveSecrets(path string, s Secrets) error {
	b, err := yaml.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".secrets-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// StreamURL returns the stream URL with credentials injected. The result is
// secret: never log it.
func (s Secrets) StreamURL(raw, ref string) (string, error) {
	if ref == "" {
		return raw, nil
	}
	c, ok := s.Cameras[ref]
	if !ok {
		return "", fmt.Errorf("credentials_ref %q not found in secrets file", ref)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	u.User = url.UserPassword(c.Username, c.Password)
	return u.String(), nil
}

const encPrefix = "enc:v1:"

// DeriveSecretsKey derives a deterministic 32-byte AES-256 key from a host
// master secret using domain-separated SHA-256.
func DeriveSecretsKey(master []byte) []byte {
	if len(master) == 0 {
		return nil
	}
	sum := sha256.Sum256(append([]byte("openvms-recorder-secrets-v1:"), master...))
	return sum[:]
}

// EncryptBytes encrypts plaintext using AES-256-GCM and encodes as `enc:v1:<base64>`.
func EncryptBytes(key, plaintext []byte) (string, error) {
	if len(key) != 32 {
		return "", errors.New("encryption key must be 32 bytes")
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, plaintext, nil)
	return encPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// DecryptBytes decrypts a value encoded by EncryptBytes. If value does not start
// with `enc:v1:`, it is returned untouched (backward compatibility with plaintext).
func DecryptBytes(key []byte, raw string) ([]byte, error) {
	trimmed := strings.TrimSpace(raw)
	if !strings.HasPrefix(trimmed, encPrefix) {
		return []byte(raw), nil
	}
	if len(key) != 32 {
		return nil, errors.New("decryption key required for encrypted secrets")
	}
	blob, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(trimmed, encPrefix))
	if err != nil {
		return nil, fmt.Errorf("decode encrypted secrets: %w", err)
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(blob) < ns {
		return nil, errors.New("encrypted secrets payload truncated")
	}
	plain, err := gcm.Open(nil, blob[:ns], blob[ns:], nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt secrets: %w", err)
	}
	return plain, nil
}

// LoadSecretsAtRest reads path, transparently decrypting `enc:v1:` envelopes
// when key is 32 bytes (falling back cleanly to plaintext YAML for migration).
func LoadSecretsAtRest(path string, key []byte) (Secrets, error) {
	var s Secrets
	if path == "" {
		return s, nil
	}
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return s, fmt.Errorf("%s: %w", path, ErrInsecureSecrets)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	plain, err := DecryptBytes(key, string(b))
	if err != nil {
		return s, err
	}
	if err := yaml.Unmarshal(plain, &s); err != nil {
		return s, fmt.Errorf("parse secrets: %w", err)
	}
	return s, nil
}

// SaveSecretsAtRest writes s atomically with mode 0600, encrypting at rest when
// key is 32 bytes.
func SaveSecretsAtRest(path string, key []byte, s Secrets) error {
	b, err := yaml.Marshal(s)
	if err != nil {
		return err
	}
	payload := b
	if len(key) == 32 {
		enc, err := EncryptBytes(key, b)
		if err != nil {
			return err
		}
		payload = []byte(enc + "\n")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".secrets-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		f.Close()
		return err
	}
	if _, err := f.Write(payload); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
