package media

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kyleraykbs/MusOak/internal/store"
)

// The tiers, as they will be read back out of the store: a playlist or a
// favourite is for ever, one listener is a week, a second listener makes it
// three months, each further listener adds a month up to a year, and a fetched
// song nobody played is a week from the download.
func TestKeepUntilIsTheHouseholdsRetentionPolicy(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	week := 7 * 24 * time.Hour
	month := 30 * 24 * time.Hour
	threeMonths := 90 * 24 * time.Hour
	year := 365 * 24 * time.Hour

	cases := []struct {
		name       string
		downloaded time.Time
		lastPlay   time.Time
		listeners  int
		pinned     bool
		want       time.Time
	}{
		{"a playlist keeps it for ever", now.Add(-year), now.Add(-year), 1, true, time.Time{}},
		{"a favourite keeps it for ever", now.Add(-year), time.Time{}, 0, true, time.Time{}},
		{"fetched and never played", now, time.Time{}, 0, false, now.Add(week)},
		{"one listener", now.Add(-year), now, 1, false, now.Add(week)},
		{"two listeners", now.Add(-year), now, 2, false, now.Add(threeMonths)},
		{"three listeners", now.Add(-year), now, 3, false, now.Add(threeMonths + month)},
		{"a household of ten", now.Add(-year), now, 10, false, now.Add(threeMonths + 8*month)},
		{"past the cap, the span stops at a year", now.Add(-year), now, 30, false, now.Add(year)},
	}
	for _, test := range cases {
		got := KeepUntil(test.downloaded, test.lastPlay, test.listeners, test.pinned)
		if !got.Equal(test.want) {
			t.Errorf("%s: KeepUntil = %v, want %v", test.name, got, test.want)
		}
	}
}

// Sweeping deletes what has run out and nothing else, and takes the rows with
// the files: a file gone but a row left is a variant the server believes it can
// serve.
func TestSweepRetentionDeletesOnlyWhatHasExpired(t *testing.T) {
	m, db := newTestManager(t, nil)
	ctx := context.Background()
	dir := m.Dir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	makeFile := func(name string) (string, int64) {
		path := dir + "/" + name
		if err := os.WriteFile(path, []byte("not a real song, but bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return path, info.Size()
	}

	// A track per case, with a rendition of each on disk.
	seed := func(title string, downloaded time.Time, play func(trackID uuid.UUID)) (uuid.UUID, string) {
		track := &store.Track{Title: title}
		if err := db.CreateTrack(ctx, track); err != nil {
			t.Fatal(err)
		}
		variant := &store.Variant{TrackID: track.ID, Provider: "ytmusic", ProviderTrackID: "v-" + title}
		if err := db.CreateVariant(ctx, variant); err != nil {
			t.Fatal(err)
		}
		path, size := makeFile(title + ".opus")
		if err := db.UpsertMediaFile(ctx, &store.MediaFile{
			VariantID: variant.ID, Path: path, Bytes: size,
			DownloadedAt: downloaded, AccessedAt: downloaded,
		}); err != nil {
			t.Fatal(err)
		}
		if play != nil {
			play(track.ID)
		}
		return variant.ID, path
	}

	old := time.Now().Add(-400 * 24 * time.Hour)
	expiredVariant, expiredPath := seed("expired", old, nil)
	freshVariant, freshPath := seed("fresh", time.Now(), nil)

	freed := m.SweepNow(ctx)

	if freed <= 0 {
		t.Error("the sweep freed nothing, but a song fetched 400 days ago and never played should have gone")
	}
	if _, err := os.Stat(expiredPath); !os.IsNotExist(err) {
		t.Errorf("the expired song's file is still on disk: %v", err)
	}
	if _, err := db.MediaFile(ctx, expiredVariant); err == nil {
		t.Error("the expired song's row is still in the store")
	}
	if _, err := os.Stat(freshPath); err != nil {
		t.Errorf("the song fetched just now was removed: %v", err)
	}
	if _, err := db.MediaFile(ctx, freshVariant); err != nil {
		t.Errorf("the song fetched just now lost its row: %v", err)
	}
}

// A pinned song is never swept, however old and however unplayed - that is what
// "keep locally for ever" has to mean for a playlist to mean anything.
func TestSweepRetentionKeepsAPlaylistsSongForever(t *testing.T) {
	m, db := newTestManager(t, nil)
	ctx := context.Background()
	dir := m.Dir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	user := &store.User{ID: uuid.New(), Username: "keeper", PasswordHash: "x"}
	if err := db.CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	track := &store.Track{Title: "Kept"}
	if err := db.CreateTrack(ctx, track); err != nil {
		t.Fatal(err)
	}
	variant := &store.Variant{TrackID: track.ID, Provider: "ytmusic", ProviderTrackID: "kept"}
	if err := db.CreateVariant(ctx, variant); err != nil {
		t.Fatal(err)
	}
	path := dir + "/kept.opus"
	if err := os.WriteFile(path, []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	ancient := time.Now().Add(-1000 * 24 * time.Hour)
	if err := db.UpsertMediaFile(ctx, &store.MediaFile{
		VariantID: variant.ID, Path: path, Bytes: 5, DownloadedAt: ancient, AccessedAt: ancient,
	}); err != nil {
		t.Fatal(err)
	}
	playlist := &store.Playlist{ID: uuid.New(), UserID: user.ID, Name: "Keepers"}
	if err := db.CreatePlaylist(ctx, playlist); err != nil {
		t.Fatal(err)
	}
	if err := db.AppendPlaylistItems(ctx, playlist.ID, []uuid.UUID{track.ID}); err != nil {
		t.Fatal(err)
	}

	m.SweepNow(ctx)

	if _, err := os.Stat(path); err != nil {
		t.Errorf("a playlist's song was swept: %v", err)
	}
	if _, err := db.MediaFile(ctx, variant.ID); err != nil {
		t.Errorf("a playlist's song lost its row: %v", err)
	}
}
