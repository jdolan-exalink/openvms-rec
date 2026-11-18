package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/auth"
)

const defaultSessionTTL = 24 * time.Hour

func (s *Server) userRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/auth/login", s.login)
	mux.HandleFunc("POST /api/v1/auth/refresh", s.authRefresh)
	mux.HandleFunc("POST /api/v1/auth/logout", s.authLogout)
	mux.HandleFunc("GET /api/v1/auth/me", s.authMe)
	mux.HandleFunc("GET /api/v1/users", s.listUsers)
	mux.HandleFunc("POST /api/v1/users", s.createUser)
	mux.HandleFunc("PUT /api/v1/users/{username}/password", s.updateUserPassword)
	mux.HandleFunc("DELETE /api/v1/users/{username}", s.deleteUser)
}

func (s *Server) userStore() *auth.UserStore {
	if s.Users != nil {
		return s.Users
	}
	if s.DB != nil {
		return auth.NewUserStore(s.DB)
	}
	return nil
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if s.Auth != nil {
		if blocked, retry := s.Auth.CheckRateLimit(r); blocked {
			w.Header().Set("Retry-After", fmt.Sprint(retry))
			writeError(w, http.StatusTooManyRequests, "too_many_attempts", "too many failed authentication attempts")
			return
		}
	}
	store := s.userStore()
	if store == nil || s.Tokens == nil {
		writeError(w, http.StatusServiceUnavailable, "auth_unavailable", "user authentication is not configured")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	u, err := store.Authenticate(r.Context(), req.Username, req.Password)
	if err != nil {
		if s.Auth != nil {
			s.Auth.RecordFailure(r)
		}
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid username or password")
		return
	}
	tok, err := s.Tokens.Issue(u.Username, u.Role, defaultSessionTTL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not issue session token")
		return
	}
	if s.Auth != nil {
		s.Auth.ResetFailures(r)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":      tok,
		"expires_at": s.now().Add(defaultSessionTTL).UTC().Format(time.RFC3339),
		"user":       u,
	})
}

func (s *Server) authRefresh(w http.ResponseWriter, r *http.Request) {
	tok := auth.TokenFromRequest(r)
	if s.Tokens == nil {
		writeError(w, http.StatusServiceUnavailable, "auth_unavailable", "user authentication is not configured")
		return
	}
	sess, ok := s.Tokens.Verify(tok)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "valid session token required")
		return
	}
	store := s.userStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "auth_unavailable", "user store unavailable")
		return
	}
	u, err := store.Get(r.Context(), sess.Username)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "user no longer active")
		return
	}
	fresh, err := s.Tokens.Issue(u.Username, u.Role, defaultSessionTTL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not issue session token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":      fresh,
		"expires_at": s.now().Add(defaultSessionTTL).UTC().Format(time.RFC3339),
		"user":       u,
	})
}

func (s *Server) authLogout(w http.ResponseWriter, r *http.Request) {
	tok := auth.TokenFromRequest(r)
	if tok != "" && s.Tokens != nil && s.DB != nil {
		if sess, ok := s.Tokens.Verify(tok); ok {
			_ = auth.RevokeToken(r.Context(), s.DB, tok, sess.Username, sess.ExpiresAt)
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged_out"})
}

func (s *Server) authMe(w http.ResponseWriter, r *http.Request) {
	tok := auth.TokenFromRequest(r)
	if s.Tokens != nil {
		if sess, ok := s.Tokens.Verify(tok); ok {
			writeJSON(w, http.StatusOK, map[string]any{
				"username":  sess.Username,
				"role":      sess.Role,
				"auth_type": "session",
			})
			return
		}
	}
	if s.Auth != nil {
		if name, ok := s.Auth.Check(tok); ok {
			writeJSON(w, http.StatusOK, map[string]any{
				"username":  name,
				"role":      "admin",
				"auth_type": "api_key",
			})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"username":  "local",
		"role":      "admin",
		"auth_type": "none",
	})
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	store := s.userStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "auth_unavailable", "user store unavailable")
		return
	}
	users, err := store.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if users == nil {
		users = []auth.User{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	store := s.userStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "auth_unavailable", "user store unavailable")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	u, err := store.Create(r.Context(), req.Username, req.Password, req.Role)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrInvalidInput):
			writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		case errors.Is(err, auth.ErrUserExists):
			writeError(w, http.StatusConflict, "user_exists", err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

func (s *Server) updateUserPassword(w http.ResponseWriter, r *http.Request) {
	store := s.userStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "auth_unavailable", "user store unavailable")
		return
	}
	username := r.PathValue("username")
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	err := store.UpdatePassword(r.Context(), username, req.Password)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrInvalidInput):
			writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		case errors.Is(err, auth.ErrUserNotFound):
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	store := s.userStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "auth_unavailable", "user store unavailable")
		return
	}
	username := r.PathValue("username")
	err := store.Delete(r.Context(), username)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrUserNotFound):
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		case errors.Is(err, auth.ErrLastAdmin):
			writeError(w, http.StatusConflict, "last_admin", err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
