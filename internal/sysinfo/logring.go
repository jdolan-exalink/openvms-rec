// Package sysinfo reports process and host resources and keeps the latest
// warnings and errors for the "Sistema" page and the tray panel.
package sysinfo

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// LogEntry is one recent warning or error.
type LogEntry struct {
	Time    time.Time         `json:"time"`
	Level   string            `json:"level"`
	Message string            `json:"message"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}

// Ring keeps the last N records at Warn level or above. Messages are
// already redacted by the code that logs them.
type Ring struct {
	mu   sync.Mutex
	buf  []LogEntry
	next int
	full bool
}

// NewRing returns a ring holding n entries.
func NewRing(n int) *Ring { return &Ring{buf: make([]LogEntry, n)} }

func (r *Ring) add(e LogEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.next] = e
	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.full = true
	}
}

// Entries returns the kept records, newest first.
func (r *Ring) Entries() []LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.next
	if r.full {
		n = len(r.buf)
	}
	out := make([]LogEntry, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, r.buf[(r.next-i+len(r.buf))%len(r.buf)])
	}
	return out
}

// Handler wraps h and copies Warn+ records into the ring.
func (r *Ring) Handler(h slog.Handler) slog.Handler { return &ringHandler{h: h, r: r} }

type ringHandler struct {
	h     slog.Handler
	r     *Ring
	attrs []slog.Attr
}

func (h *ringHandler) Enabled(ctx context.Context, l slog.Level) bool { return h.h.Enabled(ctx, l) }

func (h *ringHandler) Handle(ctx context.Context, rec slog.Record) error {
	if rec.Level >= slog.LevelWarn {
		e := LogEntry{Time: rec.Time.UTC(), Level: rec.Level.String(), Message: rec.Message, Attrs: map[string]string{}}
		for _, a := range h.attrs {
			e.Attrs[a.Key] = a.Value.String()
		}
		rec.Attrs(func(a slog.Attr) bool {
			e.Attrs[a.Key] = a.Value.String()
			return true
		})
		h.r.add(e)
	}
	return h.h.Handle(ctx, rec)
}

func (h *ringHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &ringHandler{h: h.h.WithAttrs(as), r: h.r, attrs: append(append([]slog.Attr{}, h.attrs...), as...)}
}

func (h *ringHandler) WithGroup(name string) slog.Handler {
	return &ringHandler{h: h.h.WithGroup(name), r: h.r, attrs: h.attrs}
}
