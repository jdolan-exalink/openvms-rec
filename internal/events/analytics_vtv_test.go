package events

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/cameras"
	"github.com/jdolan-exalink/openvms-rec/internal/config"
	"github.com/jdolan-exalink/openvms-rec/internal/db"
	"github.com/jdolan-exalink/openvms-rec/internal/onvif"
	"github.com/jdolan-exalink/openvms-rec/internal/platform"
	"github.com/jdolan-exalink/openvms-rec/internal/segments"
	"github.com/jdolan-exalink/openvms-rec/internal/storage"
	"github.com/jdolan-exalink/openvms-rec/internal/storage/volumes"
)

func TestTVTMultiTargetScale10000AndQueryFilters(t *testing.T) {
	store, reg := openTestEnv(t)
	store.MinConfidence = 0.40
	h := NewProvisionHandler(store, reg, slog.New(slog.NewTextHandler(io.Discard, nil)))

	xmlPayload := `<?xml version="1.0" encoding="UTF-8"?>
<SmartNotification>
  <smartType>PEA</smartType>
  <ruleName>AccesoCochera</ruleName>
  <targetList>
    <target>
      <targetType>motorbike</targetType>
      <confidence>91</confidence>
      <rect><x>4000</x><y>2000</y><width>3000</width><height>5000</height></rect>
    </target>
    <target>
      <targetType>human</targetType>
      <confidence>82</confidence>
      <rect><x>1000</x><y>1500</y><width>1500</width><height>6000</height></rect>
    </target>
    <target>
      <targetType>car</targetType>
      <confidence>25</confidence>
      <rect><x>500</x><y>500</y><width>500</width><height>500</height></rect>
    </target>
  </targetList>
</SmartNotification>`

	req := httptest.NewRequest(http.MethodPost, "/SendSmartData", strings.NewReader(xmlPayload))
	req.RemoteAddr = "192.0.2.225:51234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	// Priority elects "person" over "vehicle" above minConfidence 0.40; low-conf car (0.25) is filtered out.
	evs, _, err := store.List(context.Background(), Query{CameraID: "cam-1", Source: "provisionISR"})
	if err != nil || len(evs) != 1 {
		t.Fatalf("expected 1 event, got %+v err=%v", evs, err)
	}
	ev := evs[0]
	if ev.Type != "person" || ev.Zone != "AccesoCochera" || len(ev.Detections) != 2 {
		t.Fatalf("unexpected multi-target normalized event: %+v", ev)
	}
	// Check 0..10000 bbox normalization on person: [0.10, 0.15, 0.15, 0.60]
	if math.Abs(ev.BBox[0]-0.10) > 0.01 || math.Abs(ev.BBox[1]-0.15) > 0.01 || math.Abs(ev.BBox[3]-0.60) > 0.01 {
		t.Fatalf("expected 0..10000 scaled bbox, got %+v", ev.BBox)
	}

	// Combined class + min_confidence matches motorcycle (0.91 >= 0.85) but excludes person (0.82 < 0.85).
	minHigh := 0.85
	highMoto, _, err := store.List(context.Background(), Query{CameraID: "cam-1", Class: "motorcycle", MinConfidence: &minHigh})
	if err != nil || len(highMoto) != 1 {
		t.Fatalf("expected motorcycle 0.91 to match >=0.85, got %+v err=%v", highMoto, err)
	}
	highPerson, _, err := store.List(context.Background(), Query{CameraID: "cam-1", Class: "person", MinConfidence: &minHigh})
	if err != nil || len(highPerson) != 0 {
		t.Fatalf("expected person 0.82 filtered out at >=0.85, got %+v err=%v", highPerson, err)
	}
	minTop := 0.95
	tooHigh, _, err := store.List(context.Background(), Query{CameraID: "cam-1", MinConfidence: &minTop})
	if err != nil || len(tooHigh) != 0 {
		t.Fatalf("expected no detections >=0.95, got %+v err=%v", tooHigh, err)
	}
}

