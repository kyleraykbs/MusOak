package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func mustFriendsUser(t *testing.T, db *DB, username, displayName string) *User {
	t.Helper()
	user := &User{Username: username, DisplayName: displayName, PasswordHash: "x"}
	if err := db.CreateUser(context.Background(), user); err != nil {
		t.Fatalf("CreateUser(%s): %v", username, err)
	}
	return user
}

func TestFriendRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	kyle := mustFriendsUser(t, db, "kyle", "Kyle")
	sam := mustFriendsUser(t, db, "sam", "Sam")

	// kyle asks sam.
	if err := db.SendFriendRequest(ctx, kyle.ID, sam.ID); err != nil {
		t.Fatalf("SendFriendRequest: %v", err)
	}
	// Asking again — or asking back while the first request is pending — is
	// one request too many.
	if err := db.SendFriendRequest(ctx, kyle.ID, sam.ID); !errors.Is(err, ErrConflict) {
		t.Errorf("second request: err = %v, want ErrConflict", err)
	}
	if err := db.SendFriendRequest(ctx, sam.ID, kyle.ID); !errors.Is(err, ErrConflict) {
		t.Errorf("cross request: err = %v, want ErrConflict", err)
	}
	if err := db.SendFriendRequest(ctx, kyle.ID, kyle.ID); err == nil {
		t.Error("self request must fail")
	}

	// The request shows up on both sides, as different relationships.
	outgoing, err := db.OutgoingFriendRequests(ctx, kyle.ID)
	if err != nil {
		t.Fatalf("OutgoingFriendRequests: %v", err)
	}
	if len(outgoing) != 1 || outgoing[0].ID != sam.ID {
		t.Fatalf("outgoing = %+v", outgoing)
	}
	incoming, err := db.IncomingFriendRequests(ctx, sam.ID)
	if err != nil {
		t.Fatalf("IncomingFriendRequests: %v", err)
	}
	if len(incoming) != 1 || incoming[0].ID != kyle.ID {
		t.Fatalf("incoming = %+v", incoming)
	}
	if friends, _ := db.Friends(ctx, kyle.ID); len(friends) != 0 {
		t.Fatalf("friends before acceptance = %+v", friends)
	}

	// One lookup labels a whole batch — with the viewer itself among them.
	rels, err := db.Relationships(ctx, kyle.ID, []uuid.UUID{sam.ID, sam.ID, kyle.ID})
	if err != nil {
		t.Fatalf("Relationships: %v", err)
	}
	if rels[sam.ID] != RelPendingOut || rels[kyle.ID] != RelNone {
		t.Errorf("relationships = %v", rels)
	}
	if _, rel, err := db.UserForViewer(ctx, sam.ID, kyle.ID); err != nil || rel != RelPendingIn {
		t.Errorf("UserForViewer = %v, %v; want pending-in", rel, err)
	}

	// sam accepts: the row is now symmetric.
	if err := db.AcceptFriendRequest(ctx, sam.ID, kyle.ID); err != nil {
		t.Fatalf("AcceptFriendRequest: %v", err)
	}
	for name, id := range map[string]uuid.UUID{"kyle": kyle.ID, "sam": sam.ID} {
		friends, err := db.Friends(ctx, id)
		if err != nil {
			t.Fatalf("Friends(%s): %v", name, err)
		}
		if len(friends) != 1 || friends[0].PasswordHash != "" {
			t.Fatalf("friends(%s) = %+v", name, friends)
		}
	}
	if _, rel, err := db.UserForViewer(ctx, kyle.ID, sam.ID); err != nil || rel != RelFriend {
		t.Errorf("UserForViewer = %v, %v; want friend", rel, err)
	}
	// There is nothing pending left to accept.
	if err := db.AcceptFriendRequest(ctx, sam.ID, kyle.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second accept: err = %v, want ErrNotFound", err)
	}

	// kyle unfriends.
	if err := db.RemoveFriend(ctx, kyle.ID, sam.ID); err != nil {
		t.Fatalf("RemoveFriend: %v", err)
	}
	if friends, _ := db.Friends(ctx, sam.ID); len(friends) != 0 {
		t.Fatalf("friends after removal = %+v", friends)
	}
	if _, rel, _ := db.UserForViewer(ctx, kyle.ID, sam.ID); rel != RelNone {
		t.Errorf("relationship after removal = %v, want none", rel)
	}
	if err := db.RemoveFriend(ctx, kyle.ID, sam.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second removal: err = %v, want ErrNotFound", err)
	}

	// Declining is the same removal seen from the other side.
	if err := db.SendFriendRequest(ctx, kyle.ID, sam.ID); err != nil {
		t.Fatalf("re-request: %v", err)
	}
	if err := db.RemoveFriend(ctx, sam.ID, kyle.ID); err != nil {
		t.Fatalf("decline: %v", err)
	}
	if _, rel, _ := db.UserForViewer(ctx, sam.ID, kyle.ID); rel != RelNone {
		t.Errorf("relationship after decline = %v, want none", rel)
	}
}

