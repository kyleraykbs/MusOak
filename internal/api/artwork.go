package api

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/kyleraykbs/MusOak/internal/artwork"
	"github.com/kyleraykbs/MusOak/internal/store"
)

// artworkPath is the location clients use for an entity's image. Responses
// carry this path rather than the provider's CDN URL, so every client goes
// through the server's cache.
func artworkPath(kind store.ArtworkKind, id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return "/api/v1/artwork/" + string(kind) + "/" + id.String()
}

// noCachedArtwork says that whatever answers this request must not be kept.
// Every way a cover can be absent here is temporary - a fetch in flight, a CDN
// that just refused, an entity that may learn a cover from a later sync - so a
// browser that stores the miss shows a fallback image that nothing can fix
// short of a hard reload.
func (s *Server) noCachedArtwork(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
}

// handleArtwork serves a cached cover, fetching it on first use.
func (s *Server) handleArtwork(w http.ResponseWriter, r *http.Request) {
	kind := store.ArtworkKind(r.PathValue("kind"))
	if !kind.Valid() {
		writeError(w, http.StatusBadRequest, "unknown artwork kind")
		return
	}
	id, ok := s.collectionID(w, r, "artworkId")
	if !ok {
		return
	}

	item, err := s.artwork.Fetch(r.Context(), kind, id)
	switch {
	case errors.Is(err, artwork.ErrPending):
		// Not here yet: the fetch is still running, or it ran and the CDN
		// refused a moment ago. A page must never be held for a cover, and the
		// row is asked for again on the next render - so this is a "not now",
		// and it must not be kept: a cover that arrives a second later has to
		// be able to appear.
		s.noCachedArtwork(w)
		http.Error(w, "no artwork yet", http.StatusNotFound)
		return
	case errors.Is(err, artwork.ErrNoArtwork):
		// In the same boat: nothing points at a cover today, and a later sync
		// may learn one.
		s.noCachedArtwork(w)
		http.Error(w, "no artwork", http.StatusNotFound)
		return
	case errors.Is(err, artwork.ErrFetch):
		// The provider CDN was unreachable; the image may exist later. A
		// failure that is kept by the browser is a cover that stays missing
		// after the CDN comes back.
		s.logger.Warn("artwork fetch failed", "kind", kind, "id", id, "error", err)
		s.noCachedArtwork(w)
		http.Error(w, "artwork unavailable", http.StatusBadGateway)
		return
	case err != nil:
		writeStoreError(w, err, "artwork unavailable")
		return
	}

	file, err := os.Open(item.Path)
	if err != nil {
		http.Error(w, "artwork missing", http.StatusNotFound)
		return
	}
	defer file.Close()

	w.Header().Set("Content-Type", item.ContentType)
	if item.SHA256 != "" {
		w.Header().Set("ETag", `"`+item.SHA256+`"`)
	}
	// A cover never changes once fetched, so let clients keep it.
	w.Header().Set("Cache-Control", "public, max-age=604800, immutable")
	http.ServeContent(w, r, filepath.Base(item.Path), time.Time{}, file)
}
