package auth

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestKeyFileRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "keys")
	tok, k, err := Generate("ops")
	if err != nil || !strings.HasPrefix(tok, Prefix) {
		t.Fatal(err)
	}
	if err := AppendKey(p, k); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), tok) {
		t.Fatal("token stored in clear")
	}
	keys, err := LoadKeys(p)
	if err != nil || len(keys) != 1 {
		t.Fatal(err)
	}
	a := &Authenticator{Keys: keys}
	if name, ok := a.Check(tok); !ok || name != "ops" {
		t.Fatal("valid token rejected")
	}
	if _, ok := a.Check(tok + "x"); ok {
		t.Fatal("invalid token accepted")
	}
	if runtime.GOOS != "windows" {
		os.Chmod(p, 0o644)
		if _, err := LoadKeys(p); err == nil {
			t.Fatal("world-readable keys file accepted")
		}
	}
}

func TestMiddleware(t *testing.T) {
	tok, k, _ := Generate("ui")
	now := time.Unix(0, 0)
	a := &Authenticator{Keys: []Key{k}, Public: func(p string) bool { return strings.HasPrefix(p, "/health/") },
		MaxFailures: 3, Window: time.Minute, Now: func() time.Time { return now }}
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	do := func(path, hdr, val string) int {
		r := httptest.NewRequest("GET", path, nil)
		r.RemoteAddr = "10.0.0.9:5555"
		if hdr != "" {
			r.Header.Set(hdr, val)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if do("/health/live", "", "") != 204 {
		t.Fatal("public path blocked")
	}
	if do("/api/v1/cameras", "X-API-Key", tok) != 204 || do("/api/v1/cameras", "Authorization", "Bearer "+tok) != 204 {
		t.Fatal("valid key rejected")
	}
	for i := 0; i < 3; i++ {
		if c := do("/api/v1/cameras", "X-API-Key", "nope"); c != 401 {
			t.Fatalf("attempt %d: %d", i, c)
		}
	}
	if c := do("/api/v1/cameras", "X-API-Key", tok); c != 429 {
		t.Fatalf("not throttled: %d", c)
	}
	now = now.Add(2 * time.Minute)
	if c := do("/api/v1/cameras", "X-API-Key", tok); c != 204 {
		t.Fatalf("throttle did not expire: %d", c)
	}
}

func TestMissingTokenDoesNotRateLimit(t *testing.T) {
	tok, k, _ := Generate("ui")
	a := &Authenticator{Keys: []Key{k}, MaxFailures: 3, Window: time.Minute}
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	for i := 0; i < 20; i++ {
		r := httptest.NewRequest("GET", "/api/v1/cameras", nil)
		r.RemoteAddr = "10.0.0.15:1234"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("missing token attempt %d: got %d want 401", i, w.Code)
		}
	}
	r := httptest.NewRequest("GET", "/api/v1/cameras", nil)
	r.RemoteAddr = "10.0.0.15:1234"
	r.Header.Set("X-API-Key", tok)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("valid token after missing tokens: got %d want 204", w.Code)
	}
}

func TestSeparateLoginLimitAndResetFailures(t *testing.T) {
	tok, k, _ := Generate("ui")
	a := &Authenticator{Keys: []Key{k}, MaxFailures: 2, LoginMaxFailures: 2, Window: time.Minute}
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	req := func(hdr string) *http.Request {
		r := httptest.NewRequest("GET", "/api/v1/cameras", nil)
		r.RemoteAddr = "10.0.0.21:4444"
		if hdr != "" {
			r.Header.Set("X-API-Key", hdr)
		}
		return r
	}
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req("bad"))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("bad key %d: got %d", i, w.Code)
		}
	}
	// API middleware is locked out, while login rate limit stays independent.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req(tok))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected API 429, got %d", w.Code)
	}
	if blocked, _ := a.CheckRateLimit(req("")); blocked {
		t.Fatal("login rate limit should remain unblocked by middleware failures")
	}
	a.ResetFailures(req(""))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req(tok))
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected ResetFailures to clear middleware lockout, got %d", w.Code)
	}
}

func TestIsLoopback(t *testing.T) {
	for addr, want := range map[string]bool{"127.0.0.1:8640": true, "localhost:1": true, "[::1]:1": true, "0.0.0.0:8640": false, ":8640": false, "192.168.1.5:80": false} {
		if IsLoopback(addr) != want {
			t.Errorf("%s", addr)
		}
	}
}
