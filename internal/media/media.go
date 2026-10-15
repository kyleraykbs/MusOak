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

	"github.com/kyleraykbs/MusOak/internal/ffmpeg"
	"github.com/kyleraykbs/MusOak/internal/provider"
	"github.com/kyleraykbs/MusOak/internal/store"
)

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

// downloadTimeout bounds one attempt at fetching a variant.
//
// The fetch runs on a context detached from its caller's - a client hanging up
// must not abort a transfer everybody is sharing - and detached also means no
// deadline at all. A provider that stalls (a yt-dlp run on a bad line, a socket
// nobody closes) then held the variant in "downloading" for the rest of the
// session, with every member of the room waiting on a file that was never
// coming. Ten minutes is longer than any song takes and shorter than anybody
// will wait.
const downloadTimeout = 10 * time.Minute

// Manager downloads, stores and serves renditions.
type Manager struct {
	dir        string
	quotaBytes int64
	db         Store
	providers  *provider.Registry
	logger     *slog.Logger

	sf syncgroup

	// primary is the pipe a person's play holds: see EnsurePrimary.
	primary primaryGate

	mu               sync.Mutex
	status           map[uuid.UUID]Status
	// downloadTimeout is how long one attempt may take; see downloadTimeout.
	downloadTimeout time.Duration
	background       map[uuid.UUID]backgroundFetch
	paused           []uuid.UUID
	downloadAttempts int
	downloadBackoff  time.Duration
}

