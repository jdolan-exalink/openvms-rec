package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultIsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadExampleConfig(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "configs", "recorder.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Storage.Volumes) != 3 {
		t.Fatalf("volumes=%d", len(cfg.Storage.Volumes))
	}
	f := cfg.Storage.Volumes[2]
	if f.EffectiveMaxPercent(cfg.Storage) != 80 || cfg.Storage.Volumes[0].EffectiveMaxPercent(cfg.Storage) != 90 {
		t.Fatal("per-volume percent not applied")
	}
	if f.ResolvedPath("windows") != `F:\OpenVMS\Recordings` || f.ResolvedPath("linux") != "/srv/cctv-f/recordings" {
		t.Fatalf("paths: %q %q", f.ResolvedPath("windows"), f.ResolvedPath("linux"))
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte("storage:\n  default_max_procent: 90\n"), 0o600)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "default_max_procent") {
		t.Fatalf("err=%v", err)
	}
}

func TestLoadEmptyFileUsesDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, nil, 0o600)
	cfg, err := Load(p)
	if err != nil || cfg.Storage.DefaultMaxPercent != 90 {
		t.Fatalf("cfg=%+v err=%v", cfg.Storage, err)
	}
}

func TestValidateCatchesBadThresholds(t *testing.T) {
	c := Default()
	c.Storage.CleanupTargetPercent = 88
	c.Storage.Placement = "raid5"
	c.Storage.Volumes = []Volume{{ID: "a", Path: "/x"}, {ID: "a"}}
	err := c.Validate()
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"cleanup_target_percent", "placement", "duplicate", "path_windows"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}

func TestEnvOverrides(t *testing.T) {
	c := Default()
	env := map[string]string{"OPENVMS_RECORDER_LISTEN": "0.0.0.0:9000", "OPENVMS_RECORDER_DATA_DIR": "/data"}
	ApplyEnv(&c, func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if c.Server.Listen != "0.0.0.0:9000" || c.Data.Dir != "/data" {
		t.Fatalf("%+v", c)
	}
}
