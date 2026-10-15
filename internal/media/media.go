// Package media owns the download-first media pipeline: it fetches a variant's
// audio, stores it as Ogg/Opus under <storageDir>/media, and serves the
// finished files.
//
// Ensure is idempotent and deduplicated: concurrent callers for one variant
// share a single download, and a file becomes visible only once it is
// complete, because it is written to a temp path and renamed into place.
package media

import (
	"container/list"
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
	"sync/atomic"
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

// Status reports a variant's download state and what it is doing about it.
//
// Progress is the fraction of the current Stage that is done, and -1 when
// there is no count to give: a provider that has not said how large the file
// will be, or a stage that has nothing to count. Bytes and TotalBytes carry the
// numbers a client shows beside the bar - the download's bytes, or a
// conversion's milliseconds - and TotalBytes <= 0 means unknown, which is what
// makes a bar sweep instead of filling.
type Status struct {
	VariantID  uuid.UUID
	State      State
	Stage      string
	Progress   float64
	Err        string
	DurationMs int64
	Bytes      int64
	TotalBytes int64
	Path       string
}

// progressUnknown is the fraction for "nothing countable": a client draws a
// sweep rather than a fill, which is the honest answer while a provider is
// starting up or a length is not known.
const progressUnknown = -1

// fraction is done/total clamped to [0,1], or progressUnknown when there is no
// total to divide by.
func fraction(done, total int64) float64 {
	if total <= 0 {
		return progressUnknown
	}
	value := float64(done) / float64(total)
	switch {
	case value < 0:
		return 0
	case value > 1:
		return 1
	default:
		return value
	}
}

// sizeOf is a path's size, or 0 when it is not there yet.
func sizeOf(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// downloadWatchPoll is how often the fallback watches the file grow. A quarter
// of a second is a bar that moves smoothly and a stat per variant per tick,
// which is nothing beside the transfer it is watching.
const downloadWatchPoll = 250 * time.Millisecond

// watchDownloadGrowth reports the size of the file being written, for a
// provider that says nothing about its own progress. It stops as soon as the
// provider has spoken - its numbers are the real ones - and when the download
// is over.
func (m *Manager) watchDownloadGrowth(variantID uuid.UUID, tmp string, spoke *atomic.Bool, done <-chan struct{}) {
	ticker := time.NewTicker(downloadWatchPoll)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if spoke.Load() {
				return
			}
			size := sizeOf(tmp)
			if size == 0 {
				continue
			}
			status := m.getStatus(variantID)
			if status.State != StateDownloading {
				return
			}
			status.Bytes = size
			status.Progress = progressUnknown
			m.setStatus(status)
		}
	}
}