func TestSearchUsersByName(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	ada := mustFriendsUser(t, db, "ada", "Ada Lovelace")
	mustFriendsUser(t, db, "axb", "AXb")
	mustFriendsUser(t, db, "a_b", "Under")

	// Display names match, case-insensitively.
	got, err := db.SearchUsers(ctx, "LOVELACE", 20)
	if err != nil {
		t.Fatalf("SearchUsers: %v", err)
	}
	if len(got) != 1 || got[0].ID != ada.ID || got[0].DisplayName != "Ada Lovelace" {
		t.Fatalf("by display name = %+v", got)
	}

	// So do usernames.
	got, _ = db.SearchUsers(ctx, "ADA", 20)
	if len(got) != 1 || got[0].ID != ada.ID {
		t.Fatalf("by username = %+v", got)
	}

	// And multi-word display names, in either case.
	got, _ = db.SearchUsers(ctx, "ada love", 20)
	if len(got) != 1 || got[0].ID != ada.ID {
		t.Fatalf("by phrase = %+v", got)
	}

	// A wildcard in the query matches itself, not every name.
	got, _ = db.SearchUsers(ctx, "a_b", 20)
	if len(got) != 1 || got[0].Username != "a_b" {
		t.Fatalf("wildcard = %+v", got)
	}

	// The rows travel into other people's responses, so they carry no
	// password hash.
	if got[0].PasswordHash != "" {
		t.Errorf("search leaked a password hash for %s", got[0].Username)
	}

	// An empty query lists everyone, bounded by the limit.
	got, _ = db.SearchUsers(ctx, "", 20)
	if len(got) != 3 {
		t.Fatalf("everyone = %d rows, want 3", len(got))
	}
	got, _ = db.SearchUsers(ctx, "", 1)
	if len(got) != 1 {
		t.Fatalf("limited = %d rows, want 1", len(got))
	}
}

func TestSharesBetweenAndUserShares(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	kyle := mustFriendsUser(t, db, "kyle", "Kyle")
	sam := mustFriendsUser(t, db, "sam", "Sam")
	track := mustTrack(t, db, "Song")

	sent := Share{FromUser: kyle.ID, ToUser: sam.ID, TrackID: &track.ID}
	if err := db.RecordShare(ctx, &sent); err != nil {
		t.Fatalf("RecordShare: %v", err)
	}
	if sent.ID == uuid.Nil || sent.CreatedAt.IsZero() {
		t.Fatalf("RecordShare did not fill the row: %+v", sent)
	}
	invite := Share{FromUser: sam.ID, ToUser: kyle.ID, RoomID: "room-1"}
	if err := db.RecordShare(ctx, &invite); err != nil {
		t.Fatalf("RecordShare invite: %v", err)
	}

	// SharesBetween is directional: what from sent to to.
	fromKyle, err := db.SharesBetween(ctx, kyle.ID, sam.ID)
	if err != nil {
		t.Fatalf("SharesBetween: %v", err)
	}
	if len(fromKyle) != 1 || fromKyle[0].ID != sent.ID {
		t.Fatalf("shares from kyle = %+v", fromKyle)
	}
	if fromKyle[0].TrackID == nil || *fromKyle[0].TrackID != track.ID {
		t.Errorf("share track = %+v", fromKyle[0].TrackID)
	}

	// UserShares sees the whole conversation.
	both, err := db.UserShares(ctx, kyle.ID)
	if err != nil {
		t.Fatalf("UserShares: %v", err)
	}
	if len(both) != 2 {
		t.Fatalf("shares touching kyle = %+v", both)
	}
	if both[0].ID != invite.ID || both[1].ID != sent.ID {
		t.Errorf("newest first: %+v", both)
	}
	if both[0].RoomID != "room-1" || both[0].TrackID != nil {
		t.Errorf("invite = %+v", both[0])
	}
}

