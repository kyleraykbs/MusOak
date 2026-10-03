package api

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"codeberg.org/kyleraykbs/musoak/internal/config"
)

// A photograph is bigger than the bound the small JSON bodies use. The image
// endpoints carry it anyway, and only refuse what is genuinely too big - and
// say so about the image, not about JSON.
func TestIconUploadCarriesAnImagePastTheSmallBodyLimit(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	token := c.register("bigicon", "hunter2hunter2")

	// 1.5 MB of bytes: past the 1 MiB small-body limit, well inside 8 MB.
	photo := make([]byte, 1536<<10)
	for i := range photo {
		photo[i] = byte(i % 251)
	}
	rec := c.do(http.MethodPost, "/api/v1/me/icon", token,
		map[string]string{"data": base64.StdEncoding.EncodeToString(photo), "contentType": "image/jpeg"})
	if rec.Code != http.StatusOK {
		t.Fatalf("icon = %d: %s", rec.Code, rec.Body.String())
	}

	// Past the image cap: refused as an image, with the status a client can act on.
	oversized := make([]byte, maxArtworkBytes+1)
	rec = c.do(http.MethodPost, "/api/v1/me/icon", token,
		map[string]string{"data": base64.StdEncoding.EncodeToString(oversized), "contentType": "image/jpeg"})
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized icon = %d, want 413: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "the image is too large") {
		t.Errorf("oversized icon = %s, want it named as an image", rec.Body.String())
	}
}

func TestAccountProfileRoundTripsThroughMe(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	token := c.register("kyle", "hunter2hunter2")

	// A one-pixel PNG, which is a real image and small enough to be silly.
	const pixel = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

	// The display name is a save/discard field: no password, just the session.
	rec := c.do(http.MethodPatch, "/api/v1/me", token, map[string]string{"displayName": "Kyle Ray"})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d: %s", rec.Code, rec.Body.String())
	}
	var patched accountUserEnvelope
	c.decode(rec, &patched)
	if patched.User.DisplayName != "Kyle Ray" || patched.User.Username != "kyle" {
		t.Fatalf("user after patch = %+v", patched.User)
	}

	// The icon is the playlist artwork upload by another name, and its URL is
	// a path on this server rather than somebody's CDN.
	rec = c.do(http.MethodPost, "/api/v1/me/icon", token, map[string]string{"data": pixel, "contentType": "image/png"})
	if rec.Code != http.StatusOK {
		t.Fatalf("icon = %d: %s", rec.Code, rec.Body.String())
	}
	var iconed accountUserEnvelope
	c.decode(rec, &iconed)
	if iconed.User.IconURL != "/api/v1/artwork/user/"+iconed.User.ID {
		t.Fatalf("iconUrl = %q, want a path on this server", iconed.User.IconURL)
	}

	// The image is served from the recorded URL, unchanged.
	rec = c.do(http.MethodGet, iconed.User.IconURL, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("icon fetch = %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "image/png" {
		t.Errorf("content type = %q", got)
	}
	if rec.Body.String() == "" {
		t.Error("the stored icon is empty")
	}

	// GET /me carries both fields for the account tab.
	rec = c.do(http.MethodGet, "/api/v1/me", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("me = %d: %s", rec.Code, rec.Body.String())
	}
	var me struct {
		Authenticated bool   `json:"authenticated"`
		Username      string `json:"username"`
		DisplayName   string `json:"displayName"`
		IconURL       string `json:"iconUrl"`
	}
	c.decode(rec, &me)
	if !me.Authenticated || me.Username != "kyle" || me.DisplayName != "Kyle Ray" || me.IconURL != iconed.User.IconURL {
		t.Fatalf("me = %+v", me)
	}

	// Saving the name again must not throw the icon away.
	rec = c.do(http.MethodPatch, "/api/v1/me", token, map[string]string{"displayName": "K. Ray"})
	if rec.Code != http.StatusOK {
		t.Fatalf("second patch = %d: %s", rec.Code, rec.Body.String())
	}
	var renamed accountUserEnvelope
	c.decode(rec, &renamed)
	if renamed.User.DisplayName != "K. Ray" || renamed.User.IconURL != iconed.User.IconURL {
		t.Fatalf("user after second patch = %+v", renamed.User)
	}
}