// Resolved is what a media request has already looked up: the variant, for the
// provider its bytes come from and the name the answer carries, and its finished
// file when there is one. A serve path used to fetch the same two rows again -
// one per query, one per stat - before the first byte; resolving them once at the
// edge and handing them down is what removes that.
type Resolved struct {
	Variant *store.Variant
	File    *store.MediaFile
	// FileErr is what the file lookup returned, so that a serve can tell a
	// variant with no rendition apart from a lookup that failed.
	FileErr error
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

	mu          sync.Mutex
	status      map[uuid.UUID]Status
	statusLRU   *list.List
	statusItems map[uuid.UUID]*list.Element
	// serving counts the requests reading a variant's finished file, so a sweep
	// does not delete the bytes a listener is in the middle of.
	serving map[uuid.UUID]int
	// streams counts a variant's source fetches: see maxStreamsPerVariant.
	streams map[uuid.UUID]int
	// downloadTimeout is how long one attempt may take; see downloadTimeout.
	downloadTimeout  time.Duration
	background       map[uuid.UUID]backgroundFetch
	paused           []uuid.UUID
	downloadAttempts int
	downloadBackoff  time.Duration

	// evictMu guards evicting: a sweep runs at most one at a time, and never on
	// the request that grew the cache. evictAgain carries a request that arrived
	// while one was running, so no request is lost to coalescing.
	evictMu    sync.Mutex
	evicting   bool
	evictAgain bool
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
		statusLRU:        list.New(),
		statusItems:      make(map[uuid.UUID]*list.Element),
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
	} else {
		// A person's play has the pipe. The wait is here, before the
		// singleflight, and not inside the shared call: a play arriving for this
		// very variant must never join a call that is parked on the gate, because
		// it would wait on a gate only its own departure could open. And it is
		// the caller's context that ends it, so a request - or the prefetcher's
		// pass - that gives up stops waiting rather than holding a place for the
		// rest of the session.
		if err := m.primary.wait(ctx); err != nil {
			return "", err
		}
		// The file may have landed while this waited.
		if path, ok := m.ready(ctx, variantID); ok {
			return path, nil
		}
	}

	// The download outlives whichever caller started it: a client hanging up
	// must not abort a shared transfer for everybody else. It is still bounded -
	// each attempt has its own deadline, and the probe and transcode that follow
	// are bounded inside ffmpeg - which is what keeps a stalled provider or a
	// wedged binary from pinning the variant for the rest of the session.
	detached := context.WithoutCancel(ctx)

	v, err, _ := m.sf.Do(variantID.String(), func() (any, error) {
		return m.fetch(detached, variantID, primary)
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

// Resolve looks up the two rows a media request needs: the variant, and its
// finished file when there is one. The request resolves them once and hands the
// result to Stream and ServeFile, so neither looks the same rows up again on the
// way to the first byte. A variant that cannot be read is left nil rather than
// failing the lookup: the name and the serve are decided from what is there.
func (m *Manager) Resolve(ctx context.Context, variantID uuid.UUID) Resolved {
	variant, _ := m.db.Variant(ctx, variantID)
	file, fileErr := m.db.MediaFile(ctx, variantID)
	return Resolved{Variant: variant, File: file, FileErr: fileErr}
}

// fetch downloads, probes and stores one variant. It runs at most once per
// variant at a time (singleflight).
//
// A background fetch registers itself while it runs, so a play arriving can
// pause it. It does not wait for the pipe here: ensure waited before entering the
// singleflight, so by the time any fetch runs nobody can join a call that is
// parked on the gate. The fetch that is itself a play neither waits nor
// registers: it holds the gate from ensure and nothing cancels it.
func (m *Manager) fetch(ctx context.Context, variantID uuid.UUID, primary bool) (string, error) {
	if path, ok := m.ready(ctx, variantID); ok {
		return path, nil
	}
	if !primary {
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

	m.setStatus(Status{VariantID: variantID, State: StateDownloading, Stage: provider.StageDownload, Progress: progressUnknown})

	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		m.fail(variantID, err)
		return "", fmt.Errorf("media: create dir: %w", err)
	}

	final := m.pathFor(variantID)
	tmp := final + ".part"
	removeTmp := func() { _ = os.Remove(tmp) }

	// What the provider is doing reaches the status endpoint through here, and
	// watching the file grow covers a provider that says nothing: the numbers a
	// client draws are the provider's when it has them and the file's real size
	// when it does not, never an invented fraction.
	var spoke atomic.Bool
	watchDone := make(chan struct{})
	go m.watchDownloadGrowth(variantID, tmp, &spoke, watchDone)

	progress := func(p provider.Progress) {
		spoke.Store(true)
		m.setStatus(Status{
			VariantID:  variantID,
			State:      StateDownloading,
			Stage:      p.Stage,
			Bytes:      p.Done,
			TotalBytes: p.Total,
			Progress:   fraction(p.Done, p.Total),
		})
	}

	downloadErr := m.downloadWithRetry(ctx, variant.Provider, variant.ProviderTrackID, tmp, progress)
	close(watchDone)
	if downloadErr != nil {
		removeTmp()
		m.fail(variantID, downloadErr)
		return "", downloadErr
	}
	m.setStatus(Status{
		VariantID: variantID, State: StateDownloading,
		Stage: provider.StageStore, Progress: progressUnknown,
		Bytes: sizeOf(tmp),
	})

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
func (m *Manager) downloadWithRetry(ctx context.Context, providerName, providerTrackID, dest string, progress provider.ProgressFunc) error {
	attempts, backoff := m.ensureAttempts()
	delay := backoff
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		// Each attempt gets its own deadline: a provider that stalls is dropped
		// and tried again rather than waiting for ever, and the retry night is
		// not shortened by the time the last one burned.
		attemptCtx, cancel := context.WithTimeout(ctx, m.downloadDeadline())
		err = m.providers.Download(attemptCtx, providerName, providerTrackID, dest, progress)
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

// evictionTimeout bounds one sweep, so a database that stops answering cannot
// leave the sweep marked as running for ever.
const evictionTimeout = time.Minute

// evictionPasses is how many times one sweep may ask for a fresh list of
// victims. A rendition somebody is listening to is skipped, so one pass can
// land short of the quota; more than a couple of passes means the cache is
// almost entirely in use, and waiting for the next download is better than
// spinning.
const evictionPasses = 3

// evict asks for a quota sweep. It is called from the paths that grow the cache -
// a finished download, an import - and returns at once: the sweep itself runs
// off the request, one at a time, because re-reading the whole media table twice
// while a request is served is work nobody is waiting for. A request that arrives
// while a sweep runs is remembered rather than dropped: the running sweep may
// already have read the table, and the file this request added has to be looked
// at.
func (m *Manager) evict(ctx context.Context) {
	if m.quotaBytes <= 0 {
		return
	}
	m.evictMu.Lock()
	if m.evicting {
		m.evictAgain = true
		m.evictMu.Unlock()
		return
	}
	m.evicting = true
	m.evictMu.Unlock()
	go m.sweepLoop(ctx)
}

func (m *Manager) sweepLoop(ctx context.Context) {
	for {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), evictionTimeout)
		m.sweep(sctx)
		cancel()
		// The exit and the flag reset are one critical section, so an evict that
		// arrives as this finishes sees either "still running" - and is carried
		// by the loop - or "not running" - and starts a new one. Neither loses a
		// request.
		m.evictMu.Lock()
		if m.evictAgain {
			m.evictAgain = false
			m.evictMu.Unlock()
			continue
		}
		m.evicting = false
		m.evictMu.Unlock()
		return
	}
}

// sweep brings the media directory back under its quota, least recently served
// first. It skips anything a request is reading: the bytes may be somebody's
// song half way through, and deleting them is a listener cut off mid-word that
// the next request pays for again.
func (m *Manager) sweep(ctx context.Context) {
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

	// The list is asked for the excess, not the whole cache: the store's window
	// query stops once the running total of the least recently served files
	// covers it, so an eviction reads the rows it is about to delete rather
	// than every rendition the household owns.
	//
	// A rendition being served is skipped, which can leave a pass short - or
	// with nothing to remove at all, when the whole window is in use. A pass
	// that frees nothing widens the window by what it saw and asks again, so
	// the next one reaches past the files somebody is listening to instead of
	// re-reading them. The passes are bounded: a cache that is almost entirely
	// in use waits for the next download rather than spinning.
	excess := total - m.quotaBytes
	for pass := 0; pass < evictionPasses && total > m.quotaBytes && ctx.Err() == nil; pass++ {
		files, err := m.db.MediaEvictionCandidates(ctx, excess)
		if err != nil {
			m.logger.Warn("media: eviction list failed", "error", err)
			return
		}
		if len(files) == 0 {
			return
		}
		freed := int64(0)
		seen := int64(0)
		for _, file := range files {
			seen += file.Bytes
			if total-freed <= m.quotaBytes || ctx.Err() != nil {
				break
			}
			if m.isServing(file.VariantID) {
				continue
			}
			if err := os.Remove(file.Path); err != nil && !os.IsNotExist(err) {
				m.logger.Warn("media: could not remove", "path", file.Path, "error", err)
				continue
			}
			if err := m.db.DeleteMediaFile(ctx, file.VariantID); err != nil {
				m.logger.Warn("media: could not forget", "variant", file.VariantID, "error", err)
				continue
			}
			freed += file.Bytes
			m.logger.Info("media: evicted the least recently used rendition",
				"variant", file.VariantID, "bytes", file.Bytes, "remaining", total-freed)
		}
		if freed == 0 {
			excess += seen
			continue
		}
		total -= freed
		excess = total - m.quotaBytes
	}
}

// beginServing records that a request is reading a variant's file, and returns
// the function that says it is done. A sweep will not touch the file in between.
func (m *Manager) beginServing(variantID uuid.UUID) func() {
	m.mu.Lock()
	if m.serving == nil {
		m.serving = make(map[uuid.UUID]int)
	}
	m.serving[variantID]++
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		if n := m.serving[variantID]; n <= 1 {
			delete(m.serving, variantID)
		} else {
			m.serving[variantID] = n - 1
		}
		m.mu.Unlock()
	}
}

// isServing reports whether a request is reading a variant's file right now.
func (m *Manager) isServing(variantID uuid.UUID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.serving[variantID] > 0
}

// maxStreamsPerVariant caps how many source fetches one variant has in flight at
// once. Every stream is its own yt-dlp process, so without a cap N listeners on
// one song run N of them for the same bytes; past the cap the next listener is
// told to come back rather than starting another, and by then the download
// behind them has the file.
const maxStreamsPerVariant = 2

// beginStream takes one of a variant's stream slots, or reports false when they
// are all taken.
func (m *Manager) beginStream(variantID uuid.UUID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.streams[variantID] >= maxStreamsPerVariant {
		return false
	}
	if m.streams == nil {
		m.streams = make(map[uuid.UUID]int)
	}
	m.streams[variantID]++
	return true
}

// endStream gives a stream slot back.
func (m *Manager) endStream(variantID uuid.UUID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n := m.streams[variantID]; n <= 1 {
		delete(m.streams, variantID)
	} else {
		m.streams[variantID] = n - 1
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
//
// A known variant is answered from memory: a client watching a download polls
// this endpoint until it is ready, and a query per poll is a query per poll per
// listener. The first look at a variant still asks the database, so a file that
// was here before this process started is found, and the answer is remembered.
func (m *Manager) Status(ctx context.Context, variantID uuid.UUID) Status {
	if s, ok := m.lookupStatus(variantID); ok {
		return s
	}
	if _, ok := m.ready(ctx, variantID); ok {
		return m.getStatus(variantID)
	}
	s := Status{VariantID: variantID, State: StateNone}
	m.setStatus(s)
	return s
}

// statusCap is how many variants the in-memory status map remembers. It is a
// cache of what the database already knows, so dropping the coldest entry is
// always safe: the next Status for it looks the row up again.
const statusCap = 4096

func (m *Manager) setStatus(s Status) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status[s.VariantID] = s
	if el, ok := m.statusItems[s.VariantID]; ok {
		m.statusLRU.MoveToFront(el)
	} else {
		m.statusItems[s.VariantID] = m.statusLRU.PushFront(s.VariantID)
	}
	for m.statusLRU.Len() > statusCap {
		oldest := m.statusLRU.Back()
		id := oldest.Value.(uuid.UUID)
		m.statusLRU.Remove(oldest)
		delete(m.statusItems, id)
		delete(m.status, id)
	}
}

func (m *Manager) getStatus(variantID uuid.UUID) Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.statusItems[variantID]; ok {
		m.statusLRU.MoveToFront(el)
	}
	return m.status[variantID]
}

func (m *Manager) lookupStatus(variantID uuid.UUID) (Status, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.status[variantID]
	if el, ok := m.statusItems[variantID]; ok {
		m.statusLRU.MoveToFront(el)
	}
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
