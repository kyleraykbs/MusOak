package api

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/kyleraykbs/MusOak/internal/auth"
	"github.com/kyleraykbs/MusOak/internal/config"
	"github.com/kyleraykbs/MusOak/internal/store"
)

// An expired session is an authentication failure and nothing else. The routes
// that need an account refuse it, a route that serves guests treats the caller
// as a guest, and the row is gone the moment the token is presented - what it
// must not be is a server error, or a caller who is believed for one request
// too many.
func TestExpiredSessionIsAnAuthFailure(t *testing.T) {
	for _, requireLogin := range []bool{false, true} {
		name := "optional login"
		if requireLogin {
			name = "login required"
		}
		t.Run(name, func(t *testing.T) {
			c := newHTTPTestServer(t, func(cfg *config.Config) { cfg.RequireLogin = requireLogin })
			c.register("kyle", "hunter2hunter2")
			kyle := c.userNamed("kyle")

			// A session this server issued, whose time has passed.
			const expired = "expired-bearer-token"
			ctx := context.Background()
			if err := c.store.CreateSession(ctx, &store.Session{
				TokenHash: auth.HashToken(expired),
				UserID:    kyle.ID,
				CreatedAt: time.Now().Add(-2 * auth.SessionTTL),
				ExpiresAt: time.Now().Add(-time.Hour),
			}); err != nil {
				t.Fatalf("CreateSession: %v", err)
			}

			// A route that names the caller's own listening: refused as
			// unsigned, which is what sends a client back to logging in.
			rec := c.do(http.MethodGet, "/api/v1/me/history", expired, nil)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("expired token on an account route = %d: %s", rec.Code, rec.Body.String())
			}

			// The caller description is what a guest gets, and nothing about
			// the stale token is believed: no account, no server error.
			want := http.StatusOK
			if requireLogin {
				want = http.StatusUnauthorized
			}
			rec = c.do(http.MethodGet, "/api/v1/me", expired, nil)
			if rec.Code != want {
				t.Fatalf("/me with an expired token = %d, want %d", rec.Code, want)
			}
			if !requireLogin {
				var me struct {
					Authenticated bool `json:"authenticated"`
					Guest         bool `json:"guest"`
				}
				c.decode(rec, &me)
				if me.Authenticated || !me.Guest {
					t.Fatalf("/me = %+v, want a guest", me)
				}
			}

			// Presented once, the row is gone: a token that has been refused
			// is not kept to be looked up again.
			if _, err := c.store.Session(ctx, auth.HashToken(expired)); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("the expired session survived its rejection: %v", err)
			}

			// The account still works, so nothing above stranded it.
			if rec := c.do(http.MethodGet, "/api/v1/me/history", c.register("sam", "hunter2hunter2"), nil); rec.Code != http.StatusOK {
				t.Fatalf("a fresh session = %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}
