package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"codeberg.org/kyleraykbs/musoak/internal/config"
	"codeberg.org/kyleraykbs/musoak/internal/store"
)

type testClient struct {
	*Server
	t *testing.T
}

// newHTTPTestServer builds a server whose providers are all disabled: tests
// never touch the network.
func newHTTPTestServer(t *testing.T, mutate func(*config.Config)) *testClient {
	t.Helper()
	cfg := config.Default()
	cfg.StorageDir = t.TempDir()
	cfg.Providers.YTMusic.Enabled = false
	cfg.Providers.Spotify.Enabled = false
	// The plain YouTube provider would reach the network; a test wants none of it.
	cfg.Providers.YouTube.Enabled = false
	if mutate != nil {
		mutate(cfg)
	}
	s, err := New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return &testClient{Server: s, t: t}
}

func (c *testClient) do(method, path, token string, body any) *httptest.ResponseRecorder {
	c.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	c.Server.Handler().ServeHTTP(rec, req)
	return rec
}

// doRaw sends a body exactly as given, for requests that must not be marshalled
// first — a body that is not JSON on purpose, for instance.
func (c *testClient) doRaw(method, path, token, body string) *httptest.ResponseRecorder {
	c.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c.Server.Handler().ServeHTTP(rec, req)
	return rec
}

func (c *testClient) decode(rec *httptest.ResponseRecorder, v any) {
	c.t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		c.t.Fatalf("decode %T: %v (body %s)", v, err, rec.Body.String())
	}
}

func (c *testClient) register(username, password string) string {
	c.t.Helper()
	rec := c.do(http.MethodPost, "/api/v1/auth/register", "", map[string]string{
		"username": username, "password": password,
	})
	if rec.Code != http.StatusCreated {
		c.t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Token string `json:"token"`
	}
	c.decode(rec, &out)
	if out.Token == "" {
		c.t.Fatal("register returned no token")
	}
	return out.Token
}

