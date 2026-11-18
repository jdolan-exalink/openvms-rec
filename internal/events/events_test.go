package events

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/cameras"
	"github.com/jdolan-exalink/openvms-rec/internal/db"
	"github.com/jdolan-exalink/openvms-rec/internal/onvif"
)

func openTestEnv(t *testing.T) (*Store, *cameras.Registry) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(ctx, filepath.Join(dir, "rec.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	reg, err := cameras.Open(ctx, cameras.Options{
		DB:        d,
		CredsFile: filepath.Join(dir, "creds.yaml"),
	})
	if err != nil {
		t.Fatalf("cameras.Open: %v", err)
	}
	user := "admin"
	pass := "secret"
	if _, err := reg.Create(ctx, cameras.Input{
		ID:       "cam-1",
		Name:     "Cochera",
		Host:     "192.0.2.225",
		MainURL:  "rtsp://192.0.2.225:554/profile1",
		Username: &user,
		Password: &pass,
	}); err != nil {
		t.Fatalf("create camera: %v", err)
	}
	return NewStore(d), reg
}

func TestStoreEventsAndCapabilities(t *testing.T) {
	store, _ := openTestEnv(t)
	ctx := context.Background()
	t0 := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

	ok, err := store.Insert(ctx, Event{
		CameraID:     "cam-1",
		TimestampUTC: t0,
		Type:         "lpr",
		Class:        "AF123BC",
		Source:       "provision_isr",
	}, "k1")
	if err != nil || !ok {
		t.Fatalf("first insert failed: ok=%v err=%v", ok, err)
	}
	// Duplicate dedupe key must be ignored
	ok2, err := store.Insert(ctx, Event{
		CameraID:     "cam-1",
		TimestampUTC: t0,
		Type:         "lpr",
		Class:        "AF123BC",
		Source:       "provision_isr",
	}, "k1")
	if err != nil || ok2 {
		t.Fatalf("expected duplicate ignored: ok=%v err=%v", ok2, err)
	}

	_, _ = store.Insert(ctx, Event{
		CameraID:     "cam-1",
		TimestampUTC: t0.Add(time.Minute),
		Type:         "motion",
		Class:        "motion",
		Source:       "onvif_events",
	}, "k2")

	items, next, err := store.List(ctx, Query{CameraID: "cam-1", Limit: 1})
	if err != nil || len(items) != 1 || next == "" || items[0].Type != "motion" {
		t.Fatalf("page 1 unexpected: items=%+v next=%q err=%v", items, next, err)
	}
	items2, next2, err := store.List(ctx, Query{CameraID: "cam-1", Limit: 1, Cursor: next})
	if err != nil || len(items2) != 1 || next2 != "" || items2[0].Type != "lpr" {
		t.Fatalf("page 2 unexpected: items=%+v next=%q err=%v", items2, next2, err)
	}

	// Confirmed capability must not be downgraded to unknown by a subsequent probe
	_ = store.SaveCapabilities(ctx, "cam-1", []onvif.Capability{
		{Capability: "analytics.lpr", Status: onvif.CapConfirmed, Evidence: `{"op":"push"}`, CheckedAt: t0},
	})
	_ = store.SaveCapabilities(ctx, "cam-1", []onvif.Capability{
		{Capability: "analytics.lpr", Status: onvif.CapUnknown, Evidence: `{"op":"probe"}`, CheckedAt: t0.Add(time.Minute)},
	})
	caps, err := store.ListCapabilities(ctx, "cam-1")
	if err != nil || len(caps) != 1 || caps[0].Status != onvif.CapConfirmed {
		t.Fatalf("expected confirmed capability preserved, got %+v err=%v", caps, err)
	}
}

