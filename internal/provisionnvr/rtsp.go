package provisionnvr

import (
	"bufio"
	"context"
	"crypto/md5" //nolint:gosec // RTSP Digest authentication is defined with MD5
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// Stream is which of a channel's two streams is meant.
type Stream string

const (
	StreamMain Stream = "main"
	StreamSub  Stream = "sub"
)

// ChannelNumber is the number a channel has in the recorder's RTSP and ONVIF world. The recorder's channel
// ids carry it in their first field, in hexadecimal ({0000000A-...} is channel 10). This was checked on one
// unit, where the ids 1, 7, 8, 9, A, B, C matched the ONVIF profiles of channels 1, 7, 8, 9, 10, 11, 12;
// whether each number shows the same camera on both sides has not been compared image by image.
func ChannelNumber(id string) (int, bool) {
	first, _, _ := strings.Cut(trimID(id), "-")
	if len(first) == 0 || len(first) > 8 {
		return 0, false
	}
	n, err := strconv.ParseUint(first, 16, 32)
	if err != nil || n == 0 {
		return 0, false
	}
	return int(n), true
}

// RTSPPath is the path and query of a channel's stream on the recorder (observed through ONVIF
// GetStreamUri). It carries no host and no credentials.
func RTSPPath(channel int, stream Stream) string {
	return fmt.Sprintf("/chID=%d&streamType=%s&linkType=tcp", channel, stream)
}

// RTSPFacts is what a DESCRIBE of one stream answered.
type RTSPFacts struct {
	Channel int
	Stream  Stream
	Path    string
	// Codec is the video codec the SDP announces (H264, H265...), empty when it did not say.
	Codec string
}

// ErrRTSPAuth reports that the recorder refused the credentials for the stream.
var ErrRTSPAuth = errors.New("the recorder's RTSP server rejected the credentials")

const (
	rtspTimeout  = 6 * time.Second
	rtspMaxBytes = 64 << 10
)

// ProbeRTSP asks the recorder's RTSP server to DESCRIBE one stream and reads the codec from the SDP. It
// answers a Digest or Basic challenge once and never retries a rejection, so it cannot turn a wrong
// password into a lockout. It does not play the stream. The credentials never appear in the error.
func ProbeRTSP(ctx context.Context, host string, port int, user, password string, channel int, stream Stream) (*RTSPFacts, error) {
	path := RTSPPath(channel, stream)
	addr := net.JoinHostPort(strings.Trim(host, "[]"), strconv.Itoa(port))
	d := net.Dialer{Timeout: rtspTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, &Error{Class: ClassOffline, Command: "rtsp", Err: errors.New("cannot connect to the RTSP port")}
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(rtspTimeout))
	rd := bufio.NewReaderSize(io.LimitReader(conn, rtspMaxBytes), 8<<10)
	uri := "rtsp://" + addr + path

	status, hdr, body, err := rtspDescribe(conn, rd, uri, 1, "")
	if err != nil {
		return nil, err
	}
	if status == 401 {
		auth := authorization(hdr.Get("Www-Authenticate"), user, password, uri)
		if auth == "" {
			return nil, &Error{Class: ClassAuthFailed, Command: "rtsp", Err: ErrRTSPAuth}
		}
		if status, _, body, err = rtspDescribe(conn, rd, uri, 2, auth); err != nil {
			return nil, err
		}
		if status == 401 {
			return nil, &Error{Class: ClassAuthFailed, Command: "rtsp", Err: ErrRTSPAuth}
		}
	}
	if status != 200 {
		return nil, &Error{Class: ClassDegraded, Command: "rtsp", Err: fmt.Errorf("DESCRIBE answered %d", status)}
	}
	return &RTSPFacts{Channel: channel, Stream: stream, Path: path, Codec: sdpVideoCodec(body)}, nil
}

func rtspDescribe(w io.Writer, rd *bufio.Reader, uri string, cseq int, authorization string) (int, textproto.MIMEHeader, string, error) {
	req := fmt.Sprintf("DESCRIBE %s RTSP/1.0\r\nCSeq: %d\r\nAccept: application/sdp\r\nUser-Agent: OpenVMS\r\n", uri, cseq)
	if authorization != "" {
		req += "Authorization: " + authorization + "\r\n"
	}
	if _, err := io.WriteString(w, req+"\r\n"); err != nil {
		return 0, nil, "", &Error{Class: ClassOffline, Command: "rtsp", Err: errors.New("the RTSP connection broke")}
	}
	tp := textproto.NewReader(rd)
	line, err := tp.ReadLine()
	if err != nil {
		return 0, nil, "", &Error{Class: ClassUnsupported, Command: "rtsp", Err: errors.New("no RTSP answer")}
	}
	parts := strings.Fields(line)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "RTSP/") {
		return 0, nil, "", &Error{Class: ClassUnsupported, Command: "rtsp", Err: errors.New("the answer is not RTSP")}
	}
	code, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, nil, "", &Error{Class: ClassUnsupported, Command: "rtsp", Err: errors.New("the answer is not RTSP")}
	}
	hdr, err := tp.ReadMIMEHeader()
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, nil, "", &Error{Class: ClassUnsupported, Command: "rtsp", Err: errors.New("unreadable RTSP headers")}
	}
	var body string
	if n, _ := strconv.Atoi(hdr.Get("Content-Length")); n > 0 && n <= rtspMaxBytes {
		buf := make([]byte, n)
		if _, err := io.ReadFull(rd, buf); err == nil {
			body = string(buf)
		}
	}
	return code, hdr, body, nil
}

