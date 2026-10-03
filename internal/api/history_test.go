package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/musoak/internal/store"
)

// call invokes a handler directly, with the caller already authenticated. The
// listening routes are wired elsewhere, so the tests reach the handlers the way
// the mux will.
func (c *testClient) call(h http.HandlerFunc, method, target string, user *store.User, body any) *httptest.ResponseRecorder {
	c.t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, target, reader)
	if user != nil {
		req = req.WithContext(context.WithValue(req.Context(), userKey, user))
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func (c *testClient) userNamed(username string) *store.User {
	c.t.Helper()
	user, err := c.store.UserByUsername(context.Background(), username)
	if err != nil {
		c.t.Fatalf("UserByUsername(%q): %v", username, err)
	}
	return user
}

type historyPage struct {
	Plays []struct {
		Track struct {
			ID    string `json:"id"`
			Title string `json:"title"`
			Plays int    `json:"plays"`
		} `json:"track"`
		VariantID string `json:"variantId"`
		PlayedMs  int64  `json:"playedMs"`
		AtMs      int64  `json:"atMs"`
	} `json:"plays"`
}

type topPage struct {
	Tracks []struct {
		Track struct {
			ID string `json:"id"`
		} `json:"track"`
		Plays          int   `json:"plays"`
		LastPlayedAtMs int64 `json:"lastPlayedAtMs"`
	} `json:"tracks"`
}

func TestPlayHistoryFlow(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	token := c.register("kyle", "hunter2hunter2")
	kyle := c.userNamed("kyle")

	first := &store.Track{Title: "First", DurationMs: 180_000}
	second := &store.Track{Title: "Second", DurationMs: 200_000}
	for _, track := range []*store.Track{first, second} {
		if err := c.store.CreateTrack(context.Background(), track); err != nil {
			t.Fatal(err)
		}
	}

	variant := uuid.New()
	// The first listen started a minute ago, the second half a minute ago, so
	// the second is the newest and history must list it first.
	rec := c.call(c.handleRecordPlay, http.MethodPost, "/api/v1/me/plays", kyle, map[string]any{
		"trackId": first.ID.String(), "variantId": variant.String(), "playedMs": 60_000, "source": "web",
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("record first play = %d: %s", rec.Code, rec.Body.String())
	}
	rec = c.call(c.handleRecordPlay, http.MethodPost, "/api/v1/me/plays", kyle, map[string]any{
		"trackId": second.ID.String(), "playedMs": 30_000, "source": "web",
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("record second play = %d: %s", rec.Code, rec.Body.String())
	}

	rec = c.call(c.handleHistory, http.MethodGet, "/api/v1/me/history", kyle, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("history = %d: %s", rec.Code, rec.Body.String())
	}
	var page historyPage
	c.decode(rec, &page)
	if len(page.Plays) != 2 {
		t.Fatalf("history = %+v, want two plays", page.Plays)
	}
	if page.Plays[0].Track.ID != second.ID.String() || page.Plays[1].Track.ID != first.ID.String() {
		t.Fatalf("history order = %s, %s, want newest first", page.Plays[0].Track.ID, page.Plays[1].Track.ID)
	}
	if page.Plays[1].VariantID != variant.String() {
		t.Errorf("variantId = %q, want %q", page.Plays[1].VariantID, variant)
	}
	if page.Plays[0].PlayedMs != 30_000 || page.Plays[1].PlayedMs != 60_000 {
		t.Errorf("played lengths = %d, %d", page.Plays[0].PlayedMs, page.Plays[1].PlayedMs)
	}
	if page.Plays[0].AtMs == 0 || page.Plays[0].AtMs <= page.Plays[1].AtMs {
		t.Errorf("play times = %d, %d, want the newer first", page.Plays[0].AtMs, page.Plays[1].AtMs)
	}

	// A track skipped straight away was not listened to.
	if rec := c.call(c.handleRecordPlay, http.MethodPost, "/api/v1/me/plays", kyle, map[string]any{
		"trackId": first.ID.String(), "playedMs": 4_000,
	}); rec.Code != http.StatusNoContent {
		t.Fatalf("skipped play = %d", rec.Code)
	}
	rec = c.call(c.handleHistory, http.MethodGet, "/api/v1/me/history", kyle, nil)
	c.decode(rec, &page)
	if len(page.Plays) != 2 {
		t.Fatalf("history after a skip = %d plays, want 2", len(page.Plays))
	}

	// The limit bounds the listing.
	rec = c.call(c.handleHistory, http.MethodGet, "/api/v1/me/history?limit=1", kyle, nil)
	c.decode(rec, &page)
	if len(page.Plays) != 1 || page.Plays[0].Track.ID != second.ID.String() {
		t.Fatalf("limited history = %+v, want only the newest", page.Plays)
	}

	// One more full play of the first track makes it the most played.
	if rec := c.call(c.handleRecordPlay, http.MethodPost, "/api/v1/me/plays", kyle, map[string]any{
		"trackId": first.ID.String(), "playedMs": 90_000,
	}); rec.Code != http.StatusNoContent {
		t.Fatalf("record another play = %d", rec.Code)
	}
	rec = c.call(c.handleTopTracks, http.MethodGet, "/api/v1/me/stats/top", kyle, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("top tracks = %d: %s", rec.Code, rec.Body.String())
	}
	var top topPage
	c.decode(rec, &top)
	if len(top.Tracks) != 2 {
		t.Fatalf("top tracks = %+v, want two tracks", top.Tracks)
	}
	if top.Tracks[0].Track.ID != first.ID.String() || top.Tracks[0].Plays != 2 {
		t.Fatalf("top first = %+v, want First with two plays", top.Tracks[0])
	}
	if top.Tracks[1].Track.ID != second.ID.String() || top.Tracks[1].Plays != 1 {
		t.Errorf("top second = %+v, want Second with one play", top.Tracks[1])
	}
	if top.Tracks[0].LastPlayedAtMs == 0 {
		t.Errorf("top last played = 0, want a time")
	}
	rec = c.call(c.handleTopTracks, http.MethodGet, "/api/v1/me/stats/top?limit=1", kyle, nil)
	c.decode(rec, &top)
	if len(top.Tracks) != 1 || top.Tracks[0].Track.ID != first.ID.String() {
		t.Fatalf("limited top tracks = %+v, want only the most played", top.Tracks)
	}

	// A guest's report is accepted and ignored: the web reports plays whether
	// or not somebody is signed in.
	if rec := c.call(c.handleRecordPlay, http.MethodPost, "/api/v1/me/plays", nil, map[string]any{
		"trackId": second.ID.String(), "playedMs": 60_000,
	}); rec.Code != http.StatusNoContent {
		t.Fatalf("guest play = %d, want 204", rec.Code)
	}

	// Another account sees none of these plays.
	c.register("sam", "hunter2hunter2")
	sam := c.userNamed("sam")
	rec = c.call(c.handleHistory, http.MethodGet, "/api/v1/me/history", sam, nil)
	c.decode(rec, &page)
	if len(page.Plays) != 0 {
		t.Fatalf("sam's history = %+v, want none", page.Plays)
	}
	rec = c.call(c.handleTopTracks, http.MethodGet, "/api/v1/me/stats/top", sam, nil)
	c.decode(rec, &top)
	if len(top.Tracks) != 0 {
		t.Fatalf("sam's top tracks = %+v, want none", top.Tracks)
	}

	// Neither listing is for a guest.
	if rec := c.call(c.handleHistory, http.MethodGet, "/api/v1/me/history", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("guest history = %d, want 401", rec.Code)
	}
	if rec := c.call(c.handleTopTracks, http.MethodGet, "/api/v1/me/stats/top", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("guest top tracks = %d, want 401", rec.Code)
	}

	// The track payload carries the caller's count, and a guest's is zero.
	rec = c.do(http.MethodGet, "/api/v1/tracks/"+first.ID.String(), token, nil)
	var signedIn trackResponse
	c.decode(rec, &signedIn)
	if signedIn.Plays != 2 {
		t.Errorf("signed-in plays = %d, want 2", signedIn.Plays)
	}
	rec = c.do(http.MethodGet, "/api/v1/tracks/"+first.ID.String(), "", nil)
	var guest trackResponse
	c.decode(rec, &guest)
	if guest.Plays != 0 {
		t.Errorf("guest plays = %d, want 0", guest.Plays)
	}

	// A list endpoint counts the whole list at once and carries the same field.
	if rec := c.do(http.MethodPost, "/api/v1/me/favorites", token, map[string]string{"trackId": first.ID.String()}); rec.Code != http.StatusNoContent {
		t.Fatalf("favorite = %d: %s", rec.Code, rec.Body.String())
	}
	rec = c.do(http.MethodGet, "/api/v1/me/favorites", token, nil)
	var favorites struct {
		Tracks []trackResponse `json:"tracks"`
	}
	c.decode(rec, &favorites)
	if len(favorites.Tracks) != 1 || favorites.Tracks[0].Plays != 2 {
		t.Fatalf("favorites = %+v, want First with two plays", favorites.Tracks)
	}
}
