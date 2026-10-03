package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"codeberg.org/kyleraykbs/musoak/internal/store"
)

// maxPlaybackStateBytes bounds what a client may store as its playback state:
// a queue of a thousand tracks is a few hundred kilobytes, and more than that
// is not a queue.
const maxPlaybackStateBytes = 512 << 10

// playbackStateResponse wraps the stored document. The server does not
// interpret it: what "the queue you were on" means is the client's to decide.
type playbackStateResponse struct {
	State     json.RawMessage `json:"state"`
	UpdatedAt int64           `json:"updatedAt,omitempty"`
}

// handlePlaybackState returns what the caller was last playing.
//
// Nothing stored is an empty document rather than a 404: restoring state
// should be the same code path whether or not there is any.
func (s *Server) handlePlaybackState(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	raw, err := s.store.PlaybackState(r.Context(), user.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusOK, playbackStateResponse{State: json.RawMessage("{}")})
			return
		}
		writeStoreError(w, err, "playback state unavailable")
		return
	}
	writeJSON(w, http.StatusOK, playbackStateResponse{State: json.RawMessage(raw)})
}

// handlePlaybackStateSave records what the caller is playing, so a restart — or
// another machine — can pick up from there.
func (s *Server) handlePlaybackStateSave(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	raw, err := readBounded(r, maxPlaybackStateBytes)
	if err != nil {
		writeError(w, http.StatusBadRequest, "state must be JSON: "+err.Error())
		return
	}
	var request playbackStateResponse
	if err := json.Unmarshal(raw, &request); err != nil || len(request.State) == 0 {
		writeError(w, http.StatusBadRequest, "state must be a JSON object")
		return
	}
	stored := []byte(request.State)
	if stored[0] != '{' {
		writeError(w, http.StatusBadRequest, "state must be a JSON object")
		return
	}
	if err := s.store.SetPlaybackState(r.Context(), user.ID, stored); err != nil {
		writeStoreError(w, err, "the playback state could not be stored")
		return
	}
	// Saving state is the signal that this account is playing something right
	// now: it is what "online" is derived from.
	if err := s.store.TouchUser(r.Context(), user.ID, time.Now()); err != nil {
		writeStoreError(w, err, "the playback state could not be stored")
		return
	}
	writeJSON(w, http.StatusOK, playbackStateResponse{State: request.State})
}

// readBounded reads a request body up to the limit, refusing to buffer more
// than any caller has a reason to send.
func readBounded(r *http.Request, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("larger than %d bytes", limit)
	}
	return raw, nil
}
