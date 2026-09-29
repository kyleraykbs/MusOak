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
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/singleflight"

	"codeberg.org/kyleraykbs/prismusic/internal/store"
)

// maxImageBytes bounds one download. Covers are a few hundred kilobytes; a
// bigger response is not a cover.
const maxImageBytes = 12 << 20

// fetchTimeout bounds one provider image download.
const fetchTimeout = 20 * time.Second

// Errors.
var (
	// ErrNoArtwork means nothing is known for that entity.
	ErrNoArtwork = errors.New("no artwork known")
	// ErrFetch means the image could not be downloaded.
	ErrFetch = errors.New("artwork could not be fetched")
)

// UserAgent identifies this client to the provider CDNs; some of them refuse
// requests without one.
const userAgent = "prismusic/2 (+https://codeberg.org/kyleraykbs/prismusic)"

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
	value, err, _ := f.flight.Do(source, func() (any, error) {
		return f.cached(context.WithoutCancel(ctx), source)
	})
	if err != nil {
		return nil, err
	}
	return value.(*Item), nil
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
	// Old files under another extension would win over this one.
	for _, ext := range contentTypes {
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

	// A previous run may have stored it under any extension.
	for _, ext := range []string{".jpg", ".png", ".webp", ".gif", ".avif", ".img"} {
		path := filepath.Join(f.dir, key+ext)
		info, err := os.Stat(path)
		if err != nil || info.Size() == 0 {
			continue
		}
		if contentType, ok := contentTypeFor(ext); ok {
			return &Item{Path: path, ContentType: contentType, Bytes: info.Size(), Source: source}, nil
		}
	}

	if err := os.MkdirAll(f.dir, 0o755); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFetch, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFetch, err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "image/*")

	resp, err := f.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFetch, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: %s answered %s", ErrFetch, source, resp.Status)
	}

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
		return nil, fmt.Errorf("%w: %v", ErrFetch, err)
	}
	tmpPath := tmp.Name()
	written, err := io.Copy(tmp, io.LimitReader(resp.Body, maxImageBytes+1))
	closeErr := tmp.Close()
	if err != nil || closeErr != nil {
		_ = os.Remove(tmpPath)
		return nil, fmt.Errorf("%w: %v", ErrFetch, firstError(err, closeErr))
	}
	if written > maxImageBytes {
		_ = os.Remove(tmpPath)
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", ErrFetch, source, maxImageBytes)
	}

	final := filepath.Join(f.dir, key+ext)
	if err := os.Rename(tmpPath, final); err != nil {
		_ = os.Remove(tmpPath)
		return nil, fmt.Errorf("%w: %v", ErrFetch, err)
	}

	imageSum, _, err := hashFile(final)
	if err != nil {
		return nil, err
	}
	f.logger.Debug("artwork cached", "source", source, "bytes", written, "type", contentType)
	return &Item{Path: final, ContentType: contentType, SHA256: imageSum, Bytes: written, Source: source}, nil
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

func extensionFor(contentType string) (string, bool) {
	for ext, known := range contentTypes {
		if known == contentType {
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
