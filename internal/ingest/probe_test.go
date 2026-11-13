package ingest

import (
	"strings"
	"testing"
)

func TestParseProbe(t *testing.T) {
	ok := `Input #0, rtsp, from 'rtsp://admin:secret@10.0.0.5:554/main':
  Metadata:
    title           : Media Server
  Duration: N/A, start: 0.000000, bitrate: N/A
  Stream #0:0: Video: hevc (Main), yuvj420p(pc, bt709), 2560x1440, 20 fps, 20 tbr, 90k tbn
  Stream #0:1: Audio: pcm_mulaw, 8000 Hz, mono, s16, 64 kb/s
Stream mapping:
  Stream #0:0 -> #0:0 (hevc (native) -> wrapped_avframe (native))
Output #0, null, to 'pipe:':
  Stream #0:0: Video: wrapped_avframe, yuvj420p, 2560x1440, q=2-31, 200 kb/s, 20 fps
`
	r := ParseProbe(ok)
	if !r.OK || len(r.Streams) != 2 || r.Streams[0].Codec != "hevc" || r.Streams[0].Width != 2560 ||
		r.Streams[0].FPS != 20 || r.Streams[1].Codec != "pcm_mulaw" || r.Streams[1].SampleRate != 8000 {
		t.Fatalf("%+v", r)
	}
	cases := map[string]string{
		"[rtsp @ 0x1] method DESCRIBE failed: 401 Unauthorized\nrtsp://a:b@h/x: Server returned 401 Unauthorized (authorization failed)": "auth_failed",
		"[rtsp @ 0x1] method DESCRIBE failed: 404 Not Found":                                                                             "not_found",
		"[tcp @ 0x1] Connection to tcp://10.0.0.9:554?timeout=0 failed: Connection refused":                                              "unreachable",
		"rtsp://u:p@10.0.0.9/x: Connection timed out":                                                                                    "timeout",
		"rtsp://u:p@10.0.0.9/x: Invalid data found when processing input":                                                                "error",
	}
	for in, want := range cases {
		r := ParseProbe(in)
		if r.OK || r.Error != want {
			t.Errorf("%q: got %+v want %s", in, r, want)
		}
		if strings.Contains(r.Message, "u:p") || strings.Contains(r.Message, "secret") {
			t.Errorf("credentials leaked: %q", r.Message)
		}
	}
}
