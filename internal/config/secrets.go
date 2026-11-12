package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"runtime"

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
