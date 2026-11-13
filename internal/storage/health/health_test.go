package health

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestProbeWritableDir(t *testing.T) {
	dir := t.TempDir()
	r := Probe(dir)
	if !r.OK || r.Err != nil {
		t.Fatalf("%+v", r)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("probe left files behind: %v", entries)
	}
}

func TestProbeMissingDir(t *testing.T) {
	if r := Probe(filepath.Join(t.TempDir(), "absent")); r.OK {
		t.Fatal("probe succeeded on missing dir")
	}
}

func TestProbeReadOnlyDir(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("root and Windows ignore directory mode bits")
	}
	dir := t.TempDir()
	os.Chmod(dir, 0o500)
	defer os.Chmod(dir, 0o700)
	if r := Probe(dir); r.OK {
		t.Fatal("probe succeeded on read-only dir")
	}
}