func TestIgnoreSuppressesNotifications(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	kyle := mustFriendsUser(t, db, "kyle", "Kyle")
	sam := mustFriendsUser(t, db, "sam", "Sam")
	track := mustTrack(t, db, "Song")

	notify := func() (bool, error) {
		return db.CreateNotification(ctx, &Notification{
			UserID:   sam.ID,
			Kind:     "share",
			FromUser: &kyle.ID,
			TrackID:  &track.ID,
		})
	}

	created, err := notify()
	if err != nil || !created {
		t.Fatalf("first notification = %v, %v; want created", created, err)
	}

	// Ignored senders never reach the bell.
	if err := db.IgnoreUser(ctx, sam.ID, kyle.ID); err != nil {
		t.Fatalf("IgnoreUser: %v", err)
	}
	created, err = notify()
	if err != nil || created {
		t.Fatalf("ignored notification = %v, %v; want skipped", created, err)
	}
	entries, unread, err := db.Notifications(ctx, sam.ID)
	if err != nil {
		t.Fatalf("Notifications: %v", err)
	}
	if len(entries) != 1 || unread != 1 {
		t.Fatalf("bell after ignoring = %d entries, %d unread", len(entries), unread)
	}

	// Ignoring twice is not an error; unignoring twice is.
	if err := db.IgnoreUser(ctx, sam.ID, kyle.ID); err != nil {
		t.Errorf("IgnoreUser again: %v", err)
	}
	if err := db.UnignoreUser(ctx, sam.ID, kyle.ID); err != nil {
		t.Fatalf("UnignoreUser: %v", err)
	}
	if err := db.UnignoreUser(ctx, sam.ID, kyle.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("UnignoreUser again: err = %v, want ErrNotFound", err)
	}

	// And the bell rings again.
	if created, err = notify(); err != nil || !created {
		t.Fatalf("notification after unignoring = %v, %v; want created", created, err)
	}
	if _, unread, _ := db.Notifications(ctx, sam.ID); unread != 2 {
		t.Errorf("unread = %d, want 2", unread)
	}
}

func TestNotificationsCountAndMarkRead(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	kyle := mustFriendsUser(t, db, "kyle", "Kyle")
	sam := mustFriendsUser(t, db, "sam", "Sam")
	track := mustTrack(t, db, "Song")

	for _, n := range []Notification{
		{UserID: sam.ID, Kind: "share", FromUser: &kyle.ID, TrackID: &track.ID},
		{UserID: sam.ID, Kind: "room-invite", FromUser: &kyle.ID, RoomID: "room-1"},
		{UserID: kyle.ID, Kind: "share", FromUser: &sam.ID, TrackID: &track.ID},
	} {
		entry := n
		if created, err := db.CreateNotification(ctx, &entry); err != nil || !created {
			t.Fatalf("CreateNotification = %v, %v", created, err)
		}
	}

	// The bell lists only its owner's entries, newest first, with the unread
	// count beside them.
	entries, unread, err := db.Notifications(ctx, sam.ID)
	if err != nil {
		t.Fatalf("Notifications: %v", err)
	}
	if len(entries) != 2 || unread != 2 {
		t.Fatalf("bell = %d entries, %d unread", len(entries), unread)
	}
	if entries[0].Kind != "room-invite" || entries[1].Kind != "share" {
		t.Errorf("order = %s then %s, want newest first", entries[0].Kind, entries[1].Kind)
	}
	if entries[0].RoomID != "room-1" || entries[1].TrackID == nil {
		t.Errorf("entries = %+v", entries)
	}
	if entries[0].Read {
		t.Error("a fresh notification must start unread")
	}

	// Marking read empties the badge without losing the entries.
	if err := db.MarkNotificationsRead(ctx, sam.ID); err != nil {
		t.Fatalf("MarkNotificationsRead: %v", err)
	}
	entries, unread, _ = db.Notifications(ctx, sam.ID)
	if unread != 0 {
		t.Errorf("unread after read = %d, want 0", unread)
	}
	for _, entry := range entries {
		if !entry.Read {
			t.Errorf("entry still unread: %+v", entry)
		}
	}
	// Reading is per user: kyle's bell is untouched.
	if _, unread, _ := db.Notifications(ctx, kyle.ID); unread != 1 {
		t.Errorf("kyle's unread = %d, want 1", unread)
	}
}

func TestRecentPlayback(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	kyle := mustFriendsUser(t, db, "kyle", "Kyle")

	if _, _, err := db.RecentPlayback(ctx, kyle.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty playback: err = %v, want ErrNotFound", err)
	}

	doc := []byte(`{"queue": [], "currentTrackId": "t1", "positionMs": 5, "paused": true}`)
	if err := db.SetPlaybackState(ctx, kyle.ID, doc); err != nil {
		t.Fatalf("SetPlaybackState: %v", err)
	}
	got, updated, err := db.RecentPlayback(ctx, kyle.ID)
	if err != nil {
		t.Fatalf("RecentPlayback: %v", err)
	}
	if string(got) != string(doc) {
		t.Errorf("state = %s, want %s", got, doc)
	}
	// The timestamp is what tells "listening now" from "was listening once".
	if since := time.Since(updated); since < 0 || since > time.Minute {
		t.Errorf("updated = %v, want just now", updated)
	}
}
