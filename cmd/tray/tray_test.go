package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

func TestIcon(t *testing.T) {
	b := iconICO(colorOK)
	var hdr [3]uint16
	binary.Read(bytes.NewReader(b), binary.LittleEndian, &hdr)
	if hdr != [3]uint16{0, 1, 1} || len(b) != 6+16+40+32*32*4+32*4 {
		t.Fatalf("hdr %v len %d", hdr, len(b))
	}
}

func TestRender(t *testing.T) {
	v := render("running", &health{State: "ok", Version: "1", StreamsTotal: 2, StreamsRecording: 2, VolumesTotal: 1, VolumesWritable: 1, HeadroomBytes: 512e9, UptimeSeconds: 3700}, nil)
	if v.Color != colorOK || v.Title != "Grabando 2 de 2 cámaras" || !strings.Contains(v.Detail, "512 GB") || !strings.Contains(v.Tooltip, "1h 1m") {
		t.Fatalf("%+v", v)
	}
	if v := render("running", &health{State: "warning", StreamsTotal: 2, StreamsRecording: 1}, nil); v.Color != colorWarning {
		t.Fatal(v)
	}
	if v := render("stopped", nil, errors.New("refused")); v.Color != colorStopped {
		t.Fatal(v)
	}
	if v := render("running", nil, errors.New("refused")); v.Color != colorDown || v.Title != "El recorder no responde" {
		t.Fatal(v)
	}
}