func TestRequireLoginRejectsAnonymous(t *testing.T) {
	c := newHTTPTestServer(t, func(cfg *config.Config) { cfg.RequireLogin = true })

	if rec := c.do(http.MethodGet, "/healthz", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200 even with requireLogin", rec.Code)
	}
	if rec := c.do(http.MethodGet, "/api/v1/search?q=x", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous search = %d, want 401", rec.Code)
	}

	token := c.register("kyle", "hunter2hunter2")
	if rec := c.do(http.MethodGet, "/api/v1/search?q=x", token, nil); rec.Code != http.StatusOK {
		t.Fatalf("authenticated search = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

// doAs sends a request as a guest: no token, just the member id their browser
// made, which is how a guest is somebody in a room.
func (c *testClient) doAs(method, path, memberID string, body any) *httptest.ResponseRecorder {
	c.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set(memberHeader, memberID)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	c.Server.Handler().ServeHTTP(rec, req)
	return rec
}

// A room is joined by anybody who has the link, so requireLogin - which is
// about the library - does not stand in front of one. A guest may join, follow
// the room, and hear what it is playing; searching the library still needs an
// account.
func TestRequireLoginLeavesRoomsOpenToGuests(t *testing.T) {
	c := newHTTPTestServer(t, func(cfg *config.Config) { cfg.RequireLogin = true })
	host := c.register("kyle", "hunter2hunter2")

	rec := c.do(http.MethodPost, "/api/v1/rooms", host, map[string]string{"name": "party"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	var created roomResponse
	c.decode(rec, &created)
	roomID := created.Room.ID

	rec = c.doAs(http.MethodPost, "/api/v1/rooms/"+roomID+"/join", "guest-1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("guest join = %d: %s", rec.Code, rec.Body.String())
	}
	var joined roomResponse
	c.decode(rec, &joined)
	if joined.MemberID != "guest-1" {
		t.Errorf("member = %q, want the guest's own id", joined.MemberID)
	}
	found := false
	for _, member := range joined.Room.Members {
		if member.ID == "guest-1" {
			found = true
			if member.UserID != nil {
				t.Error("a guest member is carrying an account")
			}
		}
	}
	if !found {
		t.Errorf("members = %+v, want the guest among them", joined.Room.Members)
	}

	// Following the room, and hearing it, is the same door.
	for _, path := range []string{"/api/v1/rooms/" + roomID, "/api/v1/ws"} {
		if rec := c.doAs(http.MethodGet, path, "guest-1", nil); rec.Code == http.StatusUnauthorized {
			t.Errorf("%s = 401 for a guest, want it reachable", path)
		}
	}
	if rec := c.doAs(http.MethodGet, "/api/v1/media/00000000-0000-0000-0000-000000000000", "guest-1", nil); rec.Code == http.StatusUnauthorized {
		t.Error("media = 401 for a guest, want it reachable")
	}

	// The library is still not open: a guest cannot search it.
	if rec := c.do(http.MethodGet, "/api/v1/search?q=x", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("guest search = %d, want 401", rec.Code)
	}
}

func TestGuestReadsButHasNoPersonalData(t *testing.T) {
	c := newHTTPTestServer(t, nil)

	if rec := c.do(http.MethodGet, "/api/v1/providers", "", nil); rec.Code != http.StatusOK {
		t.Errorf("providers = %d, want 200", rec.Code)
	}
	rec := c.do(http.MethodGet, "/api/v1/search?q=anything", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("search = %d, want 200", rec.Code)
	}
	var search struct {
		Groups []struct{} `json:"groups"`
		Errors []struct {
			Provider string `json:"provider"`
		} `json:"providerErrors"`
	}
	c.decode(rec, &search)
	if len(search.Groups) != 0 || len(search.Errors) != 0 {
		t.Errorf("search with no providers = %+v", search)
	}

	rec = c.do(http.MethodGet, "/api/v1/me", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("me = %d, want 200", rec.Code)
	}
	var me struct {
		Authenticated bool `json:"authenticated"`
		Guest         bool `json:"guest"`
	}
	c.decode(rec, &me)
	if me.Authenticated || !me.Guest {
		t.Errorf("me = %+v, want an anonymous guest", me)
	}

	for _, path := range []string{"/api/v1/me/favorites", "/api/v1/me/providers/ranking"} {
		if rec := c.do(http.MethodGet, path, "", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s = %d, want 401 for a guest", path, rec.Code)
		}
	}
}

func TestFavoritesFlow(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	token := c.register("kyle", "hunter2hunter2")

	track := &store.Track{Title: "Song", DurationMs: 180_000}
	if err := c.store.CreateTrack(context.Background(), track); err != nil {
		t.Fatal(err)
	}

	rec := c.do(http.MethodPost, "/api/v1/me/favorites", token, map[string]string{"trackId": track.ID.String()})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("add favorite = %d: %s", rec.Code, rec.Body.String())
	}
	// Idempotent.
	if rec := c.do(http.MethodPost, "/api/v1/me/favorites", token, map[string]string{"trackId": track.ID.String()}); rec.Code != http.StatusNoContent {
		t.Fatalf("second add = %d", rec.Code)
	}

	rec = c.do(http.MethodGet, "/api/v1/me/favorites", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	var list struct {
		Tracks []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"tracks"`
	}
	c.decode(rec, &list)
	if len(list.Tracks) != 1 || list.Tracks[0].ID != track.ID.String() || list.Tracks[0].Title != "Song" {
		t.Fatalf("favorites = %+v", list)
	}

	if rec := c.do(http.MethodDelete, "/api/v1/me/favorites/"+track.ID.String(), token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("remove = %d", rec.Code)
	}
	if rec := c.do(http.MethodDelete, "/api/v1/me/favorites/"+track.ID.String(), token, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("second remove = %d, want 404", rec.Code)
	}
	if rec := c.do(http.MethodPost, "/api/v1/me/favorites", token, map[string]string{"trackId": "not-a-uuid"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad trackId = %d, want 400", rec.Code)
	}
}

func TestRankingFlow(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	token := c.register("kyle", "hunter2hunter2")

	rec := c.do(http.MethodGet, "/api/v1/me/providers/ranking", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get ranking = %d", rec.Code)
	}
	var before rankingResponse
	c.decode(rec, &before)
	if len(before.Ranking) != 0 {
		t.Errorf("initial ranking = %v, want empty", before.Ranking)
	}
	if len(before.Effective) == 0 || len(before.Default) == 0 {
		t.Errorf("effective/default missing: %+v", before)
	}

	rec = c.do(http.MethodPut, "/api/v1/me/providers/ranking", token, map[string][]string{
		"ranking": {"ytmusic", "spotify"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("put ranking = %d: %s", rec.Code, rec.Body.String())
	}
	var after rankingResponse
	c.decode(rec, &after)
	if len(after.Ranking) != 2 || after.Ranking[0] != "ytmusic" {
		t.Fatalf("ranking = %+v", after)
	}
	if len(after.Effective) != 4 || after.Effective[0] != "self" || after.Effective[1] != "uploaded" || after.Effective[2] != "ytmusic" {
		t.Fatalf("effective = %+v", after.Effective)
	}

	rec = c.do(http.MethodPut, "/api/v1/me/providers/ranking", token, map[string][]string{"ranking": {"deezer"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown provider = %d, want 400", rec.Code)
	}
}

// The effective order must name every enabled provider, even when the
// configured default order predates one - here ytmusic is enabled on the build
// but the default was written when only spotify was.
func TestRankingEffectiveCoversEnabledProviders(t *testing.T) {
	c := newHTTPTestServer(t, func(cfg *config.Config) {
		cfg.Providers.YTMusic.Enabled = true
		cfg.DefaultProviderOrder = []string{"spotify"}
	})
	token := c.register("kyle", "hunter2hunter2")

	rec := c.do(http.MethodGet, "/api/v1/me/providers/ranking", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get ranking = %d: %s", rec.Code, rec.Body.String())
	}
	var got rankingResponse
	c.decode(rec, &got)
	if len(got.Effective) != 4 || got.Effective[0] != "self" || got.Effective[1] != "uploaded" ||
		got.Effective[2] != "spotify" || got.Effective[3] != "ytmusic" {
		t.Fatalf("effective = %v, want [self uploaded spotify ytmusic]", got.Effective)
	}
	// The configured default itself keeps its order.
	if len(got.Default) != 1 || got.Default[0] != "spotify" {
		t.Fatalf("default = %v, want [spotify]", got.Default)
	}
}

func TestLibraryImportAndMediaServing(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	source := toneFile(t, t.TempDir(), "My Song.opus")

	rec := c.do(http.MethodPost, "/api/v1/library/import", "", map[string]string{"path": source})
	if rec.Code != http.StatusOK {
		t.Fatalf("import = %d: %s", rec.Code, rec.Body.String())
	}
	var imported struct {
		Imported []struct {
			Track struct {
				ID    string `json:"id"`
				Title string `json:"title"`
			} `json:"track"`
			Variant struct {
				ID       string `json:"id"`
				Provider string `json:"provider"`
				Media    struct {
					State      string `json:"state"`
					Bytes      int64  `json:"bytes"`
					DurationMs int64  `json:"durationMs"`
				} `json:"media"`
			} `json:"variant"`
		} `json:"imported"`
	}
	c.decode(rec, &imported)
	if len(imported.Imported) != 1 {
		t.Fatalf("imported %d entries", len(imported.Imported))
	}
	entry := imported.Imported[0]
	if entry.Track.Title != "My Song" {
		t.Errorf("title = %q", entry.Track.Title)
	}
	if entry.Variant.Provider != "local" || entry.Variant.Media.State != "ready" {
		t.Errorf("variant = %+v", entry.Variant)
	}
	if entry.Variant.Media.DurationMs <= 0 {
		t.Errorf("duration = %d", entry.Variant.Media.DurationMs)
	}

	// The imported file is playable over HTTP, with a sha256 ETag.
	mediaPath := "/api/v1/media/" + entry.Variant.ID
	rec = c.do(http.MethodGet, mediaPath, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("media = %d", rec.Code)
	}
	if rec.Header().Get("ETag") == "" {
		t.Error("media response has no ETag")
	}
	if int64(rec.Body.Len()) != entry.Variant.Media.Bytes {
		t.Errorf("body = %d bytes, want %d", rec.Body.Len(), entry.Variant.Media.Bytes)
	}

	req := httptest.NewRequest(http.MethodGet, mediaPath, nil)
	req.Header.Set("Range", "bytes=0-15")
	rangeRec := httptest.NewRecorder()
	c.Server.Handler().ServeHTTP(rangeRec, req)
	if rangeRec.Code != http.StatusPartialContent || rangeRec.Body.Len() != 16 {
		t.Errorf("range request = %d, %d bytes", rangeRec.Code, rangeRec.Body.Len())
	}

	// The track is now browsable through the track endpoints.
	rec = c.do(http.MethodGet, "/api/v1/tracks/"+entry.Track.ID+"/variants", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("variants = %d", rec.Code)
	}
	var variants struct {
		Variants []struct {
			Provider string `json:"provider"`
		} `json:"variants"`
	}
	c.decode(rec, &variants)
	if len(variants.Variants) != 1 || variants.Variants[0].Provider != "local" {
		t.Fatalf("variants = %+v", variants)
	}
}

func TestMediaDownloadRejectsUnknownVariant(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	rec := c.do(http.MethodPost, "/api/v1/media/00000000-0000-0000-0000-000000000000/download", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("download = %d, want 404", rec.Code)
	}
	if rec := c.do(http.MethodPost, "/api/v1/media/not-a-uuid/download", "", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id = %d, want 400", rec.Code)
	}
}

// toneFile renders a short sine wave opus file.
func toneFile(t *testing.T, dir, name string) string {
	t.Helper()
	dst := filepath.Join(dir, name)
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1.0",
		"-c:a", "libopus", "-f", "opus", dst)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Skipf("ffmpeg unavailable: %v: %s", err, stderr.String())
	}
	return dst
}
