package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"codeberg.org/kyleraykbs/musoak/internal/store"
	"fmt"
	"github.com/google/uuid"
)

// friendsUserID looks up a registered account's id by username.
func friendsUserID(t *testing.T, c *testClient, username string) uuid.UUID {
	t.Helper()
	user, err := c.store.UserByUsername(context.Background(), username)
	if err != nil {
		t.Fatalf("UserByUsername(%s): %v", username, err)
	}
	return user.ID
}

func TestFriendsRoundTrip(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	kyle := c.register("kyle", "hunter2hunter2")
	sam := c.register("sam", "hunter2hunter2")
	kyleID := friendsUserID(t, c, "kyle")
	samID := friendsUserID(t, c, "sam")

	// kyle asks sam for friendship.
	rec := c.do(http.MethodPost, "/api/v1/me/friends", kyle, map[string]string{"username": "sam"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("request = %d: %s", rec.Code, rec.Body.String())
	}
	var added struct {
		User publicUserResponse `json:"user"`
	}
	c.decode(rec, &added)
	if added.User.ID != samID.String() || added.User.Relationship != "pending-out" {
		t.Fatalf("requested user = %+v", added.User)
	}

	// The request shows up as outgoing on one side, incoming on the other.
	rec = c.do(http.MethodGet, "/api/v1/me/friends", kyle, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	var lists struct {
		Friends  []publicUserResponse `json:"friends"`
		Incoming []publicUserResponse `json:"incoming"`
		Outgoing []publicUserResponse `json:"outgoing"`
	}
	c.decode(rec, &lists)
	if len(lists.Friends) != 0 || len(lists.Incoming) != 0 {
		t.Fatalf("kyle before acceptance = %+v", lists)
	}
	if len(lists.Outgoing) != 1 || lists.Outgoing[0].ID != samID.String() || lists.Outgoing[0].Relationship != "pending-out" {
		t.Fatalf("kyle's outgoing = %+v", lists.Outgoing)
	}

	rec = c.do(http.MethodGet, "/api/v1/me/friends", sam, nil)
	c.decode(rec, &lists)
	if len(lists.Incoming) != 1 || lists.Incoming[0].ID != kyleID.String() || lists.Incoming[0].Relationship != "pending-in" {
		t.Fatalf("sam's incoming = %+v", lists.Incoming)
	}

	// sam accepts: both are now on each other's friends list.
	if rec := c.do(http.MethodPost, "/api/v1/me/friends/"+kyleID.String()+"/accept", sam, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("accept = %d: %s", rec.Code, rec.Body.String())
	}
	for name, token := range map[string]string{"kyle": kyle, "sam": sam} {
		rec := c.do(http.MethodGet, "/api/v1/me/friends", token, nil)
		c.decode(rec, &lists)
		if len(lists.Friends) != 1 || lists.Friends[0].Relationship != "friend" {
			t.Fatalf("%s's friends = %+v", name, lists.Friends)
		}
	}

	// kyle unfriends; they are strangers again.
	if rec := c.do(http.MethodDelete, "/api/v1/me/friends/"+samID.String(), kyle, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("unfriend = %d: %s", rec.Code, rec.Body.String())
	}
	rec = c.do(http.MethodGet, "/api/v1/me/friends", kyle, nil)
	c.decode(rec, &lists)
	if len(lists.Friends) != 0 {
		t.Fatalf("friends after unfriending = %+v", lists.Friends)
	}
	rec = c.do(http.MethodGet, "/api/v1/users/"+samID.String(), kyle, nil)
	var page struct {
		User publicUserResponse `json:"user"`
	}
	c.decode(rec, &page)
	if page.User.Relationship != "none" {
		t.Errorf("relationship after unfriending = %q, want none", page.User.Relationship)
	}
	// There is nothing left to remove.
	if rec := c.do(http.MethodDelete, "/api/v1/me/friends/"+samID.String(), kyle, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("second unfriend = %d, want 404", rec.Code)
	}

	// Declining is the same call seen from the other side.
	if rec := c.do(http.MethodPost, "/api/v1/me/friends", kyle, map[string]string{"username": "sam"}); rec.Code != http.StatusCreated {
		t.Fatalf("re-request = %d", rec.Code)
	}
	if rec := c.do(http.MethodDelete, "/api/v1/me/friends/"+kyleID.String(), sam, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("decline = %d: %s", rec.Code, rec.Body.String())
	}
	rec = c.do(http.MethodGet, "/api/v1/users/"+kyleID.String(), sam, nil)
	c.decode(rec, &page)
	if page.User.Relationship != "none" {
		t.Errorf("relationship after decline = %q, want none", page.User.Relationship)
	}

	// Guests have no friends to list.
	if rec := c.do(http.MethodGet, "/api/v1/me/friends", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("guest list = %d, want 401", rec.Code)
	}
}

func TestUserSearchFindsDisplayName(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	kyle := c.register("kyle", "hunter2hunter2")
	c.register("ada", "hunter2hunter2")
	adaID := friendsUserID(t, c, "ada")
	if err := c.store.UpdateProfile(context.Background(), adaID, "Ada Lovelace", ""); err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}

	var found struct {
		Users []publicUserResponse `json:"users"`
	}
	// Display names match, case-insensitively.
	rec := c.do(http.MethodGet, "/api/v1/users?q=LOVELACE", kyle, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("search = %d: %s", rec.Code, rec.Body.String())
	}
	c.decode(rec, &found)
	if len(found.Users) != 1 || found.Users[0].ID != adaID.String() || found.Users[0].DisplayName != "Ada Lovelace" {
		t.Fatalf("by display name = %+v", found.Users)
	}
	// So do usernames.
	rec = c.do(http.MethodGet, "/api/v1/users?q=ADA", kyle, nil)
	c.decode(rec, &found)
	if len(found.Users) != 1 || found.Users[0].ID != adaID.String() {
		t.Fatalf("by username = %+v", found.Users)
	}

	// A stranger is nobody in particular: not online, not listening, no
	// relationship. (A decoded json.RawMessage may spell null as "null"
	// rather than empty, so both forms count as nothing.)
	ada := found.Users[0]
	if ada.Relationship != "none" || ada.Online || ada.LastPlayedAt != nil {
		t.Errorf("stranger = %+v", ada)
	}
	if listening := string(ada.Listening); listening != "" && listening != "null" {
		t.Errorf("listening = %s, want null", listening)
	}

	// Nobody answers to a name nobody has.
	rec = c.do(http.MethodGet, "/api/v1/users?q=lovelace%20byron", kyle, nil)
	c.decode(rec, &found)
	if len(found.Users) != 0 {
		t.Fatalf("missing name = %+v", found.Users)
	}

	// Guests cannot search people, nor open a profile.
	if rec := c.do(http.MethodGet, "/api/v1/users?q=ada", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("guest search = %d, want 401", rec.Code)
	}
	if rec := c.do(http.MethodGet, "/api/v1/users/"+adaID.String(), "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("guest profile = %d, want 401", rec.Code)
	}
}

func TestShareCreatesExactlyOneNotification(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	kyle := c.register("kyle", "hunter2hunter2")
	sam := c.register("sam", "hunter2hunter2")
	kyleID := friendsUserID(t, c, "kyle")
	samID := friendsUserID(t, c, "sam")
	tracks := seedTracks(t, c, "Song")
	track := tracks[0]

	// kyle shares a song with sam.
	rec := c.do(http.MethodPost, "/api/v1/me/shares", kyle, map[string]string{
		"userId": samID.String(), "trackId": track.ID.String(),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("share = %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Share shareResponse `json:"share"`
	}
	c.decode(rec, &created)
	if created.Share.From.Username != "kyle" || created.Share.To.Username != "sam" {
		t.Fatalf("share ends = %+v", created.Share)
	}
	if created.Share.TrackID == nil || *created.Share.TrackID != track.ID.String() {
		t.Errorf("share trackId = %+v", created.Share.TrackID)
	}
	if created.Share.RoomID != nil || created.Share.Track == nil || created.Share.Track.Title != "Song" {
		t.Errorf("share = %+v", created.Share)
	}

	// One share is one notification.
	rec = c.do(http.MethodGet, "/api/v1/me/notifications", sam, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("notifications = %d: %s", rec.Code, rec.Body.String())
	}
	var bell struct {
		Notifications []notificationResponse `json:"notifications"`
		Unread        int                    `json:"unread"`
	}
	c.decode(rec, &bell)
	if len(bell.Notifications) != 1 || bell.Unread != 1 {
		t.Fatalf("bell = %+v, unread %d", bell.Notifications, bell.Unread)
	}
	entry := bell.Notifications[0]
	if entry.Kind != "share" || entry.Read || entry.From == nil || entry.From.Username != "kyle" {
		t.Errorf("notification = %+v", entry)
	}
	if entry.TrackID == nil || *entry.TrackID != track.ID.String() {
		t.Errorf("notification trackId = %+v", entry.TrackID)
	}

	// Both sides see the share; the recipient sees it as theirs too.
	for name, token := range map[string]string{"kyle": kyle, "sam": sam} {
		rec := c.do(http.MethodGet, "/api/v1/me/shares", token, nil)
		var shares struct {
			Shares []shareResponse `json:"shares"`
		}
		c.decode(rec, &shares)
		if len(shares.Shares) != 1 {
			t.Fatalf("%s's shares = %+v", name, shares.Shares)
		}
	}

	// A room invite is its own single entry.
	rec = c.do(http.MethodPost, "/api/v1/me/shares", kyle, map[string]string{
		"userId": samID.String(), "roomId": "room-1",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("invite = %d: %s", rec.Code, rec.Body.String())
	}
	c.decode(rec, &created)
	if created.Share.RoomID == nil || *created.Share.RoomID != "room-1" || created.Share.TrackID != nil {
		t.Errorf("invite = %+v", created.Share)
	}

	rec = c.do(http.MethodGet, "/api/v1/me/notifications", sam, nil)
	c.decode(rec, &bell)
	if len(bell.Notifications) != 2 || bell.Unread != 2 {
		t.Fatalf("bell after invite = %d entries, %d unread", len(bell.Notifications), bell.Unread)
	}
	if bell.Notifications[0].Kind != "room-invite" {
		t.Errorf("newest notification = %+v", bell.Notifications[0])
	}

	// Reading clears the badge.
	if rec := c.do(http.MethodPost, "/api/v1/me/notifications/read", sam, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("read = %d: %s", rec.Code, rec.Body.String())
	}
	rec = c.do(http.MethodGet, "/api/v1/me/notifications", sam, nil)
	c.decode(rec, &bell)
	if bell.Unread != 0 {
		t.Errorf("unread after read = %d, want 0", bell.Unread)
	}
	for _, entry := range bell.Notifications {
		if !entry.Read {
			t.Errorf("entry still unread: %+v", entry)
		}
	}

	// A share needs a track or a room, and somebody else to send it to.
	for _, body := range []map[string]string{
		{"userId": samID.String()},
		{"userId": samID.String(), "trackId": track.ID.String(), "roomId": "room-1"},
		{"userId": kyleID.String(), "trackId": track.ID.String()},
		{"userId": samID.String(), "trackId": "not-a-uuid"},
	} {
		if rec := c.do(http.MethodPost, "/api/v1/me/shares", kyle, body); rec.Code != http.StatusBadRequest {
			t.Errorf("bad share %v = %d, want 400", body, rec.Code)
		}
	}
	if rec := c.do(http.MethodPost, "/api/v1/me/shares", "", map[string]string{
		"userId": samID.String(), "trackId": track.ID.String(),
	}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("guest share = %d, want 401", rec.Code)
	}
}

func TestIgnoreSuppressesNotifications(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	kyle := c.register("kyle", "hunter2hunter2")
	sam := c.register("sam", "hunter2hunter2")
	kyleID := friendsUserID(t, c, "kyle")
	samID := friendsUserID(t, c, "sam")
	tracks := seedTracks(t, c, "Song")
	track := tracks[0]

	if rec := c.do(http.MethodPost, "/api/v1/me/friends/"+kyleID.String()+"/ignore", sam, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("ignore = %d: %s", rec.Code, rec.Body.String())
	}

	// kyle's share still happens — it just never rings sam's bell.
	rec := c.do(http.MethodPost, "/api/v1/me/shares", kyle, map[string]string{
		"userId": samID.String(), "trackId": track.ID.String(),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("share = %d: %s", rec.Code, rec.Body.String())
	}
	rec = c.do(http.MethodGet, "/api/v1/me/notifications", sam, nil)
	var bell struct {
		Notifications []notificationResponse `json:"notifications"`
		Unread        int                    `json:"unread"`
	}
	c.decode(rec, &bell)
	if len(bell.Notifications) != 0 || bell.Unread != 0 {
		t.Fatalf("ignored sender reached the bell: %+v", bell)
	}
	// The share itself is still there for the record.
	rec = c.do(http.MethodGet, "/api/v1/me/shares", sam, nil)
	var shares struct {
		Shares []shareResponse `json:"shares"`
	}
	c.decode(rec, &shares)
	if len(shares.Shares) != 1 {
		t.Fatalf("shares while ignored = %+v", shares.Shares)
	}

	// Unignoring brings the bell back for what comes next.
	if rec := c.do(http.MethodDelete, "/api/v1/me/friends/"+kyleID.String()+"/ignore", sam, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("unignore = %d: %s", rec.Code, rec.Body.String())
	}
	rec = c.do(http.MethodPost, "/api/v1/me/shares", kyle, map[string]string{
		"userId": samID.String(), "trackId": track.ID.String(),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("share after unignoring = %d: %s", rec.Code, rec.Body.String())
	}
	rec = c.do(http.MethodGet, "/api/v1/me/notifications", sam, nil)
	c.decode(rec, &bell)
	if len(bell.Notifications) != 1 || bell.Unread != 1 {
		t.Fatalf("bell after unignoring = %+v", bell)
	}

	// Unignoring somebody who is not ignored has nothing to undo.
	if rec := c.do(http.MethodDelete, "/api/v1/me/friends/"+kyleID.String()+"/ignore", sam, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("second unignore = %d, want 404", rec.Code)
	}
	// And there is nobody to ignore behind a made-up id.
	if rec := c.do(http.MethodPost, "/api/v1/me/friends/"+uuid.NewString()+"/ignore", sam, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("ignore stranger = %d, want 404", rec.Code)
	}
}

func TestPlaybackVisibleToFriendsOnly(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	kyle := c.register("kyle", "hunter2hunter2")
	sam := c.register("sam", "hunter2hunter2")
	kyleID := friendsUserID(t, c, "kyle")
	samID := friendsUserID(t, c, "sam")
	tracks := seedTracks(t, c, "Now", "Next")

	// kyle settles in: a favourite, a playlist, and what they are playing.
	if rec := c.do(http.MethodPost, "/api/v1/me/favorites", kyle, map[string]string{
		"trackId": tracks[0].ID.String(),
	}); rec.Code != http.StatusNoContent {
		t.Fatalf("favorite = %d", rec.Code)
	}
	if rec := c.do(http.MethodPost, "/api/v1/me/playlists", kyle, map[string]string{"name": "driving"}); rec.Code != http.StatusCreated {
		t.Fatalf("playlist = %d", rec.Code)
	}
	rec := c.do(http.MethodPut, "/api/v1/me/playback", kyle, map[string]any{
		"state": map[string]any{
			"queue":          []map[string]any{{"trackId": tracks[1].ID.String(), "title": "Next"}},
			"currentTrackId": tracks[0].ID.String(),
			"positionMs":     1234,
			"paused":         false,
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("playback save = %d: %s", rec.Code, rec.Body.String())
	}

	// Playback is for friends: a stranger and a guest both see nothing.
	playbackPath := "/api/v1/users/" + kyleID.String() + "/playback"
	if rec := c.do(http.MethodGet, playbackPath, sam, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("stranger playback = %d, want 403", rec.Code)
	}
	if rec := c.do(http.MethodGet, playbackPath, "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("guest playback = %d, want 401", rec.Code)
	}

	// sam asks, kyle accepts.
	if rec := c.do(http.MethodPost, "/api/v1/me/friends", sam, map[string]string{"username": "kyle"}); rec.Code != http.StatusCreated {
		t.Fatalf("request = %d", rec.Code)
	}
	if rec := c.do(http.MethodPost, "/api/v1/me/friends/"+samID.String()+"/accept", kyle, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("accept = %d", rec.Code)
	}

	// Now sam can follow along: the document, plus whose it is.
	rec = c.do(http.MethodGet, playbackPath, sam, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("friend playback = %d: %s", rec.Code, rec.Body.String())
	}
	var doc struct {
		Queue          []map[string]any `json:"queue"`
		CurrentTrackID string           `json:"currentTrackId"`
		PositionMs     int64            `json:"positionMs"`
		UserID         string           `json:"userId"`
	}
	c.decode(rec, &doc)
	if doc.UserID != kyleID.String() || doc.CurrentTrackID != tracks[0].ID.String() {
		t.Fatalf("playback document = %+v", doc)
	}
	if doc.PositionMs != 1234 || len(doc.Queue) != 1 {
		t.Errorf("playback document = %+v", doc)
	}

	// The friend page shows the four lists.
	rec = c.do(http.MethodGet, "/api/v1/users/"+kyleID.String(), sam, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("user page = %d: %s", rec.Code, rec.Body.String())
	}
	var page struct {
		User            publicUserResponse `json:"user"`
		PublicPlaylists []playlistResponse `json:"publicPlaylists"`
		Favorites       []trackResponse    `json:"favorites"`
		Recent          []trackResponse    `json:"recent"`
		Shared          []shareResponse    `json:"shared"`
	}
	c.decode(rec, &page)
	if page.User.Relationship != "friend" {
		t.Errorf("relationship = %q, want friend", page.User.Relationship)
	}
	if len(page.User.Listening) == 0 {
		t.Error("a friend's fresh playback document must show as listening")
	}
	if len(page.PublicPlaylists) != 1 || page.PublicPlaylists[0].Name != "driving" {
		t.Errorf("publicPlaylists = %+v", page.PublicPlaylists)
	}
	if len(page.Favorites) != 1 || page.Favorites[0].Title != "Now" {
		t.Errorf("favorites = %+v", page.Favorites)
	}
	// Recent comes out of the playback state: the song playing first, then
	// what was queued behind it.
	if len(page.Recent) != 2 || page.Recent[0].Title != "Now" || page.Recent[1].Title != "Next" {
		t.Errorf("recent = %+v", page.Recent)
	}
	if len(page.Shared) != 0 {
		t.Errorf("shared = %+v, want nothing yet", page.Shared)
	}

	// kyle shares a song; it shows up on kyle's page for sam.
	rec = c.do(http.MethodPost, "/api/v1/me/shares", kyle, map[string]string{
		"userId": samID.String(), "trackId": tracks[1].ID.String(),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("share = %d: %s", rec.Code, rec.Body.String())
	}
	rec = c.do(http.MethodGet, "/api/v1/users/"+kyleID.String(), sam, nil)
	c.decode(rec, &page)
	if len(page.Shared) != 1 || page.Shared[0].Track == nil || page.Shared[0].Track.Title != "Next" {
		t.Fatalf("shared = %+v", page.Shared)
	}
	if page.Shared[0].From.Username != "kyle" || page.Shared[0].To.Username != "sam" {
		t.Errorf("shared ends = %+v", page.Shared[0])
	}
}

func TestOnlineDerivedFromLastPlayedAt(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	kyle := c.register("kyle", "hunter2hunter2")
	sam := c.register("sam", "hunter2hunter2")
	kyleID := friendsUserID(t, c, "kyle")

	// An account that never played anything is offline.
	rec := c.do(http.MethodGet, "/api/v1/users/"+kyleID.String(), sam, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("user page = %d: %s", rec.Code, rec.Body.String())
	}
	var page struct {
		User publicUserResponse `json:"user"`
	}
	c.decode(rec, &page)
	if page.User.Online || page.User.LastPlayedAt != nil {
		t.Fatalf("before playing = %+v, want offline with no time", page.User)
	}

	// Playing something is the whole signal: saving playback state puts it
	// there.
	rec = c.do(http.MethodPut, "/api/v1/me/playback", kyle, map[string]any{
		"state": map[string]any{"currentTrackId": "t1", "positionMs": 50},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("playback save = %d: %s", rec.Code, rec.Body.String())
	}
	rec = c.do(http.MethodGet, "/api/v1/users/"+kyleID.String(), sam, nil)
	c.decode(rec, &page)
	if !page.User.Online || page.User.LastPlayedAt == nil {
		t.Fatalf("after playing = %+v, want online", page.User)
	}
	if len(page.User.Listening) == 0 {
		t.Error("a fresh playback document must show as listening")
	}

	// A stale last_played_at goes dark again — online is that column and
	// nothing else.
	if err := c.store.TouchUser(context.Background(), kyleID, time.Now().Add(-10*time.Minute)); err != nil {
		t.Fatalf("TouchUser: %v", err)
	}
	rec = c.do(http.MethodGet, "/api/v1/users/"+kyleID.String(), sam, nil)
	c.decode(rec, &page)
	if page.User.Online {
		t.Errorf("online with a 10 minute old last_played_at: %+v", page.User)
	}
	if page.User.LastPlayedAt == nil {
		t.Error("the last played time is still worth showing")
	}
}

// A server small enough to be read whole answers an empty people search with
// everybody. On a handful of accounts a list is what finding somebody you have
// not met needs; typing a name first is a guessing game.
func TestPeopleSearchListsEveryoneOnASmallServer(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	token := c.register("kyle", "hunter2hunter2")
	c.register("sam", "hunter2hunter2")

	var out struct {
		Users     []publicUserResponse `json:"users"`
		Directory bool                 `json:"directory"`
	}
	c.decode(c.do(http.MethodGet, "/api/v1/users?q=", token, nil), &out)
	if len(out.Users) != 2 {
		t.Fatalf("an empty search returned %d people, want both", len(out.Users))
	}
	if !out.Directory {
		t.Error("the answer should say it listed the server")
	}

	// A name narrows it, and that is not a listing.
	out.Users, out.Directory = nil, false
	c.decode(c.do(http.MethodGet, "/api/v1/users?q=sam", token, nil), &out)
	if len(out.Users) != 1 || out.Directory {
		t.Errorf("a named search returned %d people, directory %v", len(out.Users), out.Directory)
	}
}

// Past the limit the answer is nothing, and says so: an empty page would
// otherwise read as a server nobody is on.
func TestPeopleSearchRefusesToListALargeServer(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	token := c.register("kyle", "hunter2hunter2")
	// Made directly: registering fifty more accounts would only test argon2.
	for i := 0; i <= directoryUserLimit; i++ {
		err := c.store.CreateUser(context.Background(), &store.User{
			Username:     fmt.Sprintf("person%03d", i),
			PasswordHash: "x",
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	var out struct {
		Users     []publicUserResponse `json:"users"`
		Directory bool                 `json:"directory"`
	}
	c.decode(c.do(http.MethodGet, "/api/v1/users?q=", token, nil), &out)
	if len(out.Users) != 0 || out.Directory {
		t.Errorf("an empty search on a big server returned %d people, directory %v", len(out.Users), out.Directory)
	}

	// Searching by name still works: the limit is on listing, not on finding.
	c.decode(c.do(http.MethodGet, "/api/v1/users?q=person001", token, nil), &out)
	if len(out.Users) != 1 {
		t.Errorf("a named search on a big server returned %d people, want 1", len(out.Users))
	}
}
