package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"codeberg.org/kyleraykbs/musoak/internal/rooms"
	"codeberg.org/kyleraykbs/musoak/internal/store"
)

func roomTitles(items []rooms.QueueItem) []string {
	titles := make([]string, 0, len(items))
	for _, item := range items {
		titles = append(titles, item.Title)
	}
	return titles
}

// TestRoomPasswordOverHTTP covers the password contract: the create response
// and the room list report hasPassword but never the password, and joining
// with the wrong password answers the contract's 403.
func TestRoomPasswordOverHTTP(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	alice := c.register("alice", "hunter2hunter2")
	bob := c.register("bob", "hunter2hunter2")

	rec := c.do(http.MethodPost, "/api/v1/rooms", alice, map[string]string{
		"name": "party", "controls": "everyone", "password": "sesame",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "sesame") {
		t.Errorf("the create response leaks the password: %s", rec.Body.String())
	}
	var created roomResponse
	c.decode(rec, &created)
	if created.Room == nil || !created.Room.HasPassword {
		t.Fatalf("created room = %+v, want hasPassword", created.Room)
	}
	roomID := created.Room.ID

	list := c.do(http.MethodGet, "/api/v1/rooms", "", nil)
	if list.Code != http.StatusOK {
		t.Fatalf("list: %d %s", list.Code, list.Body.String())
	}
	var listed struct {
		Rooms []rooms.Snapshot `json:"rooms"`
	}
	c.decode(list, &listed)
	if len(listed.Rooms) != 1 {
		t.Fatalf("rooms = %d, want 1", len(listed.Rooms))
	}
	if listed.Rooms[0].MemberCount != 1 || !listed.Rooms[0].HasPassword {
		t.Errorf("list entry = %+v, want memberCount 1 and hasPassword", listed.Rooms[0])
	}
	if strings.Contains(list.Body.String(), "sesame") {
		t.Errorf("the room list leaks the password: %s", list.Body.String())
	}

	// The wrong password is refused with exactly the contract's error.
	bad := c.do(http.MethodPost, "/api/v1/rooms/"+roomID+"/join", bob, map[string]string{"password": "wrong"})
	if bad.Code != http.StatusForbidden {
		t.Fatalf("join with the wrong password: %d %s", bad.Code, bad.Body.String())
	}
	var errBody struct {
		Error string `json:"error"`
	}
	c.decode(bad, &errBody)
	if errBody.Error != "wrong password" {
		t.Errorf("error = %q, want %q", errBody.Error, "wrong password")
	}

	blank := c.do(http.MethodPost, "/api/v1/rooms/"+roomID+"/join", bob, map[string]string{})
	if blank.Code != http.StatusForbidden {
		t.Errorf("join without a password: %d %s", blank.Code, blank.Body.String())
	}

	good := c.do(http.MethodPost, "/api/v1/rooms/"+roomID+"/join", bob, map[string]string{"password": "sesame"})
	if good.Code != http.StatusOK {
		t.Fatalf("join with the right password: %d %s", good.Code, good.Body.String())
	}
	var joined roomResponse
	c.decode(good, &joined)
	if joined.Room == nil || joined.Room.MemberCount != 2 {
		t.Errorf("joined room = %+v, want memberCount 2", joined.Room)
	}
}

func TestRoomSyncDrivesCurrentPlaybackOverHTTP(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	alice := c.register("alice", "hunter2hunter2")
	bob := c.register("bob", "hunter2hunter2")
	createdRec := c.do(http.MethodPost, "/api/v1/rooms", alice, map[string]string{"name": "party", "controls": "everyone"})
	var created roomResponse
	c.decode(createdRec, &created)
	roomID := created.Room.ID
	if joined := c.do(http.MethodPost, "/api/v1/rooms/"+roomID+"/join", bob, nil); joined.Code != http.StatusOK {
		t.Fatalf("join: %d %s", joined.Code, joined.Body.String())
	}
	first := &store.Track{Title: "first", DurationMs: 60_000}
	second := &store.Track{Title: "second", DurationMs: 60_000}
	for _, track := range []*store.Track{first, second} {
		if err := c.store.CreateTrack(context.Background(), track); err != nil {
			t.Fatal(err)
		}
	}
	queued := c.do(http.MethodPost, "/api/v1/rooms/"+roomID+"/queue", alice,
		map[string][]string{"trackIds": {first.ID.String(), second.ID.String()}})
	if queued.Code != http.StatusOK {
		t.Fatalf("queue: %d %s", queued.Code, queued.Body.String())
	}
	get := func() rooms.Snapshot {
		t.Helper()
		rec := c.do(http.MethodGet, "/api/v1/rooms/"+roomID, "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
		}
		var room rooms.Snapshot
		c.decode(rec, &room)
		return room
	}
	state := get()
	firstItem := state.MasterQueue[0]

	body := map[string]any{
		"itemId": firstItem.ID, "trackId": first.ID.String(), "positionMs": int64(4200),
		"durationMs": int64(59000), "started": true, "paused": false,
	}
	if follower := c.do(http.MethodPost, "/api/v1/rooms/"+roomID+"/sync", bob, body); follower.Code != http.StatusForbidden {
		t.Fatalf("follower sync: %d %s", follower.Code, follower.Body.String())
	}
	if host := c.do(http.MethodPost, "/api/v1/rooms/"+roomID+"/sync", alice, body); host.Code != http.StatusNoContent {
		t.Fatalf("host sync: %d %s", host.Code, host.Body.String())
	}
	state = get()
	if state.Current == nil || state.Current.Item.ID != firstItem.ID || !state.Current.Started || state.Current.PositionMs != 4200 {
		t.Fatalf("host state = %+v", state.Current)
	}

	secondItem := state.MasterQueue[1]
	next := map[string]any{"itemId": secondItem.ID, "trackId": second.ID.String(), "positionMs": int64(0), "durationMs": int64(60000), "started": false, "paused": true}
	if host := c.do(http.MethodPost, "/api/v1/rooms/"+roomID+"/sync", alice, next); host.Code != http.StatusNoContent {
		t.Fatalf("advance sync: %d %s", host.Code, host.Body.String())
	}
	state = get()
	if state.Current == nil || state.Current.Item.ID != secondItem.ID || state.Current.Started || !state.Current.Paused {
		t.Fatalf("advanced state = %+v", state.Current)
	}

	idle := map[string]any{"itemId": "", "positionMs": int64(0), "started": false, "paused": true}
	if host := c.do(http.MethodPost, "/api/v1/rooms/"+roomID+"/sync", alice, idle); host.Code != http.StatusNoContent {
		t.Fatalf("idle sync: %d %s", host.Code, host.Body.String())
	}
	if state = get(); state.Current != nil || len(state.MasterQueue) != 0 {
		t.Fatalf("idle room = %+v", state)
	}
}

// TestQueueAddIsCapped: the room lock is held across an add, so the handler
// refuses more than it will carry in one go - and a refused batch queues
// nothing. The SDK splits longer runs at the same number, so the two must
// agree.
func TestQueueAddIsCapped(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	alice := c.register("alice", "hunter2hunter2")
	rec := c.do(http.MethodPost, "/api/v1/rooms", alice, map[string]string{"name": "party", "controls": "everyone"})
	var created roomResponse
	c.decode(rec, &created)
	roomID := created.Room.ID

	ids := make([]string, 0, maxEnqueueBatch+1)
	for range maxEnqueueBatch + 1 {
		track := &store.Track{Title: "Song", DurationMs: 180_000}
		if err := c.store.CreateTrack(context.Background(), track); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, track.ID.String())
	}

	over := c.do(http.MethodPost, "/api/v1/rooms/"+roomID+"/queue", alice, map[string][]string{"trackIds": ids})
	if over.Code != http.StatusBadRequest {
		t.Fatalf("adding %d tracks: %d %s, want 400", len(ids), over.Code, over.Body.String())
	}
	get := func() rooms.Snapshot {
		c.t.Helper()
		rec := c.do(http.MethodGet, "/api/v1/rooms/"+roomID, "", nil)
		if rec.Code != http.StatusOK {
			c.t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
		}
		var snapshot rooms.Snapshot
		c.decode(rec, &snapshot)
		return snapshot
	}
	if got := len(get().MasterQueue); got != 0 {
		t.Errorf("a refused batch queued %d tracks, want none", got)
	}

	at := c.do(http.MethodPost, "/api/v1/rooms/"+roomID+"/queue", alice, map[string][]string{"trackIds": ids[:maxEnqueueBatch]})
	if at.Code != http.StatusOK {
		t.Fatalf("adding %d tracks: %d %s, want 200", maxEnqueueBatch, at.Code, at.Body.String())
	}
	if got := len(get().Queues[created.MemberID]); got != maxEnqueueBatch {
		t.Errorf("queued %d tracks, want %d", got, maxEnqueueBatch)
	}
}

