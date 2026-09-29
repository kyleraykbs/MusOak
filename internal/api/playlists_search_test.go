package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"codeberg.org/kyleraykbs/prismusic/internal/provider"
)

// fakePlaylistProvider serves playlist browsing.
type fakePlaylistProvider struct {
	name      string
	playlists []provider.Playlist
	details   map[string]*provider.PlaylistDetail
	searchErr error
	browseErr error
}

func (f *fakePlaylistProvider) Name() string { return f.name }
func (f *fakePlaylistProvider) Capabilities() provider.Caps {
	return provider.Caps{Search: true, Download: true, Playlists: true}
}
func (f *fakePlaylistProvider) Search(ctx context.Context, q string, opts provider.SearchOpts) ([]provider.Track, error) {
	return nil, nil
}
func (f *fakePlaylistProvider) Download(ctx context.Context, id, dest string) error { return nil }
func (f *fakePlaylistProvider) SearchPlaylists(ctx context.Context, q string, opts provider.SearchOpts) ([]provider.Playlist, error) {
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	return f.playlists, nil
}
func (f *fakePlaylistProvider) Playlist(ctx context.Context, id string) (*provider.PlaylistDetail, error) {
	if f.browseErr != nil {
		return nil, f.browseErr
	}
	detail, ok := f.details[id]
	if !ok {
		return nil, errors.New("no such playlist")
	}
	return detail, nil
}

func newPlaylistProvider() *fakePlaylistProvider {
	return &fakePlaylistProvider{
		name: "ytmusic",
		playlists: []provider.Playlist{{
			ProviderPlaylistID: "PLlate",
			Title:              "Late night",
			Owner:              "kyle",
			TrackCount:         2,
			ArtworkURL:         "https://cdn.example/late.jpg",
		}},
		details: map[string]*provider.PlaylistDetail{
			"PLlate": {
				Playlist: provider.Playlist{
					ProviderPlaylistID: "PLlate",
					Title:              "Late night",
					Owner:              "kyle",
					TrackCount:         2,
					ArtworkURL:         "https://cdn.example/late.jpg",
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
	}
}

func TestPlaylistSearchAndSync(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	c.providers.Register(newPlaylistProvider())

	rec := c.do(http.MethodGet, "/api/v1/playlists/search?q=late", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("playlist search = %d: %s", rec.Code, rec.Body.String())
	}
	var search struct {
		Query     string                     `json:"query"`
		Playlists []providerPlaylistResponse `json:"playlists"`
	}
	c.decode(rec, &search)
	if len(search.Playlists) != 1 {
		t.Fatalf("playlists = %+v", search.Playlists)
	}
	playlist := search.Playlists[0]
	if playlist.Title != "Late night" || playlist.Provider != "ytmusic" {
		t.Fatalf("playlist = %+v", playlist)
	}
	if playlist.TrackCount != 2 || playlist.Owner != "kyle" {
		t.Errorf("playlist metadata = %+v", playlist)
	}
	if playlist.Synced {
		t.Error("a search hit has no tracks yet")
	}
	if playlist.ArtworkURL != artworkPathOf(playlist.ID) {
		t.Errorf("artworkUrl = %q, want the server's own path", playlist.ArtworkURL)
	}

	// Playing or importing needs the tracks, which is what sync is for.
	rec = c.do(http.MethodPost, "/api/v1/playlists/"+playlist.ID+"/sync", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("playlist sync = %d: %s", rec.Code, rec.Body.String())
	}
	var synced providerPlaylistSyncResponse
	c.decode(rec, &synced)
	if len(synced.Tracks) != 2 {
		t.Fatalf("tracks = %+v", synced.Tracks)
	}
	if synced.Tracks[0].Title != "Never Gonna Give You Up" || synced.Tracks[1].Title != "Together Forever" {
		t.Fatalf("tracks are not in playlist order: %+v", synced.Tracks)
	}
	if synced.Added != 2 {
		t.Errorf("added = %d, want 2", synced.Added)
	}
	if !synced.Playlist.Synced {
		t.Error("a synced playlist must say so")
	}

	// The synced tracks are canonical, so they can be added to a playlist of
	// the caller's own, which is how import works.
	token := c.register("importer", "hunter2hunter2")
	rec = c.do(http.MethodPost, "/api/v1/me/playlists", token, map[string]any{"name": "Imported"})
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("create playlist = %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	c.decode(rec, &created)
	rec = c.do(http.MethodPost, "/api/v1/me/playlists/"+created.ID+"/items", token, map[string]any{
		"trackIds": []string{synced.Tracks[0].ID, synced.Tracks[1].ID},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("add imported tracks = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPlaylistSearchNeedsAQuery(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	c.providers.Register(newPlaylistProvider())

	rec := c.do(http.MethodGet, "/api/v1/playlists/search", "", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestPlaylistSearchReportsAProviderThatFailed(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	broken := newPlaylistProvider()
	broken.searchErr = errors.New("token expired")
	c.providers.Register(broken)

	rec := c.do(http.MethodGet, "/api/v1/playlists/search?q=late", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var search struct {
		Playlists      []providerPlaylistResponse `json:"playlists"`
		ProviderErrors []providerProblem          `json:"providerErrors"`
	}
	c.decode(rec, &search)
	if len(search.Playlists) != 0 {
		t.Fatalf("playlists = %+v", search.Playlists)
	}
	if len(search.ProviderErrors) != 1 || search.ProviderErrors[0].Provider != "ytmusic" {
		t.Fatalf("providerErrors = %+v", search.ProviderErrors)
	}
}

func TestPlaylistSyncOnAnUnknownPlaylist(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	c.providers.Register(newPlaylistProvider())

	rec := c.do(http.MethodPost, "/api/v1/playlists/not-a-uuid/sync", "", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	rec = c.do(http.MethodPost, "/api/v1/playlists/6f0c1f6a-0000-4000-8000-000000000000/sync", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

// artworkPathOf builds the path the server serves a playlist's cover from.
func artworkPathOf(playlistID string) string {
	return "/api/v1/artwork/playlist/" + playlistID
}
