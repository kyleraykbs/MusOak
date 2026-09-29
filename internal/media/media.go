// Package media owns the download-first media pipeline: it fetches a variant's
// audio, stores it as Ogg/Opus under <storageDir>/media, and serves the
// finished files.
//
// Ensure is idempotent and deduplicated: concurrent callers for one variant
// share a single download, and a file becomes visible only once it is
// complete, because it is written to a temp path and renamed into place.
package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/singleflight"

	"codeberg.org/kyleraykbs/prismusic/internal/ffmpeg"
	"codeberg.org/kyleraykbs/prismusic/internal/provider"
	"codeberg.org/kyleraykbs/prismusic/internal/store"
)

// LocalProvider is the provider id for files from the local library.
const LocalProvider = "local"

// ErrNotDownloadable means the variant has no downloadable source (Spotify
// versions are metadata only; Block 5 matches them to a playable variant).
var ErrNotDownloadable = errors.New("variant is not downloadable")

// ErrLocalFileMissing means a local variant lost its file on disk.
var ErrLocalFileMissing = errors.New("local variant has no file on disk")

// State is a variant's download state.
type State string

// Download states, as exposed to clients.
const (
	StateNone        State = "none"
	StateDownloading State = "downloading"
	StateReady       State = "ready"
	StateFailed      State = "failed"
)

// Status reports a variant's download state and, while downloading, a coarse
// phase progress in [0,1].
type Status struct {
	VariantID  uuid.UUID
	State      State
	Progress   float64
	Err        string
	DurationMs int64
	Bytes      int64
	Path       string
}

// Store is the slice of the repository the manager needs.
type Store interface {
	store.MediaRepo
	store.VariantRepo
	store.TrackRepo
}

// Download retry policy: transient provider failures (a flaky yt-dlp run, a
// provider hiccup) are retried with backoff.
const (
	downloadAttempts = 3
	downloadBackoff  = 2 * time.Second
)

// Manager downloads, stores and serves renditions.
type Manager struct {
	dir        string
	quotaBytes int64
	db         Store
	providers  *provider.Registry
	logger     *slog.Logger

	sf syncgroup

	mu               sync.Mutex
	status           map[uuid.UUID]Status
	downloadAttempts int
	downloadBackoff  time.Duration
}

// syncgroup is the singleflight surface the manager uses.
type syncgroup interface {
	Do(key string, fn func() (any, error)) (v any, err error, shared bool)
}

// New returns a manager storing files in dir. quotaBytes caps the directory;
// zero means no cap. When the cache grows past the quota, the least recently
// served renditions are evicted.
func New(dir string, quotaBytes int64, db Store, providers *provider.Registry, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		dir:              dir,
		quotaBytes:       quotaBytes,
		db:               db,
		providers:        providers,
		logger:           logger,
		sf:               &singleflight.Group{},
		status:           make(map[uuid.UUID]Status),
		downloadAttempts: downloadAttempts,
		downloadBackoff:  downloadBackoff,
	}
}

// Dir is where finished renditions live.
func (m *Manager) Dir() string { return m.dir }

// pathFor is the final path of a variant's rendition.
func (m *Manager) pathFor(variantID uuid.UUID) string {
	return filepath.Join(m.dir, variantID.String()+".opus")
}

