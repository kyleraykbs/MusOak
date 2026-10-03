package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"codeberg.org/kyleraykbs/musoak/internal/provider"
)

// artworkProvider returns one hit carrying a cover URL.
type artworkProvider struct {
	name     string
	artwork  string
	searches atomic.Int32
}

func (a *artworkProvider) Name() string { return a.name }
func (a *artworkProvider) Capabilities() provider.Caps {
	return provider.Caps{Search: true, Download: true}
}
func (a *artworkProvider) Download(ctx context.Context, id, dest string) error { return nil }

func (a *artworkProvider) Search(ctx context.Context, q string, opts provider.SearchOpts) ([]provider.Track, error) {
	a.searches.Add(1)
	return []provider.Track{{
		ProviderTrackID: "yt-art",
		Title:           "Covered Song",
		Artists:         []string{"An Artist"},
		Album:           "Covered Album",
		DurationMs:      200_000,
		ArtworkURL:      a.artwork,
	}}, nil
}

func TestArtworkEndpointServesAndCachesCovers(t *testing.T) {
	var imageHits atomic.Int32
	image := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		imageHits.Add(1)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("fake-png-bytes"))
	}))
	defer image.Close()

	c := newHTTPTestServer(t, nil)
	c.providers.Register(&artworkProvider{name: "ytmusic", artwork: image.URL + "/cover.png"})

	// A search teaches the library the cover.
	rec := c.do(http.MethodGet, "/api/v1/search?q=covered", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("search = %d: %s", rec.Code, rec.Body.String())
	}
	var search searchResponse
	c.decode(rec, &search)
	if len(search.Groups) != 1 {
		t.Fatalf("groups = %+v", search.Groups)
	}
	track := search.Groups[0].Track
	if track.ArtworkURL != "/api/v1/artwork/track/"+track.ID {
		t.Fatalf("artworkUrl = %q, want a path on this server", track.ArtworkURL)
	}

	// Fetching it downloads once and caches.
	rec = c.do(http.MethodGet, track.ArtworkURL, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("artwork = %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "image/png" {
		t.Errorf("content type = %q", got)
	}
	if rec.Header().Get("ETag") == "" {
		t.Error("a cached cover should carry an ETag")
	}
	if body := rec.Body.String(); body != "fake-png-bytes" {
		t.Errorf("body = %q", body)
	}

	rec = c.do(http.MethodGet, track.ArtworkURL, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("second artwork request = %d", rec.Code)
	}
	if hits := imageHits.Load(); hits != 1 {
		t.Errorf("the provider was asked %d times, want 1", hits)
	}

	// An entity the providers never gave a cover to answers 404.
	lonely := seedTracks(t, c, "No Cover")[0]
	rec = c.do(http.MethodGet, "/api/v1/artwork/track/"+lonely.ID.String(), "", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("artwork for a track without one = %d, want 404", rec.Code)
	}

	// Kinds and ids are validated.
	if rec := c.do(http.MethodGet, "/api/v1/artwork/nonsense/"+track.ID, "", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown kind = %d, want 400", rec.Code)
	}
	if rec := c.do(http.MethodGet, "/api/v1/artwork/track/not-a-uuid", "", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad id = %d, want 400", rec.Code)
	}
}
