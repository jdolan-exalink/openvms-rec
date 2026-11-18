package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// SessionPrefix identifies signed session bearer tokens issued by login.
	SessionPrefix = "ovs_"

	pbkdf2Iter    = 10000
	pbkdf2SaltLen = 16
	pbkdf2KeyLen  = 32

	DefaultAdminUser = "admin"
	DefaultAdminPass = "admin123"
)

var (
	ErrInvalidCredentials = errors.New("invalid username or password")
	ErrUserExists         = errors.New("user already exists")
	ErrUserNotFound       = errors.New("user not found")
	ErrLastAdmin          = errors.New("cannot delete the last admin user")
	ErrInvalidInput       = errors.New("invalid username, password, or role")
)

// User represents a non-sensitive user record stored in SQLite.
type User struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	Role      string `json:"role"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// HashPassword hashes a plaintext password using PBKDF2-HMAC-SHA256.
func HashPassword(password string) (string, error) {
	if len(password) < 4 {
		return "", fmt.Errorf("%w: password must be at least 4 characters", ErrInvalidInput)
	}
	salt := make([]byte, pbkdf2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	dk := pbkdf2SHA256([]byte(password), salt, pbkdf2Iter, pbkdf2KeyLen)
	return fmt.Sprintf("pbkdf2_sha256$%d$%s$%s",
		pbkdf2Iter,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(dk),
	), nil
}

// VerifyPassword verifies a plaintext password against a stored PBKDF2 hash.
func VerifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2_sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1000 || iter > 1000000 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil || len(salt) == 0 {
		return false
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(expected) == 0 {
		return false
	}
	dk := pbkdf2SHA256([]byte(password), salt, iter, len(expected))
	return subtle.ConstantTimeCompare(dk, expected) == 1
}

// pbkdf2SHA256 implements RFC 2898 PBKDF2 with HMAC-SHA256 using stdlib only.
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := func(data []byte) []byte {
		mac := hmac.New(sha256.New, password)
		mac.Write(data)
		return mac.Sum(nil)
	}
	numBlocks := (keyLen + sha256.Size - 1) / sha256.Size
	var dk []byte
	var buf [4]byte
	for block := 1; block <= numBlocks; block++ {
		binary.BigEndian.PutUint32(buf[:], uint32(block))
		mac := hmac.New(sha256.New, password)
		mac.Write(salt)
		mac.Write(buf[:])
		u := mac.Sum(nil)
		t := make([]byte, len(u))
		copy(t, u)
		for i := 2; i <= iter; i++ {
			u = prf(u)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		dk = append(dk, t...)
	}
	return dk[:keyLen]
}

// UserStore manages users in SQLite.
type UserStore struct {
	db  *sql.DB
	now func() time.Time
}

// NewUserStore returns a UserStore backed by db.
func NewUserStore(db *sql.DB) *UserStore {
	return &UserStore{db: db, now: time.Now}
}

// EnsureDefaultAdmin seeds admin/admin123 if the users table is empty.
func (s *UserStore) EnsureDefaultAdmin(ctx context.Context) (bool, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return false, fmt.Errorf("count users: %w", err)
	}
	if count > 0 {
		return false, nil
	}
	if _, err := s.Create(ctx, DefaultAdminUser, DefaultAdminPass, "admin"); err != nil {
		if errors.Is(err, ErrUserExists) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Authenticate verifies username and password and returns the user on success.
func (s *UserStore) Authenticate(ctx context.Context, username, password string) (User, error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return User{}, ErrInvalidCredentials
	}
	var u User
	var hash string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, role, created_at, updated_at FROM users WHERE username = ?`,
		username,
	).Scan(&u.ID, &u.Username, &hash, &u.Role, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrInvalidCredentials
	}
	if err != nil {
		return User{}, fmt.Errorf("query user: %w", err)
	}
	if !VerifyPassword(password, hash) {
		return User{}, ErrInvalidCredentials
	}
	return u, nil
}

// Get returns a user by username.
func (s *UserStore) Get(ctx context.Context, username string) (User, error) {
	var u User
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, role, created_at, updated_at FROM users WHERE username = ?`,
		strings.TrimSpace(username),
	).Scan(&u.ID, &u.Username, &u.Role, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrUserNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("get user: %w", err)
	}
	return u, nil
}

// List returns all users ordered by username.
func (s *UserStore) List(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, username, role, created_at, updated_at FROM users ORDER BY username ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.Role, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// Create inserts a new user with a hashed password.
func (s *UserStore) Create(ctx context.Context, username, password, role string) (User, error) {
	username = strings.TrimSpace(username)
	role = strings.TrimSpace(role)
	if role == "" {
		role = "admin"
	}
	if !validUsername(username) || (role != "admin" && role != "operator" && role != "viewer") {
		return User{}, ErrInvalidInput
	}
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, err
	}
	now := s.now().UnixMilli()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, role, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		username, hash, role, now, now,
	)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return User{}, ErrUserExists
		}
		return User{}, fmt.Errorf("insert user: %w", err)
	}
	id, _ := res.LastInsertId()
	return User{ID: id, Username: username, Role: role, CreatedAt: now, UpdatedAt: now}, nil
}

// UpdatePassword updates an existing user's password.
func (s *UserStore) UpdatePassword(ctx context.Context, username, password string) error {
	username = strings.TrimSpace(username)
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	now := s.now().UnixMilli()
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, updated_at = ? WHERE username = ?`,
		hash, now, username,
	)
	if err != nil {
		return fmt.Errorf("update password: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrUserNotFound
	}
	return nil
}

