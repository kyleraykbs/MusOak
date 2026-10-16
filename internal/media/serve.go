package media

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/kyleraykbs/MusOak/internal/store"
)

// ServeFile serves a variant's finished rendition with Range support and an
// ETag of its sha256. A `?start=<ms>` asks it to begin part way into the track,
// which it turns into a byte offset; a Range header is the client's own request
// and takes precedence. Only complete files are ever visible: Ensure renames
// them into place atomically. The file row was resolved once by the handler and
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
	// occasionally so a serve does not turn into a write per request. The row
	// was resolved fresh for this request, so its access time is current.
	if time.Since(file.AccessedAt) > time.Minute {
		if err := m.db.TouchMediaFile(r.Context(), variantID, time.Now()); err != nil {
			m.logger.Warn("media: could not record access", "variant", variantID, "error", err)
		}
	}
	// A listener joining a song already under way hears the rest of it: a
	// `?start=<ms>` names a position in the track, and the file is served from
	// the byte that lands on. A Range header is the client's own request and
	// wins; a missing, invalid or zero start is the whole file, as it always was.
	if r.Header.Get("Range") == "" {
		if offset := startByteOffset(r, file.DurationMs, info.Size()); offset > 0 {
			// A synthetic single range lets ServeContent do the rest: the 206,
			// the Content-Range and a Content-Length covering the remainder.
			r.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
		}
	}
	http.ServeContent(w, r, filepath.Base(file.Path), file.DownloadedAt, f)
}

// startByteOffset maps `?start=<ms>` to a byte in a file of the given size, or
// 0 when there is no usable start (no duration known, an unparseable or
// non-positive value). A room member joining a song already under way should
// hear the rest of it rather than the head they missed, so a position in the
// track has to become a byte to begin at. The map is linear - the same fraction
// of the duration as of the file - because Opus is near-constant bitrate and
// this is a start position, not a frame-exact seek.
func startByteOffset(r *http.Request, durationMs, size int64) int64 {
	if durationMs <= 0 || size <= 0 {
		return 0
	}
	ms, err := strconv.ParseInt(r.URL.Query().Get("start"), 10, 64)
	if err != nil || ms <= 0 {
		return 0
	}
	offset := int64(float64(ms) / float64(durationMs) * float64(size))
	if offset > size-1 {
		offset = size - 1
	}
	return offset
}
