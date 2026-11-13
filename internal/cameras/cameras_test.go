package cameras

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jdolan-exalink/openvms-rec/internal/config"
	"github.com/jdolan-exalink/openvms-rec/internal/db"
)

func ptr[T any](v T) *T { return &v }

func open(t *testing.T) (*Registry, string, func() *Registry) {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(context.Background(), filepath.Join(dir, "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	opts := Options{DB: d, CredsFile: filepath.Join(dir, "creds.yaml"),
		Static: []config.Camera{{ID: "yaml-cam", MainURL: "rtsp://10.0.0.1/main"}}}
	reopen := func() *Registry {
		r, err := Open(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	return reopen(), opts.CredsFile, reopen
}

func TestCRUDAndCredentials(t *testing.T) {
	ctx := context.Background()
	r, credsFile, reopen := open(t)
	var changed []string
	r.OnChange = func(id string) { changed = append(changed, id) }

	_, err := r.Create(ctx, Input{ID: "Bad ID", MainURL: "http://x"})
	var inv *InvalidError
	if !errors.As(err, &inv) || len(inv.Issues) < 2 {
		t.Fatalf("want validation error, got %v", err)
	}
	if _, err := r.Create(ctx, Input{ID: "c1", MainURL: "rtsp://admin:pw@10.0.0.2/x"}); !errors.As(err, &inv) {
		t.Fatalf("credentials in url accepted: %v", err)
	}
	if _, err := r.Create(ctx, Input{ID: "yaml-cam", MainURL: "rtsp://10.0.0.2/x"}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate of config camera: %v", err)
	}

	e, err := r.Create(ctx, Input{ID: "c1", Name: "Entrada", MainURL: "rtsp://10.0.0.2:554/profile1",
		SubURL: "rtsp://10.0.0.2:554/profile2", Username: ptr("admin"), Password: ptr("p@ss:w/rd")})
	if err != nil {
		t.Fatal(err)
	}
	if e.Source != SourceAPI || e.CredentialsRef != "c1" || !e.IsEnabled() || e.Transport != "tcp" || e.Priority != 5 {
		t.Fatalf("%+v", e)
	}
	u, err := r.StreamURL(e, e.MainURL)
	if err != nil || u != "rtsp://admin:p%40ss%3Aw%2Frd@10.0.0.2:554/profile1" {
		t.Fatalf("%q %v", u, err)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(credsFile)
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("creds file mode %v", fi.Mode().Perm())
		}
	}

	// Update without credentials keeps them; new password only changes it.
	e, err = r.Update(ctx, "c1", Input{Name: "Entrada 2", MainURL: "rtsp://10.0.0.2:554/profile1", Enabled: ptr(false)})
	if err != nil || e.CredentialsRef != "c1" || e.IsEnabled() {
		t.Fatalf("%+v %v", e, err)
	}
	if _, err := r.Update(ctx, "c1", Input{MainURL: "rtsp://10.0.0.2/x", Password: ptr("new")}); err != nil {
		t.Fatal(err)
	}
	if c, _ := r.Credentials("c1"); c.Username != "admin" || c.Password != "new" {
		t.Fatalf("%+v", c)
	}
	if _, err := r.Update(ctx, "yaml-cam", Input{MainURL: "rtsp://h/x"}); !errors.Is(err, ErrReadOnly) {
		t.Fatal(err)
	}

	// Survives a restart.
	r2 := reopen()
	if e2, ok := r2.Get("c1"); !ok || e2.MainURL != "rtsp://10.0.0.2/x" || e2.Source != SourceAPI {
		t.Fatalf("%+v", e2)
	}
	if got := r2.List(); len(got) != 2 || got[0].ID != "yaml-cam" || got[0].Source != SourceConfig {
		t.Fatalf("%+v", got)
	}

	// Removing credentials.
	if e, _ := r.Update(ctx, "c1", Input{MainURL: "rtsp://10.0.0.2/x", Username: ptr("")}); e.CredentialsRef != "" {
		t.Fatalf("%+v", e)
	}
	b, _ := os.ReadFile(credsFile)
	if strings.Contains(string(b), "new") {
		t.Fatal("password left in file")
	}

	if err := r.Delete(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Get("c1"); ok {
		t.Fatal("still there")
	}
	if strings.Join(changed, ",") != "c1,c1,c1,c1,c1" {
		t.Fatalf("OnChange calls: %v", changed)
	}
}

func TestDeleteKeepsCamerasWithRecordings(t *testing.T) {
	ctx := context.Background()
	r, _, _ := open(t)
	if _, err := r.Create(ctx, Input{ID: "c2", MainURL: "rtsp://10.0.0.3/x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.Exec(`INSERT INTO segments(camera_id, volume_id, relative_path, start_utc, end_utc, status, created_at)
		VALUES ('c2', 'v', 'a.ts', 0, 1000, 'final', 0)`); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, "c2"); !errors.Is(err, ErrHasRecordings) {
		t.Fatal(err)
	}
}
