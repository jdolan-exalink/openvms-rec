package provisionnvr

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeRTSP answers DESCRIBE like the recorder does: a Digest challenge first, then the SDP once the
// response is right. It counts the requests it saw.
type fakeRTSP struct {
	ln       net.Listener
	requests atomic.Int32
	realm    string
	user     string
	password string
	sdp      string
	// mode: "digest" (default), "none" (no auth), "garbage" (not RTSP), "forbidden" (403 after auth)
	mode string
}

func newFakeRTSP(t *testing.T, mode string) *fakeRTSP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeRTSP{ln: ln, realm: "TVT", user: "admin", password: "pw-ok", mode: mode,
		sdp: "v=0\r\nm=audio 0 RTP/AVP 8\r\na=rtpmap:8 PCMA/8000\r\nm=video 0 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\n"}
	t.Cleanup(func() { _ = ln.Close() })
	go f.serve()
	return f
}

func (f *fakeRTSP) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeRTSP) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(c)
	}
}

func (f *fakeRTSP) handle(c net.Conn) {
	defer func() { _ = c.Close() }()
	rd := bufio.NewReader(c)
	for {
		var lines []string
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				break
			}
			lines = append(lines, line)
		}
		f.requests.Add(1)
		if f.mode == "garbage" {
			_, _ = c.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
			return
		}
		method, uri := "", ""
		if parts := strings.Fields(lines[0]); len(parts) >= 2 {
			method, uri = parts[0], parts[1]
		}
		auth := ""
		for _, l := range lines {
			if strings.HasPrefix(strings.ToLower(l), "authorization:") {
				auth = strings.TrimSpace(l[len("authorization:"):])
			}
		}
		if f.mode != "none" && !f.accepts(method, uri, auth) {
			_, _ = fmt.Fprintf(c, "RTSP/1.0 401 Unauthorized\r\nCSeq: 1\r\nWWW-Authenticate: Digest realm=\"%s\", nonce=\"abc123\"\r\n\r\n", f.realm)
			continue
		}
		if f.mode == "forbidden" {
			_, _ = c.Write([]byte("RTSP/1.0 403 Forbidden\r\nCSeq: 2\r\n\r\n"))
			return
		}
		_, _ = fmt.Fprintf(c, "RTSP/1.0 200 OK\r\nCSeq: 2\r\nContent-Type: application/sdp\r\nContent-Length: %s\r\n\r\n%s", strconv.Itoa(len(f.sdp)), f.sdp)
	}
}

func (f *fakeRTSP) accepts(method, uri, auth string) bool {
	if !strings.HasPrefix(auth, "Digest ") {
		return false
	}
	p := parseChallenge(strings.TrimPrefix(auth, "Digest "))
	ha1 := md5hex(f.user + ":" + f.realm + ":" + f.password)
	want := md5hex(ha1 + ":abc123:" + md5hex(method+":"+uri))
	return p["username"] == f.user && p["response"] == want
}

func TestProbeRTSPAnswersTheDigestChallengeAndReadsTheCodec(t *testing.T) {
	f := newFakeRTSP(t, "digest")
	facts, err := ProbeRTSP(context.Background(), "127.0.0.1", f.port(), "admin", "pw-ok", 10, StreamMain)
	if err != nil {
		t.Fatal(err)
	}
	if facts.Codec != "H264" || facts.Channel != 10 || facts.Path != "/chID=10&streamType=main&linkType=tcp" {
		t.Fatalf("facts = %+v", facts)
	}
	if got := f.requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2 (challenge, then the answer)", got)
	}
}

