package api

import (
	"context"
	"net/http"
	"testing"

	"codeberg.org/kyleraykbs/prismusic/internal/provider"
	"codeberg.org/kyleraykbs/prismusic/internal/store"
)

// fakeRadioProvider is a station-capable provider with canned suggestions.
type fakeRadioProvider struct {
	name   string
	caps   provider.Caps
	tracks []provider.Track
}

func (f *fakeRadioProvider) Name() string                                        { return f.name }
func (f *fakeRadioProvider) Capabilities() provider.Caps                         { return f.caps }
func (f *fakeRadioProvider) Download(ctx context.Context, id, dest string) error { return nil }
func (f *fakeRadioProvider) Search(ctx context.Context, q string, opts provider.SearchOpts) ([]provider.Track, error) {
	return nil, nil
}

func (f *fakeRadioProvider) Radio(ctx context.Context, seed provider.Track, limit int) ([]provider.Track, error) {
	return f.tracks, nil
}

// seedWithVariant creates a canonical track with one rendition in providerName.
func seedWithVariant(t *testing.T, c *testClient, title, providerName string) string {
	t.Helper()
	ctx := context.Background()
	track := seedTracks(t, c, title)[0]
	if err := c.store.SetTrackArtists(ctx, track.ID, []string{"Some Artist"}); err != nil {
		t.Fatalf("SetTrackArtists: %v", err)
	}
	variant := &store.Variant{
		TrackID:         track.ID,
		Provider:        providerName,
		ProviderTrackID: providerName + "-seed",
		Title:           title,
		Artists:         []string{"Some Artist"},
		DurationMs:      200_000,
		Downloadable:    true,
	}
	if err := c.store.CreateVariant(ctx, variant); err != nil {
		t.Fatalf("CreateVariant: %v", err)
	}
	return track.ID.String()
}

func TestRadioEndpoint(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	c.providers.Register(&fakeRadioProvider{
		name: "ytmusic",
		caps: provider.Caps{Search: true, Radio: true},
		tracks: []provider.Track{
			{ProviderTrackID: "yt-1", Title: "Station One", Artists: []string{"Some Artist"}, DurationMs: 201_000},
			{ProviderTrackID: "yt-2", Title: "Station Two", Artists: []string{"Some Artist"}, DurationMs: 202_000},
		},
	})
	seed := seedWithVariant(t, c, "Seed Song", "ytmusic")
	token := c.register("kyle", "hunter2hunter2")

	// A guest can listen to a station but not save one.
	rec := c.do(http.MethodPost, "/api/v1/radio", "", map[string]any{
		"seedTrackId": seed, "providers": []string{"ytmusic"}, "save": false,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("guest radio = %d: %s", rec.Code, rec.Body.String())
	}
	var guest radioResponse
	c.decode(rec, &guest)
	if len(guest.Tracks) != 2 || guest.Tracks[0].Title != "Station One" {
		t.Fatalf("tracks = %+v", guest.Tracks)
	}
	if guest.Playlist != nil {
		t.Error("a guest station must not be saved")
	}
	if len(guest.Providers) != 1 || guest.Providers[0] != "ytmusic" {
		t.Errorf("providers = %v", guest.Providers)
	}
	if guest.Seed.ID != seed {
		t.Errorf("seed = %s, want %s", guest.Seed.ID, seed)
	}

	// An account can save the station as a playlist, which is the default.
	rec = c.do(http.MethodPost, "/api/v1/radio", token, map[string]any{
		"seedTrackId": seed, "providers": []string{"ytmusic"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("saved radio = %d: %s", rec.Code, rec.Body.String())
	}
	var saved radioResponse
	c.decode(rec, &saved)
	if saved.Playlist == nil {
		t.Fatal("the station was not saved as a playlist")
	}
	if saved.Playlist.TrackCount != 2 {
		t.Errorf("playlist = %+v", saved.Playlist)
	}
	rec = c.do(http.MethodGet, "/api/v1/me/playlists/"+saved.Playlist.ID, token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("the saved playlist is not readable: %d", rec.Code)
	}
	var detail playlistDetailResponse
	c.decode(rec, &detail)
	if len(detail.Items) != 2 || detail.Items[0].Track.Title != "Station One" {
		t.Fatalf("playlist items = %+v", detail.Items)
	}

	// A custom name is honoured.
	rec = c.do(http.MethodPost, "/api/v1/radio", token, map[string]any{
		"seedTrackId": seed, "name": "late night",
	})
	var named radioResponse
	c.decode(rec, &named)
	if named.Playlist == nil || named.Playlist.Name != "late night" {
		t.Fatalf("named station = %+v", named.Playlist)
	}

	// Requests that cannot work are refused clearly.
	if rec := c.do(http.MethodPost, "/api/v1/radio", token, map[string]any{}); rec.Code != http.StatusBadRequest {
		t.Errorf("missing seed = %d, want 400", rec.Code)
	}
	if rec := c.do(http.MethodPost, "/api/v1/radio", token, map[string]any{"seedTrackId": "nope"}); rec.Code != http.StatusBadRequest {
		t.Errorf("bad seed id = %d, want 400", rec.Code)
	}
	if rec := c.do(http.MethodPost, "/api/v1/radio", token, map[string]any{
		"seedTrackId": seed, "providers": []string{"deezer"},
	}); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown provider = %d, want 400", rec.Code)
	}
	rec = c.do(http.MethodPost, "/api/v1/radio", token, map[string]any{
		"seedTrackId": "00000000-0000-0000-0000-000000000000",
	})
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing seed track = %d, want 404", rec.Code)
	}

	// A seed with no rendition in the selected provider cannot grow a station.
	barren := seedTracks(t, c, "No Renditions")[0]
	rec = c.do(http.MethodPost, "/api/v1/radio", token, map[string]any{
		"seedTrackId": barren.ID.String(), "providers": []string{"ytmusic"},
	})
	if rec.Code != http.StatusConflict {
		t.Errorf("seed without renditions = %d, want 409", rec.Code)
	}
}
