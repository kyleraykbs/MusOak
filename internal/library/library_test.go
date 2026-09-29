package library

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/prismusic/internal/match"
	"codeberg.org/kyleraykbs/prismusic/internal/provider"
	"codeberg.org/kyleraykbs/prismusic/internal/store"
)

// fakeProvider is a collection-capable provider with canned answers.
type fakeProvider struct {
	name string

	albums   []provider.Album
	details  map[string]*provider.AlbumDetail
	artists  []provider.Artist
	byArtist map[string][]provider.Album

	searchErr error
	albumErr  error
	artistErr error

	albumCalls  []string
	artistCalls []string
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) Capabilities() provider.Caps {
	return provider.Caps{Search: true, Download: true, SearchAlbums: true, SearchArtists: true}
}

func (f *fakeProvider) Search(ctx context.Context, q string, opts provider.SearchOpts) ([]provider.Track, error) {
	return nil, nil
}
func (f *fakeProvider) Download(ctx context.Context, id, dest string) error { return nil }

func (f *fakeProvider) SearchAlbums(ctx context.Context, q string, opts provider.SearchOpts) ([]provider.Album, error) {
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	return f.albums, nil
}

func (f *fakeProvider) Album(ctx context.Context, providerAlbumID string) (*provider.AlbumDetail, error) {
	f.albumCalls = append(f.albumCalls, providerAlbumID)
	if f.albumErr != nil {
		return nil, f.albumErr
	}
	detail, ok := f.details[providerAlbumID]
	if !ok {
		return nil, errors.New("no such album")
	}
	return detail, nil
}

func (f *fakeProvider) SearchArtists(ctx context.Context, q string, opts provider.SearchOpts) ([]provider.Artist, error) {
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	return f.artists, nil
}

func (f *fakeProvider) ArtistAlbums(ctx context.Context, providerArtistID string) ([]provider.Album, error) {
	f.artistCalls = append(f.artistCalls, providerArtistID)
	if f.artistErr != nil {
		return nil, f.artistErr
	}
	return f.byArtist[providerArtistID], nil
}

func track(id, title string, durationMs int64) provider.Track {
	return provider.Track{
		ProviderTrackID: id,
		Title:           title,
		Artists:         []string{"Rick Astley"},
		Album:           "Whenever You Need Somebody",
		DurationMs:      durationMs,
	}
}

type fixture struct {
	service *Service
	db      *store.DB
}

