package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func playlistFixture(t *testing.T) (*DB, context.Context) {
	t.Helper()
	ctx := context.Background()
	db, err := Open("file:playlists-" + uuid.NewString() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, ctx
}

func TestUpsertExternalPlaylistCreatesThenRefreshes(t *testing.T) {
	db, ctx := playlistFixture(t)

	created, isNew, err := db.UpsertExternalPlaylist(ctx, &ExternalPlaylist{
		Provider:           "ytmusic",
		ProviderPlaylistID: "PL123",
		Title:              "Late night",
		Owner:              "someone",
		TrackCount:         3,
		ArtworkURL:         "https://cdn.example/cover.jpg",
	})
	if err != nil {
		t.Fatalf("UpsertExternalPlaylist: %v", err)
	}
	if !isNew {
		t.Fatal("the first upsert must report a new playlist")
	}

	// Searching again finds the same playlist: same row, fresher metadata, and
	// an empty artwork field must not wipe the cover we already have.
	again, isNew, err := db.UpsertExternalPlaylist(ctx, &ExternalPlaylist{
		Provider:           "ytmusic",
		ProviderPlaylistID: "PL123",
		Title:              "Late night mix",
		Owner:              "someone else",
		TrackCount:         9,
	})
	if err != nil {
		t.Fatalf("second UpsertExternalPlaylist: %v", err)
	}
	if isNew {
		t.Fatal("the same provider playlist must not be created twice")
	}
	if again.ID != created.ID {
		t.Fatalf("id changed: %s -> %s", created.ID, again.ID)
	}
	if again.Title != "Late night mix" || again.TrackCount != 9 || again.Owner != "someone else" {
		t.Fatalf("metadata not refreshed: %+v", again)
	}
	if again.ArtworkURL != "https://cdn.example/cover.jpg" {
		t.Fatalf("artwork was lost: %q", again.ArtworkURL)
	}
}

func TestExternalPlaylistRequiresAProvider(t *testing.T) {
	db, ctx := playlistFixture(t)
	if _, _, err := db.UpsertExternalPlaylist(ctx, &ExternalPlaylist{Title: "no provider"}); err == nil {
		t.Fatal("a playlist with no provider must be refused")
	}
	if _, err := db.ExternalPlaylist(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown playlist: err = %v, want ErrNotFound", err)
	}
}

func TestExternalPlaylistTracksKeepTheirOrder(t *testing.T) {
	db, ctx := playlistFixture(t)

	playlist, _, err := db.UpsertExternalPlaylist(ctx, &ExternalPlaylist{
		Provider:           "ytmusic",
		ProviderPlaylistID: "PL456",
		Title:              "In order",
	})
	if err != nil {
		t.Fatalf("UpsertExternalPlaylist: %v", err)
	}

	var ids []uuid.UUID
	for _, title := range []string{"first", "second", "third"} {
		track := &Track{Title: title, DurationMs: 1000}
		if err := db.CreateTrack(ctx, track); err != nil {
			t.Fatalf("CreateTrack: %v", err)
		}
		ids = append(ids, track.ID)
	}

	added, err := db.SetExternalPlaylistTracks(ctx, playlist.ID, ids)
	if err != nil {
		t.Fatalf("SetExternalPlaylistTracks: %v", err)
	}
	if added != 3 {
		t.Fatalf("added = %d, want 3", added)
	}

	tracks, err := db.ExternalPlaylistTracks(ctx, playlist.ID)
	if err != nil {
		t.Fatalf("ExternalPlaylistTracks: %v", err)
	}
	if len(tracks) != 3 {
		t.Fatalf("tracks = %d, want 3", len(tracks))
	}
	for i, want := range []string{"first", "second", "third"} {
		if tracks[i].Title != want {
			t.Fatalf("track %d = %q, want %q", i, tracks[i].Title, want)
		}
	}

	// A second sync replaces the list rather than appending to it, and reports
	// nothing new when it is the same list.
	added, err = db.SetExternalPlaylistTracks(ctx, playlist.ID, []uuid.UUID{ids[2], ids[0]})
	if err != nil {
		t.Fatalf("second SetExternalPlaylistTracks: %v", err)
	}
	if added != 0 {
		t.Fatalf("re-syncing the same tracks added %d", added)
	}
	tracks, err = db.ExternalPlaylistTracks(ctx, playlist.ID)
	if err != nil {
		t.Fatalf("ExternalPlaylistTracks: %v", err)
	}
	if len(tracks) != 2 || tracks[0].Title != "third" || tracks[1].Title != "first" {
		t.Fatalf("reorder not kept: %+v", tracks)
	}

	stored, err := db.ExternalPlaylist(ctx, playlist.ID)
	if err != nil {
		t.Fatalf("ExternalPlaylist: %v", err)
	}
	if stored.TrackCount != 2 {
		t.Fatalf("track count = %d, want 2", stored.TrackCount)
	}
}

func TestPlaylistArtworkIsServedByKind(t *testing.T) {
	db, ctx := playlistFixture(t)

	playlist, _, err := db.UpsertExternalPlaylist(ctx, &ExternalPlaylist{
		Provider:           "ytmusic",
		ProviderPlaylistID: "PL789",
		Title:              "With a cover",
		ArtworkURL:         "https://cdn.example/pl.jpg",
	})
	if err != nil {
		t.Fatalf("UpsertExternalPlaylist: %v", err)
	}

	url, err := db.EntityArtwork(ctx, ArtworkPlaylist, playlist.ID)
	if err != nil {
		t.Fatalf("EntityArtwork: %v", err)
	}
	if url != "https://cdn.example/pl.jpg" {
		t.Fatalf("url = %q", url)
	}

	sources, err := db.ArtworkSources(ctx, ArtworkPlaylist, playlist.ID, nil)
	if err != nil {
		t.Fatalf("ArtworkSources: %v", err)
	}
	if len(sources) != 1 || sources[0].Provider != "ytmusic" {
		t.Fatalf("sources = %+v", sources)
	}

	bare, _, err := db.UpsertExternalPlaylist(ctx, &ExternalPlaylist{
		Provider:           "ytmusic",
		ProviderPlaylistID: "PL000",
		Title:              "No cover",
		CreatedAt:          time.Now(),
	})
	if err != nil {
		t.Fatalf("UpsertExternalPlaylist: %v", err)
	}
	if _, err := db.EntityArtwork(ctx, ArtworkPlaylist, bare.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("artwork for a playlist without one: err = %v, want ErrNotFound", err)
	}
}
