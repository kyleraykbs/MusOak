package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestPlaybackStateRoundTrip(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	token := c.register("kyle", "hunter2hunter2")

	// Nothing stored yet is an empty document, not a 404: restoring should be
	// the same code path whether or not there is anything to restore.
	rec := c.do(http.MethodGet, "/api/v1/me/playback", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d: %s", rec.Code, rec.Body.String())
	}
	var empty struct {
		State map[string]any `json:"state"`
	}
	c.decode(rec, &empty)
	if len(empty.State) != 0 {
		t.Fatalf("state = %+v, want an empty document", empty.State)
	}

	// Save what they were playing: the queue, the song, the timestamp.
	saved := map[string]any{
		"state": map[string]any{
			"queue":          []map[string]string{{"trackId": "t1", "title": "First"}, {"trackId": "t2", "title": "Second"}},
			"currentTrackId": "t1",
			"positionMs":     12_345,
			"paused":         true,
			"playlistId":     "pl-1",
		},
	}
	rec = c.do(http.MethodPut, "/api/v1/me/playback", token, saved)
	if rec.Code != http.StatusOK {
		t.Fatalf("put = %d: %s", rec.Code, rec.Body.String())
	}

	rec = c.do(http.MethodGet, "/api/v1/me/playback", token, nil)
	var stored struct {
		State map[string]any `json:"state"`
	}
	c.decode(rec, &stored)
	if stored.State["currentTrackId"] != "t1" {
		t.Fatalf("state = %+v", stored.State)
	}
	if stored.State["positionMs"].(float64) != 12_345 {
		t.Errorf("position = %+v", stored.State["positionMs"])
	}
	if stored.State["paused"] != true {
		t.Errorf("paused = %+v", stored.State["paused"])
	}
	queue, ok := stored.State["queue"].([]any)
	if !ok || len(queue) != 2 {
		t.Fatalf("queue = %+v", stored.State["queue"])
	}

	// A later save replaces it rather than adding to it.
	rec = c.do(http.MethodPut, "/api/v1/me/playback", token, map[string]any{
		"state": map[string]any{"positionMs": 100},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("put = %d: %s", rec.Code, rec.Body.String())
	}
	rec = c.do(http.MethodGet, "/api/v1/me/playback", token, nil)
	var replaced struct {
		State map[string]any `json:"state"`
	}
	c.decode(rec, &replaced)
	if len(replaced.State) != 1 {
		t.Fatalf("state = %+v, want only what was last saved", replaced.State)
	}

	// And it is per user: someone else's state is their own.
	other := c.register("other", "hunter2hunter2")
	rec = c.do(http.MethodGet, "/api/v1/me/playback", other, nil)
	var theirs struct {
		State map[string]any `json:"state"`
	}
	c.decode(rec, &theirs)
	if len(theirs.State) != 0 {
		t.Fatalf("state = %+v, want none of it", theirs.State)
	}
}

func TestPlaybackStateRefusesABadDocument(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	token := c.register("kyle", "hunter2hunter2")

	rec := c.doRaw(http.MethodPut, "/api/v1/me/playback", token, "[]")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a list = %d, want 400", rec.Code)
	}
	rec = c.doRaw(http.MethodPut, "/api/v1/me/playback", token, "{not json")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("broken JSON = %d, want 400", rec.Code)
	}
	rec = c.doRaw(http.MethodPut, "/api/v1/me/playback", token,
		`{"state":"`+strings.Repeat("x", maxPlaybackStateBytes)+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("too large = %d, want 400", rec.Code)
	}
}

func TestPlaybackStateNeedsAUser(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	if rec := c.do(http.MethodGet, "/api/v1/me/playback", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("get without a user = %d, want 401", rec.Code)
	}
	if rec := c.do(http.MethodPut, "/api/v1/me/playback", "", map[string]any{"state": map[string]any{}}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("put without a user = %d, want 401", rec.Code)
	}
}
