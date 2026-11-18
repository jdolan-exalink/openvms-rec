package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jdolan-exalink/openvms-rec/internal/auth"
	"github.com/jdolan-exalink/openvms-rec/internal/db"
)

func newUserTestServer(t *testing.T) (http.Handler, *auth.UserStore) {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	users := auth.NewUserStore(d)
	if _, err := users.EnsureDefaultAdmin(ctx); err != nil {
		t.Fatalf("EnsureDefaultAdmin: %v", err)
	}
	tokens, err := auth.NewTokenSigner(nil)
	if err != nil {
		t.Fatalf("NewTokenSigner: %v", err)
	}
	a := &auth.Authenticator{
		VerifyToken: func(tok string) (string, bool) {
			sess, ok := tokens.Verify(tok)
			return sess.Username, ok
		},
		Public: func(p string) bool { return p == "/api/v1/auth/login" },
	}
	srv := &Server{DB: d, Users: users, Tokens: tokens, Auth: a}
	return a.Middleware(srv.Handler()), users
}

func TestAuthLoginAndUsersCRUD(t *testing.T) {
	h, _ := newUserTestServer(t)

	// Unauthenticated request to /api/v1/users must return 401.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}

	// Wrong password fails.
	body, _ := json.Marshal(map[string]string{"username": "admin", "password": "wrong"})
	req = httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on bad password, got %d", rec.Code)
	}

	// Default admin/admin123 succeeds and issues session token.
	body, _ = json.Marshal(map[string]string{"username": "admin", "password": auth.DefaultAdminPass})
	req = httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login failed: %d %s", rec.Code, rec.Body.String())
	}
	var loginResp struct {
		Token string    `json:"token"`
		User  auth.User `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &loginResp); err != nil {
		t.Fatalf("unmarshal login: %v", err)
	}
	if loginResp.Token == "" || loginResp.User.Username != "admin" {
		t.Fatalf("unexpected login response: %+v", loginResp)
	}

	// /api/v1/auth/me with Bearer token.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+loginResp.Token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/auth/me: %d %s", rec.Code, rec.Body.String())
	}

	// Create user.
	body, _ = json.Marshal(map[string]string{"username": "operador1", "password": "clave123", "role": "operator"})
	req = httptest.NewRequest(http.MethodPost, "/api/v1/users", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+loginResp.Token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create user: %d %s", rec.Code, rec.Body.String())
	}

	// Update password.
	body, _ = json.Marshal(map[string]string{"password": "nueva456"})
	req = httptest.NewRequest(http.MethodPut, "/api/v1/users/operador1/password", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+loginResp.Token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update password: %d %s", rec.Code, rec.Body.String())
	}

	// Cannot delete last admin.
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/users/admin", nil)
	req.Header.Set("Authorization", "Bearer "+loginResp.Token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 deleting last admin, got %d %s", rec.Code, rec.Body.String())
	}

	// Delete operator succeeds.
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/users/operador1", nil)
	req.Header.Set("Authorization", "Bearer "+loginResp.Token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete operator: %d %s", rec.Code, rec.Body.String())
	}
}

func TestLoginClearsMiddlewareThrottleAndRefreshes(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	users := auth.NewUserStore(d)
	if _, err := users.EnsureDefaultAdmin(ctx); err != nil {
		t.Fatalf("EnsureDefaultAdmin: %v", err)
	}
	tokens, err := auth.NewTokenSigner(nil)
	if err != nil {
		t.Fatalf("NewTokenSigner: %v", err)
	}
	a := &auth.Authenticator{
		VerifyToken: func(tok string) (string, bool) {
			sess, ok := tokens.Verify(tok)
			return sess.Username, ok
		},
		MaxFailures: 2,
		Public:      func(p string) bool { return p == "/api/v1/auth/login" },
	}
	srv := &Server{DB: d, Users: users, Tokens: tokens, Auth: a}
	h := a.Middleware(srv.Handler())

	// Exceed API token failure limit on middleware.
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
		req.RemoteAddr = "192.0.2.50:9999"
		req.Header.Set("Authorization", "Bearer ovs_expired")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.RemoteAddr = "192.0.2.50:9999"
	req.Header.Set("Authorization", "Bearer ovs_expired")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 on throttled API token, got %d", rec.Code)
	}

	// Valid login still succeeds and resets middleware lockout for the client IP.
	body, _ := json.Marshal(map[string]string{"username": "admin", "password": auth.DefaultAdminPass})
	req = httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	req.RemoteAddr = "192.0.2.50:9999"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login should not be blocked by API token throttle: %d %s", rec.Code, rec.Body.String())
	}
	var loginResp struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &loginResp); err != nil || loginResp.Token == "" {
		t.Fatalf("login decode: %v %+v", err, loginResp)
	}

	// API calls work immediately after login without waiting out the 429 window.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.RemoteAddr = "192.0.2.50:9999"
	req.Header.Set("Authorization", "Bearer "+loginResp.Token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 after login reset, got %d %s", rec.Code, rec.Body.String())
	}

	// Refresh returns a new valid session token.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", nil)
	req.RemoteAddr = "192.0.2.50:9999"
	req.Header.Set("Authorization", "Bearer "+loginResp.Token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh: %d %s", rec.Code, rec.Body.String())
	}
	var refResp struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &refResp); err != nil || refResp.Token == "" {
		t.Fatalf("refresh decode: %v", err)
	}
	if _, ok := tokens.Verify(refResp.Token); !ok {
		t.Fatal("refreshed token invalid")
	}
}
