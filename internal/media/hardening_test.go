package media

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"codeberg.org/kyleraykbs/prismusic/internal/store"
)

// flakyProvider fails the first failures calls, then serves the fixture.
type flakyProvider struct {
	fakeProvider
	failures int32
	calls    atomic.Int32
}

func (f *flakyProvider) Download(ctx context.Context, id, dest string) error {
	if f.calls.Add(1) <= f.failures {
		return errors.New("transient provider failure")
	}
	return f.fakeProvider.Download(ctx, id, dest)
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
	manager := New(t.TempDir(), 2*size, db, registry, discardLogger())
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

	if _, err := db.MediaFile(ctx, second.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the least recently used rendition should have been evicted, got %v", err)
	}
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
	manager := New(m.dir, 0, db, newTestRegistry(t, fake), discardLogger())
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
