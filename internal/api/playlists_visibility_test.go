package api

import (
	"net/http"
	"testing"
)

// A playlist is public until its owner says otherwise: another account sees a
// public one on the owner's page and can open it, and neither is true once it
// is private - while the owner keeps theirs either way.
func TestPlaylistVisibility(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	owner := c.register("owner", "hunter2hunter2")
	other := c.register("other", "hunter2hunter2")

	ownerID := c.userID(t, owner)

	// New playlists are public.
	rec := c.do(http.MethodPost, "/api/v1/me/playlists", owner, map[string]string{"name": "Shared"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	var created playlistResponse
	c.decode(rec, &created)
	if !created.Public {
		t.Fatalf("a new playlist is not public: %+v", created)
	}
	shared := "/api/v1/playlists/" + created.ID

	// The other account sees it on the owner's page, and can open it.
	if names := c.profilePlaylistNames(t, ownerID, other); !contains(names, "Shared") {
		t.Errorf("another account's page = %v, want it to list Shared", names)
	}
	if rec := c.do(http.MethodGet, shared, other, nil); rec.Code != http.StatusOK {
		t.Errorf("another account opening it = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// Private takes it away from everybody else.
	rec = c.do(http.MethodPatch, "/api/v1/me/playlists/"+created.ID, owner, map[string]any{"public": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("making it private = %d: %s", rec.Code, rec.Body.String())
	}
	var patched playlistResponse
	c.decode(rec, &patched)
	if patched.Public {
		t.Errorf("the playlist is still public: %+v", patched)
	}
	if names := c.profilePlaylistNames(t, ownerID, other); contains(names, "Shared") {
		t.Errorf("a private playlist is listed to another account: %v", names)
	}
	if rec := c.do(http.MethodGet, shared, other, nil); rec.Code != http.StatusNotFound {
		t.Errorf("another account opening a private playlist = %d, want 404", rec.Code)
	}

	// The owner keeps it, and their own page still shows it.
	if rec := c.do(http.MethodGet, shared, owner, nil); rec.Code != http.StatusOK {
		t.Errorf("the owner opening their own playlist = %d, want 200", rec.Code)
	}
	if names := c.profilePlaylistNames(t, ownerID, owner); !contains(names, "Shared") {
		t.Errorf("the owner's own page = %v, want it to list their private playlist", names)
	}

	// Renaming alone leaves the visibility where it was.
	rec = c.do(http.MethodPatch, "/api/v1/me/playlists/"+created.ID, owner, map[string]string{"name": "Renamed"})
	if rec.Code != http.StatusOK {
		t.Fatalf("rename = %d: %s", rec.Code, rec.Body.String())
	}
	var renamed playlistResponse
	c.decode(rec, &renamed)
	if renamed.Name != "Renamed" || renamed.Public {
		t.Errorf("rename changed more than the name: %+v", renamed)
	}
}

// A patch with neither a name nor a visibility is not a change.
func TestPlaylistPatchNeedsSomethingToChange(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	token := c.register("owner", "hunter2hunter2")
	rec := c.do(http.MethodPost, "/api/v1/me/playlists", token, map[string]string{"name": "Mine"})
	var created playlistResponse
	c.decode(rec, &created)

	if rec := c.do(http.MethodPatch, "/api/v1/me/playlists/"+created.ID, token, map[string]string{}); rec.Code != http.StatusBadRequest {
		t.Errorf("empty patch = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// userID is the account's id, which the profile routes are addressed by.
func (c *testClient) userID(t *testing.T, token string) string {
	t.Helper()
	rec := c.do(http.MethodGet, "/api/v1/me", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("me = %d: %s", rec.Code, rec.Body.String())
	}
	var envelope accountUserEnvelope
	c.decode(rec, &envelope)
	return envelope.User.ID
}

// profilePlaylistNames reads the playlists a user's page lists.
func (c *testClient) profilePlaylistNames(t *testing.T, userID, token string) []string {
	t.Helper()
	rec := c.do(http.MethodGet, "/api/v1/users/"+userID, token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("profile = %d: %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Playlists []playlistResponse `json:"publicPlaylists"`
	}
	c.decode(rec, &payload)
	names := make([]string, 0, len(payload.Playlists))
	for _, playlist := range payload.Playlists {
		names = append(names, playlist.Name)
	}
	return names
}

func contains(names []string, wanted string) bool {
	for _, name := range names {
		if name == wanted {
			return true
		}
	}
	return false
}
