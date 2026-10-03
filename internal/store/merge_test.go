package store

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// Merging is what makes a library that matched one song twice playable again:
// the rendition sits on one row and the playlist entry on the other.
func TestMergeTracksMovesEverythingToTheKeptRow(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := mustUserNamed(t, db, "kyle")

	// The row that carries the rendition.
	withSource := &Track{Title: "Never Gonna Give You Up", DurationMs: 213_000}
	if err := db.CreateTrack(ctx, withSource); err != nil {
		t.Fatalf("CreateTrack: %v", err)
	}
	variant := &Variant{
		TrackID: withSource.ID, Provider: "ytmusic", ProviderTrackID: "yt-1",
		Title: withSource.Title, DurationMs: 213_000, Downloadable: true,
	}
	if err := db.CreateVariant(ctx, variant); err != nil {
		t.Fatalf("CreateVariant: %v", err)
	}

	// The duplicate a playlist points at, with nothing playable on it.
	dupe := &Track{Title: "Never Gonna Give You Up", DurationMs: 213_400}
	if err := db.CreateTrack(ctx, dupe); err != nil {
		t.Fatalf("CreateTrack: %v", err)
	}
	playlist := &Playlist{UserID: user.ID, Name: "driving"}
	if err := db.CreatePlaylist(ctx, playlist); err != nil {
		t.Fatalf("CreatePlaylist: %v", err)
	}
	if err := db.AppendPlaylistItems(ctx, playlist.ID, []uuid.UUID{dupe.ID}); err != nil {
		t.Fatalf("AppendPlaylistItems: %v", err)
	}
	if err := db.AddFavorite(ctx, user.ID, dupe.ID); err != nil {
		t.Fatalf("AddFavorite: %v", err)
	}
	if err := db.SetTrackArtists(ctx, dupe.ID, []string{"Rick Astley"}); err != nil {
		t.Fatalf("SetTrackArtists: %v", err)
	}
	if err := db.SetTrackAlbums(ctx, dupe.ID, []string{"Whenever You Need Somebody"}); err != nil {
		t.Fatalf("SetTrackAlbums: %v", err)
	}

	if err := db.MergeTracks(ctx, withSource.ID, dupe.ID); err != nil {
		t.Fatalf("MergeTracks: %v", err)
	}

	if _, err := db.Track(ctx, dupe.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("the dropped row is still there: %v", err)
	}

	items, err := db.PlaylistItems(ctx, playlist.ID)
	if err != nil {
		t.Fatalf("PlaylistItems: %v", err)
	}
	if len(items) != 1 || items[0].TrackID != withSource.ID || items[0].Position != 0 {
		t.Errorf("playlist entries = %+v, want the kept track at position 0", items)
	}

	favorites, err := db.Favorites(ctx, user.ID)
	if err != nil {
		t.Fatalf("Favorites: %v", err)
	}
	if len(favorites) != 1 || favorites[0] != withSource.ID {
		t.Errorf("favorites = %v, want the kept track", favorites)
	}

	artists, err := db.TrackArtists(ctx, withSource.ID)
	if err != nil {
		t.Fatalf("TrackArtists: %v", err)
	}
	if len(artists) != 1 || artists[0].Name != "Rick Astley" {
		t.Errorf("artists = %+v, want the dropped row's credit", artists)
	}

	variants, err := db.VariantsForTrack(ctx, withSource.ID)
	if err != nil {
		t.Fatalf("VariantsForTrack: %v", err)
	}
	if len(variants) != 1 || !variants[0].Downloadable {
		t.Errorf("variants = %+v, want the rendition on the kept track", variants)
	}
}

// A playlist that held both copies holds the song once afterwards, and the
// entries after it close the gap.
func TestMergeTracksCollapsesAPlaylistHoldingBoth(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := mustUserNamed(t, db, "kyle")

	keep := &Track{Title: "Song", DurationMs: 180_000}
	drop := &Track{Title: "Song", DurationMs: 180_500}
	other := &Track{Title: "Another Song", DurationMs: 200_000}
	for _, track := range []*Track{keep, drop, other} {
		if err := db.CreateTrack(ctx, track); err != nil {
			t.Fatalf("CreateTrack: %v", err)
		}
	}

	playlist := &Playlist{UserID: user.ID, Name: "both"}
	if err := db.CreatePlaylist(ctx, playlist); err != nil {
		t.Fatalf("CreatePlaylist: %v", err)
	}
	if err := db.AppendPlaylistItems(ctx, playlist.ID, []uuid.UUID{drop.ID, other.ID, keep.ID}); err != nil {
		t.Fatalf("AppendPlaylistItems: %v", err)
	}

	if err := db.MergeTracks(ctx, keep.ID, drop.ID); err != nil {
		t.Fatalf("MergeTracks: %v", err)
	}

	items, err := db.PlaylistItems(ctx, playlist.ID)
	if err != nil {
		t.Fatalf("PlaylistItems: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("entries = %d, want 2 (the song once, and the other)", len(items))
	}
	if items[0].Position != 0 || items[1].Position != 1 {
		t.Errorf("positions = %d, %d; want them dense from zero", items[0].Position, items[1].Position)
	}
	// The entry that goes is the dropped row's; the rest keep the order they
	// were in, so the kept song stays where the playlist already had it.
	if items[0].TrackID != other.ID || items[1].TrackID != keep.ID {
		t.Errorf("entries = %+v, want the other song then the kept one", items)
	}
}