func TestProbeTVTDigestAndEmptyPolygons(t *testing.T) {
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") == "" {
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     http.Header{"WWW-Authenticate": []string{`Digest realm="IPCamera", nonce="abc123", qop="auth"`}},
				Body:       io.NopCloser(strings.NewReader("unauthorized")),
			}, nil
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Digest ") {
			t.Fatalf("expected Digest auth header, got %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/GetDeviceInfo":
			return xmlHTTPResp(200, `<deviceInfo><deviceName>IPC-Cochera</deviceName><firmwareVersion>5.1.2</firmwareVersion></deviceInfo>`), nil
		case "/GetAlarmStatus":
			return xmlHTTPResp(200, `<alarmStatusInfo><motionAlarm>false</motionAlarm><perimeterAlarm>false</perimeterAlarm></alarmStatusInfo>`), nil
		case "/GetPerimeterConfig":
			return xmlHTTPResp(200, `<config><perimeter><switch>true</switch><objectFilter><person><switch>true</switch></person></objectFilter><boundary type="list" count="4"><item><point type="list" maxCount="6" count="0"/></item></boundary></perimeter></config>`), nil
		case "/GetTripwireConfig":
			return xmlHTTPResp(200, `<config><tripwire><switch>false</switch><objectFilter><car><switch>true</switch></car><person><switch>true</switch></person></objectFilter><line type="list" count="4"><item><startPoint><X>0</X><Y>0</Y></startPoint><endPoint><X>9825</X><Y>3366</Y></endPoint></item><item><startPoint><X>0</X><Y>0</Y></startPoint><endPoint><X>0</X><Y>0</Y></endPoint></item></line></tripwire></config>`), nil
		default:
			return xmlHTTPResp(404, "not found"), nil
		}
	})

	res, err := ProbeTVT(context.Background(), rt, "192.0.2.225", 80, "op", "sample", time.Unix(1700000000, 0).UTC())
	if err != nil {
		t.Fatalf("ProbeTVT: %v", err)
	}
	if !res.Supported || !res.AlarmStatus || res.Perimeter == nil || !res.Perimeter.Enabled || res.Perimeter.PolygonsConfigured != 0 {
		t.Fatalf("unexpected TVT probe result: %+v", res)
	}
	if res.Tripwire == nil || res.Tripwire.Enabled || res.Tripwire.LineCount != 1 {
		t.Fatalf("expected tripwire disabled with 1 configured line (ignoring nested objectFilter switch=true), got %+v", res.Tripwire)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "count=0") {
		t.Fatalf("expected 1 empty perimeter warning (not tripwire false-positive), got %+v", res.Warnings)
	}
}

