package api

import (
	"context"
	"errors"
	"mime"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/kyleraykbs/MusOak/internal/media"
	"github.com/kyleraykbs/MusOak/internal/store"
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

// handleMediaFile serves a rendition this server holds. There is no streaming:
// a file that is on disk is served with Range support, an ETag and `?start=`,
// and one that is not is refused with a 409 telling the caller to download it
// first. Use the status endpoint to watch for readiness and the download
// endpoint to fetch it.
func (s *Server) handleMediaFile(w http.ResponseWriter, r *http.Request) {
	id, ok := variantIDFromPath(w, r)
	if !ok {
		return
	}
	s.serveMedia(w, r, id)
}

// serveMedia answers one rendition's URL: the finished file when it is on disk,
// or a refusal when it is not. A variant whose source is metadata only is
// refused differently from one that merely has not been fetched yet, mirroring
// the download endpoint, so a caller learns which of the two it has to resolve.
func (s *Server) serveMedia(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	// Resolved once, and handed down: the variant tells the refusal which case
	// it is, and the file is the answer when the rendition is already on disk.
	res := s.media.Resolve(r.Context(), id)

	if res.File != nil {
		// The bytes are Opus in an Ogg container, served as audio/ogg - and a
		// browser left to itself names a download of that ".ogg". Say the name
		// the file actually has: the extension the library stores it under, and
		// the song it belongs to. "inline" so the same response still plays in
		// an audio element.
		w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{
			"filename": mediaFileName(res, id),
		}))
		s.media.ServeFile(w, r, id, res)
		return
	}

	// No file on disk. A variant row that could not be read, or a file lookup
	// that failed for any reason other than "absent", is the store's own error
	// to report - ServeFile answers it. Only a rendition that really is absent
	// gets the refusal, so a lookup failure is never mistaken for a missing
	// download.
	if res.Variant != nil && (res.FileErr == nil || errors.Is(res.FileErr, store.ErrNotFound)) {
		if !res.Variant.Downloadable {
			writeError(w, http.StatusConflict,
				"variant is not downloadable; resolve its track first (POST /api/v1/tracks/{trackId}/resolve)")
			return
		}
		writeError(w, http.StatusConflict,
			"the rendition is not downloaded yet; start its download (POST /api/v1/media/"+id.String()+"/download) and play it when it is ready")
		return
	}
	s.media.ServeFile(w, r, id, res)
}

// mediaFileName is what a download of a rendition should be called:
// "Artist - Title.opus", the way the file is stored. A variant that cannot be
// read still gets a name, its own id.
func mediaFileName(res media.Resolved, variantID uuid.UUID) string {
	if res.Variant == nil {
		return variantID.String() + ".opus"
	}
	return opusFileName(strings.Join(res.Variant.Artists, ", "), res.Variant.Title, variantID)
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
	VariantID string `json:"variantId"`
	State     string `json:"state"`
	// Stage is what the download is doing: "download" while bytes arrive,
	// "convert" while the file is written from the fetched source, "store"
	// while it is probed and put in place.
	Stage string `json:"stage,omitempty"`
	// Progress is the fraction of the current stage that is done, and -1 when
	// there is nothing to count (no length reported, a stage with no bytes),
	// which is a client's cue to sweep the bar rather than fill it.
	Progress float64 `json:"progress"`
	// Bytes/TotalBytes are the stage's own numbers: the bytes fetched for a
	// download, the milliseconds converted for a conversion. TotalBytes <= 0
	// means unknown.
	Bytes      int64  `json:"bytes,omitempty"`
	TotalBytes int64  `json:"totalBytes,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"durationMs,omitempty"`
}

func (s *Server) mediaStatus(ctx context.Context, variantID uuid.UUID) mediaStatusResponse {
	status := s.media.Status(ctx, variantID)
	return mediaStatusResponse{
		VariantID:  status.VariantID.String(),
		State:      string(status.State),
		Stage:      status.Stage,
		Progress:   status.Progress,
		Bytes:      status.Bytes,
		TotalBytes: status.TotalBytes,
		Error:      status.Err,
		DurationMs: status.DurationMs,
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

	if boolParam(r, "wait") {
		if _, err := s.ensureMedia(r.Context(), id, boolParam(r, "priority")); err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, s.mediaStatus(r.Context(), id))
		return
	}

	// Detached so the download survives the client hanging up: the manager
	// deduplicates it with every other requester anyway.
	ctx := context.WithoutCancel(r.Context())
	priority := boolParam(r, "priority")
	go func() {
		if _, err := s.ensureMedia(ctx, id, priority); err != nil {
			s.logger.Warn("media download failed", "variant", id, "error", err)
		}
	}()
	writeJSON(w, http.StatusAccepted, s.mediaStatus(r.Context(), id))
}

// handleMediaCancel stops a download nobody is waiting on any more: the song
// left the queue, or the queue was replaced. It answers 204 whether or not one
// was running, so a client may cancel a fetch that finished on its own.
func (s *Server) handleMediaCancel(w http.ResponseWriter, r *http.Request) {
	id, ok := variantIDFromPath(w, r)
	if !ok {
		return
	}
	s.media.Cancel(id)
	w.WriteHeader(http.StatusNoContent)
}

// ensureMedia downloads a variant either as the song somebody is waiting to
// hear - which takes the whole pipe: the downloads running for anything else
// are cancelled and none starts until it has finished - or as background work,
// which waits its turn.
func (s *Server) ensureMedia(ctx context.Context, variantID uuid.UUID, priority bool) (string, error) {
	if priority {
		return s.media.EnsurePrimary(ctx, variantID)
	}
	return s.media.Ensure(ctx, variantID)
}

// boolParam reads a flag the way the media routes are asked for one: absent is
// false, and every spelling a client or a person might reach for is true.
// `wait` (answer once the file is here) and `priority` (this is the song
// somebody is waiting to hear) are the two, and neither deserves its own parser.
func boolParam(r *http.Request, name string) bool {
	switch r.URL.Query().Get(name) {
	case "1", "true", "yes":
		return true
	}
	return false
}
