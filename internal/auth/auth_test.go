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

func TestIsLoopback(t *testing.T) {
	for addr, want := range map[string]bool{"127.0.0.1:8640": true, "localhost:1": true, "[::1]:1": true, "0.0.0.0:8640": false, ":8640": false, "192.168.1.5:80": false} {
		if IsLoopback(addr) != want {
			t.Errorf("%s", addr)
		}
	}
}
