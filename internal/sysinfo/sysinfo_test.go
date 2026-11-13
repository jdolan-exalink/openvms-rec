package sysinfo

import (
	"io"
	"log/slog"
	"testing"
)

func TestRing(t *testing.T) {
	r := NewRing(3)
	log := slog.New(r.Handler(slog.NewTextHandler(io.Discard, nil))).With("camera", "c1")
	log.Info("ignored")
	for _, m := range []string{"a", "b", "c", "d"} {
		log.Warn(m, "n", 1)
	}
	got := r.Entries()
	if len(got) != 3 || got[0].Message != "d" || got[2].Message != "b" || got[0].Attrs["camera"] != "c1" {
		t.Fatalf("%+v", got)
	}
}

func TestSampler(t *testing.T) {
	s := NewSampler()
	p := s.Self()
	if p.PID == 0 || p.RSSBytes == 0 {
		t.Fatalf("%+v", p)
	}
	if h := s.Host(); h.CPUs == 0 || h.MemTotalBytes == 0 {
		t.Fatalf("%+v", h)
	}
	if _, ok := s.Process(1 << 30); ok {
		t.Fatal("bogus pid")
	}
}