func TestExtractMotionROIsAndHTTPClassifier(t *testing.T) {
	const w, h = 160, 90
	prev := make([]byte, w*h)
	cur := make([]byte, w*h)
	// Draw two separated changed regions: a vertical blob and a horizontal blob.
	for y := 20; y < 60; y++ {
		for x := 30; x < 42; x++ {
			cur[y*w+x] = 90
		}
	}
	for y := 50; y < 70; y++ {
		for x := 100; x < 140; x++ {
			cur[y*w+x] = 90
		}
	}
	ratio, rois := ExtractMotionROIs(prev, cur, w, h, 25)
	if ratio < 0.05 || len(rois) != 2 {
		t.Fatalf("expected ratio>0.05 and 2 ROIs, got ratio=%.3f rois=%+v", ratio, rois)
	}

	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"camera_id":"cam-1"`)) {
			t.Fatalf("expected camera_id in request payload: %s", string(body))
		}
		respJSON, _ := json.Marshal(map[string]any{
			"detections": []map[string]any{
				{"type": "person", "label": "pedestrian", "confidence": 0.91, "bbox": []float64{0.18, 0.22, 0.08, 0.45}},
				{"type": "vehicle", "label": "car", "confidence": 0.31, "bbox": []float64{0.6, 0.5, 0.2, 0.2}},
			},
		})
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(respJSON)),
		}, nil
	})
	cls := NewObjectClassifier(config.ClassifierConfig{
		Mode:          "http",
		Endpoint:      "http://127.0.0.1:8765/detect",
		MinConfidence: 0.5,
		TimeoutMS:     1000,
	}, rt)
	dets, err := cls.ClassifyFrame(context.Background(), "cam-1", []byte{0xFF, 0xD8, 0xFF, 0xD9}, rois)
	if err != nil || len(dets) != 1 || dets[0].Type != "person" || dets[0].Class != "pedestrian" {
		t.Fatalf("expected 1 filtered person detection, got %+v err=%v", dets, err)
	}
}

func TestProfileGSyncGapCoverageAndPartialSplit(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	volRoot := filepath.Join(dir, "vol1")
	if err := os.MkdirAll(volRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	prober := mockProber{
		root:  volRoot,
		usage: platform.DiskUsage{TotalBytes: 100 << 30, FreeBytes: 80 << 30},
		info:  platform.VolumeInfo{StableID: "uuid-sd-test", MountPoint: volRoot, FSType: "ext4"},
	}
	cfg := config.Default()
	cfg.Data.Dir = dir
	cfg.Storage.MinFreeGB = 1
	cfg.Storage.Volumes = []config.Volume{{ID: "vol1", Path: volRoot}}
	if _, err := volumes.Initialize(prober, storage.SpecsFromConfig(cfg, runtime.GOOS)[0], time.Now()); err != nil {
		t.Fatal(err)
	}

	d, err := db.Open(ctx, filepath.Join(dir, "rec.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	segStore := segments.New(d)
	mgr, err := storage.NewManager(cfg, runtime.GOOS, prober, segStore, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.LoadVolumes(ctx, d); err != nil {
		t.Fatal(err)
	}
	mgr.Refresh(ctx)

	reg, err := cameras.Open(ctx, cameras.Options{
		DB: d,
		Static: []config.Camera{{
			ID:      "cam-g",
			Host:    "192.0.2.225",
			MainURL: "rtsp://192.0.2.225:554/profile1",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	gStart := now.Add(-30 * time.Minute)
	gEnd := now.Add(-10 * time.Minute)
	recEarliest := now.Add(-25 * time.Minute)
	recLatest := now.Add(-15 * time.Minute)
	if _, err := d.ExecContext(ctx, `INSERT INTO recording_gaps(camera_id, representation_id, start_utc, end_utc, reason) VALUES (?, 'main', ?, ?, 'stream_lost')`,
		"cam-g", gStart.UnixMilli(), gEnd.UnixMilli()); err != nil {
		t.Fatal(err)
	}

	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		req := string(b)
		switch {
		case strings.Contains(req, "GetSystemDateAndTime"):
			return soapHTTPResp(200, `<tds:GetSystemDateAndTimeResponse xmlns:tds="http://www.onvif.org/ver10/device/wsdl"/>`), nil
		case strings.Contains(req, "GetServices"):
			return soapHTTPResp(200, `<tds:GetServicesResponse xmlns:tds="http://www.onvif.org/ver10/device/wsdl">
				<tds:Service><tds:Namespace>http://www.onvif.org/ver10/search/wsdl</tds:Namespace><tds:XAddr>http://192.0.2.225/onvif/search</tds:XAddr></tds:Service>
				<tds:Service><tds:Namespace>http://www.onvif.org/ver10/replay/wsdl</tds:Namespace><tds:XAddr>http://192.0.2.225/onvif/replay</tds:XAddr></tds:Service>
			</tds:GetServicesResponse>`), nil
		case strings.Contains(req, "GetRecordingSummary"):
			return soapHTTPResp(200, `<tse:GetRecordingSummaryResponse xmlns:tse="http://www.onvif.org/ver10/search/wsdl"><tse:Summary><tse:DataFrom>`+recEarliest.Format(time.RFC3339)+`</tse:DataFrom><tse:DataUntil>`+recLatest.Format(time.RFC3339)+`</tse:DataUntil><tse:NumberRecordings>1</tse:NumberRecordings></tse:Summary></tse:GetRecordingSummaryResponse>`), nil
		case strings.Contains(req, "FindRecordings"):
			return soapHTTPResp(200, `<tse:FindRecordingsResponse xmlns:tse="http://www.onvif.org/ver10/search/wsdl"><tse:SearchToken>tok1</tse:SearchToken></tse:FindRecordingsResponse>`), nil
		case strings.Contains(req, "GetRecordingSearchResults"):
			return soapHTTPResp(200, `<tse:GetRecordingSearchResultsResponse xmlns:tse="http://www.onvif.org/ver10/search/wsdl" xmlns:tt="http://www.onvif.org/ver10/schema"><tse:ResultList><tt:SearchState>Completed</tt:SearchState><tt:RecordingInformation><tt:RecordingToken>sd_1</tt:RecordingToken><tt:Track><tt:TrackType>Video</tt:TrackType></tt:Track></tt:RecordingInformation></tse:ResultList></tse:GetRecordingSearchResultsResponse>`), nil
		case strings.Contains(req, "EndSearch"):
			return soapHTTPResp(200, `<tse:EndSearchResponse xmlns:tse="http://www.onvif.org/ver10/search/wsdl"/>`), nil
		case strings.Contains(req, "GetReplayUri"):
			return soapHTTPResp(200, `<trp:GetReplayUriResponse xmlns:trp="http://www.onvif.org/ver10/replay/wsdl"><trp:Uri>rtsp://192.0.2.225:554/replay/sd_1</trp:Uri></trp:GetReplayUriResponse>`), nil
		default:
			return soapHTTPResp(400, `<s:Fault/>`), nil
		}
	})

	svc := &ProfileGSyncService{
		DB:        d,
		Events:    NewStore(d),
		Segments:  segStore,
		Cameras:   reg,
		Storage:   mgr,
		Cfg:       cfg.Recovery,
		Transport: rt,
		Fetch: func(_ context.Context, _, _ string, _, _ time.Time, _ float64, dst string) error {
			return os.WriteFile(dst, make([]byte, 188*10), 0o640)
		},
		Log: slog.Default(),
	}
	res, err := svc.RunCameraSync(ctx, "cam-g", 2*time.Hour)
	if err != nil {
		t.Fatalf("RunCameraSync: %v", err)
	}
	// 10m window split into bounded 120s chunks => 5 recovered segments, 9400 bytes, with GetRecordingSummary fallback bounds.
	if res.SegmentsRecovered != 5 || res.BytesRecovered != 9400 || res.SummaryRecordings != 1 || res.ReplayURI != "rtsp://192.0.2.225:554/replay/sd_1" {
		t.Fatalf("unexpected Profile G sync result: %+v", res)
	}
	if res.EarliestRecording != recEarliest.Format(time.RFC3339) || res.LatestRecording != recLatest.Format(time.RFC3339) {
		t.Fatalf("expected summary bounds in result, got %s .. %s", res.EarliestRecording, res.LatestRecording)
	}
	recs, err := segStore.Range(ctx, "cam-g", "main", gStart, gEnd)
	if err != nil || len(recs) != 5 || recs[0].Source != segments.SourceRecoveredSD {
		t.Fatalf("expected 5 recovered_sd segments, got %+v err=%v", recs, err)
	}
	// Original gap [gStart..gEnd) must now be split cleanly into 2 uncovered gaps around [recEarliest..recLatest).
	remGaps, err := segStore.Gaps(ctx, "cam-g", "main", gStart, gEnd)
	if err != nil || len(remGaps) != 2 {
		t.Fatalf("expected 2 remaining partial gaps (no invented continuity), got %+v err=%v", remGaps, err)
	}

	// Verify DB-backed LastResult survives fresh ProfileGSyncService instance without in-memory cache.
	freshSvc := &ProfileGSyncService{DB: d}
	last, ok := freshSvc.LastResult(ctx, "cam-g")
	if !ok || last.SegmentsRecovered != 5 || last.Status != "succeeded" {
		t.Fatalf("expected persisted LastResult from DB, got ok=%v last=%+v", ok, last)
	}
}