func TestProvisionHandlerXMLAndLineCrossing(t *testing.T) {
	store, reg := openTestEnv(t)
	h := NewProvisionHandler(store, reg, slog.New(slog.NewTextHandler(io.Discard, nil)))

	lprXML := []byte(`<?xml version="1.0" encoding="utf-8"?>
<config version="1.0" xmlns="http://www.tvt.net.cn/schemas/config.xsd">
  <deviceName>Cochera</deviceName>
  <snapTime>1794571200000000</snapTime>
  <items><item>
    <plateNumber>AF 123 BC</plateNumber>
    <vehiceColor count="1"/>
    <PlateConfidence count="96"/>
  </item></items>
</config>`)
	req := httptest.NewRequest(http.MethodPost, "/v1/provision-lpr/event", bytes.NewReader(lprXML))
	req.Header.Set("Content-Type", "application/xml")
	req.RemoteAddr = "192.0.2.225:44123"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	lcXML := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<config version="1.7" xmlns="http://www.ipc.com/ver10">
  <alarmStatusInfo><lineCrossAlarm type="boolean" id="1">true</lineCrossAlarm></alarmStatusInfo>
</config>`)
	req2 := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(lcXML))
	req2.Header.Set("Content-Type", "application/xml")
	req2.RemoteAddr = "192.0.2.225:44124"
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec2.Code)
	}

	items, _, err := store.List(context.Background(), Query{CameraID: "cam-1"})
	if err != nil || len(items) != 2 {
		t.Fatalf("expected 2 stored events, got %+v err=%v", items, err)
	}
	caps, _ := store.ListCapabilities(context.Background(), "cam-1")
	if len(caps) != 2 {
		t.Fatalf("expected 2 confirmed analytics capabilities, got %+v", caps)
	}
}

type mockRT func(*http.Request) (*http.Response, error)

func (f mockRT) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestONVIFPullerRecordsEventAndConfirmsCapability(t *testing.T) {
	store, reg := openTestEnv(t)
	puller := NewONVIFPuller(store, reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	calls := 0
	puller.Transport = mockRT(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		s := string(b)
		wrap := func(inner string) *http.Response {
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"application/soap+xml"}},
				Body:       io.NopCloser(strings.NewReader(`<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Body>` + inner + `</s:Body></s:Envelope>`)),
			}
		}
		switch {
		case strings.Contains(s, "GetSystemDateAndTime"):
			return wrap(`<GetSystemDateAndTimeResponse/>`), nil
		case strings.Contains(s, "GetServices"):
			return wrap(`<GetServicesResponse><Service><Namespace>http://www.onvif.org/ver10/events/wsdl</Namespace><XAddr>http://192.0.2.225/onvif/events</XAddr></Service></GetServicesResponse>`), nil
		case strings.Contains(s, "CreatePullPointSubscription"):
			return wrap(`<CreatePullPointSubscriptionResponse><SubscriptionReference><Address>http://192.0.2.225/onvif/sub1</Address></SubscriptionReference></CreatePullPointSubscriptionResponse>`), nil
		case strings.Contains(s, "PullMessages"):
			calls++
			if calls == 1 {
				return wrap(`<PullMessagesResponse><NotificationMessage>
					<Topic>tns1:RuleEngine/LineDetector/Crossed</Topic>
					<Message UtcTime="2026-11-13T15:30:00Z"><Source><SimpleItem Name="RuleName" Value="Porton"/></Source><Data><SimpleItem Name="ObjectId" Value="7"/></Data></Message>
				</NotificationMessage></PullMessagesResponse>`), nil
			}
			time.Sleep(50 * time.Millisecond)
			return wrap(`<PullMessagesResponse/>`), nil
		}
		return wrap(`<Fault/>`), nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	puller.Start(ctx)

	items, _, err := store.List(context.Background(), Query{CameraID: "cam-1"})
	if err != nil || len(items) != 1 || items[0].Type != "line_crossing" || items[0].Zone != "Porton" {
		t.Fatalf("expected 1 line_crossing event, got %+v err=%v", items, err)
	}
	caps, _ := store.ListCapabilities(context.Background(), "cam-1")
	foundPull := false
	foundLC := false
	for _, c := range caps {
		if c.Capability == "onvif.events.pullpoint" && c.Status == onvif.CapConfirmed {
			foundPull = true
		}
		if c.Capability == "analytics.line_crossing" && c.Status == onvif.CapConfirmed {
			foundLC = true
		}
	}
	if !foundPull || !foundLC {
		t.Fatalf("expected confirmed pullpoint and line_crossing caps, got %+v", caps)
	}
}

