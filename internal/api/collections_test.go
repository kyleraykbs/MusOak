package api

import (
	"context"
	"net/http"
	"testing"

	"codeberg.org/kyleraykbs/prismusic/internal/provider"
)

// fakeCollectionsProvider serves album and artist browsing.
type fakeCollectionsProvider struct {
	name     string
	albums   []provider.Album
	details  map[string]*provider.AlbumDetail
	artists  []provider.Artist
	byArtist map[string][]provider.Album
}

func (f *fakeCollectionsProvider) Name() string { return f.name }
func (f *fakeCollectionsProvider) Capabilities() provider.Caps {
	return provider.Caps{Search: true, Download: true, SearchAlbums: true, SearchArtists: true}
}
func (f *fakeCollectionsProvider) Search(ctx context.Context, q string, opts provider.SearchOpts) ([]provider.Track, error) {
	return nil, nil
}
func (f *fakeCollectionsProvider) Download(ctx context.Context, id, dest string) error { return nil }
func (f *fakeCollectionsProvider) SearchAlbums(ctx context.Context, q string, opts provider.SearchOpts) ([]provider.Album, error) {
	return f.albums, nil
}
func (f *fakeCollectionsProvider) Album(ctx context.Context, id string) (*provider.AlbumDetail, error) {
	detail, ok := f.details[id]
	if !ok {
		return nil, context.Canceled // any error will do here
	}
	return detail, nil
}
func (f *fakeCollectionsProvider) SearchArtists(ctx context.Context, q string, opts provider.SearchOpts) ([]provider.Artist, error) {
	return f.artists, nil
}
func (f *fakeCollectionsProvider) ArtistAlbums(ctx context.Context, id string) ([]provider.Album, error) {
	return f.byArtist[id], nil
}

func newAlbumProvider() *fakeCollectionsProvider {
	return &fakeCollectionsProvider{
		name: "ytmusic",
		albums: []provider.Album{{
			ProviderAlbumID: "MPREb_album",
			Title:           "Whenever You Need Somebody",
			Artists:         []string{"Rick Astley"},
			Year:            "1987",
			TrackCount:      2,
		}},
		details: map[string]*provider.AlbumDetail{
			"MPREb_album": {
				Album: provider.Album{
					ProviderAlbumID: "MPREb_album", Title: "Whenever You Need Somebody",
					Artists: []string{"Rick Astley"}, Year: "1987", TrackCount: 2,
				},
				Tracks: []provider.Track{
					{
						ProviderTrackID: "yt-1", Title: "Never Gonna Give You Up",
						Artists: []string{"Rick Astley"}, Album: "Whenever You Need Somebody",
						DurationMs: 213_000,
					},
					{
						ProviderTrackID: "yt-2", Title: "Together Forever",
						Artists: []string{"Rick Astley"}, Album: "Whenever You Need Somebody",
						DurationMs: 190_000,
					},
				},
			},
		},
		artists: []provider.Artist{{ProviderArtistID: "UCrick", Name: "Rick Astley"}},
		byArtist: map[string][]provider.Album{
			"UCrick": {{
				ProviderAlbumID: "MPREb_album", Title: "Whenever You Need Somebody",
				Artists: []string{"Rick Astley"}, Year: "1987", TrackCount: 2,
			}},
		},
	}
}

