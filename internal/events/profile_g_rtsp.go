package events

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// formatONVIFClockRange formats [from, to] per ONVIF Replay Specification §5.2.
func formatONVIFClockRange(from, to time.Time) string {
	return "clock=" + from.UTC().Format("20060102T150405Z") + "-" + to.UTC().Format("20060102T150405Z")
}

// startRTSPRangeProxy opens a local loopback RTSP TCP proxy in front of targetURL
// that passes all RTSP frames through unmodified except injecting the ONVIF
// `Range: clock=...` header and `Rate-Control: no` on PLAY requests so FFmpeg can
// depacketize historical Profile G recordings from [from, to].
func startRTSPRangeProxy(ctx context.Context, rawURL string, from, to time.Time) (proxyURL string, cleanup func(), err error) {
	u, err := url.Parse(rawURL)
	if err != nil || !strings.EqualFold(u.Scheme, "rtsp") || u.Host == "" {
		return rawURL, func() {}, nil
	}
	targetHost := u.Host
	if _, _, splitErr := net.SplitHostPort(targetHost); splitErr != nil {
		targetHost = net.JoinHostPort(u.Hostname(), "554")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	proxyAddr := ln.Addr().String()
	uCopy := *u
	uCopy.Host = proxyAddr
	rangeVal := formatONVIFClockRange(from, to)

	pCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			cli, aErr := ln.Accept()
			if aErr != nil {
				return
			}
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				relayRTSPConn(pCtx, c, targetHost, proxyAddr, u.Host, rangeVal)
			}(cli)
		}
	}()

	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			_ = ln.Close()
			wg.Wait()
		})
	}
	return uCopy.String(), stop, nil
}

func relayRTSPConn(ctx context.Context, cli net.Conn, targetDialHost, proxyHost, origHost, rangeVal string) {
	defer cli.Close()
	srv, err := (&net.Dialer{Timeout: 6 * time.Second}).DialContext(ctx, "tcp", targetDialHost)
	if err != nil {
		return
	}
	defer srv.Close()

	go func() {
		<-ctx.Done()
		_ = cli.Close()
		_ = srv.Close()
	}()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(cli, srv)
		_ = cli.Close()
	}()

	br := bufio.NewReader(cli)
	for {
		first, pErr := br.Peek(1)
		if pErr != nil {
			break
		}
		if first[0] == '$' {
			var hdr [4]byte
			if _, rErr := io.ReadFull(br, hdr[:]); rErr != nil {
				break
			}
			n := int(binary.BigEndian.Uint16(hdr[2:4]))
			buf := make([]byte, 4+n)
			copy(buf[:4], hdr[:])
			if n > 0 {
				if _, rErr := io.ReadFull(br, buf[4:]); rErr != nil {
					break
				}
			}
			if _, wErr := srv.Write(buf); wErr != nil {
				break
			}
			continue
		}

		reqLine, rErr := br.ReadString('\n')
		if rErr != nil {
			break
		}
		reqLine = strings.ReplaceAll(reqLine, proxyHost, origHost)
		isPlay := strings.HasPrefix(strings.ToUpper(reqLine), "PLAY ")

		var headers []string
		contentLen := 0
		for {
			line, hErr := br.ReadString('\n')
			if hErr != nil {
				return
			}
			trimmed := strings.TrimRight(line, "\r\n")
			if trimmed == "" {
				break
			}
			low := strings.ToLower(trimmed)
			switch {
			case strings.HasPrefix(low, "host:"):
				line = strings.ReplaceAll(line, proxyHost, origHost)
			case isPlay && (strings.HasPrefix(low, "range:") || strings.HasPrefix(low, "rate-control:")):
				// Strip FFmpeg's default `Range: npt=0.000-` so ONVIF `Range: clock=...` takes effect.
				continue
			case strings.HasPrefix(low, "content-length:"):
				if n, pErr := strconv.Atoi(strings.TrimSpace(trimmed[len("content-length:"):])); pErr == nil && n > 0 {
					contentLen = n
				}
			}
			headers = append(headers, line)
		}

		var body []byte
		if contentLen > 0 && contentLen <= 4<<20 {
			body = make([]byte, contentLen)
			if _, bErr := io.ReadFull(br, body); bErr != nil {
				break
			}
		}

		var out strings.Builder
		out.WriteString(reqLine)
		for _, h := range headers {
			out.WriteString(h)
		}
		if isPlay {
			fmt.Fprintf(&out, "Range: %s\r\n", rangeVal)
			out.WriteString("Rate-Control: no\r\n")
		}
		out.WriteString("\r\n")
		if _, wErr := srv.Write([]byte(out.String())); wErr != nil {
			break
		}
		if len(body) > 0 {
			if _, wErr := srv.Write(body); wErr != nil {
				break
			}
		}
	}
	_ = srv.Close()
	wg.Wait()
}
