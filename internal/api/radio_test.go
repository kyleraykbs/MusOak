package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/kyleraykbs/MusOak/internal/provider"
	"github.com/kyleraykbs/MusOak/internal/store"
)

// fakeRadioProvider is a station-capable provider with canned suggestions.
type fakeRadioProvider struct {
	name   string
	caps   provider.Caps
	tracks []provider.Track
	err    error
}

func (f *fakeRadioProvider) Name() string                { return f.name }
func (f *fakeRadioProvider) Capabilities() provider.Caps { return f.caps }
func (f *fakeRadioProvider) Download(ctx context.Context, id, dest string, _ provider.ProgressFunc) error {
	return nil
}
func (f *fakeRadioProvider) Search(ctx context.Context, q string, opts provider.SearchOpts) ([]provider.Track, error) {
	return nil, nil
}

func (f *fakeRadioProvider) Radio(ctx context.Context, seed provider.Track, limit int) ([]provider.Track, error) {
	return f.tracks, f.err
}

// seedWithVariants creates a canonical track with one rendition per provider.
func seedWithVariants(t *testing.T, c *testClient, title string, providers ...string) string {
	t.Helper()
	ctx := context.Background()
	track := seedTracks(t, c, title)[0]
	if err := c.store.SetTrackArtists(ctx, track.ID, []string{"Some Artist"}); err != nil {
		t.Fatalf("SetTrackArtists: %v", err)
	}
	for _, name := range providers {
		variant := &store.Variant{
			TrackID:         track.ID,
			Provider:        name,
			ProviderTrackID: fmt.Sprintf("%s-%s", name, track.ID),
			Title:           title,
			Artists:         []string{"Some Artist"},
			DurationMs:      200_000,
			Downloadable:    true,
		}
		if err := c.store.CreateVariant(ctx, variant); err != nil {
			t.Fatalf("CreateVariant(%s): %v", name, err)
		}
	}
	return track.ID.String()
}

// seedWithVariant creates a canonical track with one rendition in providerName.
func seedWithVariant(t *testing.T, c *testClient, title, providerName string) string {
	t.Helper()
	return seedWithVariants(t, c, title, providerName)
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

// A station that cannot be built says why. An empty list says nothing about the
// reason - a provider that was never asked, one that failed, and one that had
// nothing to suggest all look the same - so each of those is reported with the
// provider it came from, and an empty station is not saved as an empty playlist.
func TestRadioSaysWhyAStationIsEmpty(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	ytmusic := &fakeRadioProvider{name: "ytmusic", caps: provider.Caps{Search: true, Radio: true}}
	youtube := &fakeRadioProvider{
		name: "youtube",
		caps: provider.Caps{Search: true, Radio: true},
		tracks: []provider.Track{
			{ProviderTrackID: "yt-1", Title: "Survivor", Artists: []string{"Some Artist"}, DurationMs: 201_000},
		},
	}
	for _, p := range []*fakeRadioProvider{ytmusic, youtube} {
		c.providers.Register(p)
	}
	// A platform with a seed rendition that cannot build a station at all.
	c.providers.Register(&fakeRadioProvider{name: "spotify", caps: provider.Caps{Search: true}})

	seed := seedWithVariants(t, c, "Seed Song", "ytmusic", "youtube")
	token := c.register("kyle", "hunter2hunter2")

	// A provider that cannot build a station is not the same as one that was
	// asked and had no rendition of the seed, and it must not be reported as
	// if it were.
	rec := c.do(http.MethodPost, "/api/v1/radio", token, map[string]any{
		"seedTrackId": seed, "providers": []string{"spotify"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("provider that cannot build a radio = %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "spotify") || !strings.Contains(body, "cannot build a radio") {
		t.Errorf("body = %s, want the provider named as the reason", body)
	}

	// A seed that the selected provider has no rendition of names it.
	barren := seedWithVariants(t, c, "No Renditions", "ytmusic")
	rec = c.do(http.MethodPost, "/api/v1/radio", token, map[string]any{
		"seedTrackId": barren, "providers": []string{"youtube"},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("seed without a rendition = %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "youtube") {
		t.Errorf("body = %s, want the provider it was asked of", body)
	}

	// Providers that answer with nothing at all: no station, and the reason
	// that there was nothing to build one from. The caller asked for the save
	// that is the default, and an empty playlist is not a station either.
	rec = c.do(http.MethodPost, "/api/v1/radio", token, map[string]any{
		"seedTrackId": seed, "providers": []string{"ytmusic"},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("empty station = %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "no suggestions") {
		t.Errorf("body = %s, want the providers' silence reported", body)
	}
	var listing struct {
		Playlists []playlistResponse `json:"playlists"`
	}
	if rec := c.do(http.MethodGet, "/api/v1/me/playlists", token, nil); rec.Code == http.StatusOK {
		c.decode(rec, &listing)
		if len(listing.Playlists) != 0 {
			t.Fatalf("an empty station was saved as a playlist: %+v", listing.Playlists)
		}
	}

	// One provider failing while another contributes is not an empty station:
	// the station is what answered, and the failure is named beside it.
	ytmusic.err = errors.New("connection refused")
	rec = c.do(http.MethodPost, "/api/v1/radio", token, map[string]any{
		"seedTrackId": seed, "providers": []string{"ytmusic", "youtube"}, "save": false,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("radio with one failing provider = %d: %s", rec.Code, rec.Body.String())
	}
	var partial radioResponse
	c.decode(rec, &partial)
	if len(partial.Tracks) != 1 {
		t.Fatalf("tracks = %+v, want the station the other provider built", partial.Tracks)
	}
	if len(partial.ProviderErrors) != 1 || partial.ProviderErrors[0].Provider != "ytmusic" {
		t.Fatalf("providerErrors = %+v, want ytmusic's failure", partial.ProviderErrors)
	}
	if partial.ProviderErrors[0].Error == "" {
		t.Error("the failure was reported without its reason")
	}

	// Every provider failing is an upstream failure - worth another try - and
	// it says which one failed and why.
	rec = c.do(http.MethodPost, "/api/v1/radio", token, map[string]any{
		"seedTrackId": seed, "providers": []string{"ytmusic"}, "save": false,
	})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("every provider failing = %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "connection refused") {
		t.Errorf("body = %s, want the provider's own failure", body)
	}
}
