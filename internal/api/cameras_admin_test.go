package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jdolan-exalink/openvms-rec/internal/cameras"
	"github.com/jdolan-exalink/openvms-rec/internal/config"
	"github.com/jdolan-exalink/openvms-rec/internal/db"
	"github.com/jdolan-exalink/openvms-rec/internal/ingest"
)

func send(t *testing.T, method, url, body string, out any) int {
	t.Helper()
	req, _ := http.NewRequest(method, url, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
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

func TestCameraAdmin(t *testing.T) {
	dir := t.TempDir()
	d, err := db.Open(context.Background(), filepath.Join(dir, "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	reg, err := cameras.Open(context.Background(), cameras.Options{DB: d, CredsFile: filepath.Join(dir, "c.yaml"),
		Static: []config.Camera{{ID: "yaml", MainURL: "rtsp://10.0.0.1/a"}}})
	if err != nil {
		t.Fatal(err)
	}
	var probed []string
	s := &Server{Cfg: config.Default(), DB: d, Prober: fakeProber{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Cameras: reg, FFmpeg: "ffmpeg", Probe: func(_ context.Context, _ string, src ingest.Source) ingest.ProbeResult {
			probed = append(probed, src.URL)
			return ingest.ProbeResult{OK: true, Streams: []ingest.ProbeStream{{Kind: "video", Codec: "hevc"}}}
		}}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	api := ts.URL + "/api/v1/cameras"

	var cam CameraDTO
	if c := send(t, "POST", api, `{"id":"porton","name":"Portón","main_url":"rtsp://10.0.0.5:554/profile1","username":"admin","password":"s3cr#t"}`, &cam); c != 201 {
		t.Fatalf("create %d", c)
	}
	if !cam.Editable || cam.Source != "api" || !cam.HasCredentials || cam.MainURL != "rtsp://10.0.0.5:554/profile1" {
		t.Fatalf("%+v", cam)
	}
	var e struct {
		Error struct {
			Code    string
			Details []string
		}
	}
	if c := send(t, "POST", api, `{"id":"x","main_url":"rtsp://u:p@h/x"}`, &e); c != 422 || e.Error.Code != "invalid_camera" {
		t.Fatalf("%d %+v", c, e)
	}
	if c := send(t, "POST", api, `{"id":"porton","main_url":"rtsp://h/x"}`, &e); c != 409 || e.Error.Code != "camera_exists" {
		t.Fatalf("%d %+v", c, e)
	}
	if c := send(t, "PUT", api+"/yaml", `{"main_url":"rtsp://h/x"}`, &e); c != 409 || e.Error.Code != "camera_read_only" {
		t.Fatalf("%d %+v", c, e)
	}
	if c := send(t, "PUT", api+"/porton", `{"name":"Portón norte","main_url":"rtsp://10.0.0.5:554/profile1","enabled":false}`, &cam); c != 200 || cam.Enabled || !cam.HasCredentials {
		t.Fatalf("%d %+v", c, cam)
	}

	// Listing never exposes the password.
	res, _ := http.Get(api)
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if strings.Contains(string(b), "s3cr") || !strings.Contains(string(b), "Portón norte") {
		t.Fatal(string(b))
	}

	// Test with stored credentials, then with explicit ones.
	var pr ingest.ProbeResult
	if c := send(t, "POST", api+"/test", `{"camera_id":"porton","url":"rtsp://10.0.0.5:554/profile2"}`, &pr); c != 200 || !pr.OK {
		t.Fatalf("%d %+v", c, pr)
	}
	if c := send(t, "POST", api+"/test", `{"url":"rtsp://10.0.0.6/x","username":"u","password":"p"}`, &pr); c != 200 {
		t.Fatal(c)
	}
	if c := send(t, "POST", api+"/test", `{"url":"http://10.0.0.6/x"}`, nil); c != 422 {
		t.Fatal(c)
	}
	if len(probed) != 2 || probed[0] != "rtsp://admin:s3cr%23t@10.0.0.5:554/profile2" || probed[1] != "rtsp://u:p@10.0.0.6/x" {
		t.Fatalf("%q", probed)
	}

	if c := send(t, "DELETE", api+"/porton", "", nil); c != 204 {
		t.Fatal(c)
	}
	if c := send(t, "GET", api+"/porton", "", nil); c != 404 {
		t.Fatal(c)
	}
	var n int
	d.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action LIKE 'camera.%'`).Scan(&n)
	if n != 3 {
		t.Fatalf("audit rows %d", n)
	}
}
