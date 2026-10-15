package media

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kyleraykbs/MusOak/internal/ffmpeg"
	"github.com/kyleraykbs/MusOak/internal/provider"
	"github.com/kyleraykbs/MusOak/internal/store"
)

// tone writes a short sine-wave opus file to dir and returns its path.
func tone(t *testing.T, dir string, name string, seconds float64) string {
	t.Helper()
	if missing := ffmpeg.Missing(); len(missing) > 0 {
		t.Skipf("ffmpeg unavailable: %v", missing)
	}
	dst := filepath.Join(dir, name)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	args := []string{"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=" + trimFloat(seconds),
		"-c:a", "libopus", "-f", "opus", dst}
	if err := runFFmpeg(ctx, args); err != nil {
		t.Fatalf("render tone: %v", err)
	}
	return dst
}

func trimFloat(f float64) string {
	return strconvFormat(f)
}

// fakeProvider serves a fixture file as if it had downloaded it.
type fakeProvider struct {
	name    string
	fixture string

	calls  atomic.Int32
	delay  time.Duration
	err    error
	dests  []string
	destMu sync.Mutex
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) Capabilities() provider.Caps {
	return provider.Caps{Search: true, Download: true}
}

func (f *fakeProvider) Search(ctx context.Context, q string, opts provider.SearchOpts) ([]provider.Track, error) {
	return nil, nil
}

func (f *fakeProvider) Download(ctx context.Context, id, dest string, _ provider.ProgressFunc) error {
	f.calls.Add(1)
	f.destMu.Lock()
	f.dests = append(f.dests, dest)
	f.destMu.Unlock()

	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if f.err != nil {
		return f.err
	}
	in, err := os.Open(f.fixture)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func newTestManager(t *testing.T, p provider.Provider) (*Manager, *store.DB) {
	t.Helper()
	db := openTestDB(t)
	var providers []provider.Provider
	if p != nil {
		providers = append(providers, p)
	}
	m := New(filepath.Join(t.TempDir(), "media"), 0, db, newTestRegistry(t, providers...), discardLogger())
	return m, db
}

// openTestDB returns an in-memory store with a unique DSN per test.
func openTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open("file:media-" + uuid.NewString() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// newTestRegistry returns a registry holding the given providers.
func newTestRegistry(t *testing.T, providers ...provider.Provider) *provider.Registry {
	t.Helper()
	registry := provider.NewRegistry(discardLogger(), 5*time.Second)
	for _, p := range providers {
		registry.Register(p)
	}
	return registry
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func seedVariant(t *testing.T, db *store.DB, downloadable bool, providerName string) *store.Variant {
	t.Helper()
	ctx := context.Background()
	track := &store.Track{Title: "Song"}
	if err := db.CreateTrack(ctx, track); err != nil {
		t.Fatal(err)
	}
	variant := &store.Variant{
		TrackID:         track.ID,
		Provider:        providerName,
		ProviderTrackID: uuid.NewString(),
		Title:           "Song",
		Downloadable:    downloadable,
	}
	if err := db.CreateVariant(ctx, variant); err != nil {
		t.Fatal(err)
	}
	return variant
}

func TestEnsureDeduplicatesConcurrentDownloads(t *testing.T) {
	dir := t.TempDir()
	fixture := tone(t, dir, "fixture.opus", 1.0)

	fake := &fakeProvider{name: "ytmusic", fixture: fixture, delay: 150 * time.Millisecond}
	m, db := newTestManager(t, fake)
	variant := seedVariant(t, db, true, "ytmusic")

	const callers = 4
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		paths []string
		errs  []error
	)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			path, err := m.Ensure(context.Background(), variant.ID)
			mu.Lock()
			paths = append(paths, path)
			errs = append(errs, err)
			mu.Unlock()
		}()
	}
	wg.Wait()

	if got := fake.calls.Load(); got != 1 {
		t.Errorf("downloads = %d, want exactly one shared transfer", got)
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	for _, path := range paths {
		if path != paths[0] {
			t.Fatalf("paths differ: %v", paths)
		}
	}
	if filepath.Base(paths[0]) != variant.ID.String()+".opus" {
		t.Errorf("path = %s", paths[0])
	}

	// No partial remains, the file is complete and recorded.
	if _, err := os.Stat(paths[0] + ".part"); !os.IsNotExist(err) {
		t.Errorf("partial file left behind: %v", err)
	}
	file, err := db.MediaFile(context.Background(), variant.ID)
	if err != nil {
		t.Fatalf("MediaFile: %v", err)
	}
	if file.SHA256 == "" || file.Bytes == 0 || file.DurationMs < 900 {
		t.Errorf("media row = %+v", file)
	}
}

func TestEnsureIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	fixture := tone(t, dir, "fixture.opus", 1.0)
	fake := &fakeProvider{name: "ytmusic", fixture: fixture}
	m, db := newTestManager(t, fake)
	variant := seedVariant(t, db, true, "ytmusic")

	first, err := m.Ensure(context.Background(), variant.ID)
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	second, err := m.Ensure(context.Background(), variant.ID)
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if first != second {
		t.Errorf("paths differ: %s vs %s", first, second)
	}
	if got := fake.calls.Load(); got != 1 {
		t.Errorf("downloads = %d, want 1", got)
	}
}

