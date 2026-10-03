package spotify

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codeberg.org/kyleraykbs/musoak/internal/provider"
)

// fakeSpotify serves the token and search endpoints and counts the calls.
type fakeSpotify struct {
	server *httptest.Server

	tokenCalls  atomic.Int32
	searchCalls atomic.Int32

	// expiresIn is what the token endpoint reports.
	expiresIn int
	// tokenFail makes the token endpoint fail.
	tokenFail bool
	// searchFail makes the search endpoint fail.
	searchFail bool
	// hits is what the search endpoint returns.
	hits []map[string]any
}

func newFakeSpotify(t *testing.T) *fakeSpotify {
	t.Helper()
	fake := &fakeSpotify{expiresIn: 3600}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		fake.tokenCalls.Add(1)
		if fake.tokenFail {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "missing basic auth", http.StatusUnauthorized)
			return
		}
		if err := r.ParseForm(); err != nil || r.FormValue("grant_type") != "client_credentials" {
			http.Error(w, "bad grant", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "test-token",
			"expires_in":   fake.expiresIn,
		})
	})
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		fake.searchCalls.Add(1)
		if fake.searchFail {
			http.Error(w, `{"error":{"status":502,"message":"upstream"}}`, http.StatusBadGateway)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			http.Error(w, "bad authorization: "+got, http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("type") != "track" || r.URL.Query().Get("q") == "" {
			http.Error(w, "bad query", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tracks": map[string]any{"items": fake.hits},
		})
	})

	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeSpotify) provider(t *testing.T) *Provider {
	t.Helper()
	p := New("client-id", "client-secret", slog.New(slog.DiscardHandler))
	p.apiBase = f.server.URL
	p.tokenURL = f.server.URL + "/token"
	return p
}

func (f *fakeSpotify) setHits(hits ...map[string]any) { f.hits = hits }

func TestSearchMapsSpotifyTracks(t *testing.T) {
	fake := newFakeSpotify(t)
	fake.setHits(map[string]any{
		"id":          "4cOdK2wGLETKBW3PvgPWqT",
		"name":        "Never Gonna Give You Up",
		"duration_ms": 213573,
		"artists":     []map[string]any{{"name": "Rick Astley"}},
		"album":       map[string]any{"name": "Whenever You Need Somebody"},
		"external_ids": map[string]any{
			"isrc": "gbaye8700001",
		},
	}, map[string]any{
		"id":          "",
		"name":        "no id, dropped",
		"duration_ms": 1000,
	})

	tracks, err := fake.provider(t).Search(context.Background(), "never gonna", provider.SearchOpts{Limit: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(tracks) != 1 {
		t.Fatalf("tracks = %d, want 1 (entries without an id are dropped)", len(tracks))
	}
	track := tracks[0]
	if track.ProviderTrackID != "4cOdK2wGLETKBW3PvgPWqT" || track.Title != "Never Gonna Give You Up" {
		t.Errorf("track = %+v", track)
	}
	if track.DurationMs != 213573 || track.Album != "Whenever You Need Somebody" {
		t.Errorf("track = %+v", track)
	}
	if len(track.Artists) != 1 || track.Artists[0] != "Rick Astley" {
		t.Errorf("artists = %v", track.Artists)
	}
	// The ISRC is normalised to upper case: it is the strongest match signal.
	if track.ISRC != "GBAYE8700001" {
		t.Errorf("isrc = %q, want it upper-cased", track.ISRC)
	}
}

func TestTokenIsCachedUntilItExpires(t *testing.T) {
	fake := newFakeSpotify(t)
	p := fake.provider(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := p.Search(ctx, "query", provider.SearchOpts{}); err != nil {
			t.Fatalf("Search: %v", err)
		}
	}
	if got := fake.tokenCalls.Load(); got != 1 {
		t.Errorf("token requests = %d, want 1 (the token is cached)", got)
	}
	if got := fake.searchCalls.Load(); got != 3 {
		t.Errorf("search requests = %d, want 3", got)
	}

	// A token that expires immediately must be renewed on the next use.
	fake.expiresIn = 1
	p.mu.Lock()
	p.expiresAt = time.Now().Add(-time.Second)
	p.mu.Unlock()

	if _, err := p.Search(ctx, "query", provider.SearchOpts{}); err != nil {
		t.Fatalf("Search after expiry: %v", err)
	}
	if got := fake.tokenCalls.Load(); got != 2 {
		t.Errorf("token requests = %d, want 2 after expiry", got)
	}
}

func TestSearchReportsFailures(t *testing.T) {
	fake := newFakeSpotify(t)
	p := fake.provider(t)
	ctx := context.Background()

	fake.searchFail = true
	_, err := p.Search(ctx, "query", provider.SearchOpts{})
	if err == nil {
		t.Fatal("want an error when Spotify answers with a failure")
	}
	if !strings.Contains(err.Error(), "502") && !strings.Contains(err.Error(), "upstream") {
		t.Errorf("error = %v, want it to carry the upstream message", err)
	}

	fake.searchFail = false
	fake.tokenFail = true
	// A fresh provider has no cached token, so it must ask for one and fail.
	fresh := fake.provider(t)
	if _, err := fresh.Search(ctx, "query", provider.SearchOpts{}); err == nil {
		t.Fatal("want an error when the token request fails")
	}
}

func TestSearchWithoutCredentialsFails(t *testing.T) {
	p := New("", "", slog.New(slog.DiscardHandler))
	if _, err := p.Search(context.Background(), "query", provider.SearchOpts{}); err == nil {
		t.Fatal("want a clear error when the credentials are missing")
	} else if !strings.Contains(err.Error(), "clientId") {
		t.Errorf("error = %v, want it to name the missing configuration", err)
	}
}

func TestSearchLimitIsClampedToSpotifyCap(t *testing.T) {
	fake := newFakeSpotify(t)
	var seen string
	fake.hits = nil

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/search" {
			seen = r.URL.Query().Get("limit")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tracks": map[string]any{"items": []any{}},
		})
	}))
	defer server.Close()

	p := fake.provider(t)
	p.apiBase = server.URL
	if _, err := p.Search(context.Background(), "query", provider.SearchOpts{Limit: 500}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if seen != "50" {
		t.Errorf("limit = %q, want 50", seen)
	}
}

