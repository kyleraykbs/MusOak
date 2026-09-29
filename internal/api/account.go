package api

import (
	"encoding/base64"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"codeberg.org/kyleraykbs/prismusic/internal/artwork"
	"codeberg.org/kyleraykbs/prismusic/internal/auth"
	"codeberg.org/kyleraykbs/prismusic/internal/store"
)

// accountUserResponse is the account view of a user: the identity plus the
// profile fields everyone else sees too.
type accountUserResponse struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	IconURL     string `json:"iconUrl"`
}

// accountUserEnvelope is what every account write answers with.
type accountUserEnvelope struct {
	User accountUserResponse `json:"user"`
}

func accountUser(user *store.User) accountUserEnvelope {
	return accountUserEnvelope{User: accountUserResponse{
		ID:          user.ID.String(),
		Username:    user.Username,
		DisplayName: user.DisplayName,
		IconURL:     user.IconURL,
	}}
}

// profilePatchRequest carries the display name: a plain text field the UI
// saves or discards, with no password behind it.
type profilePatchRequest struct {
	DisplayName string `json:"displayName"`
}

// passwordChangeRequest proves the caller twice over: once with the current
// password and once by holding the session that must survive the change.
type passwordChangeRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

// usernameChangeRequest renames the account; the password is the confirm
// dialog's proof that it is really you.
type usernameChangeRequest struct {
	Password string `json:"password"`
	Username string `json:"username"`
}

// usernameLike mirrors the username policy registration enforces (internal/auth
// keeps its own copy unexported). A rename must not admit a name that signing
// up would reject.
var usernameLike = regexp.MustCompile(`^[A-Za-z0-9._-]{3,32}$`)

// handleMePatch sets the display name. Only the session proves the caller:
// this is the account tab's save/discard field, not a credential change.
func (s *Server) handleMePatch(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	var req profilePatchRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	displayName := strings.TrimSpace(req.DisplayName)
	if err := s.store.UpdateProfile(r.Context(), user.ID, displayName, user.IconURL); err != nil {
		writeStoreError(w, err, "account not found")
		return
	}
	updated, err := s.store.User(r.Context(), user.ID)
	if err != nil {
		writeStoreError(w, err, "account not found")
		return
	}
	writeJSON(w, http.StatusOK, accountUser(updated))
}

// handleMeIcon stores the caller's icon. It is the playlist artwork upload by
// another name: same body, same validation, same cache. The image is filed
// under the very URL it will be served from, so fetching iconUrl later finds
// it on disk like any other cover, without a second copy or a network hop.
func (s *Server) handleMeIcon(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	var req playlistArtworkRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	contentType := artwork.ContentType(req.ContentType)
	extension, known := artwork.ExtensionFor(contentType)
	if !known {
		writeError(w, http.StatusBadRequest, "an image is needed: png, jpeg, webp, gif or avif")
		return
	}
	data, err := base64.StdEncoding.DecodeString(req.Data)
	if err != nil {
		writeError(w, http.StatusBadRequest, "data must be base64")
		return
	}
	if len(data) == 0 {
		writeError(w, http.StatusBadRequest, "the image is empty")
		return
	}
	if len(data) > maxArtworkBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "the image is too large")
		return
	}

	iconURL := artworkPath(store.ArtworkUser, user.ID)
	if err := s.artwork.Store(iconURL, data, extension); err != nil {
		writeStoreError(w, err, "the icon could not be stored")
		return
	}
	if err := s.store.UpdateProfile(r.Context(), user.ID, user.DisplayName, iconURL); err != nil {
		writeStoreError(w, err, "the icon could not be recorded")
		return
	}
	updated, err := s.store.User(r.Context(), user.ID)
	if err != nil {
		writeStoreError(w, err, "the icon could not be recorded")
		return
	}
	writeJSON(w, http.StatusOK, accountUser(updated))
}

// handleMePassword changes how this account proves itself. Replacing the hash
// would leave every stolen session valid, so all the other sessions are
// revoked here and only the caller's token keeps working.
func (s *Server) handleMePassword(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	var req passwordChangeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.NewPassword == "" {
		writeError(w, http.StatusBadRequest, "a new password is required")
		return
	}
	match, err := auth.VerifyPassword(req.CurrentPassword, user.PasswordHash)
	if err != nil {
		s.logger.Error("stored password hash is malformed", "user", user.Username, "error", err)
		writeError(w, http.StatusInternalServerError, "the stored password is unusable")
		return
	}
	if !match {
		writeError(w, http.StatusUnauthorized, "current password is wrong")
		return
	}
	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.store.UpdatePasswordHash(r.Context(), user.ID, hash); err != nil {
		writeStoreError(w, err, "account not found")
		return
	}
	if err := s.store.DeleteOtherSessions(r.Context(), user.ID, auth.HashToken(bearerToken(r))); err != nil {
		writeStoreError(w, err, "account not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleMeUsername renames the account. The password stands in for the
// confirm dialog, and a name somebody else already holds is a conflict.
func (s *Server) handleMeUsername(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	var req usernameChangeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	match, err := auth.VerifyPassword(req.Password, user.PasswordHash)
	if err != nil {
		s.logger.Error("stored password hash is malformed", "user", user.Username, "error", err)
		writeError(w, http.StatusInternalServerError, "the stored password is unusable")
		return
	}
	if !match {
		writeError(w, http.StatusUnauthorized, "password is wrong")
		return
	}
	username := strings.TrimSpace(req.Username)
	if !usernameLike.MatchString(username) {
		writeError(w, http.StatusBadRequest,
			"username must be 3-32 characters of letters, digits, dot, dash or underscore")
		return
	}
	if err := s.store.UpdateUsername(r.Context(), user.ID, username); err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeAuthError(w, auth.ErrUsernameTaken)
			return
		}
		writeStoreError(w, err, "account not found")
		return
	}
	updated, err := s.store.User(r.Context(), user.ID)
	if err != nil {
		writeStoreError(w, err, "account not found")
		return
	}
	writeJSON(w, http.StatusOK, accountUser(updated))
}
