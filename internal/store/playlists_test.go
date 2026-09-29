package store

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func mustUserNamed(t *testing.T, db *DB, name string) *User {
	t.Helper()
	user := &User{Username: name, PasswordHash: "x"}
	if err := db.CreateUser(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	return user
}

func TestPlaylistCRUD(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := mustUserNamed(t, db, "kyle")

	playlist := &Playlist{UserID: user.ID, Name: "driving"}
	if err := db.CreatePlaylist(ctx, playlist); err != nil {
		t.Fatalf("CreatePlaylist: %v", err)
	}
	if playlist.ID == uuid.Nil || playlist.UpdatedAt.IsZero() {
		t.Fatalf("CreatePlaylist did not fill the row: %+v", playlist)
	}

	got, err := db.Playlist(ctx, playlist.ID)
	if err != nil {
		t.Fatalf("Playlist: %v", err)
	}
	if got.Name != "driving" || got.UserID != user.ID {
		t.Errorf("got %+v", got)
	}
	if _, err := db.Playlist(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing playlist: err = %v", err)
	}

	other := mustUserNamed(t, db, "someone")
	if err := db.CreatePlaylist(ctx, &Playlist{UserID: other.ID, Name: "theirs"}); err != nil {
		t.Fatal(err)
	}
	mine, err := db.PlaylistsForUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("PlaylistsForUser: %v", err)
	}
	if len(mine) != 1 || mine[0].ID != playlist.ID {
		t.Fatalf("playlists = %+v, want only the caller's", mine)
	}
	if mine[0].TrackCount != 0 {
		t.Errorf("track count = %d, want 0", mine[0].TrackCount)
	}

	if err := db.RenamePlaylist(ctx, playlist.ID, "night drive"); err != nil {
		t.Fatalf("RenamePlaylist: %v", err)
	}
	if got, _ := db.Playlist(ctx, playlist.ID); got.Name != "night drive" {
		t.Errorf("name = %q", got.Name)
	}
	if err := db.RenamePlaylist(ctx, uuid.New(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("rename missing: err = %v", err)
	}

	if err := db.DeletePlaylist(ctx, playlist.ID); err != nil {
		t.Fatalf("DeletePlaylist: %v", err)
	}
	if err := db.DeletePlaylist(ctx, playlist.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: err = %v", err)
	}
}

func TestPlaylistItemsLifecycle(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := mustUserNamed(t, db, "kyle")
	playlist := &Playlist{UserID: user.ID, Name: "mix"}
	if err := db.CreatePlaylist(ctx, playlist); err != nil {
		t.Fatal(err)
	}

	a := mustTrack(t, db, "A")
	b := mustTrack(t, db, "B")
	c := mustTrack(t, db, "C")

	if err := db.AppendPlaylistItems(ctx, playlist.ID, []uuid.UUID{a.ID, b.ID, c.ID}); err != nil {
		t.Fatalf("AppendPlaylistItems: %v", err)
	}
	items, err := db.PlaylistItems(ctx, playlist.ID)
	if err != nil {
		t.Fatalf("PlaylistItems: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3", len(items))
	}
	for i, want := range []uuid.UUID{a.ID, b.ID, c.ID} {
		if items[i].Position != i || items[i].TrackID != want {
			t.Fatalf("item %d = %+v, want position %d with %s", i, items[i], i, want)
		}
	}

	// Appending more keeps going from the end.
	if err := db.AppendPlaylistItems(ctx, playlist.ID, []uuid.UUID{a.ID}); err != nil {
		t.Fatal(err)
	}
	if items, _ := db.PlaylistItems(ctx, playlist.ID); len(items) != 4 || items[3].Position != 3 {
		t.Fatalf("items after append = %+v", items)
	}

	// Removing closes the gap.
	if err := db.RemovePlaylistItem(ctx, playlist.ID, 1); err != nil {
		t.Fatalf("RemovePlaylistItem: %v", err)
	}
	items, _ = db.PlaylistItems(ctx, playlist.ID)
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3", len(items))
	}
	for i, item := range items {
		if item.Position != i {
			t.Errorf("positions are not dense after a removal: %+v", items)
		}
	}
	if items[0].TrackID != a.ID || items[1].TrackID != c.ID {
		t.Errorf("removal took the wrong entry: %+v", items)
	}
	if err := db.RemovePlaylistItem(ctx, playlist.ID, 99); !errors.Is(err, ErrNotFound) {
		t.Errorf("removing a missing position: err = %v", err)
	}

	// Reordering permutes the existing entries.
	if err := db.ReorderPlaylist(ctx, playlist.ID, []int{2, 0, 1}); err != nil {
		t.Fatalf("ReorderPlaylist: %v", err)
	}
	items, _ = db.PlaylistItems(ctx, playlist.ID)
	got := []uuid.UUID{items[0].TrackID, items[1].TrackID, items[2].TrackID}
	want := []uuid.UUID{a.ID, a.ID, c.ID} // original [a, c, a] reversed
	if got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("reorder = %v, want %v", got, want)
	}

	for name, order := range map[string][]int{
		"wrong length": {0, 1},
		"duplicate":    {0, 0, 1},
		"unknown":      {0, 1, 9},
	} {
		t.Run(name, func(t *testing.T) {
			if err := db.ReorderPlaylist(ctx, playlist.ID, order); err == nil {
				t.Fatal("want an error for an invalid order")
			}
		})
	}
}

func TestPlaylistCascadesAndOwnership(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := mustUserNamed(t, db, "kyle")
	playlist := &Playlist{UserID: user.ID, Name: "mix"}
	if err := db.CreatePlaylist(ctx, playlist); err != nil {
		t.Fatal(err)
	}
	track := mustTrack(t, db, "A")
	if err := db.AppendPlaylistItems(ctx, playlist.ID, []uuid.UUID{track.ID}); err != nil {
		t.Fatal(err)
	}

	// A playlist for an unknown user cannot exist.
	if err := db.CreatePlaylist(ctx, &Playlist{UserID: uuid.New(), Name: "ghost"}); !errors.Is(err, ErrConflict) {
		t.Errorf("unknown user: err = %v, want ErrConflict", err)
	}

	// Deleting the user takes the playlists and their entries with it.
	if _, err := db.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, user.ID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Playlist(ctx, playlist.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("playlist survived its user: %v", err)
	}
	items, err := db.PlaylistItems(ctx, playlist.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Errorf("items = %d, want none", len(items))
	}

	// Appending to a playlist that does not exist is an error.
	if err := db.AppendPlaylistItems(ctx, uuid.New(), []uuid.UUID{track.ID}); !errors.Is(err, ErrNotFound) {
		t.Errorf("append to a missing playlist: err = %v", err)
	}
}
