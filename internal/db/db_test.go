package db

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenMigratesIdempotently(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "datos con espacio ñ", "recorder.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	v, err := Version(ctx, d)
	if err != nil || v != "0002_camera_audio" {
		t.Fatalf("version=%q err=%v", v, err)
	}
	var mode string
	d.QueryRow(`PRAGMA journal_mode`).Scan(&mode)
	if mode != "wal" {
		t.Errorf("journal_mode=%s", mode)
	}
	d.Close()
	d, err = Open(ctx, path) // second open must be a no-op
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var fk int
	d.QueryRow(`PRAGMA foreign_keys`).Scan(&fk)
	if fk != 1 {
		t.Error("foreign keys disabled")
	}
}

func TestBackup(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := Open(ctx, filepath.Join(dir, "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.Exec(`INSERT INTO audit_log(at, actor, action) VALUES (1, 'x', 'y')`)
	dst := filepath.Join(dir, "backup.db")
	if err := Backup(ctx, d, dst); err != nil {
		t.Fatal(err)
	}
	b, err := Open(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var n int
	b.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&n)
	if n != 1 {
		t.Fatalf("rows=%d", n)
	}
}
