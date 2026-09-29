package api

import (
	"context"
	"net/http"
	"testing"

	"codeberg.org/kyleraykbs/prismusic/internal/store"
)

func seedTracks(t *testing.T, c *testClient, titles ...string) []*store.Track {
	t.Helper()
	tracks := make([]*store.Track, 0, len(titles))
	for _, title := range titles {
		track := &store.Track{Title: title, DurationMs: 180_000}
		if err := c.store.CreateTrack(context.Background(), track); err != nil {
			t.Fatalf("CreateTrack: %v", err)
		}
		tracks = append(tracks, track)
	}
	return tracks
}

func TestPlaylistFlow(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	token := c.register("kyle", "hunter2hunter2")
	tracks := seedTracks(t, c, "One", "Two", "Three")

	// Create.
	rec := c.do(http.MethodPost, "/api/v1/me/playlists", token, map[string]string{"name": "driving"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	var playlist playlistResponse
	c.decode(rec, &playlist)
	if playlist.ID == "" || playlist.Name != "driving" || playlist.TrackCount != 0 {
		t.Fatalf("playlist = %+v", playlist)
	}

	// Add two tracks at once.
	rec = c.do(http.MethodPost, "/api/v1/me/playlists/"+playlist.ID+"/items", token, map[string]any{
		"trackIds": []string{tracks[0].ID.String(), tracks[1].ID.String()},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("add = %d: %s", rec.Code, rec.Body.String())
	}
	var detail playlistDetailResponse
	c.decode(rec, &detail)
	if len(detail.Items) != 2 || detail.Items[0].Track.Title != "One" || detail.Items[1].Track.Title != "Two" {
		t.Fatalf("items = %+v", detail.Items)
	}
	if detail.TrackCount != 2 {
		t.Errorf("trackCount = %d, want 2", detail.TrackCount)
	}

	// One more, by single id.
	rec = c.do(http.MethodPost, "/api/v1/me/playlists/"+playlist.ID+"/items", token, map[string]string{
		"trackId": tracks[2].ID.String(),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("add one = %d", rec.Code)
	}
	c.decode(rec, &detail)
	if len(detail.Items) != 3 {
		t.Fatalf("items = %d, want 3", len(detail.Items))
	}

	// Reorder: reverse.
	rec = c.do(http.MethodPost, "/api/v1/me/playlists/"+playlist.ID+"/reorder", token, map[string][]int{"order": {2, 1, 0}})
	if rec.Code != http.StatusOK {
		t.Fatalf("reorder = %d: %s", rec.Code, rec.Body.String())
	}
	c.decode(rec, &detail)
	if detail.Items[0].Track.Title != "Three" || detail.Items[2].Track.Title != "One" {
		t.Fatalf("reorder = %+v", detail.Items)
	}

	// A bad order is rejected without touching the playlist.
	rec = c.do(http.MethodPost, "/api/v1/me/playlists/"+playlist.ID+"/reorder", token, map[string][]int{"order": {0, 0, 1}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad reorder = %d, want 400", rec.Code)
	}

	// Remove the middle entry.
	rec = c.do(http.MethodDelete, "/api/v1/me/playlists/"+playlist.ID+"/items/1", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("remove = %d: %s", rec.Code, rec.Body.String())
	}
	c.decode(rec, &detail)
	if len(detail.Items) != 2 || detail.Items[0].Position != 0 || detail.Items[1].Position != 1 {
		t.Fatalf("items after removal = %+v", detail.Items)
	}

	// Rename, then read it back from the listing.
	rec = c.do(http.MethodPatch, "/api/v1/me/playlists/"+playlist.ID, token, map[string]string{"name": "night drive"})
	if rec.Code != http.StatusOK {
		t.Fatalf("rename = %d", rec.Code)
	}
	rec = c.do(http.MethodGet, "/api/v1/me/playlists", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	var listing struct {
		Playlists []playlistResponse `json:"playlists"`
	}
	c.decode(rec, &listing)
	if len(listing.Playlists) != 1 || listing.Playlists[0].Name != "night drive" {
		t.Fatalf("listing = %+v", listing.Playlists)
	}
	if listing.Playlists[0].TrackCount != 2 {
		t.Errorf("trackCount = %d, want 2", listing.Playlists[0].TrackCount)
	}
	if listing.Playlists[0].DurationMs != 360_000 {
		t.Errorf("durationMs = %d, want 360000", listing.Playlists[0].DurationMs)
	}

	// Delete.
	if rec := c.do(http.MethodDelete, "/api/v1/me/playlists/"+playlist.ID, token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	if rec := c.do(http.MethodGet, "/api/v1/me/playlists/"+playlist.ID, token, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete = %d, want 404", rec.Code)
	}
}

func TestPlaylistOwnershipAndGuests(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	owner := c.register("owner", "hunter2hunter2")
	other := c.register("other", "hunter2hunter2")
	tracks := seedTracks(t, c, "One")

	rec := c.do(http.MethodPost, "/api/v1/me/playlists", owner, map[string]string{"name": "mine"})
	var playlist playlistResponse
	c.decode(rec, &playlist)

	// Somebody else's playlist is simply not there.
	for _, request := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/me/playlists/" + playlist.ID},
		{http.MethodPatch, "/api/v1/me/playlists/" + playlist.ID},
		{http.MethodDelete, "/api/v1/me/playlists/" + playlist.ID},
		{http.MethodPost, "/api/v1/me/playlists/" + playlist.ID + "/items"},
	} {
		rec := c.do(request.method, request.path, other, map[string]string{"name": "stolen", "trackId": tracks[0].ID.String()})
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s as another user = %d, want 404", request.method, request.path, rec.Code)
		}
	}

	// Guests have no playlists at all.
	for _, request := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/me/playlists"},
		{http.MethodPost, "/api/v1/me/playlists"},
		{http.MethodGet, "/api/v1/me/playlists/" + playlist.ID},
	} {
		if rec := c.do(request.method, request.path, "", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s as a guest = %d, want 401", request.method, request.path, rec.Code)
		}
	}

	// Input validation.
	if rec := c.do(http.MethodPost, "/api/v1/me/playlists", owner, map[string]string{"name": "   "}); rec.Code != http.StatusBadRequest {
		t.Errorf("blank name = %d, want 400", rec.Code)
	}
	if rec := c.do(http.MethodPost, "/api/v1/me/playlists/"+playlist.ID+"/items", owner, map[string]any{}); rec.Code != http.StatusBadRequest {
		t.Errorf("empty add = %d, want 400", rec.Code)
	}
	if rec := c.do(http.MethodPost, "/api/v1/me/playlists/"+playlist.ID+"/items", owner, map[string]string{"trackId": "not-a-uuid"}); rec.Code != http.StatusBadRequest {
		t.Errorf("bad track id = %d, want 400", rec.Code)
	}
	if rec := c.do(http.MethodDelete, "/api/v1/me/playlists/"+playlist.ID+"/items/99", owner, nil); rec.Code != http.StatusNotFound {
		t.Errorf("removing a missing position = %d, want 404", rec.Code)
	}
	if rec := c.do(http.MethodGet, "/api/v1/me/playlists/not-a-uuid", owner, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad playlist id = %d, want 400", rec.Code)
	}
}
