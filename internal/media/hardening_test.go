package media

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kyleraykbs/MusOak/internal/provider"
	"github.com/kyleraykbs/MusOak/internal/store"
)

// flakyProvider fails the first failures calls, then serves the fixture.
type flakyProvider struct {
	fakeProvider
	failures int32
	calls    atomic.Int32
}

func (f *flakyProvider) Download(ctx context.Context, id, dest string, progress provider.ProgressFunc) error {
	if f.calls.Add(1) <= f.failures {
		return errors.New("transient provider failure")
	}
	return f.fakeProvider.Download(ctx, id, dest, progress)
}

func TestEnsureRetriesTransientFailures(t *testing.T) {
	dir := t.TempDir()
	fixture := tone(t, dir, "fixture.opus", 0.5)

	flaky := &flakyProvider{fakeProvider: fakeProvider{name: "ytmusic", fixture: fixture}, failures: 2}
	m, db := newTestManager(t, flaky)
	m.SetDownloadRetry(3, time.Millisecond)
	variant := seedVariant(t, db, true, "ytmusic")

	path, err := m.Ensure(context.Background(), variant.ID)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if got := flaky.calls.Load(); got != 3 {
		t.Errorf("download attempts = %d, want 3 (two failures, then success)", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file missing after retries: %v", err)
	}
	if status := m.Status(context.Background(), variant.ID); status.State != StateReady {
		t.Errorf("status = %+v, want ready", status)
	}
}

func TestEnsureGivesUpAfterTheRetryBudget(t *testing.T) {
	dir := t.TempDir()
	fixture := tone(t, dir, "fixture.opus", 0.5)

	flaky := &flakyProvider{fakeProvider: fakeProvider{name: "ytmusic", fixture: fixture}, failures: 100}
	m, db := newTestManager(t, flaky)
	m.SetDownloadRetry(2, time.Millisecond)
	variant := seedVariant(t, db, true, "ytmusic")

	if _, err := m.Ensure(context.Background(), variant.ID); err == nil {
		t.Fatal("want an error after the retry budget is spent")
	}
	if got := flaky.calls.Load(); got != 2 {
		t.Errorf("download attempts = %d, want 2", got)
	}
	if status := m.Status(context.Background(), variant.ID); status.State != StateFailed {
		t.Errorf("status = %+v, want failed", status)
	}
}

// TestEvictionRemovesLeastRecentlyUsed checks the disk quota: once the cache is
// over it, the renditions nobody has played are removed first, and the one
// still in use survives.
func TestEvictionRemovesLeastRecentlyUsed(t *testing.T) {
	dir := t.TempDir()
	fixture := tone(t, dir, "fixture.opus", 0.5)
	size := fileSize(t, fixture)

	fake := &fakeProvider{name: "ytmusic", fixture: fixture}
	db := openTestDB(t)
	registry := newTestRegistry(t, fake)

	// Room for two renditions, so a third forces an eviction.
	manager := New(t.TempDir(), 2*size, db, registry, nil, discardLogger())
	manager.SetDownloadRetry(1, time.Millisecond)

	first := seedVariant(t, db, true, "ytmusic")
	second := seedVariant(t, db, true, "ytmusic")
	third := seedVariant(t, db, true, "ytmusic")

	ctx := context.Background()
	if _, err := manager.Ensure(ctx, first.ID); err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	if _, err := manager.Ensure(ctx, second.ID); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}

	// The oldest rendition is played again, which must protect it.
	if _, err := db.MediaFile(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	doomed, err := db.MediaFile(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.TouchMediaFile(ctx, first.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	if _, err := manager.Ensure(ctx, third.ID); err != nil {
		t.Fatalf("third Ensure: %v", err)
	}

	waitForEviction(t, func() bool {
		_, err := db.MediaFile(ctx, second.ID)
		return errors.Is(err, store.ErrNotFound)
	}, "evicted the least recently used rendition")
	if _, err := os.Stat(doomed.Path); !os.IsNotExist(err) {
		t.Errorf("evicted file is still on disk: %v", err)
	}
	if _, err := db.MediaFile(ctx, first.ID); err != nil {
		t.Errorf("the recently played rendition must survive: %v", err)
	}
	if _, err := db.MediaFile(ctx, third.ID); err != nil {
		t.Errorf("the freshest rendition must survive: %v", err)
	}

	total, err := db.MediaBytes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if total > 2*size {
		t.Errorf("cache is %d bytes, still over the %d byte quota", total, 2*size)
	}
}

func TestNoQuotaMeansNoEviction(t *testing.T) {
	dir := t.TempDir()
	fixture := tone(t, dir, "fixture.opus", 0.5)

	fake := &fakeProvider{name: "ytmusic", fixture: fixture}
	m, db := newTestManager(t, fake)
	manager := New(m.dir, 0, db, newTestRegistry(t, fake), nil, discardLogger())
	manager.SetDownloadRetry(1, time.Millisecond)

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		variant := seedVariant(t, db, true, "ytmusic")
		if _, err := manager.Ensure(ctx, variant.ID); err != nil {
			t.Fatalf("Ensure: %v", err)
		}
	}
	files, err := db.MediaFiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Errorf("media files = %d, want 3 (nothing evicted without a quota)", len(files))
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

// An upload is somebody's only copy: a provider can serve a rendition again, and
// nothing can serve this. Eviction must skip it and the quota must not count it,
// or a full cache deletes the uploads first - they are the oldest thing in it -
// and stays over quota while doing so.
func TestEvictionLeavesUploadsAlone(t *testing.T) {
	dir := t.TempDir()
	fixture := tone(t, dir, "fixture.opus", 0.5)
	size := fileSize(t, fixture)

	fake := &fakeProvider{name: "ytmusic", fixture: fixture}
	db := openTestDB(t)
	registry := newTestRegistry(t, fake)

	// Room for exactly one rendition, uploads excluded.
	manager := New(t.TempDir(), size, db, registry, nil, discardLogger())
	manager.SetDownloadRetry(1, time.Millisecond)

	ctx := context.Background()
	upload := seedUpload(t, db, manager, size)

	first := seedVariant(t, db, true, "ytmusic")
	if _, err := manager.Ensure(ctx, first.ID); err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	second := seedVariant(t, db, true, "ytmusic")
	if _, err := manager.Ensure(ctx, second.ID); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}

	waitForEviction(t, func() bool {
		total, err := db.MediaBytes(ctx)
		return err == nil && total <= size
	}, "brought the cache back under the quota")

	if _, err := db.MediaFile(ctx, upload.VariantID); err != nil {
		t.Errorf("the upload's row was evicted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(manager.Dir(), upload.VariantID.String()+".opus")); err != nil {
		t.Errorf("the upload's file was deleted: %v", err)
	}
	total, err := db.MediaBytes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if total > size {
		t.Errorf("cache is %d bytes, over the %d byte quota: uploads must not count", total, size)
	}
}

// waitForCalls blocks until the fake provider has been asked to download at
// least want times, or the test fails.
func waitForCalls(t *testing.T, f *fakeProvider, want int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f.calls.Load() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("downloads = %d, want at least %d", f.calls.Load(), want)
}

// waitForEviction waits for the paced sweep to reach what a test is watching.
// Eviction no longer runs on the request that finished a download, so the
// assertion has to wait for it rather than read the table straight after Ensure.
func waitForEviction(t *testing.T, met func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if met() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the sweep never %s", what)
}

// seedRendition puts a finished file and its row on disk without downloading it,
// for the tests that watch what a sweep does to files already there.
func seedRendition(t *testing.T, db *store.DB, manager *Manager, id uuid.UUID, data []byte) {
	t.Helper()
	if err := os.MkdirAll(manager.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	path := manager.pathFor(id)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertMediaFile(context.Background(), &store.MediaFile{
		VariantID: id,
		Path:      path,
		SHA256:    id.String(),
		Bytes:     int64(len(data)),
	}); err != nil {
		t.Fatal(err)
	}
}

// A sweep must not delete the file a request is reading. Cutting a listener off
// mid-word to free space is worse than briefly holding the quota: the bytes are
// paid for again by the next request, and the listener hears a skip that is not
// in the music.
func TestEvictionLeavesAServedFileAlone(t *testing.T) {
	dir := t.TempDir()
	fixture := tone(t, dir, "fixture.opus", 0.5)
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}

	fake := &fakeProvider{name: "ytmusic", fixture: fixture}
	db := openTestDB(t)
	// Room for one rendition, so a sweep with two must take one.
	manager := New(t.TempDir(), int64(len(data)), db, newTestRegistry(t, fake), nil, discardLogger())
	ctx := context.Background()

	first := seedVariant(t, db, true, "ytmusic")
	second := seedVariant(t, db, true, "ytmusic")
	seedRendition(t, db, manager, first.ID, data)
	seedRendition(t, db, manager, second.ID, data)
	// The first is the oldest, so it is the one a sweep would take first.
	if err := db.TouchMediaFile(ctx, second.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	// It is being read: the sweep takes the other one and leaves it.
	done := manager.beginServing(first.ID)
	manager.sweepQuota(ctx)
	if _, err := db.MediaFile(ctx, first.ID); err != nil {
		t.Errorf("the rendition being read was evicted: %v", err)
	}
	if _, err := db.MediaFile(ctx, second.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the sweep did not fall back to the idle rendition: %v", err)
	}
	done()
}

// A download somebody is waiting to hear takes the pipe: the ones running for
// anything else are cancelled, and none starts until it has finished.
func TestEnsurePrimaryTakesThePipe(t *testing.T) {
	dir := t.TempDir()
	fixture := tone(t, dir, "fixture.opus", 1.0)

	// Slow enough to watch: three variants from one provider - one already
	// downloading in the background, the one a person is waiting on, and one
	// that would be fetched next.
	fake := &fakeProvider{name: "ytmusic", fixture: fixture, delay: 300 * time.Millisecond}
	m, db := newTestManager(t, fake)

	background := seedVariant(t, db, true, "ytmusic")
	playing := seedVariant(t, db, true, "ytmusic")
	next := seedVariant(t, db, true, "ytmusic")

	backgroundErr := make(chan error, 1)
	go func() {
		_, err := m.Ensure(context.Background(), background.ID)
		backgroundErr <- err
	}()
	waitForCalls(t, fake, 1)

	// The song the player is waiting to hear.
	playingErr := make(chan error, 1)
	go func() {
		_, err := m.EnsurePrimary(context.Background(), playing.ID)
		playingErr <- err
	}()
	waitForCalls(t, fake, 2)

	// A fetch that was about to start waits rather than competing with it.
	nextErr := make(chan error, 1)
	go func() {
		_, err := m.Ensure(context.Background(), next.ID)
		nextErr <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if got := fake.calls.Load(); got != 2 {
		t.Errorf("downloads = %d while a play is downloading, want 2: something else was allowed to start", got)
	}

	if err := <-playingErr; err != nil {
		t.Fatalf("the play failed: %v", err)
	}
	if err := <-backgroundErr; !errors.Is(err, context.Canceled) {
		t.Errorf("the background download = %v, want it cancelled for the play", err)
	}
	if err := <-nextErr; err != nil {
		t.Fatalf("the fetch that waited failed: %v", err)
	}
}

// A play pauses the background downloads it interrupts and puts them back when
// it is over. Killing them is what leaves a room's next songs cold: the fetch
// that was warming the queue is thrown away, nothing re-issues it, and every
// transition pays for a download nobody expected.
func TestEnsurePrimaryPausesAndPutsBack(t *testing.T) {
	dir := t.TempDir()
	fixture := tone(t, dir, "fixture.opus", 1.0)
	fake := &fakeProvider{name: "ytmusic", fixture: fixture, delay: 300 * time.Millisecond}
	m, db := newTestManager(t, fake)

	warming := seedVariant(t, db, true, "ytmusic")
	playing := seedVariant(t, db, true, "ytmusic")

	// A background download, as the prefetcher starts one.
	firstDone := make(chan error, 1)
	go func() {
		_, err := m.Ensure(context.Background(), warming.ID)
		firstDone <- err
	}()
	waitForCalls(t, fake, 1)

	// Somebody plays something: the warming fetch is stopped for it.
	if _, err := m.EnsurePrimary(context.Background(), playing.ID); err != nil {
		t.Fatalf("the play failed: %v", err)
	}
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Errorf("the warming download = %v, want it paused for the play", err)
	}

	// And it is put back afterwards, without anybody asking again.
	deadline := time.Now().Add(5 * time.Second)
	for {
		ready := m.Status(context.Background(), warming.ID).State == StateReady
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the paused download was not put back (status %s, downloads %d)",
				m.Status(context.Background(), warming.ID).State, fake.calls.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := fake.calls.Load(); got < 3 {
		t.Errorf("downloads = %d, want the paused one asked for again (1 warming, 1 play, 1 put back)", got)
	}
}

// A provider that stalls must not hold a variant in "downloading" for the rest
// of the session. The fetch runs on a context detached from its caller's - a
// client hanging up must not abort a transfer others are sharing - and detached
// also means no deadline, so a stalled yt-dlp left every member of a room
// waiting on a file that was never coming. Each attempt is bounded now.
func TestEnsureGivesUpOnAStalledDownload(t *testing.T) {
	dir := t.TempDir()
	fixture := tone(t, dir, "fixture.opus", 0.5)
	fake := &fakeProvider{name: "ytmusic", fixture: fixture, delay: time.Hour}
	m, db := newTestManager(t, fake)
	m.SetDownloadTimeout(80 * time.Millisecond)
	// The retry schedule is the other half of how long this takes: a stall is
	// worth one more try, and the test does not wait for the real backoff.
	m.SetDownloadRetry(2, 10*time.Millisecond)

	variant := seedVariant(t, db, true, "ytmusic")

	started := time.Now()
	_, err := m.Ensure(context.Background(), variant.ID)
	if err == nil {
		t.Fatal("a stalled download was reported as a success")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("gave up after %s, want it bounded by the attempts and their timeout", elapsed)
	}
	if state := m.Status(context.Background(), variant.ID).State; state != StateFailed {
		t.Errorf("state = %s, want %s", state, StateFailed)
	}
	if _, statErr := os.Stat(m.pathFor(variant.ID)); !os.IsNotExist(statErr) {
		t.Errorf("a partial file was left at the destination: %v", statErr)
	}
}

// A play must never join a background fetch that is parked on the gate. That
// fetch is waiting for the pipe, and only a play's own departure opens it, so a
// play that joins one waits for itself: EnsurePrimary never returns, the hold on
// the gate it took never lifts, and every fetch behind it waits for the life of
// the process. The deadlock was exactly this - Ensure of V queued behind another
// play, then EnsurePrimary of V - and it is why the gate wait happens before the
// shared call rather than inside it.
func TestEnsurePrimaryDoesNotJoinAGateBlockedBackground(t *testing.T) {
	dir := t.TempDir()
	fixture := tone(t, dir, "fixture.opus", 0.5)

	// Slow enough that the gate is still held while the calls below line up.
	fake := &fakeProvider{name: "ytmusic", fixture: fixture, delay: 800 * time.Millisecond}
	m, db := newTestManager(t, fake)

	other := seedVariant(t, db, true, "ytmusic")
	playing := seedVariant(t, db, true, "ytmusic")

	// A play for one song holds the pipe.
	otherDone := make(chan error, 1)
	go func() {
		_, err := m.EnsurePrimary(context.Background(), other.ID)
		otherDone <- err
	}()
	waitForCalls(t, fake, 1)

	// A background fetch of a different song queues in the gate behind it.
	queuedDone := make(chan error, 1)
	go func() {
		_, err := m.Ensure(context.Background(), playing.ID)
		queuedDone <- err
	}()
	time.Sleep(150 * time.Millisecond)

	// A play for that very song must not wait on a gate only it could open: it
	// fetches, and answers.
	done := make(chan error, 1)
	go func() {
		_, err := m.EnsurePrimary(context.Background(), playing.ID)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the play failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("EnsurePrimary hung joining a background fetch parked on the gate")
	}

	// The second play replaces the first, so the first is stopped - that is the
	// skip rule now, not a failure of the gate.
	if err := <-otherDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("the first play = %v, want it cancelled by the play that replaced it", err)
	}
	if err := <-queuedDone; err != nil {
		t.Fatalf("the queued background fetch failed: %v", err)
	}
}

// Skipping a song during its download stops that download. The listener is not
// waiting on it any more, and leaving it running shares the provider's pipe
// with the song they are waiting for.
func TestANewPlayStopsTheDownloadItReplaced(t *testing.T) {
	dir := t.TempDir()
	fixture := tone(t, dir, "fixture.opus", 0.5)
	fake := &fakeProvider{name: "ytmusic", fixture: fixture, delay: 300 * time.Millisecond}
	m, db := newTestManager(t, fake)

	skipped := seedVariant(t, db, true, "ytmusic")
	playing := seedVariant(t, db, true, "ytmusic")

	skippedDone := make(chan error, 1)
	go func() {
		_, err := m.EnsurePrimary(context.Background(), skipped.ID)
		skippedDone <- err
	}()
	waitForCalls(t, fake, 1)

	// The listener skips: the next song becomes the one being waited on.
	if _, err := m.EnsurePrimary(context.Background(), playing.ID); err != nil {
		t.Fatalf("the play that replaced it failed: %v", err)
	}

	if err := <-skippedDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("the skipped download = %v, want it stopped", err)
	}
	if state := m.Status(context.Background(), skipped.ID).State; state != StateNone {
		t.Errorf("the skipped song's state = %s, want %s: a cancelled download is not a failed one", state, StateNone)
	}
	if state := m.Status(context.Background(), playing.ID).State; state != StateReady {
		t.Errorf("the song that replaced it = %s, want %s", state, StateReady)
	}
	if _, statErr := os.Stat(m.pathFor(skipped.ID)); !os.IsNotExist(statErr) {
		t.Errorf("the skipped song's partial file was left behind: %v", statErr)
	}
}

// A rendition nobody wants any more stops fetching: the song left the queue, or
// the queue was replaced. The next ask starts it again from scratch.
func TestCancelStopsABackgroundDownload(t *testing.T) {
	dir := t.TempDir()
	fixture := tone(t, dir, "fixture.opus", 0.5)
	fake := &fakeProvider{name: "ytmusic", fixture: fixture, delay: 300 * time.Millisecond}
	m, db := newTestManager(t, fake)
	variant := seedVariant(t, db, true, "ytmusic")
	ctx := context.Background()

	done := make(chan error, 1)
	go func() {
		_, err := m.Ensure(ctx, variant.ID)
		done <- err
	}()
	waitForCalls(t, fake, 1)

	m.Cancel(variant.ID)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("the cancelled download = %v, want it stopped", err)
	}
	if _, statErr := os.Stat(m.pathFor(variant.ID)); !os.IsNotExist(statErr) {
		t.Errorf("a partial file was left behind: %v", statErr)
	}

	// Cancelling again is a no-op, and the song is still fetchable when it is
	// wanted again.
	m.Cancel(variant.ID)
	if _, err := m.Ensure(ctx, variant.ID); err != nil {
		t.Fatalf("the fetch after a cancel failed: %v", err)
	}
	if state := m.Status(ctx, variant.ID).State; state != StateReady {
		t.Errorf("state after a fresh fetch = %s, want %s", state, StateReady)
	}
}
