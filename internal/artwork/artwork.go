// Package artwork fetches and caches the images the providers point at.
//
// A client asks this server for a cover; the server fetches it once, keeps it
// on disk and serves it from there. That way a GUI shows artwork even when the
// provider CDN is slow or refuses the request, and a household with five
// clients downloads each cover once.
package artwork

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/singleflight"

	"github.com/kyleraykbs/MusOak/internal/store"
)

// maxImageBytes bounds one download. Covers are a few hundred kilobytes; a
// bigger response is not a cover.
const maxImageBytes = 12 << 20

// fetchTimeout bounds one provider image download.
const fetchTimeout = 20 * time.Second

// waitForImage is how long one request waits for a cover before saying it has
// none yet.
//
// Covers are asked for by <img> elements, and a page asks for a dozen at once.
// Waiting for the whole download held the request for up to fetchTimeout, and
// over HTTP/1.1 the browser answers that by blocking every other image on the
// same six connections behind it: one slow cover made the page's artwork - the
// cached covers included - arrive after it. So a request waits what a cover
// from a working CDN takes and no longer; the fetch carries on either way,
// because the file it is writing is the household's, not this request's, and
// the next render of that row is served from disk.
const waitForImage = 2 * time.Second

// concurrentFetches is how many provider images may be in flight at once. A
// page asks for a dozen covers from one host; queueing them keeps the server
// from opening twelve sockets to it and keeps a CDN that throttles from
// answering with refusals.
const concurrentFetches = 4

// failedTTL is how long a source that just refused is left alone. It is short
// because a cover can genuinely appear later - one that is missing because the
// CDN was briefly down must not stay missing - and long enough that a room
// repainting its rows every second does not ask a broken CDN twenty seconds at
// a time, over and over.
const failedTTL = 5 * time.Minute

// failedCap bounds that memory. Reaching it drops the oldest entries, so the
// worst a hostile or enormous library can do is forget recent failures early.
const failedCap = 512

// Errors.
var (
	// ErrNoArtwork means nothing is known for that entity.
	ErrNoArtwork = errors.New("no artwork known")
	// ErrFetch means the image could not be downloaded.
	ErrFetch = errors.New("artwork could not be fetched")
	// ErrPending means there is no cover to serve right now: the fetch is still
	// running, or it ran and the CDN refused a moment ago. It is an answer to
	// come back from, never one to keep.
	ErrPending = errors.New("artwork is not here yet")
)

// UserAgent identifies this client to the provider CDNs; some of them refuse
// requests without one.
const userAgent = "musoak/2 (+https://github.com/kyleraykbs/MusOak)"

// Item is a cached image.
type Item struct {
	Path        string
	ContentType string
	SHA256      string
	Bytes       int64
	Source      string
}

// Store is the repository slice the fetcher needs.
type Store interface {
	store.ArtworkRepo
}

// Fetcher downloads images once and serves them from disk afterwards.
type Fetcher struct {
	dir    string
	db     Store
	http   *http.Client
	logger *slog.Logger

	flight singleflight.Group
	// wait is how long a request waits for a cover; see waitForImage.
	wait time.Duration
	// slots bounds the transfers that may be in flight together; see
	// concurrentFetches.
	slots chan struct{}

	mu     sync.Mutex
	failed map[string]time.Time
}

// New returns a fetcher caching images under dir.
func New(dir string, db Store, logger *slog.Logger) *Fetcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Fetcher{
		dir:    dir,
		db:     db,
		http:   &http.Client{Timeout: fetchTimeout},
		logger: logger,
		wait:   waitForImage,
		slots:  make(chan struct{}, concurrentFetches),
		failed: make(map[string]time.Time),
	}
}

// Dir is where cached images live.
func (f *Fetcher) Dir() string { return f.dir }

// Fetch returns the cached (fetching it if needed) image for an entity.
func (f *Fetcher) Fetch(ctx context.Context, kind store.ArtworkKind, id uuid.UUID) (*Item, error) {
	if !kind.Valid() {
		return nil, ErrNoArtwork
	}
	source, err := f.db.EntityArtwork(ctx, kind, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s %s", ErrNoArtwork, kind, id)
		}
		return nil, err
	}

	// The cache key is the source URL, so the same cover reached through two
	// entities is stored once.
	return f.await(ctx, source)
}