func TestProbeRTSPNeverRetriesARejectedPasswordAndDoesNotEchoIt(t *testing.T) {
	f := newFakeRTSP(t, "digest")
	_, err := ProbeRTSP(context.Background(), "127.0.0.1", f.port(), "admin", "wrong-pw", 1, StreamSub)
	if !errors.Is(err, ErrRTSPAuth) {
		t.Fatalf("error = %v, want ErrRTSPAuth", err)
	}
	if class, _ := ClassOf(err); class != ClassAuthFailed {
		t.Fatalf("class = %q, want auth_failed", class)
	}
	if got := f.requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want exactly 2: one challenge and one attempt", got)
	}
	if strings.Contains(err.Error(), "wrong-pw") {
		t.Fatalf("the error echoes the password: %v", err)
	}
}

func TestProbeRTSPWithoutAuthAndWithOtherAnswers(t *testing.T) {
	open := newFakeRTSP(t, "none")
	if facts, err := ProbeRTSP(context.Background(), "127.0.0.1", open.port(), "x", "y", 1, StreamMain); err != nil || facts.Codec != "H264" {
		t.Fatalf("no auth: %+v, %v", facts, err)
	}
	if got := open.requests.Load(); got != 1 {
		t.Fatalf("no auth: requests = %d, want 1", got)
	}
	garbage := newFakeRTSP(t, "garbage")
	_, err := ProbeRTSP(context.Background(), "127.0.0.1", garbage.port(), "x", "y", 1, StreamMain)
	if class, ok := ClassOf(err); !ok || class != ClassUnsupported {
		t.Fatalf("a non-RTSP answer = %v, want unsupported", err)
	}
	forbidden := newFakeRTSP(t, "forbidden")
	_, err = ProbeRTSP(context.Background(), "127.0.0.1", forbidden.port(), "admin", "pw-ok", 1, StreamMain)
	if class, ok := ClassOf(err); !ok || class != ClassDegraded {
		t.Fatalf("a 403 = %v, want degraded", err)
	}
}

func TestProbeRTSPOnAClosedPortIsOffline(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	_, err := ProbeRTSP(context.Background(), "127.0.0.1", port, "a", "b", 1, StreamMain)
	if class, ok := ClassOf(err); !ok || class != ClassOffline {
		t.Fatalf("closed port = %v, want offline", err)
	}
}

func TestChannelNumberReadsTheHexFirstField(t *testing.T) {
	for id, want := range map[string]int{
		"{00000001-0000-0000-0000-000000000000}": 1,
		"00000007-0000-0000-0000-000000000000":   7,
		"0000000A-0000-0000-0000-000000000000":   10,
		"0000000c-0000-0000-0000-000000000000":   12,
		"{00000010-0000-0000-0000-000000000000}": 16,
	} {
		if got, ok := ChannelNumber(id); !ok || got != want {
			t.Fatalf("ChannelNumber(%q) = %d, %v; want %d", id, got, ok, want)
		}
	}
	for _, bad := range []string{"", "not-a-guid", "00000000-0000-0000-0000-000000000000", "123456789-1-1-1-1", "zzzzzzzz-0000"} {
		if got, ok := ChannelNumber(bad); ok {
			t.Fatalf("ChannelNumber(%q) = %d, want not ok", bad, got)
		}
	}
}

func TestRTSPPathAndSDPCodec(t *testing.T) {
	if got := RTSPPath(11, StreamSub); got != "/chID=11&streamType=sub&linkType=tcp" {
		t.Fatalf("RTSPPath = %q", got)
	}
	for sdp, want := range map[string]string{
		"m=video 0 RTP/AVP 96\r\na=rtpmap:96 H265/90000\r\n":                                            "H265",
		"m=audio 0 RTP/AVP 8\r\na=rtpmap:8 PCMA/8000\r\n":                                               "",
		"m=audio 0 RTP/AVP 8\r\na=rtpmap:8 PCMA/8000\r\nm=video 0 RTP/AVP 96\r\na=rtpmap:96 h264/90000": "H264",
		"": "",
	} {
		if got := sdpVideoCodec(sdp); got != want {
			t.Fatalf("sdpVideoCodec(%q) = %q, want %q", sdp, got, want)
		}
	}
}
