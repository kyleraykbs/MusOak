package api

import (
	"net/http"

	"github.com/google/uuid"
)

// maxMediaVariantIDLen bounds path values before parsing, so a hostile path
// cannot make the UUID parser work hard.
const maxMediaVariantIDLen = 64

func variantIDFromPath(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	raw := r.PathValue("variantId")
	if len(raw) == 0 || len(raw) > maxMediaVariantIDLen {
		writeError(w, http.StatusBadRequest, "invalid variant id")
		return uuid.Nil, false
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid variant id")
		return uuid.Nil, false
	}
	return id, true
}

// handleMediaFile serves a finished rendition with Range support. It never
// starts a download; use the status endpoint to watch for readiness.
func (s *Server) handleMediaFile(w http.ResponseWriter, r *http.Request) {
	id, ok := variantIDFromPath(w, r)
	if !ok {
		return
	}
	s.media.ServeFile(w, r, id)
}

// handleMediaStatus reports a variant's download state and progress.
func (s *Server) handleMediaStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := variantIDFromPath(w, r)
	if !ok {
		return
	}
	status := s.media.Status(r.Context(), id)
	writeJSON(w, http.StatusOK, mediaStatusResponse{
		VariantID:  status.VariantID.String(),
		State:      string(status.State),
		Progress:   status.Progress,
		Error:      status.Err,
		DurationMs: status.DurationMs,
		Bytes:      status.Bytes,
	})
}

type mediaStatusResponse struct {
	VariantID  string  `json:"variantId"`
	State      string  `json:"state"`
	Progress   float64 `json:"progress"`
	Error      string  `json:"error,omitempty"`
	DurationMs int64   `json:"durationMs,omitempty"`
	Bytes      int64   `json:"bytes,omitempty"`
}
