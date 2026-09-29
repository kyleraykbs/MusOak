package auth

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/prismusic/internal/config"
	"codeberg.org/kyleraykbs/prismusic/internal/store"
)

func newTestService(t *testing.T, mutate func(*config.Config)) (*Service, *store.DB) {
	t.Helper()
	db, err := store.Open("file:auth-" + uuid.NewString() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	cfg := config.Default()
	if mutate != nil {
		mutate(cfg)
	}
	return New(cfg, db, slog.New(slog.DiscardHandler)), db
}

func TestHashPasswordRoundTrip(t *testing.T) {
	const password = "correct horse battery staple"

	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Errorf("hash = %q, want a PHC argon2id string", hash)
	}

	ok, err := VerifyPassword(password, hash)
	if err != nil || !ok {
		t.Fatalf("VerifyPassword = %v, %v", ok, err)
	}
	if ok, err := VerifyPassword("wrong", hash); err != nil || ok {
		t.Errorf("wrong password verified: %v, %v", ok, err)
	}

	other, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if other == hash {
		t.Error("hashes must be salted")
	}

	for _, bad := range []string{"", "argon2id", "$argon2id$v=19$m=65536,t=3,p=2$notbase64!$x", "$bcrypt$v=1$m=1,t=1,p=1$aa$bb"} {
		if _, err := VerifyPassword(password, bad); err == nil {
			t.Errorf("VerifyPassword(%q) must fail", bad)
		}
	}
}

func TestRegisterAuthenticateLogout(t *testing.T) {
	svc, db := newTestService(t, nil)
	ctx := context.Background()

	user, token, err := svc.Register(ctx, "kyle", "hunter2hunter2")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if user.ID == uuid.Nil || token == "" {
		t.Fatalf("user = %+v, token = %q", user, token)
	}

	got, err := svc.Authenticate(ctx, token)
	if err != nil || got.ID != user.ID {
		t.Fatalf("Authenticate = %+v, %v", got, err)
	}

	// Only the hash of the token is stored.
	if _, err := db.Session(ctx, token); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("raw token must not be stored: %v", err)
	}
	if _, err := db.Session(ctx, HashToken(token)); err != nil {
		t.Errorf("hashed token lookup: %v", err)
	}

	if _, err := svc.Authenticate(ctx, "bogus"); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("bogus token: err = %v", err)
	}

	if err := svc.Logout(ctx, token); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := svc.Authenticate(ctx, token); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("token still valid after logout")
	}
}

func TestRegisterRespectsPolicy(t *testing.T) {
	svc, _ := newTestService(t, func(c *config.Config) { c.RegistrationOpen = false })
	if _, _, err := svc.Register(context.Background(), "kyle", "hunter2hunter2"); !errors.Is(err, ErrRegistrationClosed) {
		t.Fatalf("err = %v, want ErrRegistrationClosed", err)
	}
	if !svc.RegistrationOpen() {
		// RegistrationOpen reports the configured policy, not the outcome.
		t.Log("policy is closed, as configured")
	}
}

func TestRegisterValidation(t *testing.T) {
	svc, _ := newTestService(t, nil)
	ctx := context.Background()

	cases := map[string][2]string{
		"username too short": {"ab", "hunter2hunter2"},
		"username bad chars": {"not allowed!", "hunter2hunter2"},
		"password empty":     {"kyle", ""},
	}
	for name, creds := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := svc.Register(ctx, creds[0], creds[1]); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("err = %v, want ErrInvalidInput", err)
			}
		})
	}

	// Any non-empty password is accepted: one character is enough.
	if _, _, err := svc.Register(ctx, "one-char", "x"); err != nil {
		t.Fatalf("a one-character password was refused: %v", err)
	}
	if _, token, err := svc.Login(ctx, "one-char", "x"); err != nil || token == "" {
		t.Fatalf("logging in with a one-character password: %v", err)
	}

	if _, _, err := svc.Register(ctx, "kyle", "hunter2hunter2"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Register(ctx, "kyle", "hunter2hunter2"); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("err = %v, want ErrUsernameTaken", err)
	}
}

func TestLogin(t *testing.T) {
	svc, db := newTestService(t, nil)
	ctx := context.Background()

	user, _, err := svc.Register(ctx, "kyle", "hunter2hunter2")
	if err != nil {
		t.Fatal(err)
	}

	got, token, err := svc.Login(ctx, "kyle", "hunter2hunter2")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if got.ID != user.ID || token == "" {
		t.Fatalf("Login = %+v, %q", got, token)
	}

	session, err := db.Session(ctx, HashToken(token))
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if ttl := time.Until(session.ExpiresAt); ttl < SessionTTL-time.Minute || ttl > SessionTTL+time.Minute {
		t.Errorf("session TTL = %v, want ~%v", ttl, SessionTTL)
	}

	if _, _, err := svc.Login(ctx, "kyle", "wrong password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("wrong password: err = %v", err)
	}
	if _, _, err := svc.Login(ctx, "nobody", "whatever"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("unknown user: err = %v", err)
	}
}

func TestExpiredSessionIsRejectedAndPruned(t *testing.T) {
	svc, db := newTestService(t, nil)
	ctx := context.Background()

	user, _, err := svc.Register(ctx, "kyle", "hunter2hunter2")
	if err != nil {
		t.Fatal(err)
	}

	const token = "expired-token"
	err = db.CreateSession(ctx, &store.Session{
		TokenHash: HashToken(token),
		UserID:    user.ID,
		CreatedAt: time.Now().Add(-48 * time.Hour),
		ExpiresAt: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Authenticate(ctx, token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("err = %v, want ErrInvalidToken", err)
	}
	if _, err := db.Session(ctx, HashToken(token)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expired session must be pruned, got %v", err)
	}
}

func TestIssuePrunesExpiredSessions(t *testing.T) {
	svc, db := newTestService(t, nil)
	ctx := context.Background()

	user, _, err := svc.Register(ctx, "kyle", "hunter2hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(ctx, &store.Session{
		TokenHash: "stale",
		UserID:    user.ID,
		CreatedAt: time.Now().Add(-48 * time.Hour),
		ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := svc.Login(ctx, "kyle", "hunter2hunter2"); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if _, err := db.Session(ctx, "stale"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("stale session survived a login: %v", err)
	}
}
