package media

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/prismusic/internal/store"
)

// ServeFile serves a variant's finished rendition with Range support and an
// ETag of its sha256. Only complete files are ever visible: Ensure renames
// them into place atomically.
func (m *Manager) ServeFile(w http.ResponseWriter, r *http.Request, variantID uuid.UUID) {
	file, err := m.db.MediaFile(r.Context(), variantID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "media not ready", http.StatusNotFound)
			return
		}
		http.Error(w, "media lookup failed", http.StatusInternalServerError)
		return
	}

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
	w.Header().Set("X-Content-SHA256", file.SHA256)
	http.ServeContent(w, r, filepath.Base(file.Path), file.DownloadedAt, f)
}