// An account's search platforms are a preference the client reads back and
// edits: it saves them without a password, an unknown name is refused, and an
// empty list means "no preference".
func TestSearchPlatformsRoundTripThroughMe(t *testing.T) {
	// The default order names providers this test does not enable; ytmusic is
	// the one the preference below is allowed to name.
	c := newHTTPTestServer(t, func(cfg *config.Config) {
		cfg.Providers.YTMusic.Enabled = true
	})
	token := c.register("kyle", "hunter2hunter2")

	type meResponse struct {
		DisplayName     string   `json:"displayName"`
		SearchPlatforms []string `json:"searchPlatforms"`
	}
	getMe := func() meResponse {
		t.Helper()
		rec := c.do(http.MethodGet, "/api/v1/me", token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("me = %d: %s", rec.Code, rec.Body.String())
		}
		var me meResponse
		c.decode(rec, &me)
		return me
	}

	// Never set reads back as an empty list, not null: the client's own default
	// then applies.
	me := getMe()
	if me.SearchPlatforms == nil || len(me.SearchPlatforms) != 0 {
		t.Fatalf("unset searchPlatforms = %#v, want an empty list", me.SearchPlatforms)
	}

	// The display name and the platforms are independent keys.
	rec := c.do(http.MethodPatch, "/api/v1/me", token, map[string]string{"displayName": "Kyle Ray"})
	if rec.Code != http.StatusOK {
		t.Fatalf("name patch = %d: %s", rec.Code, rec.Body.String())
	}
	rec = c.do(http.MethodPatch, "/api/v1/me", token, map[string]any{"searchPlatforms": []string{"ytmusic"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("platforms patch = %d: %s", rec.Code, rec.Body.String())
	}
	var patched accountUserEnvelope
	c.decode(rec, &patched)
	if len(patched.User.SearchPlatforms) != 1 || patched.User.SearchPlatforms[0] != "ytmusic" {
		t.Fatalf("patched searchPlatforms = %#v", patched.User.SearchPlatforms)
	}
	if me = getMe(); me.DisplayName != "Kyle Ray" {
		t.Fatalf("displayName after the platforms patch = %q, want it untouched", me.DisplayName)
	}
	if len(me.SearchPlatforms) != 1 || me.SearchPlatforms[0] != "ytmusic" {
		t.Fatalf("searchPlatforms = %#v, want [ytmusic]", me.SearchPlatforms)
	}

	// A name that is not an enabled provider is refused and changes nothing.
	rec = c.do(http.MethodPatch, "/api/v1/me", token, map[string]any{"searchPlatforms": []string{"ytmusic", "deezer"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown provider = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "deezer") {
		t.Errorf("the refusal must name the provider: %s", rec.Body.String())
	}
	if me = getMe(); len(me.SearchPlatforms) != 1 || me.SearchPlatforms[0] != "ytmusic" {
		t.Fatalf("searchPlatforms after a refused patch = %#v", me.SearchPlatforms)
	}

	// An empty list clears the preference back to "not set".
	rec = c.do(http.MethodPatch, "/api/v1/me", token, map[string]any{"searchPlatforms": []string{}})
	if rec.Code != http.StatusOK {
		t.Fatalf("clear = %d: %s", rec.Code, rec.Body.String())
	}
	if me = getMe(); me.SearchPlatforms == nil || len(me.SearchPlatforms) != 0 {
		t.Fatalf("cleared searchPlatforms = %#v, want an empty list", me.SearchPlatforms)
	}
}

func TestMePasswordChangeKeepsCallerAndRevokesOtherSessions(t *testing.T) {
	// Signing in repeatedly is the point here, not the login rate limit.
	c := newHTTPTestServer(t, func(cfg *config.Config) { cfg.RateLimit.LoginPerMinute = 0 })
	token := c.register("kyle", "hunter2hunter2")

	// A second device signs in; only the caller's session may survive.
	rec := c.do(http.MethodPost, "/api/v1/auth/login", "", map[string]string{"username": "kyle", "password": "hunter2hunter2"})
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d: %s", rec.Code, rec.Body.String())
	}
	var second struct {
		Token string `json:"token"`
	}
	c.decode(rec, &second)

	// A wrong current password is rejected and changes nothing.
	rec = c.do(http.MethodPost, "/api/v1/me/password", token, map[string]string{
		"currentPassword": "not it", "newPassword": "correct horse battery",
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong current password = %d, want 401 (%s)", rec.Code, rec.Body.String())
	}
	if rec := c.do(http.MethodPost, "/api/v1/auth/login", "", map[string]string{"username": "kyle", "password": "hunter2hunter2"}); rec.Code != http.StatusOK {
		t.Fatalf("the old password stopped working after a rejected change: %d", rec.Code)
	}

	// The change goes through and the caller's token keeps working.
	rec = c.do(http.MethodPost, "/api/v1/me/password", token, map[string]string{
		"currentPassword": "hunter2hunter2", "newPassword": "correct horse battery",
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("password change = %d: %s", rec.Code, rec.Body.String())
	}
	rec = c.do(http.MethodGet, "/api/v1/me", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("me after change = %d: %s", rec.Code, rec.Body.String())
	}
	var mine struct {
		Authenticated bool `json:"authenticated"`
	}
	c.decode(rec, &mine)
	if !mine.Authenticated {
		t.Fatal("the caller's token stopped working after the change")
	}

	// The other session is gone, and the old password with it.
	rec = c.do(http.MethodGet, "/api/v1/me", second.Token, nil)
	var theirs struct {
		Authenticated bool `json:"authenticated"`
	}
	c.decode(rec, &theirs)
	if theirs.Authenticated {
		t.Fatal("a second session survived the password change")
	}
	if rec := c.do(http.MethodPost, "/api/v1/auth/login", "", map[string]string{"username": "kyle", "password": "hunter2hunter2"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("old password = %d, want 401", rec.Code)
	}
	if rec := c.do(http.MethodPost, "/api/v1/auth/login", "", map[string]string{"username": "kyle", "password": "correct horse battery"}); rec.Code != http.StatusOK {
		t.Fatalf("new password = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

func TestMeUsernameChangeNeedsPasswordAndAUniqueName(t *testing.T) {
	// Registering and signing in here is incidental, not the rate limit's business.
	c := newHTTPTestServer(t, func(cfg *config.Config) { cfg.RateLimit.LoginPerMinute = 0 })
	token := c.register("kyle", "hunter2hunter2")
	c.register("rival", "hunter2hunter2")

	// The password is the confirm dialog: without it nothing moves.
	rec := c.do(http.MethodPost, "/api/v1/me/username", token, map[string]string{
		"password": "not it", "username": "kyle.renamed",
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d, want 401 (%s)", rec.Code, rec.Body.String())
	}

	// A name somebody else already holds is a conflict.
	rec = c.do(http.MethodPost, "/api/v1/me/username", token, map[string]string{
		"password": "hunter2hunter2", "username": "rival",
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("taken username = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}

	// The rename goes through and shows up in /me and at login.
	rec = c.do(http.MethodPost, "/api/v1/me/username", token, map[string]string{
		"password": "hunter2hunter2", "username": "kyle.renamed",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("rename = %d: %s", rec.Code, rec.Body.String())
	}
	var renamed accountUserEnvelope
	c.decode(rec, &renamed)
	if renamed.User.Username != "kyle.renamed" {
		t.Fatalf("username = %q, want %q", renamed.User.Username, "kyle.renamed")
	}
	rec = c.do(http.MethodGet, "/api/v1/me", token, nil)
	var me struct {
		Username string `json:"username"`
	}
	c.decode(rec, &me)
	if me.Username != "kyle.renamed" {
		t.Fatalf("me.username = %q, want %q", me.Username, "kyle.renamed")
	}
	if rec := c.do(http.MethodPost, "/api/v1/auth/login", "", map[string]string{"username": "kyle.renamed", "password": "hunter2hunter2"}); rec.Code != http.StatusOK {
		t.Fatalf("login with the new name = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

func TestAccountWritesNeedLogin(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	for _, call := range []struct {
		method, path string
		body         map[string]string
	}{
		{http.MethodPatch, "/api/v1/me", map[string]string{"displayName": "Ghost"}},
		{http.MethodPost, "/api/v1/me/icon", map[string]string{"data": "aGVsbG8=", "contentType": "image/png"}},
		{http.MethodPost, "/api/v1/me/password", map[string]string{"currentPassword": "x", "newPassword": "y"}},
		{http.MethodPost, "/api/v1/me/username", map[string]string{"password": "x", "username": "ghost"}},
	} {
		if rec := c.do(call.method, call.path, "", call.body); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s as guest = %d, want 401: %s", call.method, call.path, rec.Code, rec.Body.String())
		}
	}
}
