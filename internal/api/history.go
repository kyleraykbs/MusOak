package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/musoak/internal/store"
)

// minPlayMs is the shortest listen worth remembering. A track skipped straight
// away was not listened to; the client reports the length it heard when a track
// stops, and anything under this is noise.
const minPlayMs = 5000

// defaultHistoryLimit and maxHistoryLimit bound a history or top-tracks
// listing when the caller does not say, or says too much.
const (
	defaultHistoryLimit = 50
	maxHistoryLimit     = 200
)

// recordPlayRequest is one listen as the client reports it when a track stops.
type recordPlayRequest struct {
	TrackID   string `json:"trackId"`
	VariantID string `json:"variantId"`
	PlayedMs  int64  `json:"playedMs"`
	Source    string `json:"source"`
}

// historyPlayResponse is one listen with the track it was of.
type historyPlayResponse struct {
	Track     trackResponse `json:"track"`
	VariantID string        `json:"variantId"`
	PlayedMs  int64         `json:"playedMs"`
	AtMs      int64         `json:"atMs"`
}

// topTrackResponse is one track of a user's most played, with its tally.
type topTrackResponse struct {
	Track          trackResponse `json:"track"`
	Plays          int           `json:"plays"`
	LastPlayedAtMs int64         `json:"lastPlayedAtMs"`
}

// handleRecordPlay records one listen. The web reports plays whether or not
// somebody is signed in, so a guest is accepted and ignored rather than
// refused: there is no account to file it under.
func (s *Server) handleRecordPlay(w http.ResponseWriter, r *http.Request) {
	var req recordPlayRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	user := s.currentUser(r)
	if user == nil || req.PlayedMs < minPlayMs {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	trackID, err := uuid.Parse(req.TrackID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid trackId")
		return
	}
	var variantID uuid.UUID
	if req.VariantID != "" {
		if variantID, err = uuid.Parse(req.VariantID); err != nil {
			writeError(w, http.StatusBadRequest, "invalid variantId")
			return
		}
	}

	// The listen started when the track did, which is the reported length back
	// from the moment it stopped.
	started := time.Now().Add(-time.Duration(req.PlayedMs) * time.Millisecond)
	record := &store.PlayRecord{
		UserID:    user.ID,
		TrackID:   trackID,
		VariantID: variantID,
		StartedAt: started,
		PlayedMs:  req.PlayedMs,
		Source:    req.Source,
	}
	if err := s.store.RecordPlay(r.Context(), record); err != nil {
		writeStoreError(w, err, "could not record the play")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleHistory lists what the caller listened to, newest first. A play whose
// track has since been deleted is dropped: the song it named is gone, and a row
// without a song is not something to show.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	limit := historyLimit(r)
	records, err := s.store.History(r.Context(), user.ID, limit)
	if err != nil {
		writeStoreError(w, err, "history unavailable")
		return
	}

	plays := make([]historyPlayResponse, 0, len(records))
	for _, record := range records {
		track, err := s.store.Track(r.Context(), record.TrackID)
		if errors.Is(err, store.ErrNotFound) {
			continue // the song was deleted from under the listen
		}
		if err != nil {
			writeStoreError(w, err, "history unavailable")
			return
		}
		built, err := s.buildTrack(r.Context(), *track)
		if err != nil {
			writeStoreError(w, err, "history unavailable")
			return
		}
		plays = append(plays, historyPlayResponse{
			Track:     built,
			VariantID: variantIDString(record.VariantID),
			PlayedMs:  record.PlayedMs,
			AtMs:      record.StartedAt.UnixMilli(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"plays": plays})
}

// handleTopTracks lists the caller's most played tracks, most played first.
func (s *Server) handleTopTracks(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	limit := historyLimit(r)
	counts, err := s.store.TopTracks(r.Context(), user.ID, limit)
	if err != nil {
		writeStoreError(w, err, "top tracks unavailable")
		return
	}

	tracks := make([]topTrackResponse, 0, len(counts))
	for _, count := range counts {
		track, err := s.store.Track(r.Context(), count.TrackID)
		if errors.Is(err, store.ErrNotFound) {
			continue // the song was deleted from under the listen
		}
		if err != nil {
			writeStoreError(w, err, "top tracks unavailable")
			return
		}
		built, err := s.buildTrack(r.Context(), *track)
		if err != nil {
			writeStoreError(w, err, "top tracks unavailable")
			return
		}
		tracks = append(tracks, topTrackResponse{
			Track:          built,
			Plays:          count.Plays,
			LastPlayedAtMs: count.LastPlayedAt.UnixMilli(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tracks": tracks})
}

// historyLimit reads the limit query parameter, bounded to what a listing may
// return.
func historyLimit(r *http.Request) int {
	limit := intParam(r, "limit", defaultHistoryLimit)
	if limit < 1 {
		return defaultHistoryLimit
	}
	if limit > maxHistoryLimit {
		return maxHistoryLimit
	}
	return limit
}

// variantIDString renders a variant id the way a client reads it: empty for
// the default rendition.
func variantIDString(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}