// authorization answers a WWW-Authenticate challenge ("" when it is one this client does not speak).
func authorization(challenge, user, password, uri string) string {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(challenge), " ")
	switch strings.ToLower(scheme) {
	case "basic":
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
	case "digest":
		p := parseChallenge(rest)
		if p["nonce"] == "" {
			return ""
		}
		ha1 := md5hex(user + ":" + p["realm"] + ":" + password)
		ha2 := md5hex("DESCRIBE:" + uri)
		resp := md5hex(ha1 + ":" + p["nonce"] + ":" + ha2)
		out := fmt.Sprintf(`Digest username=%q, realm=%q, nonce=%q, uri=%q`, user, p["realm"], p["nonce"], uri)
		if qop := firstQOP(p["qop"]); qop != "" {
			nc, cnonce := "00000001", randomHex(8)
			resp = md5hex(ha1 + ":" + p["nonce"] + ":" + nc + ":" + cnonce + ":" + qop + ":" + ha2)
			out += fmt.Sprintf(`, qop=%s, nc=%s, cnonce=%q`, qop, nc, cnonce)
		}
		return out + fmt.Sprintf(`, response=%q`, resp)
	}
	return ""
}

func firstQOP(v string) string {
	for _, q := range strings.Split(v, ",") {
		if strings.TrimSpace(q) == "auth" {
			return "auth"
		}
	}
	return ""
}

func parseChallenge(s string) map[string]string {
	out := map[string]string{}
	for _, part := range splitOutsideQuotes(s) {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok {
			out[strings.ToLower(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return out
}

func splitOutsideQuotes(s string) []string {
	var parts []string
	start, inQuotes := 0, false
	for i, r := range s {
		switch {
		case r == '"':
			inQuotes = !inQuotes
		case r == ',' && !inQuotes:
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	return append(parts, s[start:])
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s)) //nolint:gosec // dictated by RTSP Digest
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// sdpVideoCodec reads the codec of the first video stream from an SDP body.
func sdpVideoCodec(sdp string) string {
	inVideo := false
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "m="):
			inVideo = strings.HasPrefix(line, "m=video")
		case inVideo && strings.HasPrefix(line, "a=rtpmap:"):
			_, enc, _ := strings.Cut(line, " ")
			name, _, _ := strings.Cut(enc, "/")
			return strings.ToUpper(strings.TrimSpace(name))
		}
	}
	return ""
}
