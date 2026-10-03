package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"codeberg.org/kyleraykbs/musoak/internal/auth"
)

type credentialsRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type userResponse struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

type authResponse struct {
	Token string       `json:"token"`
	User  userResponse `json:"user"`
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req credentialsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	user, token, err := s.auth.Register(r.Context(), strings.TrimSpace(req.Username), req.Password)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, authResponse{
		Token: token,
		User:  userResponse{ID: user.ID.String(), Username: user.Username},
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req credentialsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	user, token, err := s.auth.Login(r.Context(), strings.TrimSpace(req.Username), req.Password)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, authResponse{
		Token: token,
		User:  userResponse{ID: user.ID.String(), Username: user.Username},
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r); !ok {
		return
	}
	if err := s.auth.Logout(r.Context(), bearerToken(r)); err != nil && !errors.Is(err, auth.ErrInvalidToken) {
		writeStoreError(w, err, "session not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleMe describes the caller: their identity and what the server allows.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{
		"requireLogin":     s.auth.RequireLogin(),
		"registrationOpen": s.auth.RegistrationOpen(),
	}
	if user := s.currentUser(r); user != nil {
		body["authenticated"] = true
		body["user"] = userResponse{ID: user.ID.String(), Username: user.Username}
		// The account tab reads these flat, beside the policy flags.
		body["username"] = user.Username
		body["displayName"] = user.DisplayName
		body["iconUrl"] = user.IconURL
		body["iconVersion"] = user.IconVersion
		// Empty when the account never chose; the client's own default applies.
		body["searchPlatforms"] = nonNilStrings(user.SearchPlatforms)
	} else {
		body["authenticated"] = false
		body["guest"] = true
	}
	writeJSON(w, http.StatusOK, body)
}

func writeAuthError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrRegistrationClosed):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, auth.ErrUsernameTaken):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, auth.ErrInvalidCredentials):
		writeError(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, auth.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, auth.ErrInvalidToken):
		writeError(w, http.StatusUnauthorized, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

// decodeJSON reads a bounded JSON body into v.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	body := http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}
