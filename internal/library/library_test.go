package library

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kyleraykbs/MusOak/internal/match"
	"github.com/kyleraykbs/MusOak/internal/provider"
	"github.com/kyleraykbs/MusOak/internal/store"
)

// fakeProvider is a collection-capable provider with canned answers.
type fakeProvider struct {
	name string

	albums   []provider.Album
	details  map[string]*provider.AlbumDetail
	artists  []provider.Artist
	byArtist map[string][]provider.Album

	playlists      []provider.Playlist
	playlistDetail map[string]*provider.PlaylistDetail

	searchErr error
	albumErr  error
	artistErr error

	albumCalls  []string
	artistCalls []string
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) Capabilities() provider.Caps {
	return provider.Caps{
		Search: true, Download: true, SearchAlbums: true, SearchArtists: true, Playlists: true,
	}
}

func (f *fakeProvider) Search(ctx context.Context, q string, opts provider.SearchOpts) ([]provider.Track, error) {
	return nil, nil
}
func (f *fakeProvider) Download(ctx context.Context, id, dest string, _ provider.ProgressFunc) error {
	return nil
}

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

func (f *fakeProvider) SearchPlaylists(ctx context.Context, q string, opts provider.SearchOpts) ([]provider.Playlist, error) {
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	return f.playlists, nil
}

func (f *fakeProvider) Playlist(ctx context.Context, providerPlaylistID string) (*provider.PlaylistDetail, error) {
	if f.albumErr != nil {
		return nil, f.albumErr
	}
	detail, ok := f.playlistDetail[providerPlaylistID]
	if !ok {
		return nil, errors.New("no such playlist")
	}
	return detail, nil
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

	albums, problems, err := f.service.SearchAlbums(context.Background(), "never gonna", 10, nil)
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

	albums, _, err := f.service.SearchAlbums(context.Background(), "greatest hits", 10, nil)
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

	albums, _, err := f.service.SearchAlbums(ctx, "whenever", 5, nil)
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

	albums, _, err := f.service.SearchAlbums(ctx, "whenever", 5, nil)
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

	albums, _, err := f.service.SearchAlbums(ctx, "album", 5, nil)
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

	artists, _, err := f.service.SearchArtists(ctx, "rick astley", 5, nil)
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

	albums, _, err := f.service.SearchAlbums(ctx, "album", 5, nil)
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
	// A read that failed is asked a second time before it is believed, so this is
	// one call or two; what matters is that only the selected provider was asked.
	if len(spotify.albumCalls) == 0 {
		t.Errorf("spotify was never asked, want at least one call")
	}
}

