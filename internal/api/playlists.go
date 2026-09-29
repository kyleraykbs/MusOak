package api

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/prismusic/internal/artwork"
	"codeberg.org/kyleraykbs/prismusic/internal/store"
)

type playlistResponse struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	TrackCount int             `json:"trackCount"`
	ArtworkURL string          `json:"artworkUrl,omitempty"`
	CreatedAt  string          `json:"createdAt"`
	UpdatedAt  string          `json:"updatedAt"`
	Tracks     []trackResponse `json:"tracks,omitempty"`
}

// playlistArtworkRequest is an image, base64, with the type it claims to be.
// A JSON body keeps the upload to the endpoints already here rather than
// adding multipart parsing for one feature.
type playlistArtworkRequest struct {
	Data        string `json:"data"`
	ContentType string `json:"contentType"`
}

type playlistItemResponse struct {
	Position int           `json:"position"`
	Track    trackResponse `json:"track"`
}

type playlistDetailResponse struct {
	playlistResponse
	Items []playlistItemResponse `json:"items"`
}

type playlistCreateRequest struct {
	Name string `json:"name"`
}

type playlistItemsRequest struct {
	TrackID  string   `json:"trackId"`
	TrackIDs []string `json:"trackIds"`
}

type playlistReorderRequest struct {
	Order []int `json:"order"`
}

// playlistID parses the path value and returns the playlist when it belongs to
// the caller. Somebody else's playlist is reported as missing, so the endpoint
// does not leak whose playlists exist.
func (s *Server) ownedPlaylist(w http.ResponseWriter, r *http.Request, userID uuid.UUID) (*store.Playlist, bool) {
	raw := r.PathValue("playlistId")
	if len(raw) == 0 || len(raw) > maxMediaVariantIDLen {
		writeError(w, http.StatusBadRequest, "invalid playlist id")
		return nil, false
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid playlist id")
		return nil, false
	}

	playlist, err := s.store.Playlist(r.Context(), id)
	if err != nil || playlist.UserID != userID {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			writeStoreError(w, err, "playlist not found")
			return nil, false
		}
		writeError(w, http.StatusNotFound, "playlist not found")
		return nil, false
	}
	return playlist, true
}