// TestRoomQueuesOverHTTP walks the queue endpoints: each member enqueues into
// their own queue, the master queue interleaves the two fairly (A1 B1 A2 B2),
// and remove/reorder/clear only ever act on the caller's queue — with the host
// as the one exception who may act on anyone's.
func TestRoomQueuesOverHTTP(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	alice := c.register("alice", "hunter2hunter2")
	bob := c.register("bob", "hunter2hunter2")

	rec := c.do(http.MethodPost, "/api/v1/rooms", alice, map[string]string{
		"name": "party", "controls": "everyone",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created roomResponse
	c.decode(rec, &created)
	roomID := created.Room.ID
	aliceID := created.MemberID

	joined := c.do(http.MethodPost, "/api/v1/rooms/"+roomID+"/join", bob, nil)
	if joined.Code != http.StatusOK {
		t.Fatalf("join: %d %s", joined.Code, joined.Body.String())
	}
	var bobJoined roomResponse
	c.decode(joined, &bobJoined)
	bobID := bobJoined.MemberID

	tracks := map[string]string{}
	for _, title := range []string{"A1", "A2", "B1", "B2", "B3"} {
		track := &store.Track{Title: title, DurationMs: 180_000}
		if err := c.store.CreateTrack(context.Background(), track); err != nil {
			t.Fatal(err)
		}
		tracks[title] = track.ID.String()
	}
	enqueue := func(token, title string) {
		c.t.Helper()
		rec := c.do(http.MethodPost, "/api/v1/rooms/"+roomID+"/queue", token, map[string]string{"trackId": tracks[title]})
		if rec.Code != http.StatusOK {
			c.t.Fatalf("enqueue %s: %d %s", title, rec.Code, rec.Body.String())
		}
	}
	get := func() rooms.Snapshot {
		c.t.Helper()
		rec := c.do(http.MethodGet, "/api/v1/rooms/"+roomID, "", nil)
		if rec.Code != http.StatusOK {
			c.t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
		}
		var snapshot rooms.Snapshot
		c.decode(rec, &snapshot)
		return snapshot
	}

	enqueue(alice, "A1")
	enqueue(alice, "A2")
	enqueue(bob, "B1")
	enqueue(bob, "B2")

	state := get()
	if got, want := roomTitles(state.MasterQueue), []string{"A1", "B1", "A2", "B2"}; !slices.Equal(got, want) {
		t.Errorf("masterQueue = %v, want %v", got, want)
	}
	if got, want := roomTitles(state.Queues[aliceID]), []string{"A1", "A2"}; !slices.Equal(got, want) {
		t.Errorf("alice's queue = %v, want %v", got, want)
	}
	if got, want := roomTitles(state.Queues[bobID]), []string{"B1", "B2"}; !slices.Equal(got, want) {
		t.Errorf("bob's queue = %v, want %v", got, want)
	}

	// Reorder and remove act on the caller's own queue only.
	enqueue(bob, "B3")
	state = get()
	bobItems := state.Queues[bobID]
	reorder := c.do(http.MethodPost, "/api/v1/rooms/"+roomID+"/queue/reorder", bob, map[string][]string{
		"itemIds": {bobItems[1].ID, bobItems[0].ID, bobItems[2].ID},
	})
	if reorder.Code != http.StatusOK {
		t.Fatalf("reorder: %d %s", reorder.Code, reorder.Body.String())
	}
	state = get()
	if got, want := roomTitles(state.Queues[bobID]), []string{"B2", "B1", "B3"}; !slices.Equal(got, want) {
		t.Errorf("bob's queue after reorder = %v, want %v", got, want)
	}
	if got, want := roomTitles(state.Queues[aliceID]), []string{"A1", "A2"}; !slices.Equal(got, want) {
		t.Errorf("alice's queue after bob's reorder = %v, want %v", got, want)
	}

	remove := c.do(http.MethodDelete, "/api/v1/rooms/"+roomID+"/queue/"+state.Queues[bobID][0].ID, bob, nil)
	if remove.Code != http.StatusOK {
		t.Fatalf("remove: %d %s", remove.Code, remove.Body.String())
	}
	state = get()
	if got, want := roomTitles(state.Queues[bobID]), []string{"B1", "B3"}; !slices.Equal(got, want) {
		t.Errorf("bob's queue after remove = %v, want %v", got, want)
	}
	if got, want := roomTitles(state.Queues[aliceID]), []string{"A1", "A2"}; !slices.Equal(got, want) {
		t.Errorf("alice's queue after bob's remove = %v, want %v", got, want)
	}

	// The clear endpoint empties the caller's queue.
	clear := c.do(http.MethodDelete, "/api/v1/rooms/"+roomID+"/queue", bob, nil)
	if clear.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", clear.Code, clear.Body.String())
	}
	state = get()
	if len(state.Queues[bobID]) != 0 {
		t.Errorf("bob's queue after clear = %v, want empty", roomTitles(state.Queues[bobID]))
	}
	if got, want := roomTitles(state.Queues[aliceID]), []string{"A1", "A2"}; !slices.Equal(got, want) {
		t.Errorf("alice's queue after bob's clear = %v, want %v", got, want)
	}
	if got, want := roomTitles(state.MasterQueue), []string{"A1", "A2"}; !slices.Equal(got, want) {
		t.Errorf("masterQueue after clear = %v, want %v", got, want)
	}

	// The host may clear anyone's queue; anyone else may not.
	enqueue(bob, "B1")
	hostClear := c.do(http.MethodDelete, "/api/v1/rooms/"+roomID+"/queue?memberId="+bobID, alice, nil)
	if hostClear.Code != http.StatusOK {
		t.Fatalf("host clear: %d %s", hostClear.Code, hostClear.Body.String())
	}
	if len(get().Queues[bobID]) != 0 {
		t.Error("the host's clear did not empty bob's queue")
	}
	steal := c.do(http.MethodDelete, "/api/v1/rooms/"+roomID+"/queue?memberId="+aliceID, bob, nil)
	if steal.Code != http.StatusForbidden {
		t.Errorf("clearing someone else's queue: %d %s, want 403", steal.Code, steal.Body.String())
	}
}
