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

// How long a byte range the download has not reached waits for it, and how
// often it looks. A player that seeks wants the bytes within a second or it
// gives up and shows a spinner, so this is short: past it the answer falls back
// to streaming from the source, which is the old behaviour.
const (
	partRangeWait = 20 * time.Second
	partRangePoll = 50 * time.Millisecond
)

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
// `?start=<ms>` says where inside the track the listener is: a room whose song
// is a minute gone, a session picked up where it was left. The provider starts
// there and runs to the end, so what arrives is the rest of the song rather than
// its beginning - a listener is never handed the head of a track to buffer
// through before they hear anything. `X-Musoak-Duration` carries the length when
// the server knows it, so a client can draw a timeline for audio whose own
// length is unknown while it is still arriving.
//
// A streamed answer is marked `X-Musoak-Stream: 1`, carries no ETag, no length
// and no ranges, because it is not the rendition this URL names: it is a copy of
// the source, arriving now. Clients that store renditions check for that marker
// rather than mistaking half a song for a whole one.
//
// The download is not disturbed. It is started (or joined) in the background and
// the next request for the variant is served from disk, so streaming costs one
// extra fetch of the same audio and buys the listener the seconds between
// pressing play and the first byte. That fetch is its own provider process, so
// only a couple of listeners run one at a time for a variant; beyond that the
// answer is a 503 to come back to, not another process for the same bytes.
//
// What streaming does not remove is the provider's own startup: yt-dlp must
// start and reach the network before its first byte, and that wait is what is
// left. It is measured, not guessed - the wait for the provider to start is
// carried on the answer as `X-Musoak-Provider-Ms`, and one log line at the
// first byte records both that wait and the wait from there to the listener's
// first audio.
func (m *Manager) Stream(w http.ResponseWriter, r *http.Request, variantID uuid.UUID, res Resolved) bool {
	// started is when the request reached this path. Everything a listener
	// waits through before the provider's own process begins is measured from
	// here, so the floor streaming cannot remove - the provider's startup,
	// which is network-bound - is a measured number rather than a guess.
	started := time.Now()

	// A HEAD asks for what a GET would answer and none of its bytes. Starting a
	// provider fetch to answer one is an yt-dlp run per request that only wanted
	// headers; leaving it to the file answers a HEAD from what is on disk.
	if r.Method != http.MethodGet {
		return false
	}

	// A finished file is served from disk, so this is the gap before it. The
	// handler resolved the file for this request already, so this is a stat and
	// not a query.
	if res.FileErr == nil && res.File != nil {
		if info, err := os.Stat(res.File.Path); err == nil && info.Size() > 0 {
			return false
		}
	}

	variant := res.Variant
	if variant == nil {
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

	// Past a couple of listeners, the same bytes are not fetched again: the
	// answer is a moment, and by the time it is over the download behind them has
	// the file.
	if !m.beginStream(variantID) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "media stream busy", http.StatusServiceUnavailable)
		return true
	}
	defer m.endStream(variantID)

	startMs := startOffset(r)
	body, contentType, err := m.providers.Stream(r.Context(), variant.Provider, variant.ProviderTrackID, startMs)
	if err != nil {
		// A provider that cannot stream is not an error the listener has to see:
		// the request falls through to the file, and the status endpoint says how
		// far the download has got.
		m.logger.Debug("media: no stream for a downloading variant",
			"variant", variantID, "provider", variant.Provider, "error", err)
		return false
	}
	defer body.Close()

	// The provider is under way: yt-dlp has reported the container it chose,
	// which is the moment its own startup and extractor work is done and its
	// bytes are about to flow. That wait - the part streaming cannot shorten,
	// because it is the tool reaching the network - is what this records.
	providerStart := time.Since(started)

	w.Header().Set("Content-Type", contentType)
	// The finished file answers this URL later with different bytes: nothing may
	// keep this response, and it must never be validated as the file.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Musoak-Stream", "1")
	// The split between this server's own work and the provider's startup,
	// free to carry on a response that has not been written yet. The wait for
	// the first byte is the other half and is in the log line.
	w.Header().Set("X-Musoak-Provider-Ms", strconv.FormatInt(providerStart.Milliseconds(), 10))
	if length := m.streamLength(r.Context(), variant); length > 0 {
		// The length the server knows, for a client that wants a timeline: the
		// audio still arriving has no length of its own, and a bar drawn from
		// nothing is a bar that cannot show where the song is.
		w.Header().Set("X-Musoak-Duration", strconv.FormatInt(length, 10))
	}
	w.WriteHeader(http.StatusOK)
	// The headers go out on their own: a player waiting for the first byte
	// should be waiting for audio, not for a header buffer to fill.
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	m.copyStream(w, r, body, variantID, started, providerStart)
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
		// The download has not written the bytes this asks for yet. The player
		// asked for a position, not for the beginning: answering from the source
		// instead hands it a stream from zero, which cannot be seeked, so it
		// plays a moment of the start and stalls until the download reaches the
		// position anyway. Waiting for the bytes is the same wait without the
		// false start - and the download this page asked for is already running.
		info, err = m.waitForRange(r.Context(), part, r.Header.Get("Range"), info.Size())
		if err != nil {
			return false
		}
		if !rangeWithin(r.Header.Get("Range"), info.Size()) {
			return false
		}
	}
	w.Header().Set("Content-Type", "audio/ogg")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Musoak-Stream", "1")
	w.Header().Set("Accept-Ranges", "bytes")
	// ServeContent does the parsing, the 206, the Content-Range and the 416.
	http.ServeContent(w, r, "", time.Time{}, part)
	return true
}

