package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jdolan-exalink/openvms-rec/internal/cameras"
	"github.com/jdolan-exalink/openvms-rec/internal/config"
	"github.com/jdolan-exalink/openvms-rec/internal/db"
	"github.com/jdolan-exalink/openvms-rec/internal/provisionnvr"
	"github.com/jdolan-exalink/openvms-rec/internal/provisionnvr/nvmstest"
)

func TestNVRDetectAndImport(t *testing.T) {
	dir := t.TempDir()
	d, err := db.Open(context.Background(), filepath.Join(dir, "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	reg, err := cameras.Open(context.Background(), cameras.Options{DB: d, CredsFile: filepath.Join(dir, "c.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	nvr := nvmstest.New(t, "admin", "nvr-pass")
	u, _ := url.Parse(nvr.URL)
	cfg := config.Default()
	cfg.Discovery.AllowedNetworks = []string{"127.0.0.0/8"}
	s := &Server{Cfg: cfg, DB: d, Prober: fakeProber{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Cameras: reg,
		NVRDetect: func(ctx context.Context, in provisionnvr.DetectInput) (*provisionnvr.Detection, error) {
			in.ProbeONVIF = func(context.Context, provisionnvr.DetectInput) (*provisionnvr.OnvifFacts, error) { return nil, errors.New("off") }
			in.ProbeRTSP = func(_ context.Context, _ string, _ int, _, _ string, ch int, st provisionnvr.Stream) (*provisionnvr.RTSPFacts, error) {
				return &provisionnvr.RTSPFacts{Channel: ch, Stream: st, Codec: "H265"}, nil
			}
			return provisionnvr.Detect(ctx, in)
		}}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	body := fmt.Sprintf(`{"host":"%s","port":%s,"username":"admin","password":"nvr-pass"}`, u.Hostname(), u.Port())
	var res NVRDetectResponse
	if c := send(t, "POST", ts.URL+"/api/v1/nvrs/detect", body, &res); c != 200 {
		t.Fatalf("detect %d", c)
	}
	if res.Model == "" || len(res.Channels) == 0 || res.RTSPPort == 0 {
		t.Fatalf("detection %+v", res)
	}
	ch := res.Channels[0]
	if !strings.Contains(ch.MainURL, "/chID=") || !strings.Contains(ch.SubURL, "streamType=sub") {
		t.Fatalf("channel urls %+v", ch)
	}
	// Importing a channel is an ordinary camera create with the recorder credentials.
	if c := send(t, "POST", ts.URL+"/api/v1/cameras", fmt.Sprintf(`{"id":"nvr-ch%d","name":"x","main_url":%q,"sub_url":%q,"username":"admin","password":"nvr-pass"}`, ch.Number, ch.MainURL, ch.SubURL), nil); c != 201 {
		t.Fatalf("import %d", c)
	}
	res = NVRDetectResponse{}
	send(t, "POST", ts.URL+"/api/v1/nvrs/detect", body, &res)
	if res.Channels[0].CameraID == "" {
		t.Fatalf("imported channel not marked: %+v", res.Channels[0])
	}

	bad := fmt.Sprintf(`{"host":"%s","port":%s,"username":"admin","password":"wrong"}`, u.Hostname(), u.Port())
	if c := send(t, "POST", ts.URL+"/api/v1/nvrs/detect", bad, nil); c != 422 {
		t.Fatalf("bad password %d", c)
	}
	if c := send(t, "POST", ts.URL+"/api/v1/nvrs/detect", `{"host":"8.8.8.8","username":"a"}`, nil); c != 422 {
		t.Fatalf("outside allow-list %d", c)
	}
}
