package api

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/kyleraykbs/MusOak/internal/store"
)

// minPlayMs is the shortest listen worth remembering. A track skipped straight
// away was not listened to; the client reports the length it heard when a track
// stops, and anything under this is noise.
const minPlayMs = 5000

// playRepeatWindow is how close a reported listen has to be to the last one
// this account reported to be the same stop reported twice. A client that never
// saw the answer - a dropped response, a reload, a retry - reports the same
// stop again, and counting one listen twice is a wrong "most played". A second
// listen of the same song cannot land here: it takes at least minPlayMs of
// listening between the two starts, which is more than this window.
const playRepeatWindow = 3 * time.Second

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
//
// A report that repeats the last one - the same rendition, the same length
// heard, the same stop - is not a second listen, so it is not a second row:
// the client that reports a stop twice would otherwise double-count it.
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
	if s.repeatsLastPlay(r, user.ID, trackID, variantID, req.PlayedMs, started) {
		// The same stop, reported again: the listen is already on record.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := s.store.RecordPlay(r.Context(), record); err != nil {
		writeStoreError(w, err, "could not record the play")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// repeatsLastPlay reports whether this is the last listen this account reported
// being reported again. Reading the last row is one indexed lookup, and it is
// best effort: if it cannot be read, the play is recorded rather than lost.
func (s *Server) repeatsLastPlay(r *http.Request, userID, trackID, variantID uuid.UUID, playedMs int64, started time.Time) bool {
	recent, err := s.store.History(r.Context(), userID, 1)
	if err != nil || len(recent) == 0 {
		return false
	}
	last := recent[0]
	return last.TrackID == trackID &&
		last.VariantID == variantID &&
		last.PlayedMs == playedMs &&
		last.StartedAt.Sub(started).Abs() <= playRepeatWindow
}

// handleHistory lists what the caller listened to, newest first. A play whose
// track has since been deleted is dropped: the song it named is gone, and a row
// without a song is not something to show.
//
// The tracks are read in one query and rendered in one more set
// (store.TracksFor, Server.buildTracks): a page holds up to two hundred
// listens, and a row-by-row render was three queries per row at best.
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

	ids := make([]uuid.UUID, 0, len(records))
	for _, record := range records {
		ids = append(ids, record.TrackID)
	}
	tracks, err := s.store.TracksFor(r.Context(), ids)
	if err != nil {
		writeStoreError(w, err, "history unavailable")
		return
	}
	kept := make([]store.PlayRecord, 0, len(records))
	ordered := make([]store.Track, 0, len(records))
	for _, record := range records {
		track, ok := tracks[record.TrackID]
		if !ok {
			continue // the song was deleted from under the listen
		}
		kept = append(kept, record)
		ordered = append(ordered, track)
	}
	built, err := s.buildTracks(r.Context(), ordered)
	if err != nil {
		writeStoreError(w, err, "history unavailable")
		return
	}

	plays := make([]historyPlayResponse, 0, len(kept))
	for i, record := range kept {
		plays = append(plays, historyPlayResponse{
			Track:     built[i],
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

	ids := make([]uuid.UUID, 0, len(counts))
	for _, count := range counts {
		ids = append(ids, count.TrackID)
	}
	tracks, err := s.store.TracksFor(r.Context(), ids)
	if err != nil {
		writeStoreError(w, err, "top tracks unavailable")
		return
	}
	kept := make([]store.PlayCount, 0, len(counts))
	ordered := make([]store.Track, 0, len(counts))
	for _, count := range counts {
		track, ok := tracks[count.TrackID]
		if !ok {
			continue // the song was deleted from under the listen
		}
		kept = append(kept, count)
		ordered = append(ordered, track)
	}
	built, err := s.buildTracks(r.Context(), ordered)
	if err != nil {
		writeStoreError(w, err, "top tracks unavailable")
		return
	}

	out := make([]topTrackResponse, 0, len(kept))
	for i, count := range kept {
		out = append(out, topTrackResponse{
			Track:          built[i],
			Plays:          count.Plays,
			LastPlayedAtMs: count.LastPlayedAt.UnixMilli(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tracks": out})
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