// Ensure makes sure the variant has a complete file on disk and returns its
// path. Concurrent calls for the same variant share one download.
func (m *Manager) Ensure(ctx context.Context, variantID uuid.UUID) (string, error) {
	if path, ok := m.ready(ctx, variantID); ok {
		return path, nil
	}

	// The download outlives whichever caller started it: a client hanging up
	// must not abort a shared transfer for everybody else.
	detached := context.WithoutCancel(ctx)

	v, err, _ := m.sf.Do(variantID.String(), func() (any, error) {
		return m.fetch(detached, variantID)
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

// fetch downloads, probes and stores one variant. It runs at most once per
// variant at a time (singleflight).
func (m *Manager) fetch(ctx context.Context, variantID uuid.UUID) (string, error) {
	if path, ok := m.ready(ctx, variantID); ok {
		return path, nil
	}

	variant, err := m.db.Variant(ctx, variantID)
	if err != nil {
		return "", err
	}
	if !variant.Downloadable {
		return "", fmt.Errorf("%w: %s from %s", ErrNotDownloadable, variantID, variant.Provider)
	}
	if variant.Provider == LocalProvider {
		// Local variants get their media row at import time; reaching here
		// means the file disappeared from under us.
		return "", fmt.Errorf("%w: %s", ErrLocalFileMissing, variantID)
	}

	m.setStatus(Status{VariantID: variantID, State: StateDownloading, Progress: 0.10})

	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		m.fail(variantID, err)
		return "", fmt.Errorf("media: create dir: %w", err)
	}

	final := m.pathFor(variantID)
	tmp := final + ".part"
	removeTmp := func() { _ = os.Remove(tmp) }

	if err := m.downloadWithRetry(ctx, variant.Provider, variant.ProviderTrackID, tmp); err != nil {
		removeTmp()
		m.fail(variantID, err)
		return "", err
	}
	m.setStatus(Status{VariantID: variantID, State: StateDownloading, Progress: 0.70})

	duration, err := ffmpeg.Duration(ctx, tmp)
	if err != nil {
		removeTmp()
		m.fail(variantID, err)
		return "", err
	}

	sum, size, err := hashFile(tmp)
	if err != nil {
		removeTmp()
		m.fail(variantID, err)
		return "", err
	}

	// The rename is the point of visibility: readers never see a partial file.
	if err := os.Rename(tmp, final); err != nil {
		removeTmp()
		m.fail(variantID, err)
		return "", fmt.Errorf("media: move into place: %w", err)
	}

	file := &store.MediaFile{
		VariantID:  variantID,
		Path:       final,
		SHA256:     sum,
		DurationMs: duration.Milliseconds(),
		Bytes:      size,
	}
	if err := m.db.UpsertMediaFile(ctx, file); err != nil {
		_ = os.Remove(final)
		m.fail(variantID, err)
		return "", err
	}

	// The canonical track inherits a duration the first time we learn it.
	if track, err := m.db.Track(ctx, variant.TrackID); err == nil && track.DurationMs == 0 {
		if err := m.db.SetTrackDuration(ctx, variant.TrackID, file.DurationMs); err != nil {
			m.logger.Warn("media: set track duration", "track", variant.TrackID, "error", err)
		}
	}

	m.setStatus(Status{
		VariantID:  variantID,
		State:      StateReady,
		Progress:   1,
		DurationMs: file.DurationMs,
		Bytes:      file.Bytes,
		Path:       final,
	})
	m.evict(ctx)
	return final, nil
}

// SetDownloadRetry tunes the retry schedule. Tests use it to keep the suite
// fast; deployments with an unreliable provider may raise the attempts.
func (m *Manager) SetDownloadRetry(attempts int, backoff time.Duration) {
	if attempts < 1 {
		attempts = 1
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.downloadAttempts = attempts
	m.downloadBackoff = backoff
}

// ensureAttempts reads the retry policy under the lock.
func (m *Manager) ensureAttempts() (int, time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.downloadAttempts, m.downloadBackoff
}

// downloadWithRetry retries transient provider failures. A provider that cannot
// download at all (Spotify) is never retried, and neither is a cancelled
// context.
func (m *Manager) downloadWithRetry(ctx context.Context, providerName, providerTrackID, dest string) error {
	attempts, backoff := m.ensureAttempts()
	delay := backoff
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		err = m.providers.Download(ctx, providerName, providerTrackID, dest)
		if err == nil {
			return nil
		}
		if errors.Is(err, provider.ErrDownloadUnsupported) || errors.Is(err, provider.ErrNotEnabled) || ctx.Err() != nil {
			return err
		}
		if attempt == attempts {
			break
		}
		m.logger.Warn("media: download failed, retrying",
			"provider", providerName, "provider_track_id", providerTrackID,
			"attempt", attempt, "of", attempts, "error", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
	}
	return err
}

// evict brings the media directory back under its quota, least recently served
// first.
func (m *Manager) evict(ctx context.Context) {
	if m.quotaBytes <= 0 {
		return
	}
	total, err := m.db.MediaBytes(ctx)
	if err != nil {
		m.logger.Warn("media: quota check failed", "error", err)
		return
	}
	if total <= m.quotaBytes {
		return
	}

	files, err := m.db.MediaFilesByLastUse(ctx)
	if err != nil {
		m.logger.Warn("media: eviction list failed", "error", err)
		return
	}
	for _, file := range files {
		if total <= m.quotaBytes {
			break
		}
		if err := os.Remove(file.Path); err != nil && !os.IsNotExist(err) {
			m.logger.Warn("media: could not remove", "path", file.Path, "error", err)
			continue
		}
		if err := m.db.DeleteMediaFile(ctx, file.VariantID); err != nil {
			m.logger.Warn("media: could not forget", "variant", file.VariantID, "error", err)
			continue
		}
		total -= file.Bytes
		m.logger.Info("media: evicted the least recently used rendition",
			"variant", file.VariantID, "bytes", file.Bytes, "remaining", total)
	}
}

// ready reports a completed file if one exists on disk.
func (m *Manager) ready(ctx context.Context, variantID uuid.UUID) (string, bool) {
	file, err := m.db.MediaFile(ctx, variantID)
	if err != nil {
		return "", false
	}
	info, err := os.Stat(file.Path)
	if err != nil || info.Size() == 0 {
		return "", false
	}
	m.setStatus(Status{
		VariantID:  variantID,
		State:      StateReady,
		Progress:   1,
		DurationMs: file.DurationMs,
		Bytes:      file.Bytes,
		Path:       file.Path,
	})
	return file.Path, true
}

// Status reports what is known about a variant without starting any work.
func (m *Manager) Status(ctx context.Context, variantID uuid.UUID) Status {
	if _, ok := m.ready(ctx, variantID); ok {
		return m.getStatus(variantID)
	}
	if s, ok := m.lookupStatus(variantID); ok {
		return s
	}
	return Status{VariantID: variantID, State: StateNone}
}

func (m *Manager) setStatus(s Status) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status[s.VariantID] = s
}

func (m *Manager) getStatus(variantID uuid.UUID) Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status[variantID]
}

func (m *Manager) lookupStatus(variantID uuid.UUID) (Status, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.status[variantID]
	return s, ok
}

func (m *Manager) fail(variantID uuid.UUID, err error) {
	m.logger.Warn("media download failed", "variant", variantID, "error", err)
	m.setStatus(Status{VariantID: variantID, State: StateFailed, Err: err.Error()})
}

// hashFile returns the sha256 and size of a file.
func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()

	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// ReadyAt reports when a variant's file was completed, for HTTP caching.
func (m *Manager) ReadyAt(ctx context.Context, variantID uuid.UUID) (time.Time, bool) {
	file, err := m.db.MediaFile(ctx, variantID)
	if err != nil {
		return time.Time{}, false
	}
	return file.DownloadedAt, true
}
