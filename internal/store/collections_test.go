package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

func TestAlbumIdentityIsTitleAndArtist(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	first, err := db.EnsureAlbum(ctx, "Greatest Hits", []string{"Queen"})
	if err != nil {
		t.Fatalf("EnsureAlbum: %v", err)
	}
	again, err := db.EnsureAlbum(ctx, "Greatest Hits", []string{"Queen"})
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Errorf("the same album got two ids: %s vs %s", first, again)
	}

	// The same title by another artist is a different album.
	other, err := db.EnsureAlbum(ctx, "Greatest Hits", []string{"ABBA"})
	if err != nil {
		t.Fatal(err)
	}
	if other == first {
		t.Error("two artists' Greatest Hits must not merge")
	}

	// Case and spacing do not create a new album.
	noisy, err := db.EnsureAlbum(ctx, "  greatest   hits ", []string{"queen"})
	if err != nil {
		t.Fatal(err)
	}
	if noisy != first {
		t.Errorf("normalisation failed: %s vs %s", noisy, first)
	}

	// Credits accumulate, and the primary artist stays the identity.
	if err := db.AttachAlbumArtist(ctx, other, uuid.New()); !errors.Is(err, ErrConflict) {
		t.Errorf("crediting a unknown artist: err = %v, want ErrConflict", err)
	}
	duo, err := db.EnsureAlbum(ctx, "Together", []string{"Artist One", "Artist Two"})
	if err != nil {
		t.Fatal(err)
	}
	artists, err := db.AlbumArtists(ctx, duo)
	if err != nil {
		t.Fatalf("AlbumArtists: %v", err)
	}
	if len(artists) != 2 || artists[0].Name != "Artist One" || artists[1].Name != "Artist Two" {
		t.Fatalf("artists = %+v", artists)
	}
}

