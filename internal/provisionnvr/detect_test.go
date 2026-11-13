package provisionnvr_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jdolan-exalink/openvms-rec/internal/provisionnvr"
	"github.com/jdolan-exalink/openvms-rec/internal/provisionnvr/nvmstest"
)

const onvifOn = `<?xml version="1.0" encoding="UTF-8"?><response><status>success</status><content><enable>true</enable><switch>true</switch></content></response>`

func detectInput(srv *nvmstest.Server, probe func(context.Context, provisionnvr.DetectInput) (*provisionnvr.OnvifFacts, error)) provisionnvr.DetectInput {
	return provisionnvr.DetectInput{BaseURL: srv.URL, Username: testUser, Password: testPass, ProbeONVIF: probe,
		// No test dials a real RTSP port; the ones that care about RTSP replace this.
		ProbeRTSP: func(context.Context, string, int, string, string, int, provisionnvr.Stream) (*provisionnvr.RTSPFacts, error) {
			return nil, errors.New("rtsp is not available in this test")
		}}
}

func capOf(t *testing.T, d *provisionnvr.Detection, name string) provisionnvr.Capability {
	t.Helper()
	for _, c := range d.Capabilities {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("capability %q missing from %+v", name, d.Capabilities)
	return provisionnvr.Capability{}
}

func wantCap(t *testing.T, d *provisionnvr.Detection, name string, state provisionnvr.CapState) provisionnvr.Capability {
	t.Helper()
	c := capOf(t, d, name)
	if c.State != state {
		t.Fatalf("capability %s = %s (%s), want %s", name, c.State, c.Detail, state)
	}
	if c.Source == "" {
		t.Fatalf("capability %s has no source", name)
	}
	return c
}

func TestDetectWithOnvifSwitchedOff(t *testing.T) {
	srv := nvmstest.New(t, testUser, testPass)
	probed := false
	d, err := provisionnvr.Detect(context.Background(), detectInput(srv, func(context.Context, provisionnvr.DetectInput) (*provisionnvr.OnvifFacts, error) {
		probed = true
		return nil, errors.New("must not be called")
	}))
	if err != nil {
		t.Fatal(err)
	}
	if probed {
		t.Fatal("ONVIF was probed although the recorder reports it switched off")
	}
	if d.Device.Model != "NVR8-16400AN(1U)" || len(d.Channels) != 7 || d.Channels[0].Status == nil || len(d.Storage.Disks) != 1 {
		t.Fatalf("detection = %+v", d)
	}
	wantCap(t, d, provisionnvr.CapWebAPI, provisionnvr.CapConfirmed)
	if c := wantCap(t, d, provisionnvr.CapChannels, provisionnvr.CapConfirmed); c.Detail != "7 channels" {
		t.Fatalf("channels detail = %q", c.Detail)
	}
	wantCap(t, d, provisionnvr.CapChannelStatus, provisionnvr.CapConfirmed)
	wantCap(t, d, provisionnvr.CapStorageHealth, provisionnvr.CapConfirmed)
	wantCap(t, d, provisionnvr.CapONVIF, provisionnvr.CapUnsupported)
	wantCap(t, d, provisionnvr.CapRTSP, provisionnvr.CapUnknown)
	wantCap(t, d, provisionnvr.CapPTZ, provisionnvr.CapUnknown)
	wantCap(t, d, provisionnvr.CapPlayback, provisionnvr.CapUnknown)
	wantCap(t, d, provisionnvr.CapEvents, provisionnvr.CapUnknown)
	wantCap(t, d, provisionnvr.CapNativeAPI, provisionnvr.CapUnsupported)
	wantCap(t, d, provisionnvr.CapPlateMatch, provisionnvr.CapUnknown)
	wantCap(t, d, provisionnvr.CapFaceMatch, provisionnvr.CapUnsupported)
	for _, c := range d.Capabilities {
		if c.State == provisionnvr.CapConfirmed && !slices.Contains([]string{provisionnvr.CapWebAPI, provisionnvr.CapChannels, provisionnvr.CapChannelStatus, provisionnvr.CapStorageHealth}, c.Name) {
			t.Fatalf("%s is confirmed without having been exercised", c.Name)
		}
	}
}

func TestDetectWithOnvifAnswering(t *testing.T) {
	srv := nvmstest.New(t, testUser, testPass)
	srv.Override["queryOnvifCfg"] = onvifOn
	d, err := provisionnvr.Detect(context.Background(), detectInput(srv, func(context.Context, provisionnvr.DetectInput) (*provisionnvr.OnvifFacts, error) {
		return &provisionnvr.OnvifFacts{Services: []string{"device", "events", "media", "ptz", "replay"}, Profiles: 14, StreamURIsOK: true,
			SampleStreamPath: "/chID=10&streamType=main&linkType=tcp"}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c := wantCap(t, d, provisionnvr.CapONVIF, provisionnvr.CapConfirmed); c.Detail != "14 media profiles" {
		t.Fatalf("onvif detail = %q", c.Detail)
	}
	if c := wantCap(t, d, provisionnvr.CapRTSP, provisionnvr.CapUnknown); !strings.Contains(c.Detail, "/chID=10&streamType=main&linkType=tcp") {
		t.Fatalf("rtsp detail = %q: the reported URL path must be shown, but playback was never exercised", c.Detail)
	}
	wantCap(t, d, provisionnvr.CapPTZ, provisionnvr.CapUnsupported) // PTZ service, but no profile has a PTZ configuration
	wantCap(t, d, provisionnvr.CapPlayback, provisionnvr.CapUnknown)
	wantCap(t, d, provisionnvr.CapEvents, provisionnvr.CapUnknown)
	wantCap(t, d, provisionnvr.CapAnalytics, provisionnvr.CapUnsupported) // not advertised in this probe
}

func TestDetectWhenOnvifSwitchIsOnButItDoesNotAnswer(t *testing.T) {
	srv := nvmstest.New(t, testUser, testPass)
	srv.Override["queryOnvifCfg"] = onvifOn
	d, err := provisionnvr.Detect(context.Background(), detectInput(srv, func(context.Context, provisionnvr.DetectInput) (*provisionnvr.OnvifFacts, error) {
		return nil, errors.New("connection closed")
	}))
	if err != nil {
		t.Fatalf("an unreachable ONVIF must not fail the detection: %v", err)
	}
	wantCap(t, d, provisionnvr.CapONVIF, provisionnvr.CapUnknown)
	wantCap(t, d, provisionnvr.CapRTSP, provisionnvr.CapUnknown)
}

func TestDetectKeepsGoingWhenAnAdvancedReadFails(t *testing.T) {
	srv := nvmstest.New(t, testUser, testPass)
	srv.Override["queryStorageDevInfo"] = `<response><status>fail</status><errorCode>536870943</errorCode></response>`
	srv.Override["queryChlStatus"] = `<response><status>fail</status><errorCode>536870943</errorCode></response>`
	d, err := provisionnvr.Detect(context.Background(), detectInput(srv, nil))
	if err != nil {
		t.Fatalf("a failed advanced read must not block the integration: %v", err)
	}
	wantCap(t, d, provisionnvr.CapStorageHealth, provisionnvr.CapUnknown)
	wantCap(t, d, provisionnvr.CapChannelStatus, provisionnvr.CapUnknown)
	wantCap(t, d, provisionnvr.CapChannels, provisionnvr.CapConfirmed)
	if len(d.Channels) != 7 || d.Channels[0].Status != nil {
		t.Fatalf("channels must still be listed, without status: %+v", d.Channels)
	}
}

func TestDetectFailsWhereItMust(t *testing.T) {
	t.Run("wrong password", func(t *testing.T) {
		srv := nvmstest.New(t, testUser, testPass)
		in := detectInput(srv, nil)
		in.Password = "nope"
		_, err := provisionnvr.Detect(context.Background(), in)
		mustClass(t, err, provisionnvr.ClassAuthFailed)
		if srv.FailedLogins() != 1 {
			t.Fatalf("failed logins = %d, want 1", srv.FailedLogins())
		}
	})
	t.Run("not activated", func(t *testing.T) {
		srv := nvmstest.New(t, testUser, testPass)
		srv.Override["queryActivationStatus"] = `<response><status>success</status><content><activated>false</activated></content></response>`
		_, err := provisionnvr.Detect(context.Background(), detectInput(srv, nil))
		mustClass(t, err, provisionnvr.ClassUnsupported)
		if !errors.Is(err, provisionnvr.ErrNotActivated) {
			t.Fatalf("error = %v, want ErrNotActivated", err)
		}
		if srv.Logins()+srv.FailedLogins() != 0 {
			t.Fatal("an unactivated recorder must not receive a login")
		}
	})
	t.Run("not an NVMS device", func(t *testing.T) {
		srv := nvmstest.New(t, testUser, testPass)
		srv.Override["queryActivationStatus"] = "<html>hello</html>"
		_, err := provisionnvr.Detect(context.Background(), detectInput(srv, nil))
		mustClass(t, err, provisionnvr.ClassUnsupported)
		if srv.Logins()+srv.FailedLogins() != 0 {
			t.Fatal("a device that is not NVMS must not receive credentials")
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		srv := nvmstest.New(t, testUser, testPass)
		in := detectInput(srv, nil)
		srv.Close()
		_, err := provisionnvr.Detect(context.Background(), in)
		mustClass(t, err, provisionnvr.ClassOffline)
	})
}

func TestDetectIsReadOnlyAndKeepsSecretsOut(t *testing.T) {
	srv := nvmstest.New(t, testUser, testPass)
	d, err := provisionnvr.Detect(context.Background(), detectInput(srv, nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, cmd := range srv.Calls() {
		if cmd != "reqLogin" && cmd != "doLogin" && cmd != "doLogout" && !strings.HasPrefix(cmd, "query") {
			t.Fatalf("Detect sent %q, which is not a login or a query", cmd)
		}
	}
	if srv.Logins() != 1 {
		t.Fatalf("logins = %d, want 1", srv.Logins())
	}
	if calls := srv.Calls(); calls[len(calls)-1] != "doLogout" {
		t.Fatalf("the session must be closed, calls = %v", calls)
	}
	dump := fmt.Sprintf("%+v", d)
	for _, secret := range []string{testPass, "SERIAL", "DIGEST", "test-session-token", "DEADBEEF"} {
		if strings.Contains(dump, secret) {
			t.Fatalf("detection carries %q", secret)
		}
	}
}

func TestDetectConfirmsRTSPWhenTheRecorderAnswersDescribe(t *testing.T) {
	srv := nvmstest.New(t, testUser, testPass)
	in := detectInput(srv, nil)
	var asked struct {
		host    string
		port    int
		channel int
		stream  provisionnvr.Stream
	}
	in.ProbeRTSP = func(_ context.Context, host string, port int, user, pass string, channel int, stream provisionnvr.Stream) (*provisionnvr.RTSPFacts, error) {
		asked.host, asked.port, asked.channel, asked.stream = host, port, channel, stream
		if user != testUser || pass != testPass {
			t.Fatalf("RTSP probed with %q/%q, want the recorder's account", user, pass)
		}
		return &provisionnvr.RTSPFacts{Channel: channel, Stream: stream, Path: provisionnvr.RTSPPath(channel, stream), Codec: "H264"}, nil
	}
	d, err := provisionnvr.Detect(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	// The first channel of the fixture is {00000001-...}, which is channel 1; the port is the one the recorder reports.
	if asked.channel != 1 || asked.stream != provisionnvr.StreamMain || asked.port != 554 || asked.host != "127.0.0.1" {
		t.Fatalf("probe asked %+v", asked)
	}
	c := wantCap(t, d, provisionnvr.CapRTSP, provisionnvr.CapConfirmed)
	if !strings.Contains(c.Detail, "H264") || !strings.Contains(c.Detail, "not wired yet") {
		t.Fatalf("rtsp detail = %q", c.Detail)
	}
	if strings.Contains(fmt.Sprintf("%+v", d), testPass) {
		t.Fatal("the detection carries the password")
	}
}

func TestDetectDoesNotConfirmRTSPWhenTheCheckFails(t *testing.T) {
	srv := nvmstest.New(t, testUser, testPass)
	in := detectInput(srv, nil) // the stub fails
	d, err := provisionnvr.Detect(context.Background(), in)
	if err != nil {
		t.Fatalf("a failed RTSP check must not fail the detection: %v", err)
	}
	wantCap(t, d, provisionnvr.CapRTSP, provisionnvr.CapUnknown)
}

func TestDetectSkipsRTSPWhenTheServerIsOff(t *testing.T) {
	srv := nvmstest.New(t, testUser, testPass)
	srv.Override["queryRTSPServer"] = `<response><status>success</status><content><rtspServerSwitch>false</rtspServerSwitch><rtspPort>554</rtspPort></content></response>`
	in := detectInput(srv, nil)
	in.ProbeRTSP = func(context.Context, string, int, string, string, int, provisionnvr.Stream) (*provisionnvr.RTSPFacts, error) {
		t.Fatal("RTSP was probed although the recorder reports its RTSP server off")
		return nil, nil
	}
	d, err := provisionnvr.Detect(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	wantCap(t, d, provisionnvr.CapRTSP, provisionnvr.CapUnsupported)
}
