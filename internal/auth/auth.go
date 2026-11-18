// Package auth implements API key authentication (PRD M10). Keys are random
// 256-bit tokens shown once at creation; only their SHA-256 is stored.
package auth

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Prefix makes keys recognisable in secret scanners.
const Prefix = "ovr_"

// Key is a stored key: a name and the hex SHA-256 of the token.
type Key struct {
	Name string
	Hash [32]byte
}

// Generate returns a new token and its stored form.
func Generate(name string) (token string, k Key, err error) {
	if name == "" || strings.ContainsAny(name, ":\n ") {
		return "", Key{}, errors.New("key name must be non-empty without spaces or ':'")
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", Key{}, err
	}
	token = Prefix + hex.EncodeToString(b)
	return token, Key{Name: name, Hash: sha256.Sum256([]byte(token))}, nil
}

// Line is the file representation "name:sha256hex".
func (k Key) Line() string { return k.Name + ":" + hex.EncodeToString(k.Hash[:]) }

// LoadKeys reads a keys file (one "name:sha256hex" per line, # comments).
// A missing file means no keys. On Unix the file must be 0600 or stricter.
func LoadKeys(path string) ([]Key, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s must not be accessible by group or others (chmod 600)", path)
	}
	var keys []Key
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, h, ok := strings.Cut(line, ":")
		raw, err := hex.DecodeString(h)
		if !ok || err != nil || len(raw) != 32 {
			return nil, fmt.Errorf("%s:%d: expected name:sha256hex", path, n)
		}
		var k Key
		k.Name = name
		copy(k.Hash[:], raw)
		keys = append(keys, k)
	}
	return keys, sc.Err()
}

// AppendKey adds a key to the file, creating it with mode 0600.
func AppendKey(path string, k Key) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(f, k.Line()); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Authenticator checks requests and throttles repeated failures per client.
type Authenticator struct {
	Keys []Key
	// Public reports paths that need no key (health, token-signed playback).
	Public      func(path string) bool
	VerifyToken func(token string) (string, bool)
	// MaxFailures per client IP within Window before Middleware returns 429. Defaults 30 / 1 min.
	MaxFailures int
	// LoginMaxFailures per client IP within Window before login returns 429. Defaults 10 / 1 min.
	LoginMaxFailures int
	Window           time.Duration
	Now              func() time.Time

	initOnce   sync.Once
	mu         sync.Mutex
	fails      map[string][]time.Time
	loginFails map[string][]time.Time
}

// Enabled reports whether any key or token verifier is configured.
func (a *Authenticator) Enabled() bool { return len(a.Keys) > 0 || a.VerifyToken != nil }

// Check returns the key or user name for a valid token.
func (a *Authenticator) Check(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	h := sha256.Sum256([]byte(token))
	name, ok := "", false
	for _, k := range a.Keys { // compare all keys: constant time per key
		if subtle.ConstantTimeCompare(h[:], k.Hash[:]) == 1 {
			name, ok = k.Name, true
		}
	}
	if !ok && a.VerifyToken != nil {
		return a.VerifyToken(token)
	}
	return name, ok
}

// TokenFromRequest extracts the bearer token or X-API-Key from r.
func TokenFromRequest(r *http.Request) string { return tokenFrom(r) }

// CheckRateLimit reports whether the caller's IP is currently rate-limited for login attempts.
func (a *Authenticator) CheckRateLimit(r *http.Request) (bool, int) {
	if a == nil {
		return false, 0
	}
	a.initDefaults()
	if a.isBlockedIn(&a.loginFails, clientIP(r), a.LoginMaxFailures) {
		return true, int(a.Window.Seconds())
	}
	return false, 0
}

// RecordFailure records a failed login attempt for the caller's IP.
func (a *Authenticator) RecordFailure(r *http.Request) {
	if a == nil {
		return
	}
	a.initDefaults()
	a.recordFailIn(&a.loginFails, clientIP(r))
}

// ResetFailures clears failed API and login attempt counters for the caller's IP.
func (a *Authenticator) ResetFailures(r *http.Request) {
	if a == nil {
		return
	}
	ip := clientIP(r)
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.fails, ip)
	delete(a.loginFails, ip)
}

func (a *Authenticator) initDefaults() {
	a.initOnce.Do(func() {
		if a.MaxFailures == 0 {
			a.MaxFailures = 30
		}
		if a.LoginMaxFailures == 0 {
			a.LoginMaxFailures = 10
		}
		if a.Window == 0 {
			a.Window = time.Minute
		}
		if a.Now == nil {
			a.Now = time.Now
		}
	})
}

func tokenFrom(r *http.Request) string {
	if t := r.Header.Get("X-API-Key"); t != "" {
		return t
	}
	if v, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// Middleware enforces keys and session tokens when enabled.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	a.initDefaults()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.Enabled() || (a.Public != nil && a.Public(r.URL.Path)) {
			next.ServeHTTP(w, r)
			return
		}
		tok := tokenFrom(r)
		if tok == "" {
			// Requests without credentials are not credential-guessing attempts; keep
			// them 401 so browser tabs and probes never trigger IP lockouts.
			w.Header().Set("WWW-Authenticate", `Bearer realm="openvms-recorder"`)
			httpError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid API key")
			return
		}
		ip := clientIP(r)
		if a.isBlockedIn(&a.fails, ip, a.MaxFailures) {
			w.Header().Set("Retry-After", fmt.Sprint(int(a.Window.Seconds())))
			httpError(w, http.StatusTooManyRequests, "too_many_attempts", "too many failed authentication attempts")
			return
		}
		if _, ok := a.Check(tok); !ok {
			a.recordFailIn(&a.fails, ip)
			w.Header().Set("WWW-Authenticate", `Bearer realm="openvms-recorder"`)
			httpError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid API key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *Authenticator) pruneBucket(m map[string][]time.Time, ip string) []time.Time {
	cut := a.Now().Add(-a.Window)
	l := m[ip]
	i := 0
	for i < len(l) && l[i].Before(cut) {
		i++
	}
	l = l[i:]
	if len(l) == 0 {
		delete(m, ip)
		return nil
	}
	m[ip] = l
	return l
}

func (a *Authenticator) isBlockedIn(m *map[string][]time.Time, ip string, limit int) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if *m == nil {
		return false
	}
	return len(a.pruneBucket(*m, ip)) >= limit
}

func (a *Authenticator) recordFailIn(m *map[string][]time.Time, ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if *m == nil {
		*m = map[string][]time.Time{}
	}
	(*m)[ip] = append(a.pruneBucket(*m, ip), a.Now())
}

func clientIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

func httpError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":{"code":%q,"message":%q}}`+"\n", code, msg)
}

// IsLoopback reports whether a listen address only accepts local clients.
func IsLoopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
