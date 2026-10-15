package media

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/kyleraykbs/MusOak/internal/store"
)

// ServeFile serves a variant's finished rendition with Range support and an
// ETag of its sha256. Only complete files are ever visible: Ensure renames them
// into place atomically. The file row was resolved once by the handler and
// travels in res, so this does not look it up again.
func (m *Manager) ServeFile(w http.ResponseWriter, r *http.Request, variantID uuid.UUID, res Resolved) {
	if res.FileErr != nil {
		if errors.Is(res.FileErr, store.ErrNotFound) {
			http.Error(w, "media not ready", http.StatusNotFound)
			return
		}
		http.Error(w, "media lookup failed", http.StatusInternalServerError)
		return
	}
	file := res.File
	if file == nil {
		http.Error(w, "media not ready", http.StatusNotFound)
		return
	}

	// Held for the length of the response, so an eviction sweep does not delete
	// the bytes under a listener who is half way through them.
	done := m.beginServing(variantID)
	defer done()

	f, err := os.Open(file.Path)
	if err != nil {
		http.Error(w, "media file missing", http.StatusNotFound)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		http.Error(w, "media file missing", http.StatusNotFound)
		return
	}

	w.Header().Set("ETag", `"`+file.SHA256+`"`)
	w.Header().Set("Content-Type", "audio/ogg")
	// A rendition is immutable in practice: the bytes for a variant are written
	// once, and the ETag is their hash. Without a lifetime a browser keeps
	// nothing, so a client that fetched a song ahead of time fetches it again
	// when it plays - which is how a room that started on time was heard a
	// minute late. Private, because a library is not a public cache's business.
	w.Header().Set("Cache-Control", "private, max-age=604800")
	w.Header().Set("X-Content-SHA256", file.SHA256)

	// Serving a file is what keeps it in the cache; only refresh the record
	// occasionally so streaming does not turn into a write per request. The row
	// was resolved fresh for this request, so its access time is current.
	if time.Since(file.AccessedAt) > time.Minute {
		if err := m.db.TouchMediaFile(r.Context(), variantID, time.Now()); err != nil {
			m.logger.Warn("media: could not record access", "variant", variantID, "error", err)
		}
	}
	http.ServeContent(w, r, filepath.Base(file.Path), file.DownloadedAt, f)
}
