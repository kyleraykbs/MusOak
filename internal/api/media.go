package api

import (
	"context"
	"mime"
	"net/http"
	"strings"

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
	// The bytes are Opus in an Ogg container, served as audio/ogg - and a
	// browser left to itself names a download of that ".ogg". Say the name the
	// file actually has: the extension the library stores it under, and the song
	// it belongs to. "inline" so the same response still plays in an audio
	// element.
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{
		"filename": s.mediaFileName(r.Context(), id),
	}))
	s.media.ServeFile(w, r, id)
}

// mediaFileName is what a download of a rendition should be called:
// "Artist - Title.opus", the way the file is stored. A variant that cannot be
// read still gets a name, its own id.
func (s *Server) mediaFileName(ctx context.Context, variantID uuid.UUID) string {
	variant, err := s.store.Variant(ctx, variantID)
	if err != nil {
		return variantID.String() + ".opus"
	}
	return opusFileName(strings.Join(variant.Artists, ", "), variant.Title, variantID)
}

// opusFileName builds a name a file system will accept out of the song's own.
func opusFileName(artists, title string, variantID uuid.UUID) string {
	base := strings.TrimSpace(title)
	if artist := strings.TrimSpace(artists); artist != "" && base != "" {
		base = artist + " - " + base
	}
	if base == "" {
		base = variantID.String()
	}
	var cleaned strings.Builder
	for _, r := range base {
		if r < 0x20 || strings.ContainsRune(`\/:*?"<>|`, r) {
			cleaned.WriteRune('_')
			continue
		}
		cleaned.WriteRune(r)
	}
	return strings.TrimSpace(cleaned.String()) + ".opus"
}

// handleMediaStatus reports a variant's download state and progress.
func (s *Server) handleMediaStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := variantIDFromPath(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.mediaStatus(r.Context(), id))
}

type mediaStatusResponse struct {
	VariantID  string  `json:"variantId"`
	State      string  `json:"state"`
	Progress   float64 `json:"progress"`
	Error      string  `json:"error,omitempty"`
	DurationMs int64   `json:"durationMs,omitempty"`
	Bytes      int64   `json:"bytes,omitempty"`
}

func (s *Server) mediaStatus(ctx context.Context, variantID uuid.UUID) mediaStatusResponse {
	status := s.media.Status(ctx, variantID)
	return mediaStatusResponse{
		VariantID:  status.VariantID.String(),
		State:      string(status.State),
		Progress:   status.Progress,
		Error:      status.Err,
		DurationMs: status.DurationMs,
		Bytes:      status.Bytes,
	}
}

// handleMediaDownload starts (or joins) a variant's download. It is
// idempotent: concurrent callers share one transfer. Poll the status endpoint
// to watch progress, or pass wait=1 to block until the file is ready.
func (s *Server) handleMediaDownload(w http.ResponseWriter, r *http.Request) {
	id, ok := variantIDFromPath(w, r)
	if !ok {
		return
	}
	variant, err := s.store.Variant(r.Context(), id)
	if err != nil {
		writeStoreError(w, err, "variant not found")
		return
	}
	if !variant.Downloadable {
		writeError(w, http.StatusConflict,
			"variant is not downloadable; resolve its track first (POST /api/v1/tracks/{trackId}/resolve)")
		return
	}

	if waitParam(r) {
		if _, err := s.media.Ensure(r.Context(), id); err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, s.mediaStatus(r.Context(), id))
		return
	}

	// Detached so the download survives the client hanging up: the manager
	// deduplicates it with every other requester anyway.
	ctx := context.WithoutCancel(r.Context())
	go func() {
		if _, err := s.media.Ensure(ctx, id); err != nil {
			s.logger.Warn("media download failed", "variant", id, "error", err)
		}
	}()
	writeJSON(w, http.StatusAccepted, s.mediaStatus(r.Context(), id))
}

func waitParam(r *http.Request) bool {
	switch r.URL.Query().Get("wait") {
	case "1", "true", "yes":
		return true
	}
	return false
}
