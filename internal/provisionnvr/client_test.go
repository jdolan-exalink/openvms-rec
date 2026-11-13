package provisionnvr_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jdolan-exalink/openvms-rec/internal/provisionnvr"
	"github.com/jdolan-exalink/openvms-rec/internal/provisionnvr/nvmstest"
)

const (
	testUser = "admin"
	testPass = "s3cret-Pass"
)

func newClient(t *testing.T, srv *nvmstest.Server, user, pass string) *provisionnvr.Client {
	t.Helper()
	c, err := provisionnvr.NewClient(srv.URL, provisionnvr.NewCredentials(user, pass), provisionnvr.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func mustClass(t *testing.T, err error, want provisionnvr.Class) {
	t.Helper()
	got, ok := provisionnvr.ClassOf(err)
	if !ok || got != want {
		t.Fatalf("error class = %q (ok=%v), want %q; err=%v", got, ok, want, err)
	}
}

func TestChannelsAreReadAfterLoginWithoutBraces(t *testing.T) {
	srv := nvmstest.New(t, testUser, testPass)
	chs, err := newClient(t, srv, testUser, testPass).Channels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(chs) != 7 {
		t.Fatalf("channels = %d, want 7", len(chs))
	}
	for _, c := range chs {
		if strings.ContainsAny(c.ID, "{}") || c.ID == "" {
			t.Fatalf("channel id %q must be a GUID without braces", c.ID)
		}
		if !strings.HasPrefix(c.Address, "192.0.2.") || c.Model != "DI-320IPSN-VF-V2" || c.Protocol != "TVT_IPCAMERA" {
			t.Fatalf("unexpected channel %+v", c)
		}
	}
	if srv.Logins() != 1 {
		t.Fatalf("logins = %d, want 1", srv.Logins())
	}
}

func TestActivationNeedsNoLogin(t *testing.T) {
	srv := nvmstest.New(t, testUser, testPass)
	ok, err := newClient(t, srv, testUser, "wrong").Activated(context.Background())
	if err != nil || !ok {
		t.Fatalf("Activated = %v, %v; want true", ok, err)
	}
	if got := srv.Calls(); len(got) != 1 || got[0] != "queryActivationStatus" {
		t.Fatalf("calls = %v, want only queryActivationStatus", got)
	}
}

func TestWrongPasswordIsNotRetriedAndNeverEchoed(t *testing.T) {
	srv := nvmstest.New(t, testUser, testPass)
	c := newClient(t, srv, testUser, "not-the-password")
	_, err := c.Channels(context.Background())
	mustClass(t, err, provisionnvr.ClassAuthFailed)
	for range 3 {
		_, again := c.DeviceInfo(context.Background())
		mustClass(t, again, provisionnvr.ClassAuthFailed)
	}
	if srv.FailedLogins() != 1 {
		t.Fatalf("failed logins = %d, want 1: a rejected password must not be sent again", srv.FailedLogins())
	}
	if strings.Contains(err.Error(), "not-the-password") || strings.Contains(err.Error(), testPass) {
		t.Fatalf("error leaks the password: %v", err)
	}
}

func TestWrongUserIsAuthFailed(t *testing.T) {
	srv := nvmstest.New(t, testUser, testPass)
	_, err := newClient(t, srv, "nobody", testPass).Channels(context.Background())
	mustClass(t, err, provisionnvr.ClassAuthFailed)
}

func TestLockedAccountMapsToAuthFailedState(t *testing.T) {
	srv := nvmstest.New(t, testUser, testPass)
	srv.LockAfter = 1
	_, _ = newClient(t, srv, testUser, "wrong").Channels(context.Background())
	_, err := newClient(t, srv, testUser, testPass).Channels(context.Background())
	mustClass(t, err, provisionnvr.ClassLocked)
	if got, _ := provisionnvr.ClassOf(err); got.State() != "auth_failed" {
		t.Fatalf("locked state = %q, want auth_failed", got.State())
	}
}

func TestExpiredSessionLogsInOnce(t *testing.T) {
	srv := nvmstest.New(t, testUser, testPass)
	c := newClient(t, srv, testUser, testPass)
	if _, err := c.DeviceInfo(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv.ExpireSession()
	if _, err := c.Channels(context.Background()); err != nil {
		t.Fatalf("a timed-out session must be renewed: %v", err)
	}
	if srv.Logins() != 2 {
		t.Fatalf("logins = %d, want 2", srv.Logins())
	}
}

func TestOnlyQueryCommandsAreSent(t *testing.T) {
	srv := nvmstest.New(t, testUser, testPass)
	c := newClient(t, srv, testUser, testPass)
	for _, cmd := range []string{"editBasicCfg", "doReboot", "delChl", "queryBasicCfg/../editX", "queryA&b=1", ""} {
		if _, err := c.Query(context.Background(), cmd, ""); !errors.Is(err, provisionnvr.ErrNotReadOnly) {
			t.Fatalf("Query(%q) error = %v, want ErrNotReadOnly", cmd, err)
		}
	}
	if _, err := c.QueryUnauthenticated(context.Background(), "queryBasicCfg"); !errors.Is(err, provisionnvr.ErrNotReadOnly) {
		t.Fatalf("QueryUnauthenticated of a command that needs a session = %v", err)
	}
	if got := srv.Calls(); len(got) != 0 {
		t.Fatalf("a refused command reached the recorder: %v", got)
	}
}

func TestResponseWithEntityDeclarationIsInert(t *testing.T) {
	srv := nvmstest.New(t, testUser, testPass)
	srv.Override["queryBasicCfg"] = `<?xml version="1.0"?><!DOCTYPE r [<!ENTITY x SYSTEM "file:///etc/passwd">]>` +
		`<response><status>success</status><content><name>&x;</name></content></response>`
	info, err := newClient(t, srv, testUser, testPass).DeviceInfo(context.Background())
	if err == nil {
		t.Fatalf("an undefined entity must fail the parse, got %+v", info)
	}
	mustClass(t, err, provisionnvr.ClassUnsupported)
	if strings.Contains(info.Name, "root:") || strings.Contains(err.Error(), "root:") {
		t.Fatal("external entity content was read")
	}
}

func TestOversizedAndForeignAnswersAreUnsupported(t *testing.T) {
	srv := nvmstest.New(t, testUser, testPass)
	c := newClient(t, srv, testUser, testPass)
	srv.Override["queryBasicCfg"] = "<response><status>success</status><content><name>" + strings.Repeat("a", 2<<20) + "</name></content></response>"
	_, err := c.DeviceInfo(context.Background())
	mustClass(t, err, provisionnvr.ClassUnsupported)
	srv.Override["querySystemCaps"] = "<html><body>not an NVR</body></html>"
	_, err = c.SystemCaps(context.Background())
	mustClass(t, err, provisionnvr.ClassUnsupported)
}

func TestUnreachableRecorderIsOffline(t *testing.T) {
	srv := nvmstest.New(t, testUser, testPass)
	c := newClient(t, srv, testUser, testPass)
	srv.Close()
	_, err := c.Channels(context.Background())
	mustClass(t, err, provisionnvr.ClassOffline)
	if strings.Contains(err.Error(), srv.URL) {
		t.Fatalf("transport error repeats the request URL: %v", err)
	}
}

func TestHTTPFailureIsUnsupported(t *testing.T) {
	srv := nvmstest.New(t, testUser, testPass)
	srv.Offline = true
	_, err := newClient(t, srv, testUser, testPass).Activated(context.Background())
	mustClass(t, err, provisionnvr.ClassUnsupported)
}

func TestTypedReadsMatchTheRecordedUnit(t *testing.T) {
	srv := nvmstest.New(t, testUser, testPass)
	c := newClient(t, srv, testUser, testPass)
	ctx := context.Background()

	info, err := c.DeviceInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Model != "NVR8-16400AN(1U)" || info.HardwareModel != "TD-3116H2-B1" || !strings.HasPrefix(info.SoftwareVersion, "1.4.7.") || info.Name != "Test Recorder" {
		t.Fatalf("device info = %+v", info)
	}
	if strings.Contains(fmt.Sprintf("%+v", info), "SERIAL") || strings.Contains(fmt.Sprintf("%+v", info), "DIGEST") {
		t.Fatalf("device info carries an identifier that must not be kept: %+v", info)
	}

	caps, err := c.SystemCaps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if caps.IPChannelMax != 16 || !caps.PlateMatch || caps.FaceMatch || !caps.SNMP || !caps.Recorder {
		t.Fatalf("system caps = %+v", caps)
	}

	st, err := c.ChannelStatuses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 7 || !st[0].Online || st[0].Recording == "" {
		t.Fatalf("channel statuses = %+v", st)
	}

	storage, err := c.Storage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(storage.Disks) != 1 || storage.Disks[0].SizeMB != 1907729 || storage.Disks[0].Status != "read/write" || !storage.CycleRecord ||
		storage.Disks[0].RecordStart != "2026-04-06" || storage.Disks[0].Model != "WDC WD20EARZ-00C5XB0" {
		t.Fatalf("storage = %+v", storage)
	}

	svc, err := c.Services(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if svc.ONVIFSwitch || !svc.ONVIFEnabled || !svc.RTSPOn || svc.RTSPPort != 554 || svc.RTSPAuth != "Digest" || svc.RTSPAnon || svc.APIServerOn || svc.HTTPPort != 80 || svc.NetPort != 6036 {
		t.Fatalf("services = %+v", svc)
	}
}

func TestEndpointValidationAndCredentialRedaction(t *testing.T) {
	for _, bad := range []string{"", "ftp://x", "http://", "http://u:p@host", "http://host?x=1", "host:80"} {
		if _, err := provisionnvr.NewClient(bad, provisionnvr.NewCredentials("a", "b"), provisionnvr.Config{}); !errors.Is(err, provisionnvr.ErrInvalidEndpoint) {
			t.Fatalf("NewClient(%q) error = %v, want ErrInvalidEndpoint", bad, err)
		}
	}
	creds := provisionnvr.NewCredentials("admin", "hunter2")
	for _, s := range []string{fmt.Sprint(creds), fmt.Sprintf("%+v", creds), fmt.Sprintf("%#v", creds), fmt.Sprintf("%v", &creds)} {
		if strings.Contains(s, "hunter2") {
			t.Fatalf("credentials print the password: %q", s)
		}
	}
}