func TestSpotifyNeverDownloads(t *testing.T) {
	p := New("id", "secret", slog.New(slog.DiscardHandler))

	if caps := p.Capabilities(); caps.Download {
		t.Error("Spotify must never advertise downloads")
	}
	err := p.Download(context.Background(), "4cOdK2wGLETKBW3PvgPWqT", "/tmp/out.opus")
	if !errors.Is(err, provider.ErrDownloadUnsupported) {
		t.Fatalf("err = %v, want ErrDownloadUnsupported", err)
	}
	if !strings.Contains(err.Error(), "DRM") {
		t.Errorf("error = %v, want it to explain why", err)
	}
}

// TestRegistryRefusesSpotifyDownloads is the server-level guarantee: even asked
// directly, the registry will not route a download to Spotify.
func TestRegistryRefusesSpotifyDownloads(t *testing.T) {
	registry := provider.NewRegistry(slog.New(slog.DiscardHandler), time.Second)
	registry.Register(New("id", "secret", slog.New(slog.DiscardHandler)))

	err := registry.Download(context.Background(), "spotify", "track-id", "/tmp/out.opus")
	if !errors.Is(err, provider.ErrDownloadUnsupported) {
		t.Fatalf("err = %v, want ErrDownloadUnsupported", err)
	}
}

func TestTokenRequestShape(t *testing.T) {
	var (
		seenAuth  string
		seenGrant string
		seenForm  url.Values
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		if err := r.ParseForm(); err == nil {
			seenForm = r.Form
		}
		seenGrant = r.FormValue("grant_type")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "t", "expires_in": 3600})
	}))
	defer server.Close()

	p := New("my-id", "my-secret", slog.New(slog.DiscardHandler))
	p.tokenURL = server.URL
	if _, err := p.accessToken(context.Background()); err != nil {
		t.Fatalf("accessToken: %v", err)
	}

	if !strings.HasPrefix(seenAuth, "Basic ") {
		t.Errorf("authorization = %q, want basic auth with the client credentials", seenAuth)
	}
	if seenGrant != "client_credentials" {
		t.Errorf("grant_type = %q", seenGrant)
	}
	if seenForm.Get("client_id") != "" {
		t.Errorf("client credentials must travel in the header, not the form: %v", seenForm)
	}
}