// Delete removes a user, refusing to delete the last admin.
func (s *UserStore) Delete(ctx context.Context, username string) error {
	username = strings.TrimSpace(username)
	u, err := s.Get(ctx, username)
	if err != nil {
		return err
	}
	if u.Role == "admin" {
		var admins int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role = 'admin'`).Scan(&admins); err != nil {
			return fmt.Errorf("count admins: %w", err)
		}
		if admins <= 1 {
			return ErrLastAdmin
		}
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE username = ?`, username)
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrUserNotFound
	}
	return nil
}

func validUsername(u string) bool {
	if len(u) < 2 || len(u) > 64 {
		return false
	}
	for _, r := range u {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			continue
		}
		return false
	}
	return true
}

// Session represents claims inside a signed session bearer token.
type Session struct {
	Username  string `json:"u"`
	Role      string `json:"r"`
	ExpiresAt int64  `json:"exp"`
}

// TokenSigner issues and verifies HMAC-SHA256 signed session tokens.
type TokenSigner struct {
	secret []byte
	now    func() time.Time
}

// NewTokenSigner creates a TokenSigner with the given secret (or a random 32-byte key if empty).
func NewTokenSigner(secret []byte) (*TokenSigner, error) {
	if len(secret) == 0 {
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, fmt.Errorf("generate session secret: %w", err)
		}
	}
	cp := make([]byte, len(secret))
	copy(cp, secret)
	return &TokenSigner{secret: cp, now: time.Now}, nil
}

// LoadOrCreateSecret loads or creates a persistent 32-byte HMAC secret in dir/.session_secret (mode 0600).
func LoadOrCreateSecret(dir string) ([]byte, error) {
	if dir == "" {
		return nil, nil
	}
	p := filepath.Join(dir, ".session_secret")
	if b, err := os.ReadFile(p); err == nil && len(b) >= 32 {
		return b, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		return nil, err
	}
	return b, nil
}

// Issue creates a signed session token valid for ttl.
func (s *TokenSigner) Issue(username, role string, ttl time.Duration) (string, error) {
	if s == nil || len(s.secret) == 0 {
		return "", errors.New("token signer not initialized")
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	payload, err := json.Marshal(Session{
		Username:  username,
		Role:      role,
		ExpiresAt: s.now().Add(ttl).Unix(),
	})
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding.EncodeToString(payload)
	sig := s.sign(enc)
	return SessionPrefix + enc + "." + sig, nil
}

// Verify validates a session token and returns its Session claims.
func (s *TokenSigner) Verify(token string) (Session, bool) {
	if s == nil || len(s.secret) == 0 {
		return Session{}, false
	}
	body, ok := strings.CutPrefix(token, SessionPrefix)
	if !ok {
		return Session{}, false
	}
	dot := strings.IndexByte(body, '.')
	if dot <= 0 || dot == len(body)-1 {
		return Session{}, false
	}
	enc, sig := body[:dot], body[dot+1:]
	expected := s.sign(enc)
	if subtle.ConstantTimeCompare([]byte(sig), []byte(expected)) != 1 {
		return Session{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return Session{}, false
	}
	var sess Session
	if err := json.Unmarshal(raw, &sess); err != nil || sess.Username == "" {
		return Session{}, false
	}
	if sess.ExpiresAt <= s.now().Unix() {
		return Session{}, false
	}
	return sess, true
}

func (s *TokenSigner) sign(data string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(data))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// HashToken returns the SHA-256 hex digest of a bearer session token so
// revocation tables never store raw tokens.
func HashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// RevokeToken records a session token as revoked until its expiry time.
func RevokeToken(ctx context.Context, db *sql.DB, token, username string, expiresAt int64) error {
	if db == nil || token == "" {
		return nil
	}
	now := time.Now().UTC().Unix()
	if expiresAt <= 0 {
		expiresAt = now + 24*3600
	}
	_, err := db.ExecContext(ctx, `INSERT INTO revoked_tokens(token_hash, username, revoked_at, expires_at)
		VALUES (?, ?, ?, ?) ON CONFLICT(token_hash) DO NOTHING`,
		HashToken(token), username, now, expiresAt)
	if err != nil {
		return err
	}
	_, _ = db.ExecContext(ctx, `DELETE FROM revoked_tokens WHERE expires_at < ?`, now-3600)
	return nil
}

// IsTokenRevoked reports whether token has been explicitly revoked.
func IsTokenRevoked(ctx context.Context, db *sql.DB, token string) bool {
	if db == nil || token == "" {
		return false
	}
	var hit int
	err := db.QueryRowContext(ctx, `SELECT 1 FROM revoked_tokens WHERE token_hash = ?`, HashToken(token)).Scan(&hit)
	return err == nil && hit == 1
}