// waitForRange waits for a download to write the bytes a range asks for, up to
// partRangeWait. It returns the file's information once the range is covered, or
// an error if it never was - and the caller then streams from the source, which
// is what this path did before any of this.
//
// The wait is bounded because the download can fail: a provider that cannot
// deliver must not hold a listener's request open for ever.
func (m *Manager) waitForRange(ctx context.Context, part *os.File, header string, size int64) (os.FileInfo, error) {
	deadline := time.Now().Add(partRangeWait)
	ticker := time.NewTicker(partRangePoll)
	defer ticker.Stop()
	for {
		if rangeWithin(header, size) {
			return part.Stat()
		}
		if time.Now().After(deadline) {
			return nil, errRangeTooFar
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
		if info, err := part.Stat(); err == nil {
			size = info.Size()
		}
	}
}

// errRangeTooFar is a range the download did not reach in time.
var errRangeTooFar = errors.New("media: the download has not reached that range")

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
//
// started is when the request reached Stream and providerStart is when the
// provider's own fetch began producing; the first bytes are where their sum
// ends, and the one log line says both, so the floor streaming leaves in place
// - the provider's network-bound startup - is on record rather than in a guess.
func (m *Manager) copyStream(w http.ResponseWriter, r *http.Request, body io.Reader, variantID uuid.UUID, started time.Time, providerStart time.Duration) {
	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, streamChunk)
	first := true
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if first {
				// One line per stream, not per chunk: a stream can run for
				// minutes and every chunk is the socket, not new information.
				first = false
				m.logger.Info("media: stream started",
					"variant", variantID,
					"provider_ms", providerStart.Milliseconds(),
					"first_byte_ms", (time.Since(started) - providerStart).Milliseconds())
			}
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

// startOffset is where inside a track a stream should begin: the `start` query
// parameter in milliseconds, clamped to something sane. Anything a client sends
// that is not a number is the whole track from the beginning, which is the
// answer that always works.
func startOffset(r *http.Request) int64 {
	raw := strings.TrimSpace(r.URL.Query().Get("start"))
	if raw == "" {
		return 0
	}
	ms, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || ms <= 0 {
		return 0
	}
	return ms
}

// streamLength is how long the track is, from whichever row knows: the variant
// carries the provider's own figure, and the canonical track usually has one
// even when a fresh variant's is still zero. Zero means nobody knows, and the
// answer then carries no length at all.
func (m *Manager) streamLength(ctx context.Context, variant *store.Variant) int64 {
	if variant.DurationMs > 0 {
		return variant.DurationMs
	}
	if variant.TrackID == uuid.Nil {
		return 0
	}
	track, err := m.db.Track(ctx, variant.TrackID)
	if err != nil || track == nil {
		return 0
	}
	return track.DurationMs
}
