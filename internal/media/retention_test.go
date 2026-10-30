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
	year := 12 * month

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
		{"one listener is a month", now.Add(-year), now, 1, false, now.Add(month)},
		{"two listeners are two", now.Add(-year), now, 2, false, now.Add(2 * month)},
		{"three listeners are three", now.Add(-year), now, 3, false, now.Add(3 * month)},
		{"a household of ten", now.Add(-year), now, 10, false, now.Add(10 * month)},
		{"the twelfth listener hits the cap", now.Add(-year), now, 12, false, now.Add(year)},
		{"past the cap, a listen only resets the year", now.Add(-year), now, 30, false, now.Add(year)},
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

// The tiers walked through a year, on a clock a test can move.
//
// A policy counted in months cannot be exercised by waiting, so the sweep reads
// the calendar from `m.now`. Five songs, one per rule: never played, one
// listener, three, twelve, and one a playlist pins. The assertions are at the
// calendar's own boundaries - a day past a week, a day past a month, a day past
// three, a day past the year - so an off-by-one in any tier shows up as a song
// that stayed too long or went too early.
func TestRetentionWalksTheCalendar(t *testing.T) {
	m, db := newTestManager(t, nil)
	ctx := context.Background()
	dir := m.Dir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := base
	m.now = func() time.Time { return clock }

	owner := &store.User{ID: uuid.New(), Username: "retention", PasswordHash: "x"}
	if err := db.CreateUser(ctx, owner); err != nil {
		t.Fatal(err)
	}
	pinned := &store.Playlist{ID: uuid.New(), UserID: owner.ID, Name: "Kept"}
	if err := db.CreatePlaylist(ctx, pinned); err != nil {
		t.Fatal(err)
	}

	seed := func(title string, listeners int, pin bool) (uuid.UUID, string) {
		track := &store.Track{Title: title}
		if err := db.CreateTrack(ctx, track); err != nil {
			t.Fatal(err)
		}
		variant := &store.Variant{TrackID: track.ID, Provider: "ytmusic", ProviderTrackID: "r-" + title}
		if err := db.CreateVariant(ctx, variant); err != nil {
			t.Fatal(err)
		}
		path := dir + "/" + title + ".opus"
		if err := os.WriteFile(path, []byte("bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := db.UpsertMediaFile(ctx, &store.MediaFile{
			VariantID: variant.ID, Path: path, Bytes: 5, DownloadedAt: base, AccessedAt: base,
		}); err != nil {
			t.Fatal(err)
		}
		for index := 0; index < listeners; index += 1 {
			if err := db.RecordPlay(ctx, &store.PlayRecord{
				UserID: uuid.New(), TrackID: track.ID, VariantID: variant.ID,
				StartedAt: base, PlayedMs: 30_000, Source: "web",
			}); err != nil {
				t.Fatal(err)
			}
		}
		if pin {
			if err := db.AppendPlaylistItems(ctx, pinned.ID, []uuid.UUID{track.ID}); err != nil {
				t.Fatal(err)
			}
		}
		return variant.ID, path
	}

	unplayed, unplayedPath := seed("unplayed", 0, false)
	one, onePath := seed("one", 1, false)
	three, threePath := seed("three", 3, false)
	twelve, twelvePath := seed("twelve", 12, false)
	kept, keptPath := seed("kept", 0, true)

	survives := func(variant uuid.UUID, path string) bool {
		_, err := db.MediaFile(ctx, variant)
		_, statErr := os.Stat(path)
		return err == nil && statErr == nil
	}

	// A week later: only the song nobody played has gone.
	clock = base.Add(8 * 24 * time.Hour)
	m.SweepNow(ctx)
	if survives(unplayed, unplayedPath) {
		t.Error("a song fetched eight days ago and never played is still here")
	}
	for name, variant := range map[string]struct {
		id   uuid.UUID
		path string
	}{"one": {one, onePath}, "three": {three, threePath}, "twelve": {twelve, twelvePath}, "kept": {kept, keptPath}} {
		if !survives(variant.id, variant.path) {
			t.Errorf("%s went at eight days", name)
		}
	}

	// A month and a day: the one-listener song has run out.
	clock = base.Add(31 * 24 * time.Hour)
	m.SweepNow(ctx)
	if survives(one, onePath) {
		t.Error("a song one person played is still here a month later")
	}
	if !survives(three, threePath) || !survives(twelve, twelvePath) || !survives(kept, keptPath) {
		t.Error("a shared or pinned song went at a month")
	}

	// Three months and a day: the three-listener song has run out.
	clock = base.Add(91 * 24 * time.Hour)
	m.SweepNow(ctx)
	if survives(three, threePath) {
		t.Error("a song three people played is still here after three months")
	}
	if !survives(twelve, twelvePath) {
		t.Error("a song twelve people played went at three months")
	}

	// A year and a day: the twelve-listener song has run out; the pin has not.
	clock = base.Add(361 * 24 * time.Hour)
	m.SweepNow(ctx)
	if survives(twelve, twelvePath) {
		t.Error("a song twelve people played outlived the year")
	}
	if !survives(kept, keptPath) {
		t.Error("a playlist's song was swept")
	}

	// And a play resets the clock: the kept-out song is played again, and then
	// survives past the day the old clock would have taken it.
	renewed, renewedPath := seed("renewed", 1, false)
	clock = base.Add(20 * 24 * time.Hour)
	if err := db.RecordPlay(ctx, &store.PlayRecord{
		UserID: uuid.New(), TrackID: trackIDFor(t, db, renewed), VariantID: renewed,
		StartedAt: clock, PlayedMs: 30_000, Source: "web",
	}); err != nil {
		t.Fatal(err)
	}
	clock = base.Add(31 * 24 * time.Hour)
	m.SweepNow(ctx)
	if !survives(renewed, renewedPath) {
		t.Error("a play did not reset the clock: the song went when the old month ran out")
	}
}

// trackIDFor is the track a variant belongs to, for a test that only kept the
// variant's id.
func trackIDFor(t *testing.T, db *store.DB, variantID uuid.UUID) uuid.UUID {
	t.Helper()
	variant, err := db.Variant(context.Background(), variantID)
	if err != nil {
		t.Fatal(err)
	}
	return variant.TrackID
}
