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
	if err != nil || v != "0001_init" {
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
