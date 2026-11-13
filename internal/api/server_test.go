package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jdolan-exalink/openvms-rec/internal/config"
	"github.com/jdolan-exalink/openvms-rec/internal/db"
	"github.com/jdolan-exalink/openvms-rec/internal/platform"
)

type fakeProber struct{}

func (fakeProber) Usage(string) (platform.DiskUsage, error) {
	return platform.DiskUsage{TotalBytes: 1000e9, FreeBytes: 1000e9}, nil
}

func (fakeProber) Volume(p string) (platform.VolumeInfo, error) {
	return platform.VolumeInfo{StableID: "uuid:" + filepath.Base(filepath.Dir(p)), FSType: "ext4"}, nil
}

func newServer(t *testing.T) (*httptest.Server, config.Config) {
	t.Helper()
	base := t.TempDir()
	cfg := config.Default()
	for _, id := range []string{"d", "e"} {
		root := filepath.Join(base, id, "rec")
		os.MkdirAll(root, 0o755)
		v := config.Volume{ID: "cctv-" + id}
		if runtime.GOOS == "windows" {
			v.PathWindows = root
		} else {
			v.Path = root
		}
		cfg.Storage.Volumes = append(cfg.Storage.Volumes, v)
	}
	d, err := db.Open(context.Background(), filepath.Join(base, "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	s := &Server{Cfg: cfg, DB: d, Prober: fakeProber{}, Version: "test", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, cfg
}

func getJSON(t *testing.T, method, url string, out any) int {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if out != nil {
		json.NewDecoder(res.Body).Decode(out)
	}
	return res.StatusCode
}

func TestHealth(t *testing.T) {
	ts, _ := newServer(t)
	if c := getJSON(t, "GET", ts.URL+"/health/live", nil); c != 200 {
		t.Fatal(c)
	}
	if c := getJSON(t, "GET", ts.URL+"/health/ready", nil); c != 200 {
		t.Fatal(c)
	}
}

func TestVolumesLifecycle(t *testing.T) {
	ts, _ := newServer(t)
	var list struct{ Items []VolumeDTO }
	getJSON(t, "GET", ts.URL+"/api/v1/storage/volumes", &list)
	if len(list.Items) != 2 || list.Items[0].State != "uninitialized" || list.Items[0].Writable {
		t.Fatalf("%+v", list.Items)
	}
	var v VolumeDTO
	if c := getJSON(t, "POST", ts.URL+"/api/v1/storage/volumes/cctv-d/validate", &v); c != 200 {
		t.Fatalf("validate status %d", c)
	}
	if v.State != "healthy" || !v.Writable || v.Probe == nil || !v.Probe.OK {
		t.Fatalf("%+v", v)
	}
	if v.Limits.EffectiveBytes != 900e9 || v.Limits.Binding != "quota_percent" {
		t.Fatalf("limits %+v", v.Limits)
	}
	var sum SummaryDTO
	getJSON(t, "GET", ts.URL+"/api/v1/storage", &sum)
	if sum.Volumes != 2 || sum.WritableVolumes != 1 || sum.EffectiveLimitBytes != 900e9 {
		t.Fatalf("%+v", sum)
	}
	var e struct{ Error struct{ Code string } }
	if c := getJSON(t, "POST", ts.URL+"/api/v1/storage/volumes/nope/validate", &e); c != 404 || e.Error.Code != "volume_not_found" {
		t.Fatalf("%d %+v", c, e)
	}
}