func TestProvisionPersonVehicleBBoxAndMultipartSnapshot(t *testing.T) {
	store, reg := openTestEnv(t)
	snapDir := t.TempDir()
	snap := &Snapshotter{DB: store.db, Dir: snapDir}
	store.OnInsert = func(ev Event) {
		if len(ev.SnapshotData) == 0 {
			return
		}
		if _, err := snap.Capture(context.Background(), ev); err != nil {
			t.Errorf("snap.Capture: %v", err)
		}
	}
	h := NewProvisionHandler(store, reg, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// 1. Multipart XML with child-element targetRect + humanAlarm + attached JPEG.
	raw := makeSolidJPEG(t, 640, 360, color.RGBA{30, 32, 36, 255})
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)

	xmlHdr := make(textproto.MIMEHeader)
	xmlHdr.Set("Content-Type", "application/xml")
	xp, _ := mw.CreatePart(xmlHdr)
	_, _ = xp.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<config version="1.7" xmlns="http://www.ipc.com/ver10">
  <picWidth>1280</picWidth>
  <picHeight>720</picHeight>
  <alarmStatusInfo><humanAlarm>true</humanAlarm></alarmStatusInfo>
  <targetRect><x>320</x><y>144</y><width>256</width><height>432</height></targetRect>
  <confidence>94</confidence>
</config>`))

	imgHdr := make(textproto.MIMEHeader)
	imgHdr.Set("Content-Type", "image/jpeg")
	ip, _ := mw.CreatePart(imgHdr)
	_, _ = ip.Write(raw)
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/events/provision", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.RemoteAddr = "192.0.2.225:55111"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	items, _, err := store.List(context.Background(), Query{CameraID: "cam-1", Type: "person"})
	if err != nil || len(items) != 1 {
		t.Fatalf("expected 1 person event, got %+v err=%v", items, err)
	}
	ev := items[0]
	if len(ev.BBox) != 4 || ev.BBox[0] != 0.25 || ev.BBox[1] != 0.20 || ev.BBox[2] != 0.20 || ev.BBox[3] != 0.60 {
		t.Fatalf("unexpected normalized bbox: %+v", ev.BBox)
	}
	if ev.Snapshot == "" {
		t.Fatalf("expected snapshot_ref to be saved")
	}
	savedBytes, err := os.ReadFile(filepath.Join(snapDir, filepath.FromSlash(ev.Snapshot)))
	if err != nil {
		t.Fatalf("read saved snapshot: %v", err)
	}
	img, _, err := image.Decode(bytes.NewReader(savedBytes))
	if err != nil {
		t.Fatalf("decode saved snapshot: %v", err)
	}
	// Verify blue person box at top border (x=200, y=72 -> 0.20*360).
	r, _, b, _ := img.At(200, 72).RGBA()
	if (b>>8) < 180 || (b>>8) <= (r>>8)+60 {
		t.Fatalf("expected blue person box in saved snapshot at (200,72), got r=%d b=%d", r>>8, b>>8)
	}

	// 2. JSON vehicle detection with bbox array.
	jsonPayload := `{"event_type":"vehicle","target_type":"car","confidence":0.87,"bbox":[0.1,0.3,0.4,0.4]}`
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/events/provision", strings.NewReader(jsonPayload))
	req2.Header.Set("Content-Type", "application/json")
	req2.RemoteAddr = "192.0.2.225:55112"
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 for json vehicle, got %d", rec2.Code)
	}
	vehs, _, err := store.List(context.Background(), Query{CameraID: "cam-1", Type: "vehicle"})
	if err != nil || len(vehs) != 1 || len(vehs[0].BBox) != 4 || vehs[0].Class != "car" {
		t.Fatalf("unexpected vehicle event: %+v err=%v", vehs, err)
	}
}

func TestProvisionPerimeterXMLAndTargetPromotion(t *testing.T) {
	store, reg := openTestEnv(t)
	h := NewProvisionHandler(store, reg, slog.New(slog.NewTextHandler(io.Discard, nil)))

	perimXML := append([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<config version="1.7" xmlns="http://www.ipc.com/ver10">
  <alarmStatusInfo><perimeterAlarm type="boolean" id="1">true</perimeterAlarm></alarmStatusInfo>
  <targetType>person</targetType>
</config>`), 0, 0)
	req := httptest.NewRequest(http.MethodPost, "/SendAlarmData", bytes.NewReader(perimXML))
	req.RemoteAddr = "192.0.2.225:55113"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for perimeter xml, got %d", rec.Code)
	}
	persons, _, err := store.List(context.Background(), Query{CameraID: "cam-1", Type: "person"})
	if err != nil || len(persons) != 1 || persons[0].Class != "person" || persons[0].Zone != "intrusion" {
		t.Fatalf("expected 1 person event promoted from perimeter xml, got %+v err=%v", persons, err)
	}

	sceneXML := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<config version="1.7" xmlns="http://www.ipc.com/ver10">
  <alarmStatusInfo><sceneChange type="boolean" id="1">true</sceneChange></alarmStatusInfo>
</config>`)
	req2 := httptest.NewRequest(http.MethodPost, "/SendAlarmData", bytes.NewReader(sceneXML))
	req2.RemoteAddr = "192.0.2.225:55114"
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 for sceneChange xml, got %d", rec2.Code)
	}
	tampers, _, err := store.List(context.Background(), Query{CameraID: "cam-1", Type: "tamper"})
	if err != nil || len(tampers) != 1 {
		t.Fatalf("expected 1 tamper event from sceneChange xml, got %+v err=%v", tampers, err)
	}
}