func TestSearchPicksTheLargestCover(t *testing.T) {
	fake := newFakeSpotify(t)
	fake.setHits(map[string]any{
		"id":          "4cOdK2wGLETKBW3PvgPWqT",
		"name":        "Never Gonna Give You Up",
		"duration_ms": 213573,
		"artists":     []map[string]any{{"name": "Rick Astley"}},
		"album": map[string]any{
			"name": "Whenever You Need Somebody",
			"images": []map[string]any{
				{"url": "https://i.scdn.co/small.jpg", "width": 64, "height": 64},
				{"url": "https://i.scdn.co/large.jpg", "width": 640, "height": 640},
				{"url": "", "width": 1000, "height": 1000},
			},
		},
		"external_ids": map[string]any{"isrc": "GBAYE8700001"},
	})

	tracks, err := fake.provider(t).Search(context.Background(), "never gonna", provider.SearchOpts{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(tracks) != 1 || tracks[0].ArtworkURL != "https://i.scdn.co/large.jpg" {
		t.Fatalf("tracks = %+v", tracks)
	}
}

// The embed page is the only way in without credentials, and its shape is
// Spotify's to change: these pin what is read out of it, and that a page
// without a playlist is reported rather than half-read.
func TestPublicPlaylistReadsTheEmbedPage(t *testing.T) {
	const page = `<!DOCTYPE html><html><head></head><body>
<script id="__NEXT_DATA__" type="application/json">{"props":{"pageProps":{"state":{"data":{"entity":{"type":"playlist","name":"Road Trip","title":"Road Trip","subtitle":"kyleraykbs","trackList":[{"uri":"spotify:track:abc123","title":"First Song","subtitle":"Artist One, Artist Two","duration":213000},{"uri":"spotify:track:def456","title":"Second Song","subtitle":"Someone Else","duration":190500},{"uri":"","title":"No id","subtitle":"Nobody","duration":1000}]}}}}}}</script>
</body></html>`

	entity, ok := embedEntity([]byte(page))
	if !ok {
		t.Fatal("no playlist found in the embed page")
	}
	detail := detailFromEntity("PL1", entity)

	if detail.Title != "Road Trip" || detail.Owner != "kyleraykbs" {
		t.Fatalf("playlist = %+v", detail.Playlist)
	}
	if len(detail.Tracks) != 2 || detail.TrackCount != 2 {
		t.Fatalf("tracks = %d (%+v)", len(detail.Tracks), detail.Tracks)
	}
	first := detail.Tracks[0]
	if first.ProviderTrackID != "abc123" || first.Title != "First Song" {
		t.Fatalf("first track = %+v", first)
	}
	if len(first.Artists) != 2 || first.Artists[1] != "Artist Two" {
		t.Fatalf("first track artists = %+v", first.Artists)
	}
	if first.DurationMs != 213000 {
		t.Fatalf("first track duration = %d", first.DurationMs)
	}
}

func TestEmbedEntityFindsThePlaylistWhereverItSits(t *testing.T) {
	// Spotify has moved this object before; the search is by shape, not path.
	const page = `<script id="__NEXT_DATA__" type="application/json">{"a":{"b":[{"c":{"title":"Deep","trackList":[{"uri":"spotify:track:x","title":"One","subtitle":"An Artist"}]}}]}}</script>`

	entity, ok := embedEntity([]byte(page))
	if !ok {
		t.Fatal("no playlist found when it is nested")
	}
	if detail := detailFromEntity("id", entity); len(detail.Tracks) != 1 {
		t.Fatalf("tracks = %+v", detail.Tracks)
	}
}

func TestEmbedEntityReportsAPageWithoutAPlaylist(t *testing.T) {
	cases := map[string]string{
		"no script":   `<html><body>nothing here</body></html>`,
		"broken json": `<script id="__NEXT_DATA__" type="application/json">{oops</script>`,
		"no playlist": `<script id="__NEXT_DATA__" type="application/json">{"props":{"pageProps":{"state":{"data":{"entity":{"type":"album","name":"Not a playlist"}}}}}}</script>`,
	}
	for name, page := range cases {
		if _, ok := embedEntity([]byte(page)); ok {
			t.Fatalf("%s: reported a playlist", name)
		}
	}
}

func TestConfiguredNeedsBothHalvesOfTheCredentials(t *testing.T) {
	if (&Provider{clientID: "id"}).configured() {
		t.Fatal("an id without a secret is not configured")
	}
	if (&Provider{}).configured() {
		t.Fatal("nothing is not configured")
	}
	if !(&Provider{clientID: "id", clientSecret: "secret"}).configured() {
		t.Fatal("both halves are configured")
	}
}
