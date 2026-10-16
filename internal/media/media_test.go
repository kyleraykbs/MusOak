package media

import (
	"bytes"
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

	"github.com/dhowden/tag"
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

// mp3WithCover renders a one-second mp3 carrying an embedded JPEG cover, and
// returns the audio beside the picture it embedded so a test can compare what
// was stored with what went in. ffmpeg renders the cover first and copies it
// into the tag unchanged, which is the shape a file off somebody's disk has.
func mp3WithCover(t *testing.T, dir string) (audio, cover string) {
	t.Helper()
	if missing := ffmpeg.Missing(); len(missing) > 0 {
		t.Skipf("ffmpeg unavailable: %v", missing)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cover = filepath.Join(dir, "cover.jpg")
	if err := runFFmpeg(ctx, []string{"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "color=c=red:s=64x64:d=1", "-frames:v", "1", cover}); err != nil {
		t.Skipf("ffmpeg cannot render a cover: %v", err)
	}

	audio = filepath.Join(dir, "Covered Song.mp3")
	if err := runFFmpeg(ctx, []string{"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1.0",
		"-i", cover,
		"-map", "0:a", "-map", "1:v",
		"-c:a", "libmp3lame", "-c:v", "copy",
		"-id3v2_version", "3",
		"-metadata", "title=Covered Song",
		"-metadata", "artist=An Artist",
		"-metadata", "album=An Album",
		"-metadata:s:v", "title=Album cover",
		"-metadata:s:v", "comment=Cover (front)",
		audio}); err != nil {
		t.Skipf("ffmpeg cannot embed a cover: %v", err)
	}
	return audio, cover
}

// storedPicture is one call into the artwork storer.
type storedPicture struct {
	trackID     uuid.UUID
	contentType string
	data        []byte
}

// fakeArtworkStorer stands in for the artwork cache the server wires in: it
// remembers what it was handed rather than writing a file.
type fakeArtworkStorer struct {
	// err, when set, refuses every picture, as an image the server cannot
	// serve does.
	err error
	// answer, when set, is the source Store reports for a picture it filed.
	answer *string

	mu    sync.Mutex
	calls []storedPicture
}

func (f *fakeArtworkStorer) StoreImportedArtwork(trackID uuid.UUID, contentType string, data []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	f.calls = append(f.calls, storedPicture{trackID: trackID, contentType: contentType, data: data})
	if f.answer != nil {
		return *f.answer, nil
	}
	return "local:" + trackID.String(), nil
}

// stored returns the calls made so far.
func (f *fakeArtworkStorer) stored() []storedPicture {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]storedPicture(nil), f.calls...)
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
	return newTestManagerWithArtwork(t, p, nil)
}

// newTestManagerWithArtwork is newTestManager for an import whose cover has to
// be filed somewhere.
func newTestManagerWithArtwork(t *testing.T, p provider.Provider, artwork ArtworkStorer) (*Manager, *store.DB) {
	t.Helper()
	db := openTestDB(t)
	var providers []provider.Provider
	if p != nil {
		providers = append(providers, p)
	}
	dir, err := os.MkdirTemp("", "musoak-test-media-*")
	if err != nil {
		t.Fatal(err)
	}
	// Not t.TempDir(): a fetch the manager still has in flight when a test ends
	// can touch this path a moment later, and t.TempDir's cleanup fails the run
	// with "directory not empty" for that - a flake that says nothing about what
	// the test asserted. A fetch the manager still has in flight runs on a
	// goroutine by design, so a test cannot make that race go away; this can.
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	m := New(dir, 0, db, newTestRegistry(t, providers...), artwork, discardLogger())
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

// An imported file that carries a cover keeps it: the picture goes through the
// artwork storer and the source it answers is recorded on the track, which is
// what a browser fetches the cover from.
func TestImportRecordsEmbeddedCover(t *testing.T) {
	dir := t.TempDir()
	source, coverPath := mp3WithCover(t, dir)
	cover, err := os.ReadFile(coverPath)
	if err != nil {
		t.Fatalf("read the embedded picture: %v", err)
	}

	storer := &fakeArtworkStorer{}
	m, db := newTestManagerWithArtwork(t, nil, storer)
	ctx := context.Background()

	variant, err := m.Import(ctx, source)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	calls := storer.stored()
	if len(calls) != 1 {
		t.Fatalf("the cover was filed %d times, want 1", len(calls))
	}
	if calls[0].trackID != variant.TrackID {
		t.Errorf("cover filed for track %s, want %s", calls[0].trackID, variant.TrackID)
	}
	if calls[0].contentType != "image/jpeg" {
		t.Errorf("content type = %q, want the type the tag carried", calls[0].contentType)
	}
	if !bytes.Equal(calls[0].data, cover) {
		t.Errorf("filed %d bytes, want the %d the file embedded", len(calls[0].data), len(cover))
	}

	recorded, err := db.TrackArtwork(ctx, variant.TrackID)
	if err != nil {
		t.Fatalf("TrackArtwork: %v", err)
	}
	if recorded != "local:"+variant.TrackID.String() {
		t.Errorf("artwork_url = %q, want the source the cover was filed under", recorded)
	}
}

// A manager built without an artwork cache - every manager but the ones wired
// through the server - still imports a file that carries a cover, and records
// none, rather than tripping over the picture.
func TestImportWithoutAnArtworkStorerRecordsNoCover(t *testing.T) {
	dir := t.TempDir()
	source, _ := mp3WithCover(t, dir)

	m, db := newTestManager(t, nil)
	ctx := context.Background()
	variant, err := m.Import(ctx, source)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if _, err := db.TrackArtwork(ctx, variant.TrackID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("artwork = %v, want none recorded", err)
	}
}

// A file with no cover still imports, and records none: the picture is a bonus,
// never a condition.
func TestScanDirRecordsCoversOnlyForFilesThatCarryOne(t *testing.T) {
	dir := t.TempDir()
	mp3WithCover(t, dir)
	tone(t, dir, "Bare.opus", 0.5)

	storer := &fakeArtworkStorer{}
	m, db := newTestManagerWithArtwork(t, nil, storer)
	ctx := context.Background()
	imported, err := m.ScanDir(ctx, dir)
	if err != nil {
		t.Fatalf("ScanDir: %v", err)
	}
	if len(imported) != 2 {
		t.Fatalf("imported = %d, want 2", len(imported))
	}
	for _, variant := range imported {
		_, err := db.TrackArtwork(ctx, variant.TrackID)
		hasCover := err == nil
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("TrackArtwork: %v", err)
		}
		if want := variant.Title == "Covered Song"; hasCover != want {
			t.Errorf("track %q has a cover = %v, want %v", variant.Title, hasCover, want)
		}
	}
	if calls := storer.stored(); len(calls) != 1 {
		t.Errorf("the cover was filed %d times, want 1", len(calls))
	}
}

// Every way a cover can be unusable leaves the import alone: a picture past the
// cap is never handed to the cache, and one the cache refuses is logged rather
// than recorded. The pictures here are the ones ffmpeg will not embed for a
// test, so the seam is called the way an import calls it.
func TestUnusableCoversAreSkippedNotFatal(t *testing.T) {
	ctx := context.Background()
	storer := &fakeArtworkStorer{}
	m, db := newTestManagerWithArtwork(t, nil, storer)

	track := &store.Track{Title: "Song"}
	if err := db.CreateTrack(ctx, track); err != nil {
		t.Fatal(err)
	}

	// Past the cap the upload endpoint applies.
	m.storeCover(ctx, track.ID, &tag.Picture{MIMEType: "image/jpeg", Data: make([]byte, maxImportedArtworkBytes+1)})
	if calls := storer.stored(); len(calls) != 0 {
		t.Errorf("an oversized picture reached the cache: %d bytes", len(calls[0].data))
	}

	// A type this server does not serve: the cache refuses it.
	storer.err = errors.New("artwork: image/bmp is not an image this server serves")
	m.storeCover(ctx, track.ID, &tag.Picture{MIMEType: "image/bmp", Data: []byte("bmp")})
	if calls := storer.stored(); len(calls) != 0 {
		t.Errorf("a refused picture was stored anyway: %+v", calls)
	}

	// A cache that files nothing has nothing to record.
	storer.err = nil
	none := ""
	storer.answer = &none
	m.storeCover(ctx, track.ID, &tag.Picture{MIMEType: "image/png", Data: []byte("png")})
	if calls := storer.stored(); len(calls) != 1 {
		t.Errorf("the picture was filed %d times, want 1", len(calls))
	}

	if _, err := db.TrackArtwork(ctx, track.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("artwork = %v, want none recorded", err)
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