func TestSearchReportsProviderErrors(t *testing.T) {
	broken := &fakeProvider{name: "ytmusic", searchErr: errors.New("upstream is down")}
	working := &fakeProvider{name: "spotify", albums: []provider.Album{
		{ProviderAlbumID: "b", Title: "Album", Artists: []string{"Artist"}},
	}}
	f := newFixture(t, broken, working)

	albums, problems, err := f.service.SearchAlbums(context.Background(), "album", 5, nil)
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

func TestSearchPlaylistsRecordsEveryProviderHit(t *testing.T) {
	ytmusic := &fakeProvider{name: "ytmusic", playlists: []provider.Playlist{{
		ProviderPlaylistID: "PL1",
		Title:              "Late night",
		Owner:              "kyle",
		TrackCount:         12,
		ArtworkURL:         "https://cdn.example/pl1.jpg",
	}}}
	spotify := &fakeProvider{name: "spotify", playlists: []provider.Playlist{{
		ProviderPlaylistID: "37i9dQ",
		Title:              "Late night",
		Owner:              "Spotify",
	}}}
	f := newFixture(t, ytmusic, spotify)

	playlists, problems, err := f.service.SearchPlaylists(context.Background(), "late night", 10, nil)
	if err != nil {
		t.Fatalf("SearchPlaylists: %v", err)
	}
	if len(problems) != 0 {
		t.Fatalf("problems = %+v", problems)
	}
	if len(playlists) != 2 {
		t.Fatalf("playlists = %d, want 2: a playlist is provider-scoped, never merged", len(playlists))
	}

	providers := map[string]bool{}
	for _, playlist := range playlists {
		providers[playlist.Provider] = true
		if playlist.ID == uuid.Nil {
			t.Fatal("a stored playlist needs an id")
		}
	}
	if !providers["ytmusic"] || !providers["spotify"] {
		t.Fatalf("providers = %v", providers)
	}

	// Searching again finds the same rows rather than new ones.
	again, _, err := f.service.SearchPlaylists(context.Background(), "late night", 10, nil)
	if err != nil {
		t.Fatalf("second SearchPlaylists: %v", err)
	}
	if len(again) != 2 {
		t.Fatalf("second search returned %d playlists, want 2", len(again))
	}
	for i := range again {
		if again[i].ID != playlists[i].ID {
			t.Fatalf("playlist %d changed id between searches", i)
		}
	}
}

func TestSearchPlaylistsReportsAProviderThatFailed(t *testing.T) {
	good := &fakeProvider{name: "ytmusic", playlists: []provider.Playlist{{
		ProviderPlaylistID: "PL1",
		Title:              "Works",
	}}}
	broken := &fakeProvider{name: "spotify", searchErr: errors.New("token expired")}
	f := newFixture(t, good, broken)

	playlists, problems, err := f.service.SearchPlaylists(context.Background(), "anything", 10, nil)
	if err != nil {
		t.Fatalf("SearchPlaylists: %v", err)
	}
	if len(playlists) != 1 {
		t.Fatalf("playlists = %d, want the one that worked", len(playlists))
	}
	if len(problems) != 1 || problems[0].Provider != "spotify" {
		t.Fatalf("problems = %+v", problems)
	}
}

func TestSyncPlaylistMatchesTracksInOrder(t *testing.T) {
	ytmusic := &fakeProvider{
		name: "ytmusic",
		playlists: []provider.Playlist{{
			ProviderPlaylistID: "PL1",
			Title:              "Late night",
		}},
		playlistDetail: map[string]*provider.PlaylistDetail{
			"PL1": {
				Playlist: provider.Playlist{
					ProviderPlaylistID: "PL1",
					Title:              "Late night",
					Owner:              "kyle",
					TrackCount:         3,
					ArtworkURL:         "https://cdn.example/pl1.jpg",
				},
				Tracks: []provider.Track{
					track("v1", "First", 200_000),
					track("v2", "Second", 210_000),
					track("v3", "Third", 220_000),
				},
			},
		},
	}
	f := newFixture(t, ytmusic)
	ctx := context.Background()

	playlists, _, err := f.service.SearchPlaylists(ctx, "late", 10, nil)
	if err != nil {
		t.Fatalf("SearchPlaylists: %v", err)
	}
	result, err := f.service.SyncPlaylist(ctx, playlists[0].ID, nil)
	if err != nil {
		t.Fatalf("SyncPlaylist: %v", err)
	}
	if len(result.Tracks) != 3 {
		t.Fatalf("tracks = %d, want 3", len(result.Tracks))
	}
	for i, want := range []string{"First", "Second", "Third"} {
		if result.Tracks[i].Title != want {
			t.Fatalf("track %d = %q, want %q", i, result.Tracks[i].Title, want)
		}
	}
	if result.Playlist.ArtworkURL != "https://cdn.example/pl1.jpg" {
		t.Fatalf("the sync should take the cover from the playlist itself: %+v", result.Playlist)
	}

	// The tracks are canonical, so they are reachable by id and by provider.
	stored, err := f.db.Track(ctx, result.Tracks[0].ID)
	if err != nil {
		t.Fatalf("Track: %v", err)
	}
	if stored.Title != "First" {
		t.Fatalf("stored track = %q", stored.Title)
	}

	// Syncing again replaces the tracklist instead of duplicating it.
	again, err := f.service.SyncPlaylist(ctx, playlists[0].ID, nil)
	if err != nil {
		t.Fatalf("second SyncPlaylist: %v", err)
	}
	if len(again.Tracks) != 3 {
		t.Fatalf("re-sync tracks = %d, want 3", len(again.Tracks))
	}
	if again.Added != 0 {
		t.Fatalf("re-sync added = %d, want 0", again.Added)
	}
}

func TestSyncPlaylistRefusesAProviderThePlaylistIsNotFrom(t *testing.T) {
	ytmusic := &fakeProvider{name: "ytmusic", playlists: []provider.Playlist{{
		ProviderPlaylistID: "PL1",
		Title:              "Late night",
	}}}
	f := newFixture(t, ytmusic)

	playlists, _, err := f.service.SearchPlaylists(context.Background(), "late", 10, nil)
	if err != nil {
		t.Fatalf("SearchPlaylists: %v", err)
	}
	_, err = f.service.SyncPlaylist(context.Background(), playlists[0].ID, []string{"spotify"})
	if !errors.Is(err, ErrNoSources) {
		t.Fatalf("err = %v, want ErrNoSources", err)
	}
}

func TestSyncPlaylistOnAnUnknownPlaylist(t *testing.T) {
	f := newFixture(t, &fakeProvider{name: "ytmusic"})
	_, err := f.service.SyncPlaylist(context.Background(), uuid.New(), nil)
	if !errors.Is(err, ErrNoPlaylist) {
		t.Fatalf("err = %v, want ErrNoPlaylist", err)
	}
}

// A discography is records and singles, and a page lists them apart: the records
// under the artist, and everything they put out as a single collected into one
// album of its own.
func TestSyncArtistCollectsSinglesIntoOneAlbum(t *testing.T) {
	release := func(id, title, year, kind, album string, titles ...string) (provider.Album, *provider.AlbumDetail) {
		tracks := make([]provider.Track, 0, len(titles))
		for i, song := range titles {
			tracks = append(tracks, provider.Track{
				ProviderTrackID: id + "-" + string(rune('a'+i)), Title: song,
				Artists: []string{"Pink Floyd"}, Album: album, DurationMs: 200_000,
			})
		}
		hit := provider.Album{
			ProviderAlbumID: id, Title: title, Artists: []string{"Pink Floyd"},
			Year: year, TrackCount: len(tracks), Kind: kind,
		}
		return hit, &provider.AlbumDetail{Album: hit, Tracks: tracks}
	}
	record, recordDetail := release("rel_album", "The Dark Side of the Moon", "1973",
		store.ReleaseKindAlbum, "The Dark Side of the Moon", "Speak to Me", "Breathe (In the Air)")
	single, singleDetail := release("rel_single", "Money (Early Mix)", "2020",
		store.ReleaseKindSingle, "Money (Early Mix)", "Money (Early Mix)")
	ep, epDetail := release("rel_ep", "Proper Education - EP", "2006",
		store.ReleaseKindEP, "Proper Education - EP", "Proper Education", "Another Brick")
	ytmusic := &fakeProvider{
		name:     "ytmusic",
		artists:  []provider.Artist{{ProviderArtistID: "UCpf", Name: "Pink Floyd"}},
		byArtist: map[string][]provider.Album{"UCpf": {record, single, ep}},
		details: map[string]*provider.AlbumDetail{
			"rel_album": recordDetail, "rel_single": singleDetail, "rel_ep": epDetail,
		},
	}
	f := newFixture(t, ytmusic)
	ctx := context.Background()

	artists, _, err := f.service.SearchArtists(ctx, "pink floyd", 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	artistID := artists[0].ID

	if _, err := f.service.SyncArtist(ctx, artistID, nil, true, false); err != nil {
		t.Fatalf("SyncArtist: %v", err)
	}

	detailed, err := f.service.GetArtist(ctx, artistID)
	if err != nil {
		t.Fatal(err)
	}
	// The container is first, then the releases newest first: the single, the EP,
	// the record.
	if len(detailed.Albums) != 4 {
		t.Fatalf("albums = %d, want the container and three releases", len(detailed.Albums))
	}
	if detailed.Albums[0].Kind != store.AlbumKindSingles ||
		detailed.Albums[0].Title != store.SinglesAlbumTitle {
		t.Fatalf("first album = %+v, want the singles container", detailed.Albums[0])
	}
	containerTracks, err := f.db.AlbumTracks(ctx, detailed.Albums[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(containerTracks) != 3 {
		t.Errorf("the container holds %d tracks, want the single and the EP's three",
			len(containerTracks))
	}

	// What the API filters on, and what each release keeps: its own kind, and its
	// own songs.
	kindAndTracks := func(id uuid.UUID) (string, int) {
		tracks, err := f.db.AlbumTracks(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		variants, err := f.db.AlbumVariants(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		kind := ""
		for _, variant := range variants {
			kind = variant.Kind
		}
		return kind, len(tracks)
	}
	want := map[string]struct {
		kind   string
		tracks int
	}{
		"The Dark Side of the Moon": {store.ReleaseKindAlbum, 2},
		"Money (Early Mix)":         {store.ReleaseKindSingle, 1},
		"Proper Education - EP":     {store.ReleaseKindEP, 2},
	}
	for _, album := range detailed.Albums[1:] {
		expected, ok := want[album.Title]
		if !ok {
			t.Errorf("unexpected album %q", album.Title)
			continue
		}
		if kind, tracks := kindAndTracks(album.ID); kind != expected.kind || tracks != expected.tracks {
			t.Errorf("%q = %q with %d tracks, want %q with %d", album.Title, kind, tracks,
				expected.kind, expected.tracks)
		}
	}
}

// Opening an album shows every song it has. An album a *track* created has no
// release to read, which is how "The Dark Side of the Moon" sat there holding the
// one song somebody had searched for: the page finds the release and reads it.
func TestEnsureAlbumTracksAdoptsTheReleaseOfATrackBornAlbum(t *testing.T) {
	ytmusic := &fakeProvider{name: "ytmusic"}
	f := newFixture(t, ytmusic)
	ctx := context.Background()

	// One song searched: it creates the album holding only itself.
	hit := provider.Track{
		ProviderTrackID: "yt_time", Title: "Time", Artists: []string{"Pink Floyd"},
		Album: "The Dark Side of the Moon", DurationMs: 414_000,
	}
	track, _, err := f.service.matcher.Attach(ctx, "ytmusic", hit)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	albums, err := f.db.TrackAlbums(ctx, track.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(albums) != 1 {
		t.Fatalf("albums for the track = %d, want 1", len(albums))
	}
	albumID := albums[0].ID
	before, err := f.db.AlbumTracks(ctx, albumID)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 {
		t.Fatalf("the album holds %d tracks before the page opens, want 1", len(before))
	}
	if variants, err := f.db.AlbumVariants(ctx, albumID); err != nil {
		t.Fatal(err)
	} else if len(variants) != 0 {
		t.Fatalf("a track-born album has %d releases, want none", len(variants))
	}

	// The provider knows the record.
	tracks := []provider.Track{
		{ProviderTrackID: "yt_speak", Title: "Speak to Me", Artists: []string{"Pink Floyd"},
			Album: "The Dark Side of the Moon", DurationMs: 66_000},
		{ProviderTrackID: "yt_breathe", Title: "Breathe (In the Air)", Artists: []string{"Pink Floyd"},
			Album: "The Dark Side of the Moon", DurationMs: 170_000},
		{ProviderTrackID: "yt_ontherun", Title: "On the Run", Artists: []string{"Pink Floyd"},
			Album: "The Dark Side of the Moon", DurationMs: 226_000},
		// The same recording the search found, so it joins that song rather than
		// becoming a second one.
		{ProviderTrackID: "yt_time_album", Title: "Time", Artists: []string{"Pink Floyd"},
			Album: "The Dark Side of the Moon", DurationMs: 414_000},
	}
	record := provider.Album{
		ProviderAlbumID: "MPREb_dsotm", Title: "The Dark Side of the Moon",
		Artists: []string{"Pink Floyd"}, Year: "1973", TrackCount: len(tracks),
		Kind: store.ReleaseKindAlbum,
	}
	ytmusic.albums = []provider.Album{record}
	ytmusic.details = map[string]*provider.AlbumDetail{"MPREb_dsotm": {Album: record, Tracks: tracks}}

	f.service.EnsureAlbumTracks(ctx, albumID)

	after, err := f.db.AlbumTracks(ctx, albumID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(tracks) {
		t.Fatalf("the album holds %d tracks after the page opened, want %d", len(after), len(tracks))
	}
	variants, err := f.db.AlbumVariants(ctx, albumID)
	if err != nil {
		t.Fatal(err)
	}
	if len(variants) != 1 || variants[0].ProviderAlbumID != "MPREb_dsotm" {
		t.Fatalf("the album's release was not recorded: %+v", variants)
	}
	if variants[0].TrackCount != len(tracks) {
		t.Errorf("release track count = %d, want %d", variants[0].TrackCount, len(tracks))
	}
}

// Opening an artist reads their whole catalogue in the background, and says how
// far it has got while it does.
func TestCatalogueReadsTheWholeDiscographyOnce(t *testing.T) {
	release := func(id, title, year, kind string, tracks ...provider.Track) (provider.Album, *provider.AlbumDetail) {
		hit := provider.Album{
			ProviderAlbumID: id, Title: title, Artists: []string{"Pink Floyd"},
			Year: year, TrackCount: len(tracks), Kind: kind,
		}
		return hit, &provider.AlbumDetail{Album: hit, Tracks: tracks}
	}
	song := func(id, title, album string, durationMs int64) provider.Track {
		return provider.Track{ProviderTrackID: id, Title: title, Artists: []string{"Pink Floyd"},
			Album: album, DurationMs: durationMs}
	}
	dsotm, dsotmDetail := release("rel_1", "The Dark Side of the Moon", "1973", store.ReleaseKindAlbum,
		song("t1", "Time", "The Dark Side of the Moon", 414_000),
		song("t2", "Money", "The Dark Side of the Moon", 383_000))
	wish, wishDetail := release("rel_2", "Wish You Were Here", "1975", store.ReleaseKindAlbum,
		song("t3", "Shine On You Crazy Diamond", "Wish You Were Here", 800_000))
	single, singleDetail := release("rel_3", "The Machine Song (Demo #2)", "2023",
		store.ReleaseKindSingle,
		song("t4", "The Machine Song (Demo #2)", "The Machine Song (Demo #2)", 260_000))
	ytmusic := &fakeProvider{
		name:     "ytmusic",
		artists:  []provider.Artist{{ProviderArtistID: "UCpf", Name: "Pink Floyd"}},
		byArtist: map[string][]provider.Album{"UCpf": {dsotm, wish, single}},
		details: map[string]*provider.AlbumDetail{
			"rel_1": dsotmDetail, "rel_2": wishDetail, "rel_3": singleDetail,
		},
	}
	f := newFixture(t, ytmusic)
	ctx := context.Background()

	artists, _, err := f.service.SearchArtists(ctx, "pink floyd", 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	artistID := artists[0].ID

	first := f.service.StartCatalogue(artistID)
	if !first.Running {
		t.Fatal("the catalogue read did not start")
	}
	state := first
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		state, _ = f.service.CatalogueStatus(artistID)
		if !state.Running {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if state.Running {
		t.Fatalf("the catalogue read never finished: %+v", state)
	}
	if state.Total != 3 || state.Done != 3 {
		t.Errorf("read %d of %d releases, want 3 of 3", state.Done, state.Total)
	}
	if state.Added != 4 {
		t.Errorf("added = %d, want the artist's four songs", state.Added)
	}
	if len(state.Errors) != 0 {
		t.Errorf("errors = %+v", state.Errors)
	}

	// Every song is in the library, and the single is in the container.
	songs, err := f.db.ArtistTracks(ctx, artistID, "", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(songs) != 4 {
		t.Errorf("songs = %d, want 4", len(songs))
	}
	detailed, err := f.service.GetArtist(ctx, artistID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detailed.Albums) != 4 || detailed.Albums[0].Kind != store.AlbumKindSingles {
		t.Fatalf("albums = %+v", detailed.Albums)
	}
	containerTracks, err := f.db.AlbumTracks(ctx, detailed.Albums[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(containerTracks) != 1 {
		t.Errorf("the container holds %d tracks, want the single's one", len(containerTracks))
	}

	// Asking again while the read is fresh answers the same thing and asks the
	// provider nothing.
	reads := len(ytmusic.artistCalls)
	again := f.service.StartCatalogue(artistID)
	if again.Running || again.Done != state.Done {
		t.Errorf("a fresh read was restarted: %+v", again)
	}
	if len(ytmusic.artistCalls) != reads {
		t.Errorf("the provider was asked again: %d then %d", reads, len(ytmusic.artistCalls))
	}
}