func TestAlbumVariantsAndTracklist(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	albumID, err := db.EnsureAlbum(ctx, "Whenever You Need Somebody", []string{"Rick Astley"})
	if err != nil {
		t.Fatal(err)
	}

	variant := &AlbumVariant{
		AlbumID:         albumID,
		Provider:        "ytmusic",
		ProviderAlbumID: "MPREb_album",
		Title:           "Whenever You Need Somebody",
		Artists:         []string{"Rick Astley"},
		Year:            "1987",
		TrackCount:      10,
	}
	attached, created, err := db.AttachAlbumVariant(ctx, variant)
	if err != nil {
		t.Fatalf("AttachAlbumVariant: %v", err)
	}
	if !created || attached.ID == uuid.Nil {
		t.Fatalf("attached = %+v, created = %v", attached, created)
	}
	// Attaching the same provider release again is a no-op, not a duplicate.
	again, created, err := db.AttachAlbumVariant(ctx, &AlbumVariant{
		AlbumID: albumID, Provider: "ytmusic", ProviderAlbumID: "MPREb_album", Title: "Different Title",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created || again.ID != attached.ID {
		t.Errorf("re-attach created a duplicate: %+v", again)
	}
	if again.Title != "Whenever You Need Somebody" {
		t.Errorf("re-attach overwrote the stored variant: %+v", again)
	}

	resolved, err := db.AlbumByProviderID(ctx, "ytmusic", "MPREb_album")
	if err != nil {
		t.Fatalf("AlbumByProviderID: %v", err)
	}
	if resolved.ID != albumID {
		t.Errorf("resolved %s, want %s", resolved.ID, albumID)
	}

	variants, err := db.AlbumVariants(ctx, albumID)
	if err != nil {
		t.Fatal(err)
	}
	if len(variants) != 1 || variants[0].Year != "1987" || len(variants[0].Artists) != 1 {
		t.Fatalf("variants = %+v", variants)
	}

	// The tracklist keeps its order, and a second sync only adds what is new.
	tracks := []*Track{
		mustTrack(t, db, "Track One"),
		mustTrack(t, db, "Track Two"),
		mustTrack(t, db, "Track Three"),
	}
	ids := []uuid.UUID{tracks[0].ID, tracks[1].ID}
	if err := db.SetAlbumTracks(ctx, albumID, ids); err != nil {
		t.Fatalf("SetAlbumTracks: %v", err)
	}
	added, err := db.AppendAlbumTracks(ctx, albumID, []uuid.UUID{tracks[1].ID, tracks[2].ID})
	if err != nil {
		t.Fatalf("AppendAlbumTracks: %v", err)
	}
	if added != 1 {
		t.Errorf("added = %d, want 1 (the second track was already there)", added)
	}

	stored, err := db.AlbumTracks(ctx, albumID)
	if err != nil {
		t.Fatalf("AlbumTracks: %v", err)
	}
	if len(stored) != 3 {
		t.Fatalf("tracks = %d, want 3", len(stored))
	}
	for i, want := range []string{"Track One", "Track Two", "Track Three"} {
		if stored[i].Title != want {
			t.Errorf("track %d = %q, want %q", i, stored[i].Title, want)
		}
	}

	// The artist knows its albums.
	artist, err := db.ArtistByName(ctx, "Rick Astley")
	if err != nil {
		t.Fatalf("ArtistByName: %v", err)
	}
	albums, err := db.AlbumsForArtist(ctx, artist.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(albums) != 1 || albums[0].ID != albumID {
		t.Fatalf("albums = %+v", albums)
	}
}

func TestArtistVariants(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	artistID, err := db.EnsureArtist(ctx, "Rick Astley")
	if err != nil {
		t.Fatal(err)
	}

	variant := &ArtistVariant{
		ArtistID:         artistID,
		Provider:         "ytmusic",
		ProviderArtistID: "UCuAXFkgsw1L7xaCfnd5JJOw",
		Name:             "Rick Astley",
	}
	attached, created, err := db.AttachArtistVariant(ctx, variant)
	if err != nil {
		t.Fatalf("AttachArtistVariant: %v", err)
	}
	if !created {
		t.Error("the first attach should create")
	}
	again, created, err := db.AttachArtistVariant(ctx, &ArtistVariant{
		ArtistID: artistID, Provider: "ytmusic", ProviderArtistID: "UCuAXFkgsw1L7xaCfnd5JJOw", Name: "Rick Astley",
	})
	if err != nil || created || again.ID != attached.ID {
		t.Fatalf("re-attach = %+v, created = %v, err = %v", again, created, err)
	}

	resolved, err := db.ArtistByProviderID(ctx, "ytmusic", "UCuAXFkgsw1L7xaCfnd5JJOw")
	if err != nil {
		t.Fatalf("ArtistByProviderID: %v", err)
	}
	if resolved.ID != artistID {
		t.Errorf("resolved %s, want %s", resolved.ID, artistID)
	}

	variants, err := db.ArtistVariants(ctx, artistID)
	if err != nil {
		t.Fatal(err)
	}
	if len(variants) != 1 || variants[0].Name != "Rick Astley" {
		t.Fatalf("variants = %+v", variants)
	}
}

// TestAlbumMigrationKeepsExistingCredits checks the album table rebuild: albums
// that only existed as track credits must survive with their links intact.
func TestAlbumMigrationKeepsExistingCredits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "musoak.db")
	ctx := context.Background()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	track := mustTrack(t, db, "Song")
	if err := db.SetTrackArtists(ctx, track.ID, []string{"Artist"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetTrackAlbums(ctx, track.ID, []string{"Album"}); err != nil {
		t.Fatal(err)
	}

	albums, err := db.TrackAlbums(ctx, track.ID)
	if err != nil {
		t.Fatalf("TrackAlbums: %v", err)
	}
	if len(albums) != 1 || albums[0].Title != "Album" {
		t.Fatalf("albums = %+v", albums)
	}

	// The credit's album now has an identity, so the same title by another
	// artist is distinct while the original is still found.
	same, err := db.EnsureAlbum(ctx, "Album", []string{"Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if same != albums[0].ID {
		t.Errorf("the credit and the ensure disagree: %s vs %s", same, albums[0].ID)
	}
	other, err := db.EnsureAlbum(ctx, "Album", []string{"Someone Else"})
	if err != nil {
		t.Fatal(err)
	}
	if other == albums[0].ID {
		t.Error("a different artist's same-titled album must be distinct")
	}

	// Reopening runs no migration again, and the data is still there.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	albums, err = reopened.TrackAlbums(ctx, track.ID)
	if err != nil || len(albums) != 1 {
		t.Fatalf("albums after reopen = %+v, err = %v", albums, err)
	}
}

// An artist's page reads their albums in the order a discography is read: the
// singles container first, because it is not a release and has no date, then the
// newest release, and anything undated last rather than at year zero.
func TestAlbumsForArtistOrdersByRelease(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	artistID, err := db.EnsureArtist(ctx, "Pink Floyd")
	if err != nil {
		t.Fatal(err)
	}

	add := func(title, year, kind string) uuid.UUID {
		id, err := db.EnsureAlbum(ctx, title, []string{"Pink Floyd"})
		if err != nil {
			t.Fatalf("EnsureAlbum(%q): %v", title, err)
		}
		if err := db.AttachAlbumArtist(ctx, id, artistID); err != nil {
			t.Fatal(err)
		}
		if year != "" || kind != "" {
			if _, _, err := db.AttachAlbumVariant(ctx, &AlbumVariant{
				AlbumID: id, Provider: "ytmusic", ProviderAlbumID: "release-" + title,
				Title: title, Artists: []string{"Pink Floyd"}, Year: year, TrackCount: 10, Kind: kind,
			}); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	oldest := add("The Dark Side of the Moon", "1973", ReleaseKindAlbum)
	newest := add("8-Tracks", "2026", ReleaseKindAlbum)
	single := add("Time (edit)", "2023", ReleaseKindSingle)
	undated := add("A Bootleg", "", "")

	container, err := db.EnsureSinglesAlbum(ctx, artistID)
	if err != nil {
		t.Fatalf("EnsureSinglesAlbum: %v", err)
	}
	again, err := db.EnsureSinglesAlbum(ctx, artistID)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != container.ID {
		t.Errorf("the container was not reused: %s then %s", container.ID, again.ID)
	}
	if container.Kind != AlbumKindSingles || container.Title != SinglesAlbumTitle {
		t.Errorf("container = %+v", container)
	}

	albums, err := db.AlbumsForArtist(ctx, artistID)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		id    uuid.UUID
		title string
	}{
		{container.ID, SinglesAlbumTitle},
		{newest, "8-Tracks"},
		{single, "Time (edit)"},
		{oldest, "The Dark Side of the Moon"},
		{undated, "A Bootleg"},
	}
	if len(albums) != len(want) {
		t.Fatalf("albums = %d, want %d", len(albums), len(want))
	}
	for i, expected := range want {
		if albums[i].ID != expected.id || albums[i].Title != expected.title {
			t.Errorf("album %d = %q (%s), want %q (%s)", i, albums[i].Title, albums[i].ID,
				expected.title, expected.id)
		}
	}

	// The container is not a record anybody released, so a provider release
	// called "Singles" must never be matched onto it.
	candidates, err := db.AlbumCandidates(ctx, "singles", "pink floyd", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range candidates {
		if candidate.ID == container.ID {
			t.Fatal("the singles container was offered as a match for a release")
		}
	}
}

// A library holds the spelling a release arrived with and the spelling somebody
// searched for, and the same artist under two casings is one artist's songs. A
// row can hold both, so the songs read has to see both.
func TestArtistTracksFindsEverySpellingOfTheName(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	artistID, err := db.EnsureArtist(ctx, "Pink Floyd")
	if err != nil {
		t.Fatal(err)
	}
	// An older library's twin: the same artist, upper-cased by the provider.
	twinID := uuid.NewString()
	if _, err := db.db.ExecContext(ctx, `INSERT INTO artists (id, name) VALUES (?, ?)`, twinID, "PINK FLOYD"); err != nil {
		t.Fatal(err)
	}
	credit := func(title, artistName string) *Track {
		track := mustTrack(t, db, title)
		if err := db.SetTrackArtists(ctx, track.ID, []string{artistName}); err != nil {
			t.Fatal(err)
		}
		return track
	}
	credit("Time", "Pink Floyd")
	credit("Money", "PINK FLOYD")
	credit("Breathe (In the Air)", "Pink Floyd")
	credit("Wish You Were Here", "David Gilmour")

	tracks, err := db.ArtistTracks(ctx, artistID, "", 10, 0)
	if err != nil {
		t.Fatalf("ArtistTracks: %v", err)
	}
	if len(tracks) != 3 {
		t.Fatalf("tracks = %d, want 3 (both spellings, and nobody else)", len(tracks))
	}
	for i, want := range []string{"Breathe (In the Air)", "Money", "Time"} {
		if tracks[i].Title != want {
			t.Errorf("track %d = %q, want %q", i, tracks[i].Title, want)
		}
	}
	total, err := db.CountArtistTracks(ctx, artistID, "")
	if err != nil || total != 3 {
		t.Errorf("CountArtistTracks = %d, %v, want 3", total, err)
	}

	narrowed, err := db.ArtistTracks(ctx, artistID, "tim", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(narrowed) != 1 || narrowed[0].Title != "Time" {
		t.Errorf("searching \"tim\" = %+v, want Time", narrowed)
	}
	paged, err := db.ArtistTracks(ctx, artistID, "", 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(paged) != 2 || paged[0].Title != "Money" {
		t.Errorf("page 2 = %+v", paged)
	}
}

// Re-attaching a release fills in what the first read did not know - above all
// what it is - without overwriting anything that was known.
func TestAttachAlbumVariantFillsWhatIsUnknown(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	albumID, err := db.EnsureAlbum(ctx, "Time (edit)", []string{"Pink Floyd"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.AttachAlbumVariant(ctx, &AlbumVariant{
		AlbumID: albumID, Provider: "ytmusic", ProviderAlbumID: "MPREb_time",
		Title: "Time (edit)", TrackCount: 0,
	}); err != nil {
		t.Fatal(err)
	}
	filled, created, err := db.AttachAlbumVariant(ctx, &AlbumVariant{
		AlbumID: albumID, Provider: "ytmusic", ProviderAlbumID: "MPREb_time",
		Title: "Time (edit)", Year: "2023", TrackCount: 1, Kind: ReleaseKindSingle,
		Artists: []string{"Pink Floyd"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("re-attaching created a second variant")
	}
	if filled.Kind != ReleaseKindSingle || filled.Year != "2023" || filled.TrackCount != 1 {
		t.Errorf("the re-attach did not fill the release in: %+v", filled)
	}
	// What was already known stays.
	if filled.Title != "Time (edit)" {
		t.Errorf("title = %q", filled.Title)
	}
}
