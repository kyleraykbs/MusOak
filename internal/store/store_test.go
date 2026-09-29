package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

func newTestDB(t *testing.T) *DB {
	t.Helper()
	dsn := "file:store-" + uuid.NewString() + "?mode=memory&cache=shared"
	db, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func mustTrack(t *testing.T, db *DB, title string) *Track {
	t.Helper()
	tr := &Track{Title: title, DurationMs: 180_000}
	if err := db.CreateTrack(context.Background(), tr); err != nil {
		t.Fatalf("CreateTrack: %v", err)
	}
	return tr
}

func mustVariant(t *testing.T, db *DB, trackID uuid.UUID, provider, providerTrackID string) *Variant {
	t.Helper()
	v := &Variant{
		TrackID:         trackID,
		Provider:        provider,
		ProviderTrackID: providerTrackID,
		Title:           "Song",
		Artists:         []string{"Artist"},
		Album:           "Album",
		DurationMs:      180_000,
		Downloadable:    true,
		ISRC:            "USABC1234567",
	}
	if err := db.CreateVariant(context.Background(), v); err != nil {
		t.Fatalf("CreateVariant: %v", err)
	}
	return v
}

func TestTrackCRUDAndCredits(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	tr := mustTrack(t, db, "Song")
	if tr.ID == uuid.Nil || tr.CreatedAt.IsZero() {
		t.Fatalf("CreateTrack did not fill id/created_at: %+v", tr)
	}

	got, err := db.Track(ctx, tr.ID)
	if err != nil {
		t.Fatalf("Track: %v", err)
	}
	if got.Title != "Song" || got.DurationMs != 180_000 {
		t.Errorf("got %+v", got)
	}

	if _, err := db.Track(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing track: err = %v, want ErrNotFound", err)
	}

	if err := db.SetTrackArtists(ctx, tr.ID, []string{"First", "Second"}); err != nil {
		t.Fatalf("SetTrackArtists: %v", err)
	}
	artists, err := db.TrackArtists(ctx, tr.ID)
	if err != nil {
		t.Fatalf("TrackArtists: %v", err)
	}
	if len(artists) != 2 || artists[0].Name != "First" || artists[1].Name != "Second" {
		t.Errorf("artists = %+v, want ordered [First Second]", artists)
	}

	// Replacing credits must not leave stale join rows behind.
	if err := db.SetTrackArtists(ctx, tr.ID, []string{"Second"}); err != nil {
		t.Fatalf("SetTrackArtists replace: %v", err)
	}
	artists, _ = db.TrackArtists(ctx, tr.ID)
	if len(artists) != 1 || artists[0].Name != "Second" {
		t.Errorf("artists after replace = %+v", artists)
	}

	if err := db.SetTrackAlbums(ctx, tr.ID, []string{"Album"}); err != nil {
		t.Fatalf("SetTrackAlbums: %v", err)
	}
	albums, err := db.TrackAlbums(ctx, tr.ID)
	if err != nil {
		t.Fatalf("TrackAlbums: %v", err)
	}
	if len(albums) != 1 || albums[0].Title != "Album" {
		t.Errorf("albums = %+v", albums)
	}

	if err := db.SetTrackDuration(ctx, tr.ID, 1234); err != nil {
		t.Fatalf("SetTrackDuration: %v", err)
	}
	if got, _ := db.Track(ctx, tr.ID); got.DurationMs != 1234 {
		t.Errorf("duration = %d, want 1234", got.DurationMs)
	}
	if err := db.SetTrackDuration(ctx, uuid.New(), 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetTrackDuration missing: err = %v", err)
	}
	if err := db.SetTrackArtists(ctx, uuid.New(), []string{"X"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetTrackArtists missing track: err = %v", err)
	}
}

func TestEnsureArtistAndAlbumAreIdempotent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	a1, err := db.EnsureArtist(ctx, "Artist")
	if err != nil {
		t.Fatalf("EnsureArtist: %v", err)
	}
	a2, err := db.EnsureArtist(ctx, "Artist")
	if err != nil {
		t.Fatalf("EnsureArtist again: %v", err)
	}
	if a1 != a2 {
		t.Errorf("ids differ: %s vs %s", a1, a2)
	}
	a3, _ := db.EnsureArtist(ctx, "Other")
	if a3 == a1 {
		t.Error("distinct names must have distinct ids")
	}

	b1, _ := db.EnsureAlbum(ctx, "Album")
	b2, _ := db.EnsureAlbum(ctx, "Album")
	if b1 != b2 {
		t.Errorf("album ids differ: %s vs %s", b1, b2)
	}
}

func TestVariantUniquenessAndLookup(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tr := mustTrack(t, db, "Song")

	v := mustVariant(t, db, tr.ID, "ytmusic", "abc")

	dup := &Variant{TrackID: tr.ID, Provider: "ytmusic", ProviderTrackID: "abc", Title: "Song"}
	if err := db.CreateVariant(ctx, dup); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate (provider, provider_track_id): err = %v, want ErrConflict", err)
	}

	// The same provider id under another provider is a different variant.
	other := &Variant{TrackID: tr.ID, Provider: "spotify", ProviderTrackID: "abc", Title: "Song"}
	if err := db.CreateVariant(ctx, other); err != nil {
		t.Fatalf("same id, other provider: %v", err)
	}

	got, err := db.Variant(ctx, v.ID)
	if err != nil {
		t.Fatalf("Variant: %v", err)
	}
	if got.ProviderTrackID != "abc" || !got.Downloadable || got.ISRC != "USABC1234567" {
		t.Errorf("got %+v", got)
	}
	if len(got.Artists) != 1 || got.Artists[0] != "Artist" {
		t.Errorf("artists round-trip: %+v", got.Artists)
	}

	byProvider, err := db.VariantByProviderTrack(ctx, "ytmusic", "abc")
	if err != nil {
		t.Fatalf("VariantByProviderTrack: %v", err)
	}
	if byProvider.ID != v.ID {
		t.Errorf("got %s, want %s", byProvider.ID, v.ID)
	}
	if _, err := db.VariantByProviderTrack(ctx, "ytmusic", "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}

	variants, err := db.VariantsForTrack(ctx, tr.ID)
	if err != nil {
		t.Fatalf("VariantsForTrack: %v", err)
	}
	if len(variants) != 2 {
		t.Errorf("variants = %d, want 2", len(variants))
	}
}

func TestVariantForeignKeyIsEnforced(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	v := &Variant{TrackID: uuid.New(), Provider: "ytmusic", ProviderTrackID: "x"}
	if err := db.CreateVariant(ctx, v); !errors.Is(err, ErrConflict) {
		t.Errorf("unknown track: err = %v, want ErrConflict", err)
	}
}

func TestSetVariantTrackRepointsVariant(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	t1 := mustTrack(t, db, "One")
	t2 := mustTrack(t, db, "Two")
	v := mustVariant(t, db, t1.ID, "ytmusic", "vid")

	if err := db.SetVariantTrack(ctx, v.ID, t2.ID); err != nil {
		t.Fatalf("SetVariantTrack: %v", err)
	}
	if got, _ := db.VariantsForTrack(ctx, t2.ID); len(got) != 1 {
		t.Errorf("variants on target track = %d, want 1", len(got))
	}
	if got, _ := db.VariantsForTrack(ctx, t1.ID); len(got) != 0 {
		t.Errorf("variants left on source track = %d, want 0", len(got))
	}
	if err := db.SetVariantTrack(ctx, uuid.New(), t2.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestMediaFileUpsertAndDelete(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tr := mustTrack(t, db, "Song")
	v := mustVariant(t, db, tr.ID, "ytmusic", "abc")

	m := &MediaFile{VariantID: v.ID, Path: "/media/a.opus", SHA256: "aa", DurationMs: 180_000, Bytes: 100}
	if err := db.UpsertMediaFile(ctx, m); err != nil {
		t.Fatalf("UpsertMediaFile: %v", err)
	}
	got, err := db.MediaFile(ctx, v.ID)
	if err != nil {
		t.Fatalf("MediaFile: %v", err)
	}
	if got.SHA256 != "aa" || got.Bytes != 100 || got.DownloadedAt.IsZero() {
		t.Errorf("got %+v", got)
	}

	m.SHA256, m.Bytes = "bb", 200
	if err := db.UpsertMediaFile(ctx, m); err != nil {
		t.Fatalf("UpsertMediaFile update: %v", err)
	}
	got, _ = db.MediaFile(ctx, v.ID)
	if got.SHA256 != "bb" || got.Bytes != 200 {
		t.Errorf("upsert did not update: %+v", got)
	}

	if files, _ := db.MediaFiles(ctx); len(files) != 1 {
		t.Errorf("MediaFiles = %d, want 1", len(files))
	}

	if err := db.DeleteMediaFile(ctx, v.ID); err != nil {
		t.Fatalf("DeleteMediaFile: %v", err)
	}
	if _, err := db.MediaFile(ctx, v.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if err := db.DeleteMediaFile(ctx, v.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: err = %v, want ErrNotFound", err)
	}
}

func TestMediaFileForeignKeyIsEnforced(t *testing.T) {
	db := newTestDB(t)
	m := &MediaFile{VariantID: uuid.New(), Path: "/x.opus"}
	if err := db.UpsertMediaFile(context.Background(), m); !errors.Is(err, ErrConflict) {
		t.Errorf("err = %v, want ErrConflict", err)
	}
}

func TestUserCRUD(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	u := &User{Username: "kyle", PasswordHash: "hash"}
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if err := db.CreateUser(ctx, &User{Username: "kyle", PasswordHash: "other"}); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate username: err = %v, want ErrConflict", err)
	}

	byName, err := db.UserByUsername(ctx, "kyle")
	if err != nil {
		t.Fatalf("UserByUsername: %v", err)
	}
	if byName.ID != u.ID || byName.PasswordHash != "hash" {
		t.Errorf("got %+v", byName)
	}
	if got, err := db.User(ctx, u.ID); err != nil || got.Username != "kyle" {
		t.Errorf("User: %+v, %v", got, err)
	}
	if _, err := db.UserByUsername(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestSessionLifecycle(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	u := &User{Username: "kyle", PasswordHash: "hash"}
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	live := &Session{TokenHash: "live", UserID: u.ID, ExpiresAt: now.Add(time.Hour)}
	if err := db.CreateSession(ctx, live); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	stale := &Session{TokenHash: "stale", UserID: u.ID, ExpiresAt: now.Add(-time.Hour)}
	if err := db.CreateSession(ctx, stale); err != nil {
		t.Fatalf("CreateSession stale: %v", err)
	}

	got, err := db.Session(ctx, "live")
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if got.UserID != u.ID || got.ExpiresAt.IsZero() {
		t.Errorf("got %+v", got)
	}

	n, err := db.DeleteExpiredSessions(ctx, now)
	if err != nil {
		t.Fatalf("DeleteExpiredSessions: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned %d sessions, want 1", n)
	}
	if _, err := db.Session(ctx, "stale"); !errors.Is(err, ErrNotFound) {
		t.Errorf("stale session still present: %v", err)
	}

	if err := db.DeleteSession(ctx, "live"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if err := db.DeleteSession(ctx, "live"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if _, err := db.Session(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestFavorites(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	u := &User{Username: "kyle", PasswordHash: "hash"}
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	tr := mustTrack(t, db, "Song")

	if err := db.AddFavorite(ctx, u.ID, tr.ID); err != nil {
		t.Fatalf("AddFavorite: %v", err)
	}
	if err := db.AddFavorite(ctx, u.ID, tr.ID); err != nil {
		t.Errorf("AddFavorite must be idempotent, got %v", err)
	}
	favs, err := db.Favorites(ctx, u.ID)
	if err != nil {
		t.Fatalf("Favorites: %v", err)
	}
	if len(favs) != 1 || favs[0] != tr.ID {
		t.Errorf("favorites = %v", favs)
	}

	if err := db.RemoveFavorite(ctx, u.ID, tr.ID); err != nil {
		t.Fatalf("RemoveFavorite: %v", err)
	}
	if err := db.RemoveFavorite(ctx, u.ID, tr.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if favs, _ := db.Favorites(ctx, u.ID); len(favs) != 0 {
		t.Errorf("favorites = %v, want empty", favs)
	}
}

func TestFavoriteForeignKeyIsEnforced(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tr := mustTrack(t, db, "Song")

	if err := db.AddFavorite(ctx, uuid.New(), tr.ID); !errors.Is(err, ErrConflict) {
		t.Errorf("unknown user: err = %v, want ErrConflict", err)
	}
}

func TestProviderRankings(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	u := &User{Username: "kyle", PasswordHash: "hash"}
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}

	if got, err := db.Ranking(ctx, u.ID); err != nil || len(got) != 0 {
		t.Errorf("unranked user: %v, %v", got, err)
	}

	if err := db.SetRanking(ctx, u.ID, []string{"spotify", "ytmusic", "local"}); err != nil {
		t.Fatalf("SetRanking: %v", err)
	}
	got, err := db.Ranking(ctx, u.ID)
	if err != nil {
		t.Fatalf("Ranking: %v", err)
	}
	want := []string{"spotify", "ytmusic", "local"}
	if len(got) != len(want) {
		t.Fatalf("ranking = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ranking = %v, want %v", got, want)
		}
	}

	// Replacing removes the old rows rather than appending.
	if err := db.SetRanking(ctx, u.ID, []string{"local"}); err != nil {
		t.Fatalf("SetRanking replace: %v", err)
	}
	if got, _ := db.Ranking(ctx, u.ID); len(got) != 1 || got[0] != "local" {
		t.Errorf("ranking = %v, want [local]", got)
	}

	if err := db.SetRanking(ctx, u.ID, []string{"x", "x"}); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate provider: err = %v, want ErrConflict", err)
	}

	all, err := db.AllRankings(ctx)
	if err != nil {
		t.Fatalf("AllRankings: %v", err)
	}
	if len(all) != 1 || len(all[u.ID]) != 1 {
		t.Errorf("AllRankings = %v", all)
	}
}

func TestMigrationsAreIdempotentOnReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prismusic.db")
	ctx := context.Background()

	first, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	tr := &Track{Title: "Song"}
	if err := first.CreateTrack(ctx, tr); err != nil {
		t.Fatalf("CreateTrack: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()
	if got, err := second.Track(ctx, tr.ID); err != nil || got.Title != "Song" {
		t.Errorf("data lost on reopen: %+v, %v", got, err)
	}
}