// backgroundFetch is a download nobody is waiting on: how to stop it, and when
// it started - a play leaves the ones that are nearly done alone, because they
// free the pipe themselves in a moment and cancelling one would throw the work
// away for nothing.
type backgroundFetch struct {
	cancel    context.CancelFunc
	startedAt time.Time
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
		downloadTimeout:  downloadTimeout,
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
//
// This is the background shape of a fetch. While a person's play is having one
// downloaded (EnsurePrimary), it waits for that to finish first.
func (m *Manager) Ensure(ctx context.Context, variantID uuid.UUID) (string, error) {
	return m.ensure(ctx, variantID, false)
}

// EnsurePrimary fetches the variant somebody is waiting to hear, and owns the
// pipe until it is done: every other download already running is cancelled, and
// none starts until this one has finished. It is otherwise the same download
// Ensure would do, shared with anybody who asked for the same file - a
// background fetch of the very variant this is waiting on is kept rather than
// cancelled, because it is the file this call wants.
func (m *Manager) EnsurePrimary(ctx context.Context, variantID uuid.UUID) (string, error) {
	return m.ensure(ctx, variantID, true)
}

func (m *Manager) ensure(ctx context.Context, variantID uuid.UUID, primary bool) (string, error) {
	if path, ok := m.ready(ctx, variantID); ok {
		return path, nil
	}
	if primary {
		// Marked before the fetch starts, so a background download that was
		// about to begin waits instead of competing with it.
		m.enterPrimary(variantID)
		defer m.leavePrimary()
	}

	// The download outlives whichever caller started it: a client hanging up
	// must not abort a shared transfer for everybody else.
	detached := context.WithoutCancel(ctx)

	v, err, _ := m.sf.Do(variantID.String(), func() (any, error) {
		return m.fetch(detached, variantID, primary)
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

// fetch downloads, probes and stores one variant. It runs at most once per
// variant at a time (singleflight).
//
// A background fetch waits for the pipe (see EnsurePrimary) and is registered
// while it runs, so that a play arriving can stop it. The fetch that is itself a
// play does neither: it never waits, and nothing cancels it.
func (m *Manager) fetch(ctx context.Context, variantID uuid.UUID, primary bool) (string, error) {
	if path, ok := m.ready(ctx, variantID); ok {
		return path, nil
	}
	if !primary {
		// A person's play has the pipe. The caller's context is what ends the
		// wait, so a request that gives up does not stay queued behind it.
		if err := m.primary.wait(ctx); err != nil {
			return "", err
		}
		// It may have arrived while this waited.
		if path, ok := m.ready(ctx, variantID); ok {
			return path, nil
		}
		var unregister func()
		ctx, unregister = m.registerBackground(ctx, variantID)
		defer unregister()
	}

	variant, err := m.db.Variant(ctx, variantID)
	if err != nil {
		return "", m.fail(variantID, err)
	}
	if !variant.Downloadable {
		return "", m.fail(variantID, fmt.Errorf("%w: %s from %s", ErrNotDownloadable, variantID, variant.Provider))
	}
	if variant.Provider == store.LocalProvider || variant.Provider == store.UploadProvider {
		// These bytes live on this disk and nowhere else, so there is nothing to
		// download them from: an upload or a local file gets its media row when
		// it arrives, and reaching here means the file is gone. Asking a
		// provider for it would only report that no such provider exists.
		return "", m.fail(variantID, fmt.Errorf("%w: %s", ErrLocalFileMissing, variantID))
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

// TrimTrailingSilence takes the silence off the end of a rendition that ends in
// some, in place, and records the new length. It reports whether it rewrote the
// file.
//
// A room plays to the end of the file, so a file that ends in a second of
// silence is a second of the room sitting quiet before the next song. Downloads
// are cut as they arrive; this is for the ones fetched before that was true.
func (m *Manager) TrimTrailingSilence(ctx context.Context, variantID uuid.UUID) (bool, error) {
	file, err := m.db.MediaFile(ctx, variantID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	info, err := os.Stat(file.Path)
	if err != nil || info.Size() == 0 {
		return false, nil
	}

	end := ffmpeg.TrailingSilence(ctx, file.Path)
	if end <= 0 {
		return false, nil
	}

	tmp := file.Path + ".trim"
	defer os.Remove(tmp)
	if err := ffmpeg.TrimTo(ctx, file.Path, tmp, end); err != nil {
		return false, err
	}
	sum, size, err := hashFile(tmp)
	if err != nil {
		return false, err
	}
	trimmed, err := ffmpeg.Duration(ctx, tmp)
	if err != nil {
		return false, err
	}
	// The rename is the point of visibility, exactly as it is for a download.
	if err := os.Rename(tmp, file.Path); err != nil {
		return false, err
	}

	before := file.DurationMs
	file.SHA256 = sum
	file.Bytes = size
	file.DurationMs = trimmed.Milliseconds()
	if err := m.db.UpsertMediaFile(ctx, file); err != nil {
		return false, err
	}
	m.logger.Info("media: cut the silence off the end",
		"variant", variantID, "was_ms", before, "now_ms", file.DurationMs)
	return true, nil
}

// TrimAll walks every rendition on disk and takes the silence off the ones that
// end in some. It is for a library fetched before the transcode cut them, and it
// is safe to run again: a file with nothing to cut is left exactly as it is.
func (m *Manager) TrimAll(ctx context.Context, progress func(done, total int)) (int, int, error) {
	files, err := m.db.MediaFiles(ctx)
	if err != nil {
		return 0, 0, err
	}
	trimmed := 0
	for i, file := range files {
		if err := ctx.Err(); err != nil {
			return trimmed, i, err
		}
		changed, err := m.TrimTrailingSilence(ctx, file.VariantID)
		switch {
		case err != nil:
			m.logger.Warn("media: could not trim a rendition", "variant", file.VariantID, "error", err)
		case changed:
			trimmed++
		}
		if progress != nil {
			progress(i+1, len(files))
		}
	}
	return trimmed, len(files), nil
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
		// Each attempt gets its own deadline: a provider that stalls is dropped
		// and tried again rather than waiting for ever, and the retry night is
		// not shortened by the time the last one burned.
		attemptCtx, cancel := context.WithTimeout(ctx, m.downloadDeadline())
		err = m.providers.Download(attemptCtx, providerName, providerTrackID, dest)
		cancel()
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

// downloadDeadline is how long one attempt may take.
func (m *Manager) downloadDeadline() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.downloadTimeout <= 0 {
		return downloadTimeout
	}
	return m.downloadTimeout
}

// SetDownloadTimeout tunes how long one download attempt may take. Tests use it
// to make a stalled provider fail in milliseconds rather than minutes.
func (m *Manager) SetDownloadTimeout(d time.Duration) {
	m.mu.Lock()
	m.downloadTimeout = d
	m.mu.Unlock()
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

// fail records a failed download and hands the error back, so that a caller can
// report the failure it has just recorded.
func (m *Manager) fail(variantID uuid.UUID, err error) error {
	m.logger.Warn("media download failed", "variant", variantID, "error", err)
	m.setStatus(Status{VariantID: variantID, State: StateFailed, Err: err.Error()})
	return err
}

// primaryGate counts the downloads somebody is waiting to hear. A background
// fetch waits while the count is above zero; a play never waits on anything.
type primaryGate struct {
	mu   sync.Mutex
	n    int
	done chan struct{}
}

func (g *primaryGate) enter() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.n == 0 {
		g.done = make(chan struct{})
	}
	g.n++
}

// leave lets go of the pipe, and reports whether that was the last hold on it.
func (g *primaryGate) leave() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n--
	if g.n > 0 {
		return false
	}
	close(g.done)
	g.done = nil
	return true
}

// wait blocks until no play is downloading, or the context ends first.
func (g *primaryGate) wait(ctx context.Context) error {
	for {
		g.mu.Lock()
		if g.n == 0 {
			g.mu.Unlock()
			return nil
		}
		done := g.done
		g.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// resumeWindow is how long a background download must have been running before
// a play leaves it alone. What is nearly done frees the pipe by itself; what
// has just started is stopped, and started again when the play is over.
const resumeWindow = 20 * time.Second

// registerBackground makes a background download cancellable and remembers it,
// so a play arriving can stop it. The returned function forgets it again.
func (m *Manager) registerBackground(ctx context.Context, variantID uuid.UUID) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	if m.background == nil {
		m.background = make(map[uuid.UUID]backgroundFetch)
	}
	m.background[variantID] = backgroundFetch{cancel: cancel, startedAt: time.Now()}
	m.mu.Unlock()
	return ctx, func() {
		m.mu.Lock()
		delete(m.background, variantID)
		m.mu.Unlock()
		cancel()
	}
}

// enterPrimary takes the pipe for the song somebody is waiting to hear: the
// background downloads that have just started are stopped so nothing shares the
// provider's bandwidth with it, and what was stopped is remembered.
//
// It is a pause, not a killing: the fetch of the very variant the play is
// waiting on is left running (it is the file this call wants, and it is shared),
// a background download past the window is left to finish on its own, and
// everything stopped here is put back when the play is over - see leavePrimary.
// Cancelling the downloads that keep the room's next songs on disk is how a
// room ends up with nothing warmed and every transition paying for a fetch.
func (m *Manager) enterPrimary(variantID uuid.UUID) {
	m.primary.enter()
	m.mu.Lock()
	now := time.Now()
	var stop []context.CancelFunc
	for id, fetch := range m.background {
		if id == variantID || now.Sub(fetch.startedAt) >= resumeWindow {
			continue
		}
		stop = append(stop, fetch.cancel)
		delete(m.background, id)
		m.paused = append(m.paused, id)
	}
	m.mu.Unlock()
	for _, cancel := range stop {
		cancel()
	}
}

// leavePrimary gives the pipe back, and puts back what the play paused: those
// downloads are asked for again now that nothing is waiting on one.
func (m *Manager) leavePrimary() {
	if !m.primary.leave() {
		// Another play is still downloading; it will put them back.
		return
	}
	m.mu.Lock()
	paused := m.paused
	m.paused = nil
	m.mu.Unlock()
	for _, variantID := range paused {
		go func(id uuid.UUID) {
			if _, err := m.Ensure(context.Background(), id); err != nil {
				m.logger.Debug("media: a paused download could not be put back", "variant", id, "error", err)
			}
		}(variantID)
	}
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
