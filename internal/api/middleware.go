package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"codeberg.org/kyleraykbs/prismusic/internal/auth"
	"codeberg.org/kyleraykbs/prismusic/internal/store"
)

type contextKey int

// userKey carries the authenticated user through the request context.
const userKey contextKey = iota

// withAuth resolves the bearer token (if any) and enforces requireLogin.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token := bearerToken(r); token != "" {
			user, err := s.auth.Authenticate(r.Context(), token)
			switch {
			case err == nil:
				r = r.WithContext(context.WithValue(r.Context(), userKey, user))
			case errors.Is(err, auth.ErrInvalidToken):
				// fall through: treated as anonymous
			default:
				s.logger.Warn("auth: token lookup failed", "error", err)
			}
		}

		if s.cfg.RequireLogin && s.currentUser(r) == nil && !publicPaths[r.URL.Path] {
			writeError(w, http.StatusUnauthorized, "login required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerToken extracts the token from the Authorization header.
func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	const prefix = "bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

// currentUser returns the authenticated user, or nil for a guest.
func (s *Server) currentUser(r *http.Request) *store.User {
	user, _ := r.Context().Value(userKey).(*store.User)
	return user
}

// requireUser returns the authenticated user, answering 401 for guests.
// Unauthenticated access is read-only: guests have no personal data.
func (s *Server) requireUser(w http.ResponseWriter, r *http.Request) (*store.User, bool) {
	user := s.currentUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return nil, false
	}
	return user, true
}