func playlistSummary(playlist *store.Playlist) playlistResponse {
	response := playlistResponse{
		ID:         playlist.ID.String(),
		Name:       playlist.Name,
		TrackCount: playlist.TrackCount,
		CreatedAt:  playlist.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:  playlist.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	if playlist.ArtworkURL != "" {
		response.ArtworkURL = artworkPath(store.ArtworkPlaylist, playlist.ID)
	}
	return response
}

// handlePlaylistArtwork stores a cover for one of the caller's playlists.
//
// The image is written where the artwork cache would have put it, and the
// playlist records "upload:<id>" as its source, so serving it later needs no
// network and no second copy.
func (s *Server) handlePlaylistArtwork(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	playlist, ok := s.ownedPlaylist(w, r, user.ID)
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

	source := uploadSource(playlist.ID)
	if err := s.artwork.Store(source, data, extension); err != nil {
		writeStoreError(w, err, "the cover could not be stored")
		return
	}
	if err := s.store.SetPlaylistArtwork(r.Context(), playlist.ID, source); err != nil {
		writeStoreError(w, err, "the cover could not be recorded")
		return
	}

	updated, err := s.store.Playlist(r.Context(), playlist.ID)
	if err != nil {
		writeStoreError(w, err, "the cover could not be recorded")
		return
	}
	writeJSON(w, http.StatusOK, playlistSummary(updated))
}

// uploadSource is the pseudo-URL an uploaded cover is cached under.
func uploadSource(id uuid.UUID) string { return "upload:" + id.String() }

// maxArtworkBytes bounds an upload; the cache's own limit is for downloads.
const maxArtworkBytes = 8 << 20

// handlePlaylistList lists the caller's playlists.
func (s *Server) handlePlaylistList(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	playlists, err := s.store.PlaylistsForUser(r.Context(), user.ID)
	if err != nil {
		writeStoreError(w, err, "playlists unavailable")
		return
	}

	out := make([]playlistResponse, 0, len(playlists))
	for i := range playlists {
		out = append(out, playlistSummary(&playlists[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"playlists": out})
}

// handlePlaylistCreate makes an empty playlist.
func (s *Server) handlePlaylistCreate(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	var req playlistCreateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if len(name) > 200 {
		writeError(w, http.StatusBadRequest, "name is too long")
		return
	}

	playlist := &store.Playlist{UserID: user.ID, Name: name}
	if err := s.store.CreatePlaylist(r.Context(), playlist); err != nil {
		writeStoreError(w, err, "could not create the playlist")
		return
	}
	writeJSON(w, http.StatusCreated, playlistSummary(playlist))
}

// handlePlaylistGet returns one playlist with its tracks.
func (s *Server) handlePlaylistGet(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	playlist, ok := s.ownedPlaylist(w, r, user.ID)
	if !ok {
		return
	}
	s.writePlaylistDetail(w, r, playlist)
}

// writePlaylistDetail renders a playlist and resolves its entries.
func (s *Server) writePlaylistDetail(w http.ResponseWriter, r *http.Request, playlist *store.Playlist) {
	items, err := s.store.PlaylistItems(r.Context(), playlist.ID)
	if err != nil {
		writeStoreError(w, err, "playlist unavailable")
		return
	}

	detail := playlistDetailResponse{
		playlistResponse: playlistSummary(playlist),
		Items:            make([]playlistItemResponse, 0, len(items)),
	}
	detail.TrackCount = len(items)
	for _, item := range items {
		track, err := s.store.Track(r.Context(), item.TrackID)
		if errors.Is(err, store.ErrNotFound) {
			continue // the track was deleted from under the playlist
		}
		if err != nil {
			writeStoreError(w, err, "playlist unavailable")
			return
		}
		response, err := s.buildTrack(r.Context(), *track)
		if err != nil {
			writeStoreError(w, err, "playlist unavailable")
			return
		}
		detail.Items = append(detail.Items, playlistItemResponse{Position: item.Position, Track: response})
	}
	writeJSON(w, http.StatusOK, detail)
}

// handlePlaylistRename renames a playlist.
func (s *Server) handlePlaylistRename(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	playlist, ok := s.ownedPlaylist(w, r, user.ID)
	if !ok {
		return
	}
	var req playlistCreateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if err := s.store.RenamePlaylist(r.Context(), playlist.ID, name); err != nil {
		writeStoreError(w, err, "could not rename the playlist")
		return
	}
	updated, err := s.store.Playlist(r.Context(), playlist.ID)
	if err != nil {
		writeStoreError(w, err, "playlist unavailable")
		return
	}
	writeJSON(w, http.StatusOK, playlistSummary(updated))
}

// handlePlaylistDelete removes a playlist.
func (s *Server) handlePlaylistDelete(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	playlist, ok := s.ownedPlaylist(w, r, user.ID)
	if !ok {
		return
	}
	if err := s.store.DeletePlaylist(r.Context(), playlist.ID); err != nil {
		writeStoreError(w, err, "could not delete the playlist")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlePlaylistAdd appends one or more tracks.
func (s *Server) handlePlaylistAdd(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	playlist, ok := s.ownedPlaylist(w, r, user.ID)
	if !ok {
		return
	}
	var req playlistItemsRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	ids := make([]uuid.UUID, 0, len(req.TrackIDs)+1)
	if req.TrackID != "" {
		id, err := uuid.Parse(req.TrackID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid trackId")
			return
		}
		ids = append(ids, id)
	}
	for _, raw := range req.TrackIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid trackIds entry")
			return
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		writeError(w, http.StatusBadRequest, "trackId or trackIds is required")
		return
	}
	for _, id := range ids {
		if _, err := s.store.Track(r.Context(), id); err != nil {
			writeStoreError(w, err, "track not found")
			return
		}
	}

	if err := s.store.AppendPlaylistItems(r.Context(), playlist.ID, ids); err != nil {
		writeStoreError(w, err, "could not update the playlist")
		return
	}
	s.writePlaylistDetail(w, r, playlist)
}

// handlePlaylistRemove drops one entry by position.
func (s *Server) handlePlaylistRemove(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	playlist, ok := s.ownedPlaylist(w, r, user.ID)
	if !ok {
		return
	}
	position, err := parsePathInt(r.PathValue("position"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid position")
		return
	}
	if err := s.store.RemovePlaylistItem(r.Context(), playlist.ID, position); err != nil {
		writeStoreError(w, err, "playlist entry not found")
		return
	}
	s.writePlaylistDetail(w, r, playlist)
}

// handlePlaylistReorder applies a new order.
func (s *Server) handlePlaylistReorder(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	playlist, ok := s.ownedPlaylist(w, r, user.ID)
	if !ok {
		return
	}
	var req playlistReorderRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.store.ReorderPlaylist(r.Context(), playlist.ID, req.Order); err != nil {
		if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeStoreError(w, err, "could not reorder the playlist")
		return
	}
	s.writePlaylistDetail(w, r, playlist)
}

// parsePathInt reads a non-negative integer path value.
func parsePathInt(raw string) (int, error) {
	value := 0
	if raw == "" {
		return 0, errors.New("empty")
	}
	for _, r := range raw {
		if r < '0' || r > '9' {
			return 0, errors.New("not a number")
		}
		value = value*10 + int(r-'0')
	}
	return value, nil
}
