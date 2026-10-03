package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/musoak/internal/store"
)

// lyricsFetchTimeout bounds a lookup. Lyrics are a nicety on top of playing a
// song, so a slow or dead source is given a short leash rather than being
// allowed to hold a request open.
const lyricsFetchTimeout = 10 * time.Second

// lyricsResponse is a song's words for one rendition. Synced is always a list
// so a client can draw it without a null check; VariantID echoes which copy
// the timings belong to.
type lyricsResponse struct {
	Synced     []store.LyricLine `json:"synced"`
	Plain      string            `json:"plain"`
	Source     string            `json:"source"`
	DurationMs int64             `json:"durationMs"`
	VariantID  string            `json:"variantId"`
}

// handleTrackLyrics returns one rendition's words, or null when there are none.
//
// A source being unreachable is not an error: the honest answer is that there
// are no words for this one, and the client says so plainly.
func (s *Server) handleTrackLyrics(w http.ResponseWriter, r *http.Request) {
	trackID, ok := trackIDFromPath(w, r)
	if !ok {
		return
	}
	variantID := uuid.Nil
	if raw := r.URL.Query().Get("variantId"); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid variant id")
			return
		}
		variantID = parsed
	}

	ctx, cancel := context.WithTimeout(r.Context(), lyricsFetchTimeout)
	defer cancel()

	found, err := s.lyrics.Resolve(ctx, trackID, variantID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "track not found")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	case found == nil:
		writeJSON(w, http.StatusOK, map[string]any{"lyrics": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lyrics": lyricsBody(found)})
}

func lyricsBody(l *store.Lyrics) lyricsResponse {
	synced := l.Synced
	if synced == nil {
		synced = []store.LyricLine{}
	}
	return lyricsResponse{
		Synced:     synced,
		Plain:      l.Plain,
		Source:     l.Source,
		DurationMs: l.DurationMs,
		VariantID:  l.VariantID.String(),
	}
}