func TestAlbumEndpoints(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	c.providers.Register(newAlbumProvider())

	// Search.
	rec := c.do(http.MethodGet, "/api/v1/albums/search?q=whenever", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("album search = %d: %s", rec.Code, rec.Body.String())
	}
	var search struct {
		Albums []albumResponse `json:"albums"`
	}
	c.decode(rec, &search)
	if len(search.Albums) != 1 {
		t.Fatalf("albums = %+v", search.Albums)
	}
	album := search.Albums[0]
	if album.Title != "Whenever You Need Somebody" || len(album.Providers) != 1 || album.Providers[0] != "ytmusic" {
		t.Fatalf("album = %+v", album)
	}
	if len(album.Tracks) != 0 {
		t.Errorf("a search result should not carry a tracklist: %+v", album.Tracks)
	}
	if album.Year != "1987" {
		t.Errorf("year = %q", album.Year)
	}

	// Sync it and read it back with tracks.
	rec = c.do(http.MethodPost, "/api/v1/albums/"+album.ID+"/sync", "", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("album sync = %d: %s", rec.Code, rec.Body.String())
	}
	var synced syncResponse
	c.decode(rec, &synced)
	if synced.Added != 2 || len(synced.Tracks) != 2 {
		t.Fatalf("sync = %+v", synced)
	}

	rec = c.do(http.MethodGet, "/api/v1/albums/"+album.ID, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("album get = %d", rec.Code)
	}
	var detail albumResponse
	c.decode(rec, &detail)
	if detail.TrackCount != 2 || len(detail.Tracks) != 2 {
		t.Fatalf("album = %+v", detail)
	}
	if detail.Tracks[0].Title != "Never Gonna Give You Up" {
		t.Errorf("tracklist = %+v", detail.Tracks)
	}
	if len(detail.Tracks[0].Artists) != 1 || detail.Tracks[0].Artists[0] != "Rick Astley" {
		t.Errorf("track credits = %+v", detail.Tracks[0].Artists)
	}

	// Syncing twice adds nothing.
	rec = c.do(http.MethodPost, "/api/v1/albums/"+album.ID+"/sync", "", map[string]any{})
	c.decode(rec, &synced)
	if synced.Added != 0 {
		t.Errorf("second sync added %d", synced.Added)
	}

	// Errors.
	if rec := c.do(http.MethodGet, "/api/v1/albums/search", "", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("search without q = %d, want 400", rec.Code)
	}
	if rec := c.do(http.MethodGet, "/api/v1/albums/not-a-uuid", "", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad id = %d, want 400", rec.Code)
	}
	if rec := c.do(http.MethodGet, "/api/v1/albums/00000000-0000-0000-0000-000000000000", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown album = %d, want 404", rec.Code)
	}
	if rec := c.do(http.MethodPost, "/api/v1/albums/"+album.ID+"/sync", "", map[string]any{
		"providers": []string{"deezer"},
	}); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown provider = %d, want 400", rec.Code)
	}

	// An album with no provider release cannot be synced from anywhere.
	ctx := context.Background()
	orphan := seedTracks(t, c, "Orphan Album Track")[0]
	if err := c.store.SetTrackArtists(ctx, orphan.ID, []string{"Orphan Artist"}); err != nil {
		t.Fatal(err)
	}
	if err := c.store.SetTrackAlbums(ctx, orphan.ID, []string{"Orphan Album"}); err != nil {
		t.Fatal(err)
	}
	credits, err := c.store.TrackAlbums(ctx, orphan.ID)
	if err != nil || len(credits) == 0 {
		t.Fatalf("album credits = %+v, err = %v", credits, err)
	}
	rec = c.do(http.MethodPost, "/api/v1/albums/"+credits[0].ID.String()+"/sync", "", map[string]any{})
	if rec.Code != http.StatusConflict {
		t.Errorf("album without sources = %d, want 409", rec.Code)
	}
}

func TestArtistEndpoints(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	provider := newAlbumProvider()
	c.providers.Register(provider)

	rec := c.do(http.MethodGet, "/api/v1/artists/search?q=rick", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("artist search = %d: %s", rec.Code, rec.Body.String())
	}
	var search struct {
		Artists []artistResponse `json:"artists"`
	}
	c.decode(rec, &search)
	if len(search.Artists) != 1 || search.Artists[0].Name != "Rick Astley" {
		t.Fatalf("artists = %+v", search.Artists)
	}
	artist := search.Artists[0]
	if len(artist.Providers) != 1 || artist.Providers[0] != "ytmusic" {
		t.Errorf("providers = %v", artist.Providers)
	}

	// Sync the artist's albums, including their tracklists.
	rec = c.do(http.MethodPost, "/api/v1/artists/"+artist.ID+"/sync", "", map[string]any{"syncAlbums": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("artist sync = %d: %s", rec.Code, rec.Body.String())
	}
	var synced syncResponse
	c.decode(rec, &synced)
	if len(synced.Albums) != 1 {
		t.Fatalf("albums = %+v", synced.Albums)
	}
	if synced.Added != 2 {
		t.Errorf("added = %d, want the album's two tracks", synced.Added)
	}

	rec = c.do(http.MethodGet, "/api/v1/artists/"+artist.ID, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("artist get = %d", rec.Code)
	}
	var detail artistResponse
	c.decode(rec, &detail)
	if len(detail.Albums) != 1 {
		t.Fatalf("artist = %+v", detail)
	}
	if detail.Albums[0].TrackCount != 2 {
		t.Errorf("album = %+v", detail.Albums[0])
	}

	if rec := c.do(http.MethodGet, "/api/v1/artists/00000000-0000-0000-0000-000000000000", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown artist = %d, want 404", rec.Code)
	}
}