func newFixture(t *testing.T, providers ...*fakeProvider) *fixture {
	t.Helper()
	db, err := store.Open("file:library-" + uuid.NewString() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	logger := slog.New(slog.DiscardHandler)
	registry := provider.NewRegistry(logger, 5*time.Second)
	for _, p := range providers {
		registry.Register(p)
	}
	matcher := match.New(db, registry, 0.8, logger)
	return &fixture{service: New(db, registry, matcher, logger), db: db}
}

func TestSearchAlbumsMatchesAcrossProviders(t *testing.T) {
	ytmusic := &fakeProvider{name: "ytmusic", albums: []provider.Album{{
		ProviderAlbumID: "MPREb_1",
		Title:           "Whenever You Need Somebody",
		Artists:         []string{"Rick Astley"},
		Year:            "1987",
		TrackCount:      10,
	}}}
	spotify := &fakeProvider{name: "spotify", albums: []provider.Album{{
		ProviderAlbumID: "4cOdK",
		Title:           "Whenever You Need Somebody",
		Artists:         []string{"Rick Astley"},
		Year:            "1987",
		TrackCount:      10,
	}}}
	f := newFixture(t, ytmusic, spotify)

	albums, problems, err := f.service.SearchAlbums(context.Background(), "never gonna", 10)
	if err != nil {
		t.Fatalf("SearchAlbums: %v", err)
	}
	if len(problems) != 0 {
		t.Errorf("problems = %+v", problems)
	}
	if len(albums) != 1 {
		t.Fatalf("albums = %d, want one canonical album from two sources", len(albums))
	}

	detailed, err := f.service.GetAlbum(context.Background(), albums[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detailed.Variants) != 2 {
		t.Fatalf("variants = %d, want 2", len(detailed.Variants))
	}
	providers := map[string]bool{}
	for _, variant := range detailed.Variants {
		providers[variant.Provider] = true
	}
	if !providers["ytmusic"] || !providers["spotify"] {
		t.Errorf("providers = %v", providers)
	}
}

func TestSearchAlbumsKeepsDistinctRecordsApart(t *testing.T) {
	ytmusic := &fakeProvider{name: "ytmusic", albums: []provider.Album{
		{ProviderAlbumID: "a", Title: "Greatest Hits", Artists: []string{"Queen"}, Year: "1981", TrackCount: 17},
		{ProviderAlbumID: "b", Title: "Greatest Hits", Artists: []string{"ABBA"}, Year: "1992", TrackCount: 18},
	}}
	f := newFixture(t, ytmusic)

	albums, _, err := f.service.SearchAlbums(context.Background(), "greatest hits", 10)
	if err != nil {
		t.Fatalf("SearchAlbums: %v", err)
	}
	if len(albums) != 2 {
		t.Fatalf("albums = %d, want two (they are different records by different artists)", len(albums))
	}
}

func TestSyncAlbumPullsAndMatchesTracks(t *testing.T) {
	ytmusic := &fakeProvider{
		name: "ytmusic",
		albums: []provider.Album{{
			ProviderAlbumID: "MPREb_1", Title: "Whenever You Need Somebody",
			Artists: []string{"Rick Astley"}, Year: "1987", TrackCount: 3,
		}},
		details: map[string]*provider.AlbumDetail{
			"MPREb_1": {
				Album: provider.Album{
					ProviderAlbumID: "MPREb_1", Title: "Whenever You Need Somebody",
					Artists: []string{"Rick Astley"}, Year: "1987", TrackCount: 3,
				},
				Tracks: []provider.Track{
					track("yt-1", "Never Gonna Give You Up", 213_000),
					track("yt-2", "Whenever You Need Somebody", 200_000),
					track("yt-3", "Together Forever", 190_000),
				},
			},
		},
	}
	f := newFixture(t, ytmusic)
	ctx := context.Background()

	albums, _, err := f.service.SearchAlbums(ctx, "whenever", 5)
	if err != nil {
		t.Fatal(err)
	}
	albumID := albums[0].ID

	synced, err := f.service.SyncAlbum(ctx, albumID, nil, false)
	if err != nil {
		t.Fatalf("SyncAlbum: %v", err)
	}
	if synced.Added != 3 || len(synced.Tracks) != 3 {
		t.Fatalf("sync = %+v", synced)
	}
	if synced.Tracks[0].Title != "Never Gonna Give You Up" {
		t.Errorf("tracklist order was not kept: %+v", synced.Tracks)
	}

	// The tracks are real canonical tracks now, with ytmusic renditions.
	for _, track := range synced.Tracks {
		variants, err := f.db.VariantsForTrack(ctx, track.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(variants) != 1 || variants[0].Provider != "ytmusic" {
			t.Errorf("track %s variants = %+v", track.Title, variants)
		}
	}

	// Syncing again adds nothing: the same album, the same tracks.
	again, err := f.service.SyncAlbum(ctx, albumID, nil, false)
	if err != nil {
		t.Fatalf("second SyncAlbum: %v", err)
	}
	if again.Added != 0 {
		t.Errorf("second sync added %d tracks, want 0", again.Added)
	}
	if len(again.Tracks) != 3 {
		t.Errorf("album has %d tracks after the second sync, want 3", len(again.Tracks))
	}
}

// TestSyncAlbumBringsASecondSource checks the point of the whole exercise: an
// album known from one provider gains the other provider's renditions, matched
// onto the same canonical tracks.
func TestSyncAlbumBringsASecondSource(t *testing.T) {
	ytmusic := &fakeProvider{
		name: "ytmusic",
		albums: []provider.Album{{
			ProviderAlbumID: "MPREb_1", Title: "Whenever You Need Somebody",
			Artists: []string{"Rick Astley"}, Year: "1987", TrackCount: 2,
		}},
		details: map[string]*provider.AlbumDetail{
			"MPREb_1": {
				Album: provider.Album{ProviderAlbumID: "MPREb_1", Title: "Whenever You Need Somebody",
					Artists: []string{"Rick Astley"}, Year: "1987", TrackCount: 2},
				Tracks: []provider.Track{
					track("yt-1", "Never Gonna Give You Up", 213_000),
					track("yt-2", "Together Forever", 190_000),
				},
			},
		},
	}
	spotify := &fakeProvider{
		name: "spotify",
		albums: []provider.Album{{
			ProviderAlbumID: "sp-album", Title: "Whenever You Need Somebody",
			Artists: []string{"Rick Astley"}, Year: "1987", TrackCount: 2,
		}},
		details: map[string]*provider.AlbumDetail{
			"sp-album": {
				Album: provider.Album{ProviderAlbumID: "sp-album", Title: "Whenever You Need Somebody",
					Artists: []string{"Rick Astley"}, Year: "1987", TrackCount: 2},
				Tracks: []provider.Track{
					track("sp-1", "Never Gonna Give You Up", 213_400),
					// Spotify spells the second title slightly differently.
					track("sp-2", "Together Forever (Official Audio)", 190_200),
				},
			},
		},
	}
	f := newFixture(t, ytmusic, spotify)
	ctx := context.Background()

	albums, _, err := f.service.SearchAlbums(ctx, "whenever", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(albums) != 1 {
		t.Fatalf("albums = %d, want the two sources merged", len(albums))
	}
	albumID := albums[0].ID

	if _, err := f.service.SyncAlbum(ctx, albumID, []string{"ytmusic"}, false); err != nil {
		t.Fatalf("ytmusic sync: %v", err)
	}
	before, err := f.service.GetAlbum(ctx, albumID)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Tracks) != 2 {
		t.Fatalf("tracks = %d, want 2", len(before.Tracks))
	}

	synced, err := f.service.SyncAlbum(ctx, albumID, []string{"spotify"}, false)
	if err != nil {
		t.Fatalf("spotify sync: %v", err)
	}
	if synced.Added != 0 {
		t.Errorf("the spotify sync added %d tracks; they should have matched the existing ones", synced.Added)
	}

	after, err := f.service.GetAlbum(ctx, albumID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Tracks) != 2 {
		t.Fatalf("tracks after the second source = %d, want 2", len(after.Tracks))
	}
	for _, track := range after.Tracks {
		variants, err := f.db.VariantsForTrack(ctx, track.ID)
		if err != nil {
			t.Fatal(err)
		}
		sources := map[string]bool{}
		for _, variant := range variants {
			sources[variant.Provider] = true
		}
		if !sources["ytmusic"] || !sources["spotify"] {
			t.Errorf("%q has renditions from %v, want both providers", track.Title, sources)
		}
	}
}

func TestSyncAlbumReportsProviderFailures(t *testing.T) {
	broken := &fakeProvider{
		name:     "ytmusic",
		albums:   []provider.Album{{ProviderAlbumID: "x", Title: "Album", Artists: []string{"Artist"}}},
		albumErr: errors.New("provider is down"),
	}
	f := newFixture(t, broken)
	ctx := context.Background()

	albums, _, err := f.service.SearchAlbums(ctx, "album", 5)
	if err != nil {
		t.Fatal(err)
	}
	synced, err := f.service.SyncAlbum(ctx, albums[0].ID, nil, false)
	if err != nil {
		t.Fatalf("SyncAlbum: %v", err)
	}
	if len(synced.Errors) != 1 || synced.Errors[0].Provider != "ytmusic" {
		t.Fatalf("errors = %+v", synced.Errors)
	}
	if synced.Added != 0 {
		t.Errorf("added = %d, want 0", synced.Added)
	}
}

func TestSyncAlbumValidation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, err := f.service.SyncAlbum(ctx, uuid.New(), nil, false); !errors.Is(err, ErrNoAlbum) {
		t.Errorf("unknown album: err = %v, want ErrNoAlbum", err)
	}

	// An album with no provider release cannot be synced from anywhere.
	albumID, err := f.db.EnsureAlbum(ctx, "Unknown Album", []string{"Nobody"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.SyncAlbum(ctx, albumID, nil, false); !errors.Is(err, ErrNoSources) {
		t.Errorf("album without sources: err = %v, want ErrNoSources", err)
	}
}

func TestSyncArtistPullsAlbumsAndTracks(t *testing.T) {
	ytmusic := &fakeProvider{
		name:    "ytmusic",
		artists: []provider.Artist{{ProviderArtistID: "UCrick", Name: "Rick Astley"}},
		byArtist: map[string][]provider.Album{
			"UCrick": {
				{ProviderAlbumID: "MPREb_1", Title: "Whenever You Need Somebody",
					Artists: []string{"Rick Astley"}, Year: "1987", TrackCount: 2},
				{ProviderAlbumID: "MPREb_2", Title: "Hold Me In Your Arms",
					Artists: []string{"Rick Astley"}, Year: "1988", TrackCount: 1},
			},
		},
		details: map[string]*provider.AlbumDetail{
			"MPREb_1": {
				Album: provider.Album{ProviderAlbumID: "MPREb_1", Title: "Whenever You Need Somebody",
					Artists: []string{"Rick Astley"}, Year: "1987", TrackCount: 2},
				Tracks: []provider.Track{
					track("yt-1", "Never Gonna Give You Up", 213_000),
					track("yt-2", "Together Forever", 190_000),
				},
			},
			"MPREb_2": {
				Album: provider.Album{ProviderAlbumID: "MPREb_2", Title: "Hold Me In Your Arms",
					Artists: []string{"Rick Astley"}, Year: "1988", TrackCount: 1},
				Tracks: []provider.Track{
					track("yt-3", "She Wants To Dance With Me", 195_000),
				},
			},
		},
	}
	f := newFixture(t, ytmusic)
	ctx := context.Background()

	artists, _, err := f.service.SearchArtists(ctx, "rick astley", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(artists) != 1 {
		t.Fatalf("artists = %d, want 1", len(artists))
	}
	artistID := artists[0].ID

	// Without syncAlbums only the album entries are pulled.
	result, err := f.service.SyncArtist(ctx, artistID, nil, false, false)
	if err != nil {
		t.Fatalf("SyncArtist: %v", err)
	}
	if len(result.Albums) != 2 {
		t.Fatalf("albums = %d, want 2", len(result.Albums))
	}
	if result.Added != 0 {
		t.Errorf("added = %d, want 0 without syncAlbums", result.Added)
	}

	// With it, the discography's tracklists come along.
	result, err = f.service.SyncArtist(ctx, artistID, nil, true, false)
	if err != nil {
		t.Fatalf("SyncArtist with albums: %v", err)
	}
	if result.Added != 3 {
		t.Errorf("added = %d, want the artist's three tracks", result.Added)
	}

	detailed, err := f.service.GetArtist(ctx, artistID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detailed.Albums) != 2 {
		t.Fatalf("albums = %d, want 2", len(detailed.Albums))
	}
	if len(detailed.Variants) != 1 || detailed.Variants[0].ProviderArtistID != "UCrick" {
		t.Fatalf("variants = %+v", detailed.Variants)
	}
}

func TestProviderSelectionIsHonoured(t *testing.T) {
	ytmusic := &fakeProvider{name: "ytmusic", albums: []provider.Album{
		{ProviderAlbumID: "a", Title: "Album", Artists: []string{"Artist"}},
	}}
	spotify := &fakeProvider{name: "spotify", albums: []provider.Album{
		{ProviderAlbumID: "b", Title: "Album", Artists: []string{"Artist"}},
	}}
	f := newFixture(t, ytmusic, spotify)
	ctx := context.Background()

	albums, _, err := f.service.SearchAlbums(ctx, "album", 5)
	if err != nil {
		t.Fatal(err)
	}
	albumID := albums[0].ID

	// Only the named provider is asked.
	if _, err := f.service.SyncAlbum(ctx, albumID, []string{"spotify"}, false); err != nil {
		t.Fatal(err)
	}
	if len(ytmusic.albumCalls) != 0 {
		t.Errorf("ytmusic was called %v, want nothing", ytmusic.albumCalls)
	}
	if len(spotify.albumCalls) != 1 {
		t.Errorf("spotify calls = %v, want one", spotify.albumCalls)
	}
}

func TestSearchReportsProviderErrors(t *testing.T) {
	broken := &fakeProvider{name: "ytmusic", searchErr: errors.New("upstream is down")}
	working := &fakeProvider{name: "spotify", albums: []provider.Album{
		{ProviderAlbumID: "b", Title: "Album", Artists: []string{"Artist"}},
	}}
	f := newFixture(t, broken, working)

	albums, problems, err := f.service.SearchAlbums(context.Background(), "album", 5)
	if err != nil {
		t.Fatalf("SearchAlbums: %v", err)
	}
	if len(albums) != 1 {
		t.Errorf("albums = %d, want the working provider's one", len(albums))
	}
	if len(problems) != 1 || problems[0].Provider != "ytmusic" {
		t.Errorf("problems = %+v", problems)
	}
}
