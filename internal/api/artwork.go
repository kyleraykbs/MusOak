package api

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/prismusic/internal/artwork"
	"codeberg.org/kyleraykbs/prismusic/internal/store"
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
	case errors.Is(err, artwork.ErrNoArtwork):
		http.Error(w, "no artwork", http.StatusNotFound)
		return
	case errors.Is(err, artwork.ErrFetch):
		// The provider CDN was unreachable; the image may exist later.
		s.logger.Warn("artwork fetch failed", "kind", kind, "id", id, "error", err)
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