func TestEnsureRefusesNonDownloadableVariant(t *testing.T) {
	fake := &fakeProvider{name: "spotify"}
	m, db := newTestManager(t, fake)
	variant := seedVariant(t, db, false, "spotify")

	_, err := m.Ensure(context.Background(), variant.ID)
	if !errors.Is(err, ErrNotDownloadable) {
		t.Fatalf("err = %v, want ErrNotDownloadable", err)
	}
	if got := fake.calls.Load(); got != 0 {
		t.Errorf("downloads = %d, want none", got)
	}
	// A refusal leaves a state behind, as every other failed fetch does: a
	// client waiting on the file polls this, and a state that never changes is
	// a spinner that never stops.
	if status := m.Status(context.Background(), variant.ID); status.State != StateFailed || status.Err == "" {
		t.Errorf("status = %+v, want failed with a message", status)
	}
}

func TestEnsureReportsFailureAndCleansUp(t *testing.T) {
	fake := &fakeProvider{name: "ytmusic", err: errors.New("upstream exploded")}
	m, db := newTestManager(t, fake)
	m.SetDownloadRetry(1, time.Millisecond)
	variant := seedVariant(t, db, true, "ytmusic")

	_, err := m.Ensure(context.Background(), variant.ID)
	if err == nil || !errors.Is(err, fake.err) {
		t.Fatalf("err = %v, want the provider error", err)
	}
	if status := m.Status(context.Background(), variant.ID); status.State != StateFailed || status.Err == "" {
		t.Errorf("status = %+v, want failed with a message", status)
	}
	if _, err := db.MediaFile(context.Background(), variant.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("media row should not exist: %v", err)
	}
	if _, err := os.Stat(m.pathFor(variant.ID)); !os.IsNotExist(err) {
		t.Errorf("stored file should not exist: %v", err)
	}
}

