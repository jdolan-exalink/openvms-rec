package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/db"
)

func TestHashAndVerifyPassword(t *testing.T) {
	hash, err := HashPassword("admin123")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !VerifyPassword("admin123", hash) {
		t.Fatal("expected password to verify")
	}
	if VerifyPassword("wrong", hash) {
		t.Fatal("expected wrong password to fail")
	}
	if VerifyPassword("admin123", "invalid-format") {
		t.Fatal("expected invalid hash format to fail")
	}
}

func TestUserStoreLifecycleAndDefaultAdmin(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	store := NewUserStore(d)
	created, err := store.EnsureDefaultAdmin(ctx)
	if err != nil || !created {
		t.Fatalf("EnsureDefaultAdmin: created=%v err=%v", created, err)
	}
	created2, err := store.EnsureDefaultAdmin(ctx)
	if err != nil || created2 {
		t.Fatalf("EnsureDefaultAdmin second call: created=%v err=%v", created2, err)
	}

	u, err := store.Authenticate(ctx, DefaultAdminUser, DefaultAdminPass)
	if err != nil || u.Username != "admin" || u.Role != "admin" {
		t.Fatalf("Authenticate default admin: u=%+v err=%v", u, err)
	}
	if _, err := store.Authenticate(ctx, "admin", "badpass"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}

	// Cannot delete the last admin.
	if err := store.Delete(ctx, "admin"); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("expected ErrLastAdmin, got %v", err)
	}

	// Create second user (operator) and update password.
	op, err := store.Create(ctx, "operador", "secret123", "operator")
	if err != nil || op.Role != "operator" {
		t.Fatalf("Create operator: op=%+v err=%v", op, err)
	}
	if _, err := store.Create(ctx, "operador", "other123", "operator"); !errors.Is(err, ErrUserExists) {
		t.Fatalf("expected ErrUserExists, got %v", err)
	}
	if err := store.UpdatePassword(ctx, "operador", "newpass99"); err != nil {
		t.Fatalf("UpdatePassword: %v", err)
	}
	if _, err := store.Authenticate(ctx, "operador", "secret123"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatal("old password should fail")
	}
	if _, err := store.Authenticate(ctx, "operador", "newpass99"); err != nil {
		t.Fatalf("new password failed: %v", err)
	}

	list, err := store.List(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("List: len=%d err=%v", len(list), err)
	}

	if err := store.Delete(ctx, "operador"); err != nil {
		t.Fatalf("Delete operator: %v", err)
	}
}

func TestTokenSigner(t *testing.T) {
	signer, err := NewTokenSigner(nil)
	if err != nil {
		t.Fatalf("NewTokenSigner: %v", err)
	}
	now := time.Unix(1700000000, 0)
	signer.now = func() time.Time { return now }

	tok, err := signer.Issue("admin", "admin", time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	sess, ok := signer.Verify(tok)
	if !ok || sess.Username != "admin" || sess.Role != "admin" {
		t.Fatalf("Verify failed: ok=%v sess=%+v", ok, sess)
	}
	// Tampered token fails.
	if _, ok := signer.Verify(tok + "x"); ok {
		t.Fatal("tampered token should fail")
	}
	// Expired token fails.
	signer.now = func() time.Time { return now.Add(2 * time.Hour) }
	if _, ok := signer.Verify(tok); ok {
		t.Fatal("expired token should fail")
	}
}