// await runs the fetch for a source, or joins the one already running, and
// answers the caller with whatever there is by the time its wait is over.
//
// The fetch itself is never abandoned when the caller gives up - it runs on a
// context that outlives the request, because the file it writes is every
// client's cover and not the one that happened to ask first.
func (f *Fetcher) await(ctx context.Context, source string) (*Item, error) {
	type answer struct {
		item *Item
		err  error
	}
	// Buffered, so the goroutine that owns the fetch never waits on a caller
	// that has already walked away.
	done := make(chan answer, 1)
	go func() {
		value, err, _ := f.flight.Do(source, func() (any, error) {
			return f.cached(context.WithoutCancel(ctx), source)
		})
		if err != nil {
			done <- answer{err: err}
			return
		}
		done <- answer{item: value.(*Item)}
	}()

	timer := time.NewTimer(f.wait)
	defer timer.Stop()
	select {
	case got := <-done:
		return got.item, got.err
	case <-timer.C:
		return nil, ErrPending
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ContentType normalises what a client claims an image is.
func ContentType(value string) string { return normalizeContentType(value) }

// ExtensionFor reports the file suffix an accepted image type is stored under,
// and whether the type is one this server serves.
func ExtensionFor(contentType string) (string, bool) { return extensionFor(contentType) }

// Store writes an image for a source that is not fetched from anywhere, which
// is what an uploaded cover is. It lands where the cache would have put it, so
// serving it later is the same code path as any other cover.
func (f *Fetcher) Store(source string, data []byte, extension string) error {
	if source == "" || len(data) == 0 {
		return errors.New("artwork: nothing to store")
	}
	// The cache directory is created on first use, as it is for a download.
	if err := os.MkdirAll(f.dir, 0o755); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(source))
	path := filepath.Join(f.dir, hex.EncodeToString(sum[:])+extension)

	tmp, err := os.CreateTemp(f.dir, "upload-*")
	if err != nil {
		return err
	}
	defer func() {
		_ = os.Remove(tmp.Name())
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// The file this replaces may be under another extension, and serving looks
	// for the oldest of them first: leaving it there would hide the new one.
	for _, ext := range storedExtensions {
		_ = os.Remove(filepath.Join(f.dir, hex.EncodeToString(sum[:])+ext))
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	f.logger.Info("artwork stored", "source", source, "bytes", len(data))
	return nil
}

// cached returns the image for a source URL, downloading it on first use.
func (f *Fetcher) cached(ctx context.Context, source string) (*Item, error) {
	sum := sha256.Sum256([]byte(source))
	key := hex.EncodeToString(sum[:])

	// A previous run may have stored it under any extension, and a replace that
	// predates the removal in Store can have left two. The newest is the one
	// stored last, which is the one that was asked for: serving any other would
	// be showing an image its owner has already replaced.
	var (
		newest    *Item
		newestAge time.Time
	)
	for _, ext := range storedExtensions {
		path := filepath.Join(f.dir, key+ext)
		info, err := os.Stat(path)
		if err != nil || info.Size() == 0 {
			continue
		}
		contentType, ok := contentTypeFor(ext)
		if !ok {
			continue
		}
		if newest == nil || info.ModTime().After(newestAge) {
			newest = &Item{Path: path, ContentType: contentType, Bytes: info.Size(), Source: source}
			newestAge = info.ModTime()
		}
	}
	if newest != nil {
		return newest, nil
	}

	// A source that refused a moment ago is not asked again yet: a page that
	// repaints asks once a second, and each ask would otherwise be a fresh
	// attempt at a host that is down.
	if f.recentlyFailed(source) {
		return nil, ErrPending
	}

	if err := os.MkdirAll(f.dir, 0o755); err != nil {
		f.rememberFailure(source)
		return nil, fmt.Errorf("%w: %v", ErrFetch, err)
	}

	// One host serves every cover, so the transfers are queued rather than
	// fired together.
	select {
	case f.slots <- struct{}{}:
		defer func() { <-f.slots }()
	case <-ctx.Done():
		return nil, ErrPending
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		f.rememberFailure(source)
		return nil, fmt.Errorf("%w: %v", ErrFetch, err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "image/*")

	resp, err := f.http.Do(req)
	if err != nil {
		f.rememberFailure(source)
		return nil, fmt.Errorf("%w: %v", ErrFetch, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		f.rememberFailure(source)
		return nil, fmt.Errorf("%w: %s answered %s", ErrFetch, source, resp.Status)
	}
	f.forgetFailure(source)

	contentType := normalizeContentType(resp.Header.Get("Content-Type"))
	ext, ok := extensionFor(contentType)
	if !ok {
		// Some CDNs send a generic type; fall back to the URL's extension.
		ext = extensionFromURL(source)
		if contentType == "" {
			contentType = "image/jpeg"
		}
	}

	tmp, err := os.CreateTemp(f.dir, ".artwork-*")
	if err != nil {
		f.rememberFailure(source)
		return nil, fmt.Errorf("%w: %v", ErrFetch, err)
	}
	tmpPath := tmp.Name()
	written, err := io.Copy(tmp, io.LimitReader(resp.Body, maxImageBytes+1))
	closeErr := tmp.Close()
	if err != nil || closeErr != nil {
		_ = os.Remove(tmpPath)
		f.rememberFailure(source)
		return nil, fmt.Errorf("%w: %v", ErrFetch, firstError(err, closeErr))
	}
	if written > maxImageBytes {
		_ = os.Remove(tmpPath)
		f.rememberFailure(source)
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", ErrFetch, source, maxImageBytes)
	}

	final := filepath.Join(f.dir, key+ext)
	if err := os.Rename(tmpPath, final); err != nil {
		_ = os.Remove(tmpPath)
		f.rememberFailure(source)
		return nil, fmt.Errorf("%w: %v", ErrFetch, err)
	}

	imageSum, _, err := hashFile(final)
	if err != nil {
		f.rememberFailure(source)
		return nil, err
	}
	f.logger.Debug("artwork cached", "source", source, "bytes", written, "type", contentType)
	return &Item{Path: final, ContentType: contentType, SHA256: imageSum, Bytes: written, Source: source}, nil
}

// recentlyFailed reports whether this source refused within failedTTL.
func (f *Fetcher) recentlyFailed(source string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	at, ok := f.failed[source]
	return ok && time.Since(at) < failedTTL
}

// rememberFailure notes that a source just refused, dropping the oldest note
// when the map is full.
func (f *Fetcher) rememberFailure(source string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.failed[source]; !ok && len(f.failed) >= failedCap {
		var (
			oldest    string
			oldestAge time.Time
		)
		for candidate, at := range f.failed {
			if oldest == "" || at.Before(oldestAge) {
				oldest, oldestAge = candidate, at
			}
		}
		delete(f.failed, oldest)
	}
	f.failed[source] = time.Now()
}

// forgetFailure clears a note once the image has arrived, so a source that
// works again is not held back by an old refusal.
func (f *Fetcher) forgetFailure(source string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.failed, source)
}

func firstError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func normalizeContentType(value string) string {
	if value == "" {
		return ""
	}
	if idx := strings.IndexByte(value, ';'); idx >= 0 {
		value = value[:idx]
	}
	return strings.ToLower(strings.TrimSpace(value))
}

var contentTypes = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".webp": "image/webp",
	".gif":  "image/gif",
	".avif": "image/avif",
}

// storedExtensions is every suffix a cached image may be under, and the order
// serving looks for them. One image is one file: storing a new one removes the
// rest, or the copy it replaced stays on disk and is served in its place -
// which is what a gif uploaded over a png used to run into.
//
// A list rather than the map, because the map's order is random and this is an
// order: "image/jpeg" has two suffixes and the first one wins.
var storedExtensions = []string{".jpg", ".jpeg", ".png", ".webp", ".gif", ".avif", ".img"}

func extensionFor(contentType string) (string, bool) {
	for _, ext := range storedExtensions {
		if known, ok := contentTypes[ext]; ok && known == contentType {
			return ext, true
		}
	}
	return "", false
}

func contentTypeFor(ext string) (string, bool) {
	value, ok := contentTypes[ext]
	return value, ok
}

func extensionFromURL(source string) string {
	if idx := strings.IndexAny(source, "?#"); idx >= 0 {
		source = source[:idx]
	}
	ext := strings.ToLower(filepath.Ext(source))
	if _, ok := contentTypes[ext]; ok {
		return ext
	}
	return ".img"
}

func hashFile(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()

	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}
