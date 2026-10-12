package media

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/musoak/internal/store"
)

// streamChunk is how much of a source is handed on at a time.
const streamChunk = 32 * 1024

// Stream answers a request for a variant whose download is still running with
// the source's own audio, so playback starts now rather than when the whole file
// has landed. It reports false when there is nothing to stream from - the file
// is already on disk, the source is not downloadable, or the provider cannot
// hand out its bytes - and the caller then answers exactly as it did before.
//
// The bytes come from the provider's own fetch (yt-dlp writing to stdout), not
// from a URL resolved for somebody else to fetch: YouTube refuses those, and the
// fetch a download already uses is the one that works.
//
// A streamed answer is marked `X-Musoak-Stream: 1`, carries no ETag, no length
// and no ranges, because it is not the rendition this URL names: it is a copy of
// the source, arriving now. Clients that store renditions check for that marker
// rather than mistaking half a song for a whole one.
//
// The download is not disturbed. It is started (or joined) in the background and
// the next request for the variant is served from disk, so streaming costs one
// extra fetch of the same audio and buys the listener the seconds between
// pressing play and the first byte.
func (m *Manager) Stream(w http.ResponseWriter, r *http.Request, variantID uuid.UUID) bool {
	if _, ok := m.ready(r.Context(), variantID); ok {
		return false
	}

	variant, err := m.db.Variant(r.Context(), variantID)
	if err != nil {
		return false
	}
	if !variant.Downloadable ||
		variant.Provider == store.LocalProvider || variant.Provider == store.UploadProvider {
		// Bytes that exist only on this disk cannot be streamed from anywhere,
		// and a provider with metadata but no audio (Spotify) has none to send.
		return false
	}

	// What the listener hears now is the stream; what they hear next time is the
	// file. Detached, because somebody skipping mid-song must not call off a
	// download the next listener is waiting for.
	go func() {
		if _, err := m.Ensure(context.WithoutCancel(r.Context()), variantID); err != nil {
			m.logger.Debug("media: background download behind a stream failed", "variant", variantID, "error", err)
		}
	}()

	body, contentType, err := m.providers.Stream(r.Context(), variant.Provider, variant.ProviderTrackID)
	if err != nil {
		// A provider that cannot stream is not an error the listener has to see:
		// the request falls through to the file, and the status endpoint says how
		// far the download has got.
		m.logger.Debug("media: no stream for a downloading variant",
			"variant", variantID, "provider", variant.Provider, "error", err)
		return false
	}
	defer body.Close()

	w.Header().Set("Content-Type", contentType)
	// The finished file answers this URL later with different bytes: nothing may
	// keep this response, and it must never be validated as the file.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Musoak-Stream", "1")
	w.WriteHeader(http.StatusOK)
	// The headers go out on their own: a player waiting for the first byte
	// should be waiting for audio, not for a header buffer to fill.
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	m.copyStream(w, r, body, variantID)
	return true
}

// copyStream hands a source's bytes on as they arrive. A failed write is a
// listener that has gone; the source is left to the caller's Close.
func (m *Manager) copyStream(w http.ResponseWriter, r *http.Request, body io.Reader, variantID uuid.UUID) {
	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, streamChunk)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && r.Context().Err() == nil {
				m.logger.Warn("media: stream ended early", "variant", variantID, "error", err)
			}
			return
		}
	}
}
