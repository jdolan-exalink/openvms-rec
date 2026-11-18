package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecretsEncryptedAtRestAndLegacyFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "camera-credentials.yaml")
	key := DeriveSecretsKey([]byte("master-key-for-at-rest-test-32b!"))

	sec := Secrets{Cameras: map[string]Credentials{
		"cam1": {Username: "admin", Password: "SuperSecretPassword123!"},
	}}
	if err := SaveSecretsAtRest(path, key, sec); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "SuperSecretPassword123!") || !strings.HasPrefix(string(raw), "enc:v1:") {
		t.Fatalf("secrets file must be encrypted at rest, got: %s", string(raw))
	}

	got, err := LoadSecretsAtRest(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if got.Cameras["cam1"] != sec.Cameras["cam1"] {
		t.Fatalf("decrypted mismatch: %+v", got.Cameras)
	}

	// Missing/wrong key fails cleanly rather than returning corrupted credentials.
	if _, err := LoadSecretsAtRest(path, nil); err == nil {
		t.Fatal("expected error decrypting without key")
	}

	// Legacy plaintext YAML still loads transparently.
	legacy := filepath.Join(dir, "legacy.yaml")
	if err := SaveSecrets(legacy, sec); err != nil {
		t.Fatal(err)
	}
	gotLegacy, err := LoadSecretsAtRest(legacy, key)
	if err != nil {
		t.Fatal(err)
	}
	if gotLegacy.Cameras["cam1"] != sec.Cameras["cam1"] {
		t.Fatalf("legacy load mismatch: %+v", gotLegacy)
	}
}
