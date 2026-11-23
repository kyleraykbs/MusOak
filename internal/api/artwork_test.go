package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/kyleraykbs/MusOak/internal/provider"
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
func (a *artworkProvider) Download(ctx context.Context, id, dest string, _ provider.ProgressFunc) error {
	return nil
}

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

// tinyPNG is a 1x1 PNG: the cover cache keeps what the bytes show to be an
// image, so a stand-in has to be one.
var tinyPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0a, 0x49, 0x44, 0x41,
	0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00,
	0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
	0x42, 0x60, 0x82,
}

// A cover's address can be handed in by a client, so what comes back from it is
// only kept when it is a picture: a page served under an image's name is not
// stored, and not served from this server's own origin.
func TestArtworkEndpointRefusesWhatIsNotAnImage(t *testing.T) {
	notAnImage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("<html><script>alert(1)</script></html>"))
	}))
	defer notAnImage.Close()

	c := newHTTPTestServer(t, nil)
	c.providers.Register(&artworkProvider{name: "ytmusic", artwork: notAnImage.URL + "/cover.png"})
	rec := c.do(http.MethodGet, "/api/v1/search?q=anything", "", nil)
	var search searchResponse
	c.decode(rec, &search)
	if len(search.Groups) == 0 || search.Groups[0].Track.ArtworkURL == "" {
		t.Fatalf("search = %s", rec.Body.String())
	}

	rec = c.do(http.MethodGet, search.Groups[0].Track.ArtworkURL, "", nil)
	if rec.Code == http.StatusOK {
		t.Fatalf("markup was served as a cover: %q (%s)", rec.Body.String(), rec.Header().Get("Content-Type"))
	}
}

func TestArtworkEndpointServesAndCachesCovers(t *testing.T) {
	var imageHits atomic.Int32
	image := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		imageHits.Add(1)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(tinyPNG)
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
	if body := rec.Body.Bytes(); !bytes.Equal(body, tinyPNG) {
		t.Errorf("body = %q", body)
	}

	rec = c.do(http.MethodGet, track.ArtworkURL, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("second artwork request = %d", rec.Code)
	}
	if hits := imageHits.Load(); hits != 1 {
		t.Errorf("the provider was asked %d times, want 1", hits)
	}

	// A row that asks for a small cover asks for a width: the answer is the
	// cover either way, shrunk where it can be and as it is where it cannot.
	rec = c.do(http.MethodGet, track.ArtworkURL+"?w=64", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("artwork with a width = %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "image/jpeg" && got != "image/png" {
		t.Errorf("a cover asked for at a width came back as %q", got)
	}
	// Garbage in the width is not an error either: it is read as "no width".
	if rec := c.do(http.MethodGet, track.ArtworkURL+"?w=nonsense", "", nil); rec.Code != http.StatusOK {
		t.Errorf("artwork with a nonsense width = %d, want the cover", rec.Code)
	}

	// An entity the providers never gave a cover to answers 404.
	lonely := seedTracks(t, c, "No Cover")[0]
	rec = c.do(http.MethodGet, "/api/v1/artwork/track/"+lonely.ID.String(), "", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("artwork for a track without one = %d, want 404", rec.Code)
	}
	// A cover can be learned later, and a CDN that is down comes back: a miss
	// the browser keeps is an image nothing can fix short of a hard reload.
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("a missing cover: Cache-Control = %q, want no-store", got)
	}

	// Kinds and ids are validated.
	if rec := c.do(http.MethodGet, "/api/v1/artwork/nonsense/"+track.ID, "", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown kind = %d, want 400", rec.Code)
	}
	if rec := c.do(http.MethodGet, "/api/v1/artwork/track/not-a-uuid", "", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad id = %d, want 400", rec.Code)
	}
}