func TestProfileGSyncIgnores1970SentinelAndReportsUnformattedSD(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	d, err := db.Open(ctx, filepath.Join(base, "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	reg, err := cameras.Open(ctx, cameras.Options{
		DB: d,
		Static: []config.Camera{{
			ID:      "cochera",
			Host:    "192.0.2.225",
			MainURL: "rtsp://192.0.2.225:554/profile1",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	segStore := segments.New(d)
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := d.ExecContext(ctx, `INSERT INTO recording_gaps(camera_id, representation_id, start_utc, end_utc, reason) VALUES ('cochera', 'main', ?, ?, 'stream_lost')`,
		now.Add(-20*time.Minute).UnixMilli(), now.Add(-10*time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}

	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/GetDiskInfo" {
			return xmlHTTPResp(200, `<config><diskInfo count="1"><item><totalSpace>0</totalSpace><freeSpace>0</freeSpace><status>unformat</status></item></diskInfo></config>`), nil
		}
		b, _ := io.ReadAll(r.Body)
		req := string(b)
		switch {
		case strings.Contains(req, "GetSystemDateAndTime"):
			return soapHTTPResp(200, `<tds:GetSystemDateAndTimeResponse xmlns:tds="http://www.onvif.org/ver10/device/wsdl"/>`), nil
		case strings.Contains(req, "GetServices"):
			return soapHTTPResp(200, `<tds:GetServicesResponse xmlns:tds="http://www.onvif.org/ver10/device/wsdl">
				<tds:Service><tds:Namespace>http://www.onvif.org/ver10/search/wsdl</tds:Namespace><tds:XAddr>http://192.0.2.225/onvif/SearchRecording</tds:XAddr></tds:Service>
				<tds:Service><tds:Namespace>http://www.onvif.org/ver10/replay/wsdl</tds:Namespace><tds:XAddr>http://192.0.2.225/onvif/Replay</tds:XAddr></tds:Service>
				<tds:Service><tds:Namespace>http://www.onvif.org/ver10/recording/wsdl</tds:Namespace><tds:XAddr>http://192.0.2.225/onvif/Recording</tds:XAddr></tds:Service>
			</tds:GetServicesResponse>`), nil
		case strings.Contains(req, "GetRecordingSummary"):
			return soapHTTPResp(200, `<tse:GetRecordingSummaryResponse xmlns:tse="http://www.onvif.org/ver10/search/wsdl"><tse:Summary><tse:DataFrom>1970-01-01T00:00:00Z</tse:DataFrom><tse:DataUntil>1970-01-01T00:00:00Z</tse:DataUntil><tse:NumberRecordings>1</tse:NumberRecordings></tse:Summary></tse:GetRecordingSummaryResponse>`), nil
		case strings.Contains(req, "FindRecordings"):
			return soapHTTPResp(200, `<tse:FindRecordingsResponse xmlns:tse="http://www.onvif.org/ver10/search/wsdl"><tse:SearchToken>tok1</tse:SearchToken></tse:FindRecordingsResponse>`), nil
		case strings.Contains(req, "GetRecordingSearchResults"):
			return soapHTTPResp(200, `<tse:GetRecordingSearchResultsResponse xmlns:tse="http://www.onvif.org/ver10/search/wsdl" xmlns:tt="http://www.onvif.org/ver10/schema"><tse:ResultList><tt:SearchState>Completed</tt:SearchState></tse:ResultList></tse:GetRecordingSearchResultsResponse>`), nil
		case strings.Contains(req, "EndSearch"):
			return soapHTTPResp(200, `<tse:EndSearchResponse xmlns:tse="http://www.onvif.org/ver10/search/wsdl"/>`), nil
		case strings.Contains(req, "GetRecordings"):
			return soapHTTPResp(200, `<trc:GetRecordingsResponse xmlns:trc="http://www.onvif.org/ver10/recording/wsdl" xmlns:tt="http://www.onvif.org/ver10/schema"><trc:RecordingItem><tt:RecordingToken>tokenRecording1</tt:RecordingToken></trc:RecordingItem></trc:GetRecordingsResponse>`), nil
		case strings.Contains(req, "GetRecordingJobs"):
			return soapHTTPResp(200, `<trc:GetRecordingJobsResponse xmlns:trc="http://www.onvif.org/ver10/recording/wsdl"/>`), nil
		case strings.Contains(req, "GetReplayUri"):
			return soapHTTPResp(200, `<trp:GetReplayUriResponse xmlns:trp="http://www.onvif.org/ver10/replay/wsdl"><trp:Uri>rtsp://192.0.2.225:554/Recording</trp:Uri></trp:GetReplayUriResponse>`), nil
		default:
			return soapHTTPResp(400, `<s:Fault/>`), nil
		}
	})

	svc := &ProfileGSyncService{
		DB:        d,
		Events:    NewStore(d),
		Segments:  segStore,
		Cameras:   reg,
		Storage:   &storage.Manager{},
		Cfg:       config.Recovery{CameraSDBackfill: "when_supported"},
		Transport: rt,
		Log:       slog.Default(),
	}
	res, err := svc.RunCameraSync(ctx, "cochera", 2*time.Hour)
	if err != nil {
		t.Fatalf("RunCameraSync: %v", err)
	}
	if res.EarliestRecording != "" || res.LatestRecording != "" {
		t.Fatalf("expected 1970-01-01 sentinel stripped, got %q .. %q", res.EarliestRecording, res.LatestRecording)
	}
	if res.GapsEvaluated != 0 || !strings.Contains(res.Message, "unformat") {
		t.Fatalf("expected unformat message and 0 evaluated gaps, got %+v", res)
	}
}

func TestRTSPRangeClockReplayProxyAndAutoGapSync(t *testing.T) {
	if got := sanitizeRTSPError("Error opening rtsp://u:p@127.0.0.1:554/Recording and rtsp://127.0.0.1/ok"); got != "Error opening rtsp://***@127.0.0.1:554/Recording and rtsp://127.0.0.1/ok" {
		t.Fatalf("unexpected sanitizeRTSPError output: %q", got)
	}
	// 1. Verify startRTSPRangeProxy rewrites host and injects ONVIF Range: clock= + Rate-Control: no on PLAY.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var seenMu sync.Mutex
	var seenPlay string
	srvDone := make(chan struct{})
	go func() {
		defer close(srvDone)
		c, aErr := ln.Accept()
		if aErr != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		for {
			var lines []string
			for {
				l, rErr := br.ReadString('\n')
				if rErr != nil {
					return
				}
				lines = append(lines, l)
				if strings.TrimRight(l, "\r\n") == "" {
					break
				}
			}
			full := strings.Join(lines, "")
			if strings.HasPrefix(full, "PLAY ") {
				seenMu.Lock()
				seenPlay = full
				seenMu.Unlock()
			}
			_, _ = io.WriteString(c, "RTSP/1.0 200 OK\r\nCSeq: 1\r\n\r\n")
			if strings.HasPrefix(full, "TEARDOWN ") {
				return
			}
		}
	}()

	from := time.Date(2026, 11, 17, 10, 15, 0, 0, time.UTC)
	to := from.Add(90 * time.Second)
	rawURL := "rtsp://" + ln.Addr().String() + "/Recording"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proxyURL, cleanup, err := startRTSPRangeProxy(ctx, rawURL, from, to)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	pu, _ := url.Parse(proxyURL)
	conn, err := net.Dial("tcp", pu.Host)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(conn, "PLAY "+proxyURL+" RTSP/1.0\r\nCSeq: 1\r\nSession: 123\r\nRange: npt=0.000-\r\n\r\n")
	buf := make([]byte, 256)
	_, _ = conn.Read(buf)
	_, _ = io.WriteString(conn, "TEARDOWN "+proxyURL+" RTSP/1.0\r\nCSeq: 2\r\nSession: 123\r\n\r\n")
	conn.Close()
	<-srvDone

	seenMu.Lock()
	gotPlay := seenPlay
	seenMu.Unlock()
	if !strings.Contains(gotPlay, "PLAY "+rawURL+" RTSP/1.0") ||
		!strings.Contains(gotPlay, "Range: clock=20261117T101500Z-20261117T101630Z") ||
		!strings.Contains(gotPlay, "Rate-Control: no") {
		t.Fatalf("unexpected proxied PLAY request:\n%s", gotPlay)
	}

	// 2. Verify StartLoop wakes up cleanly on NotifyCamera when a recording cut closes a gap.
	dir := t.TempDir()
	volRoot := filepath.Join(dir, "vol1")
	_ = os.MkdirAll(volRoot, 0o750)
	prober := mockProber{
		root:  volRoot,
		usage: platform.DiskUsage{TotalBytes: 100 << 30, FreeBytes: 80 << 30},
		info:  platform.VolumeInfo{StableID: "uuid-auto-sync", MountPoint: volRoot, FSType: "ext4"},
	}
	cfg := config.Default()
	cfg.Data.Dir = dir
	cfg.Storage.MinFreeGB = 1
	cfg.Storage.Volumes = []config.Volume{{ID: "vol1", Path: volRoot}}
	_, _ = volumes.Initialize(prober, storage.SpecsFromConfig(cfg, runtime.GOOS)[0], time.Now())

	d, err := db.Open(ctx, filepath.Join(dir, "rec.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	segStore := segments.New(d)
	mgr, _ := storage.NewManager(cfg, runtime.GOOS, prober, segStore, slog.Default())
	_ = mgr.LoadVolumes(ctx, d)
	mgr.Refresh(ctx)
	reg, _ := cameras.Open(ctx, cameras.Options{
		DB:     d,
		Static: []config.Camera{{ID: "cam-cut", Host: "192.0.2.225", MainURL: "rtsp://192.0.2.225:554/profile1"}},
	})
	now := time.Now().UTC().Truncate(time.Second)
	gStart, gEnd := now.Add(-4*time.Minute), now.Add(-2*time.Minute)
	if _, err := d.ExecContext(ctx, `INSERT INTO recording_gaps(camera_id, representation_id, start_utc, end_utc, reason) VALUES ('cam-cut', 'main', ?, ?, 'stream_lost')`,
		gStart.UnixMilli(), gEnd.UnixMilli()); err != nil {
		t.Fatal(err)
	}

	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		req := string(b)
		switch {
		case strings.Contains(req, "GetSystemDateAndTime"):
			return soapHTTPResp(200, `<tds:GetSystemDateAndTimeResponse xmlns:tds="http://www.onvif.org/ver10/device/wsdl"/>`), nil
		case strings.Contains(req, "GetServices"):
			return soapHTTPResp(200, `<tds:GetServicesResponse xmlns:tds="http://www.onvif.org/ver10/device/wsdl">
				<tds:Service><tds:Namespace>http://www.onvif.org/ver10/search/wsdl</tds:Namespace><tds:XAddr>http://192.0.2.225/onvif/search</tds:XAddr></tds:Service>
				<tds:Service><tds:Namespace>http://www.onvif.org/ver10/replay/wsdl</tds:Namespace><tds:XAddr>http://192.0.2.225/onvif/replay</tds:XAddr></tds:Service>
			</tds:GetServicesResponse>`), nil
		case strings.Contains(req, "GetRecordingSummary"):
			return soapHTTPResp(200, `<tse:GetRecordingSummaryResponse xmlns:tse="http://www.onvif.org/ver10/search/wsdl"><tse:Summary><tse:DataFrom>`+gStart.Add(-time.Minute).Format(time.RFC3339)+`</tse:DataFrom><tse:DataUntil>`+gEnd.Add(time.Minute).Format(time.RFC3339)+`</tse:DataUntil><tse:NumberRecordings>1</tse:NumberRecordings></tse:Summary></tse:GetRecordingSummaryResponse>`), nil
		case strings.Contains(req, "FindRecordings"):
			return soapHTTPResp(200, `<tse:FindRecordingsResponse xmlns:tse="http://www.onvif.org/ver10/search/wsdl"><tse:SearchToken>t1</tse:SearchToken></tse:FindRecordingsResponse>`), nil
		case strings.Contains(req, "GetRecordingSearchResults"):
			return soapHTTPResp(200, `<tse:GetRecordingSearchResultsResponse xmlns:tse="http://www.onvif.org/ver10/search/wsdl" xmlns:tt="http://www.onvif.org/ver10/schema"><tse:ResultList><tt:SearchState>Completed</tt:SearchState><tt:RecordingInformation><tt:RecordingToken>sd_1</tt:RecordingToken><tt:Track><tt:TrackType>Video</tt:TrackType></tt:Track></tt:RecordingInformation></tse:ResultList></tse:GetRecordingSearchResultsResponse>`), nil
		case strings.Contains(req, "EndSearch"):
			return soapHTTPResp(200, `<tse:EndSearchResponse xmlns:tse="http://www.onvif.org/ver10/search/wsdl"/>`), nil
		case strings.Contains(req, "GetReplayUri"):
			return soapHTTPResp(200, `<trp:GetReplayUriResponse xmlns:trp="http://www.onvif.org/ver10/replay/wsdl"><trp:Uri>rtsp://192.0.2.225:554/Recording</trp:Uri></trp:GetReplayUriResponse>`), nil
		default:
			return soapHTTPResp(400, `<s:Fault/>`), nil
		}
	})

	svc := &ProfileGSyncService{
		DB: d, Events: NewStore(d), Segments: segStore, Cameras: reg, Storage: mgr,
		Cfg: cfg.Recovery, Transport: rt,
		Fetch: func(_ context.Context, _, _ string, _, _ time.Time, _ float64, dst string) error {
			return os.WriteFile(dst, make([]byte, 188*5), 0o640)
		},
		Log: slog.Default(),
	}

	loopCtx, stopLoop := context.WithCancel(ctx)
	defer stopLoop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.StartLoop(loopCtx, time.Hour)
	}()
	svc.NotifyCamera("cam-cut")

	deadline := time.Now().Add(10 * time.Second)
	recovered := false
	for time.Now().Before(deadline) {
		if r, ok := svc.LastResult(ctx, "cam-cut"); ok && r.SegmentsRecovered == 1 {
			recovered = true
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	stopLoop()
	<-done
	if !recovered {
		r, ok := svc.LastResult(ctx, "cam-cut")
		t.Fatalf("expected NotifyCamera auto backfill to recover 1 segment after recording cut, got ok=%v res=%+v", ok, r)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func xmlHTTPResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/xml"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func soapHTTPResp(status int, body string) *http.Response {
	env := `<?xml version="1.0" encoding="UTF-8"?><s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Body>` + body + `</s:Body></s:Envelope>`
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/soap+xml"}},
		Body:       io.NopCloser(strings.NewReader(env)),
	}
}

type mockProber struct {
	root  string
	usage platform.DiskUsage
	info  platform.VolumeInfo
}

func (p mockProber) Usage(path string) (platform.DiskUsage, error) {
	if strings.HasPrefix(path, p.root) {
		return p.usage, nil
	}
	return platform.DiskUsage{}, os.ErrNotExist
}

func (p mockProber) Volume(path string) (platform.VolumeInfo, error) {
	if strings.HasPrefix(path, p.root) {
		return p.info, nil
	}
	return platform.VolumeInfo{}, os.ErrNotExist
}

func TestProvisionTVTFeatureResultBase64AndNumericTarget(t *testing.T) {
	store, reg := openTestEnv(t)
	snapDir := filepath.Join(t.TempDir(), "snapshots")
	snap := &Snapshotter{
		DB:  store.db,
		Dir: snapDir,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	store.OnInsert = func(ev Event) {
		if len(ev.SnapshotData) == 0 {
			return
		}
		if _, err := snap.Capture(context.Background(), ev); err != nil {
			t.Errorf("snap.Capture: %v", err)
		}
	}
	h := NewProvisionHandler(store, reg, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Create a uniform gray 1280x720 JPEG for <sourceBase64Data>.
	srcImg := image.NewRGBA(image.Rect(0, 0, 1280, 720))
	for y := 0; y < 720; y++ {
		for x := 0; x < 1280; x++ {
			srcImg.SetRGBA(x, y, color.RGBA{R: 120, G: 120, B: 120, A: 255})
		}
	}
	var jpgBuf bytes.Buffer
	if err := jpeg.Encode(&jpgBuf, srcImg, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("jpeg.Encode: %v", err)
	}
	b64 := base64.StdEncoding.EncodeToString(jpgBuf.Bytes())

	// 1. Metadata-only PEA packet (no image) with flat x1/y1/x2/y2 and numeric targetType=1 (person).
	metaXML := `<?xml version="1.0" encoding="UTF-8"?>
<config>
  <smartType>PEA</smartType>
  <subscribeRelation>FEATURE_RESULT</subscribeRelation>
  <status>SMART_START</status>
  <targetId>716</targetId>
  <x1>426</x1><y1>6840</y1><x2>2159</x2><y2>9722</y2>
  <listInfo>
    <item>
      <targetId>716</targetId>
      <x1>421</x1><y1>6833</y1><x2>2148</x2><y2>9708</y2>
      <targetType>1</targetType>
    </item>
  </listInfo>
</config>`
	req1 := httptest.NewRequest(http.MethodPost, "/SendAlarmData", strings.NewReader(metaXML))
	req1.RemoteAddr = "192.0.2.225:41001"
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("metaXML status=%d", rec1.Code)
	}

	// 2. Photo-bearing packet arrives in the same 15s window with <width>1280</width><height>720</height>
	// and <sourceBase64Data>, while <x1>..<y2> stay on 0..10000 grid.
	photoXML := `<?xml version="1.0" encoding="UTF-8"?>
<config>
  <smartType>PEA</smartType>
  <subscribeRelation>FEATURE_RESULT</subscribeRelation>
  <status>SMART_START</status>
  <width>1280</width><height>720</height>
  <targetId>716</targetId>
  <x1>426</x1><y1>6840</y1><x2>2159</x2><y2>9722</y2>
  <sourceBase64Data>` + b64 + `</sourceBase64Data>
  <listInfo>
    <item>
      <targetId>716</targetId>
      <x1>421</x1><y1>6833</y1><x2>2148</x2><y2>9708</y2>
      <targetType>1</targetType>
    </item>
  </listInfo>
</config>`
	req2 := httptest.NewRequest(http.MethodPost, "/SendAlarmData", strings.NewReader(photoXML))
	req2.RemoteAddr = "192.0.2.225:41002"
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("photoXML status=%d", rec2.Code)
	}

	// 3. Empty SMART_STOP and empty FEATURE_RESULT listInfo must not record phantom intrusion events.
	stopXML := `<?xml version="1.0" encoding="UTF-8"?>
<config><smartType>PEA</smartType><status>SMART_STOP</status><eventId>916</eventId></config>`
	req3 := httptest.NewRequest(http.MethodPost, "/SendAlarmData", strings.NewReader(stopXML))
	req3.RemoteAddr = "192.0.2.225:41003"
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Fatalf("stopXML status=%d", rec3.Code)
	}
	emptyListXML := `<?xml version="1.0" encoding="UTF-8"?>
<config><smartType>PEA</smartType><subscribeRelation>FEATURE_RESULT</subscribeRelation><sourceBase64Length>0</sourceBase64Length><listInfo/></config>`
	req3b := httptest.NewRequest(http.MethodPost, "/SendAlarmData", strings.NewReader(emptyListXML))
	req3b.RemoteAddr = "192.0.2.225:41004"
	rec3b := httptest.NewRecorder()
	h.ServeHTTP(rec3b, req3b)
	if rec3b.Code != http.StatusOK {
		t.Fatalf("emptyListXML status=%d", rec3b.Code)
	}

	// 4. Trailing metadata-only FEATURE_RESULT after photo was seen must be suppressed (no duplicate/ghost box).
	req4 := httptest.NewRequest(http.MethodPost, "/SendAlarmData", strings.NewReader(metaXML))
	req4.RemoteAddr = "192.0.2.225:41005"
	rec4 := httptest.NewRecorder()
	h.ServeHTTP(rec4, req4)
	if rec4.Code != http.StatusOK {
		t.Fatalf("trailing metaXML status=%d", rec4.Code)
	}

	evs, _, err := store.List(context.Background(), Query{CameraID: "cam-1", Source: "provisionISR"})
	if err != nil || len(evs) != 2 {
		t.Fatalf("expected 2 person events (initial meta + photo, no phantoms or trailing meta dupes), got %d err=%v (%+v)", len(evs), err, evs)
	}
	var photoEv *Event
	for i := range evs {
		ev := &evs[i]
		if ev.Type != "person" || ev.Class != "person" {
			t.Fatalf("expected person/person, got type=%q class=%q", ev.Type, ev.Class)
		}
		if len(ev.Detections) != 1 || len(ev.BBox) != 4 {
			t.Fatalf("expected 1 deduplicated detection with 4-coord bbox, got dets=%+v bbox=%+v", ev.Detections, ev.BBox)
		}
		if math.Abs(ev.BBox[0]-0.0421) > 0.002 || math.Abs(ev.BBox[1]-0.6833) > 0.002 ||
			math.Abs(ev.BBox[2]-0.1727) > 0.002 || math.Abs(ev.BBox[3]-0.2875) > 0.002 {
			t.Fatalf("expected 0..10000 scaled bbox despite 1280x720 image tags, got %+v", ev.BBox)
		}
		if ev.Snapshot != "" {
			photoEv = ev
		}
	}
	if photoEv == nil {
		t.Fatalf("expected photo-bearing packet not to be suppressed by prior metadata-only packet")
	}

	// Verify saved snapshot JPEG exists and has non-gray blue border pixels from Frigate-style bbox annotation.
	rawSnap, err := os.ReadFile(filepath.Join(snapDir, filepath.FromSlash(photoEv.Snapshot)))
	if err != nil {
		t.Fatalf("read saved snapshot: %v", err)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(rawSnap))
	if err != nil {
		t.Fatalf("decode saved snapshot: %v", err)
	}
	xBorder := int(math.Round(0.0421 * float64(decoded.Bounds().Dx())))
	yMid := int(math.Round((0.6833 + 0.14) * float64(decoded.Bounds().Dy())))
	r, g, b, _ := decoded.At(xBorder, yMid).RGBA()
	if (b>>8) <= (r>>8)+30 || (b>>8) <= (g>>8)+10 {
		t.Fatalf("expected blue person bbox stroke at (%d,%d), got RGBA=(%d,%d,%d)", xBorder, yMid, r>>8, g>>8, b>>8)
	}
}

func TestProvisionTripwirePEAZoneAttributionAndQueryFilter(t *testing.T) {
	store, reg := openTestEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := store.SaveCapabilities(ctx, "cam-1", []onvif.Capability{
		{Capability: "tvt.analytics.tripwire", Status: onvif.CapConfirmed, Evidence: `{"switch":true}`, CheckedAt: now},
		{Capability: "tvt.analytics.perimeter", Status: onvif.CapUnsupported, Evidence: `{"switch":false}`, CheckedAt: now},
	}); err != nil {
		t.Fatalf("SaveCapabilities: %v", err)
	}

	h := NewProvisionHandler(store, reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	xmlPayload := `<?xml version="1.0" encoding="UTF-8"?>
<config>
  <smartType>PEA</smartType>
  <subscribeRelation>FEATURE_RESULT</subscribeRelation>
  <targetId>42</targetId>
  <status>SMART_START</status>
  <listInfo>
    <item>
      <targetId>42</targetId>
      <x1>4937</x1><y1>5208</y1><x2>5929</x2><y2>9333</y2>
      <targetType>1</targetType>
    </item>
  </listInfo>
</config>`
	req := httptest.NewRequest(http.MethodPost, "/SendAlarmData", strings.NewReader(xmlPayload))
	req.RemoteAddr = "192.0.2.225:57666"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}

	// Filtering by type=line_crossing must find the person event attributed to Tripwire, including object class & bbox.
	byLine, _, err := store.List(ctx, Query{CameraID: "cam-1", Type: "line_crossing"})
	if err != nil || len(byLine) != 1 {
		t.Fatalf("expected 1 person event matched by Type=line_crossing, got %+v err=%v", byLine, err)
	}
	if byLine[0].Type != "person" || byLine[0].Class != "person" || byLine[0].Zone != "line_crossing" || len(byLine[0].Detections) != 1 {
		t.Fatalf("expected person/person in line_crossing zone with detection box, got %+v", byLine[0])
	}

	// Subsequent TVTPoller line_crossing alarm within 15s enriches/deduplicates instead of inserting empty duplicate.
	handled, err := store.EnrichRecentPushRule(ctx, "cam-1", "line_crossing", time.Now().UTC(), 15*time.Second)
	if err != nil || !handled {
		t.Fatalf("expected EnrichRecentPushRule handled=true, got handled=%v err=%v", handled, err)
	}
}
