package media

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kyleraykbs/MusOak/internal/store"
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

	// A player that starts part-way in - a session picked up where it was left,
	// or a room whose song is already a minute gone - asks for a byte range, and
	// what a source hands out starts at zero: the element then buffers from the
	// beginning to reach that position, which on a three minute song is most of
	// it. The file the download is being written to holds the bytes from the
	// start, so a range it already covers is answered from there at once.
	if r.Header.Get("Range") != "" && m.servePartRange(w, r, variantID) {
		return true
	}

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

// servePartRange answers a range from the file the download is being written to,
// when the bytes asked for are already in it. It reports false when they are
// not, and the caller streams from the source instead - the behaviour this path
// had for every range before.
//
// The part file grows while it is read and its last page may be half written, so
// a range that reaches the end is only answered when the end is written. Nothing
// about the answer may be stored: these are not the bytes this URL names once
// the file is whole, and it is marked as a stream for exactly that reason.
func (m *Manager) servePartRange(w http.ResponseWriter, r *http.Request, variantID uuid.UUID) bool {
	part, err := os.Open(m.pathFor(variantID) + ".part")
	if err != nil {
		return false
	}
	defer part.Close()
	info, err := part.Stat()
	if err != nil || info.Size() == 0 {
		return false
	}
	if !rangeWithin(r.Header.Get("Range"), info.Size()) {
		// The download has not written the bytes this asks for yet: a 416 would
		// only make the player start over, and streaming from the source is what
		// it did before.
		return false
	}
	w.Header().Set("Content-Type", "audio/ogg")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Musoak-Stream", "1")
	w.Header().Set("Accept-Ranges", "bytes")
	// ServeContent does the parsing, the 206, the Content-Range and the 416.
	http.ServeContent(w, r, "", time.Time{}, part)
	return true
}

// rangeWithin reports whether a single-range request asks for bytes the file
// already holds. A suffix range ("-500", the last five hundred bytes) is not
// this path's to answer: what the end of a half written file is depends on when
// it is read.
func rangeWithin(header string, size int64) bool {
	if size <= 0 {
		return false
	}
	spec := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(header), "bytes="))
	if spec == "" || strings.Contains(spec, ",") {
		return false
	}
	first, last, found := strings.Cut(spec, "-")
	if !found {
		return false
	}
	start, err := strconv.ParseInt(strings.TrimSpace(first), 10, 64)
	if err != nil {
		return false
	}
	if start < 0 || start >= size {
		return false
	}
	rest := strings.TrimSpace(last)
	if rest == "" {
		return true
	}
	stop, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		return false
	}
	return stop >= start && stop < size
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