func TestStatusTransitions(t *testing.T) {
	dir := t.TempDir()
	fixture := tone(t, dir, "fixture.opus", 1.0)
	fake := &fakeProvider{name: "ytmusic", fixture: fixture, delay: 200 * time.Millisecond}
	m, db := newTestManager(t, fake)
	variant := seedVariant(t, db, true, "ytmusic")

	if got := m.Status(context.Background(), variant.ID); got.State != StateNone {
		t.Fatalf("initial status = %+v, want none", got)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = m.Ensure(context.Background(), variant.ID)
	}()

	// A provider that says nothing: the honest answer while its bytes are
	// somewhere this manager cannot see is "no count", and -1 is what a client
	// draws as a sweep instead of an invented fill.
	deadline := time.Now().Add(2 * time.Second)
	for {
		status := m.Status(context.Background(), variant.ID)
		if status.State == StateDownloading {
			if status.Progress != progressUnknown {
				t.Errorf("progress = %v, want %v while nothing has been reported", status.Progress, progressUnknown)
			}
			if status.Stage != provider.StageDownload {
				t.Errorf("stage = %q, want %q", status.Stage, provider.StageDownload)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never observed downloading; last = %+v", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	<-done

	status := m.Status(context.Background(), variant.ID)
	if status.State != StateReady || status.Progress != 1 || status.DurationMs < 900 {
		t.Errorf("final status = %+v", status)
	}
}

// reportingProvider says how its own download is going, which is what a bar
// draws. A manager that invented fractions instead is why this exists.
type reportingProvider struct {
	fakeProvider
	done  int64
	total int64
}

func (p *reportingProvider) Download(ctx context.Context, id, dest string, progress provider.ProgressFunc) error {
	if progress != nil {
		for _, done := range []int64{p.total / 4, p.total / 2, p.total} {
			progress(provider.Progress{Stage: provider.StageDownload, Done: done, Total: p.total})
			time.Sleep(20 * time.Millisecond)
		}
	}
	return p.fakeProvider.Download(ctx, id, dest, progress)
}

func TestAProvidersReportsReachTheStatus(t *testing.T) {
	dir := t.TempDir()
	fixture := tone(t, dir, "fixture.opus", 0.5)
	fake := &reportingProvider{
		fakeProvider: fakeProvider{name: "ytmusic", fixture: fixture, delay: 200 * time.Millisecond},
		total:        4096,
	}
	m, db := newTestManager(t, fake)
	variant := seedVariant(t, db, true, "ytmusic")

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = m.Ensure(context.Background(), variant.ID)
	}()

	// The fractions the provider reported have to appear, in order, with the
	// numbers beside them: a client's bar is drawn from exactly these.
	var seen []float64
	deadline := time.Now().Add(2 * time.Second)
	for len(seen) < 3 && time.Now().Before(deadline) {
		status := m.Status(context.Background(), variant.ID)
		if status.TotalBytes == fake.total && status.Progress >= 0 {
			if len(seen) == 0 || seen[len(seen)-1] != status.Progress {
				seen = append(seen, status.Progress)
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	<-done
	if len(seen) < 3 {
		t.Fatalf("saw %v, want the reported fractions 0.25, 0.5 and 1", seen)
	}
	if seen[0] != 0.25 || seen[1] != 0.5 || seen[2] != 1 {
		t.Errorf("progress went %v, want [0.25 0.5 1]", seen)
	}
}

func TestImportCreatesLocalVariantAndDedupes(t *testing.T) {
	dir := t.TempDir()
	source := tone(t, dir, "My Song.opus", 1.0)

	m, db := newTestManager(t, nil)
	ctx := context.Background()

	variant, err := m.Import(ctx, source)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if variant.Provider != store.LocalProvider || !variant.Downloadable {
		t.Errorf("variant = %+v", variant)
	}
	if variant.Title != "My Song" {
		t.Errorf("title = %q, want the file name when there are no tags", variant.Title)
	}
	if variant.DurationMs < 900 {
		t.Errorf("duration = %d", variant.DurationMs)
	}

	file, err := db.MediaFile(ctx, variant.ID)
	if err != nil {
		t.Fatalf("MediaFile: %v", err)
	}
	if info, err := os.Stat(file.Path); err != nil || info.Size() == 0 {
		t.Fatalf("stored file: %v", err)
	}

	// Re-importing the same bytes yields the same variant, not a copy.
	again, err := m.Import(ctx, source)
	if err != nil {
		t.Fatalf("second Import: %v", err)
	}
	if again.ID != variant.ID {
		t.Errorf("re-import created a new variant: %s vs %s", again.ID, variant.ID)
	}
	files, err := db.MediaFiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Errorf("media files = %d, want 1", len(files))
	}

	// A local variant never needs the network, yet still satisfies Ensure.
	path, err := m.Ensure(ctx, variant.ID)
	if err != nil {
		t.Fatalf("Ensure on imported variant: %v", err)
	}
	if path != file.Path {
		t.Errorf("Ensure path = %s, want %s", path, file.Path)
	}
}

func TestScanDirImportsAudioFiles(t *testing.T) {
	dir := t.TempDir()
	tone(t, dir, "one.opus", 0.5)
	tone(t, dir, "two.flac", 0.5)
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("skip me"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "nested")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	tone(t, sub, "three.opus", 0.5)

	m, _ := newTestManager(t, nil)
	imported, err := m.ScanDir(context.Background(), dir)
	if err != nil {
		t.Fatalf("ScanDir: %v", err)
	}
	if len(imported) != 3 {
		t.Errorf("imported = %d, want 3", len(imported))
	}
	for _, v := range imported {
		if v.Provider != store.LocalProvider {
			t.Errorf("variant %s provider = %s", v.ID, v.Provider)
		}
	}
}

// seedUpload stores an upload the way the API does: a user-sourced variant whose
// bytes sit in the media directory under the variant's own name.
func seedUpload(t *testing.T, db *store.DB, manager *Manager, bytes int64) *store.Upload {
	t.Helper()
	ctx := context.Background()
	user := &store.User{Username: "uploader", PasswordHash: "hash"}
	if err := db.CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	track := &store.Track{Title: "Mine"}
	if err := db.CreateTrack(ctx, track); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(manager.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	variantID := uuid.New()
	path := filepath.Join(manager.Dir(), variantID.String()+".opus")
	if err := os.WriteFile(path, []byte("the bytes somebody uploaded"), 0o644); err != nil {
		t.Fatal(err)
	}
	upload := &store.Upload{UserID: user.ID, Filename: "mine.opus"}
	variant := &store.Variant{ID: variantID, TrackID: track.ID, Title: "Mine", Downloadable: true}
	file := &store.MediaFile{Path: path, SHA256: "sum", Bytes: bytes}
	if err := db.CreateUpload(ctx, upload, variant, file); err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	return upload
}

// An upload's bytes live on this disk, so a missing file is a missing file. The
// manager used to hand the variant to a provider named after the upload, which
// answered "provider not enabled" - sending somebody to look for a switch that
// does not exist.
func TestUploadWithNoFileBlamesTheFileNotAProvider(t *testing.T) {
	manager, db := newTestManager(t, nil)
	variant := seedVariant(t, db, true, store.UploadProvider)

	_, err := manager.Ensure(context.Background(), variant.ID)
	if !errors.Is(err, ErrLocalFileMissing) {
		t.Fatalf("Ensure(upload) = %v, want ErrLocalFileMissing", err)
	}
	if errors.Is(err, provider.ErrNotEnabled) {
		t.Errorf("an upload is local, no provider is involved: %v", err)
	}

	// A client waiting for the file polls the status, and stops when it says
	// failed. A state that never changes is a spinner that never stops - which
	// is exactly how a lost upload looked from the bar.
	status := manager.Status(context.Background(), variant.ID)
	if status.State != StateFailed {
		t.Errorf("status = %q, want %q so a waiting client stops", status.State, StateFailed)
	}
	if status.Err == "" {
		t.Error("a failed status carries no reason to show")
	}
}
