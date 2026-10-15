package api

import (
	"context"
	"mime"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/kyleraykbs/MusOak/internal/media"
	"github.com/kyleraykbs/MusOak/internal/ranking"
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

// handleMediaFile serves a rendition, finished or still arriving. A finished
// file is served from disk with Range support and an ETag. One that is still
// downloading is played from its source - the bytes exist upstream, and waiting
// for the whole song to land is a silence nobody asked for - while the download
// carries on in the background, so the next request is answered from disk. Use
// the status endpoint to watch for readiness.
func (s *Server) handleMediaFile(w http.ResponseWriter, r *http.Request) {
	id, ok := variantIDFromPath(w, r)
	if !ok {
		return
	}
	s.serveMedia(w, r, id)
}

// handleTrackStream plays a track a caller knows only by id, without the extra
// round trip of listing its variants and choosing one: the server picks the
// rendition it would play for this caller - the first downloadable one in their
// effective provider order, the same choice the picker and the room path make -
// and answers exactly as /api/v1/media/{variantId} does, streaming while the
// file is still downloading and serving it with Range once it is on disk.
//
// The chosen rendition is named in X-Musoak-Variant, so a client can hand this
// URL to its player and later swap to that rendition's media URL if it wants
// them to be the same request from then on. A track with no downloadable
// rendition at all is a 404 saying so, not an empty stream.
func (s *Server) handleTrackStream(w http.ResponseWriter, r *http.Request) {
	id, ok := trackIDFromPath(w, r)
	if !ok {
		return
	}
	if _, err := s.store.Track(r.Context(), id); err != nil {
		writeStoreError(w, err, "track not found")
		return
	}
	variant, found, err := s.preferredVariant(r.Context(), id, s.currentUser(r))
	if err != nil {
		writeStoreError(w, err, "variants unavailable")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound,
			"track has no downloadable rendition; resolve it first (POST /api/v1/tracks/{trackId}/resolve)")
		return
	}
	w.Header().Set("X-Musoak-Variant", variant.ID.String())
	s.serveMedia(w, r, variant.ID)
}

// preferredVariant is the rendition this server would play for a caller who
// named only a track: the first downloadable one in their effective provider
// order, which is the order the picker lists and the room path picks from. A
// guest has no ranking of their own, so the aggregate order decides, exactly as
// it does for an anonymous room member. The order is shared with the variants
// endpoint, so a client that does resolve them and pick the first answers the
// same variant this does.
func (s *Server) preferredVariant(ctx context.Context, trackID uuid.UUID, user *store.User) (store.Variant, bool, error) {
	variants, err := s.store.VariantsForTrack(ctx, trackID)
	if err != nil {
		return store.Variant{}, false, err
	}
	counts, err := s.store.VariantVoteCounts(ctx, trackID)
	if err != nil {
		return store.Variant{}, false, err
	}
	uploaders, err := s.store.VariantUploaders(ctx, trackID)
	if err != nil {
		return store.Variant{}, false, err
	}
	order, err := s.effectiveOrder(ctx, user)
	if err != nil {
		return store.Variant{}, false, err
	}
	var caller uuid.UUID
	if user != nil {
		caller = user.ID
	}
	for _, variant := range ranking.OrderVariants(order, variants, uploaders, counts, caller) {
		if variant.Downloadable {
			return variant, true, nil
		}
	}
	return store.Variant{}, false, nil
}

// serveMedia answers one rendition's URL: the name it downloads under, the
// source's own bytes while the file is still arriving, and the finished file
// once it is on disk. Every path that plays a rendition goes through it, so the
// track-level stream and the media URL cannot answer differently.
func (s *Server) serveMedia(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	// Resolved once, and handed down: the variant tells the answer its name and
	// where its bytes come from, and the file is the answer when the rendition is
	// already on disk. The serve paths used to fetch both again on the way to the
	// first byte, once per query and once per stat.
	res := s.media.Resolve(r.Context(), id)

	// The bytes are Opus in an Ogg container, served as audio/ogg - and a
	// browser left to itself names a download of that ".ogg". Say the name the
	// file actually has: the extension the library stores it under, and the song
	// it belongs to. "inline" so the same response still plays in an audio
	// element.
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{
		"filename": mediaFileName(res, id),
	}))
	if s.media.Stream(w, r, id, res) {
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
