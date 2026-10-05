package rooms

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/musoak/internal/config"
	"codeberg.org/kyleraykbs/musoak/internal/store"
)

// manualClock holds time still so positions are arithmetic, not waits.
type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func newManualClock() *manualClock {
	return &manualClock{now: time.Unix(1_700_000_000, 0)}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type fixture struct {
	t     *testing.T
	m     *Manager
	clock *manualClock
	db    *store.DB
	cfg   *config.Config
}

func newFixture(t *testing.T, mutate func(*config.Config)) *fixture {
	t.Helper()
	db, err := store.Open("file:rooms-" + uuid.NewString() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	cfg := config.Default()
	cfg.StorageDir = t.TempDir()
	if mutate != nil {
		mutate(cfg)
	}
	logger := slog.New(slog.DiscardHandler)
	manager := NewManager(cfg, db, logger)
	manager.grace = 10 * time.Millisecond
	clock := newManualClock()
	manager.SetClock(clock)

	t.Cleanup(func() {
		manager.Close()
		_ = db.Close()
	})
	return &fixture{t: t, m: manager, clock: clock, db: db, cfg: cfg}
}

// track creates a canonical track with a known length.
func (f *fixture) track(title string, durationMs int64) *store.Track {
	f.t.Helper()
	track := &store.Track{Title: title, DurationMs: durationMs}
	if err := f.db.CreateTrack(context.Background(), track); err != nil {
		f.t.Fatal(err)
	}
	return track
}

func guest(id string) Member { return Member{ID: id, Name: id} }

func (f *fixture) create(name string, controls Controls, password string, host Member) *Snapshot {
	f.t.Helper()
	snapshot, err := f.m.Create(name, controls, password, host)
	if err != nil {
		f.t.Fatal(err)
	}
	return snapshot
}

func (f *fixture) enqueue(roomID, memberID string, trackIDs ...uuid.UUID) *Snapshot {
	f.t.Helper()
	snapshot, err := f.m.EnqueueMany(context.Background(), roomID, memberID, trackIDs)
	if err != nil {
		f.t.Fatalf("enqueue: %v", err)
	}
	return snapshot
}

func (f *fixture) start(roomID, memberID string, trackID uuid.UUID, positionMs, durationMs int64) *Snapshot {
	f.t.Helper()
	snapshot, err := f.m.Started(roomID, memberID, trackID, positionMs, durationMs)
	if err != nil {
		f.t.Fatalf("started: %v", err)
	}
	return snapshot
}

func (f *fixture) ended(roomID, memberID string, trackID uuid.UUID) *Snapshot {
	f.t.Helper()
	snapshot, err := f.m.Ended(roomID, memberID, trackID)
	if err != nil {
		f.t.Fatalf("ended: %v", err)
	}
	return snapshot
}

func (f *fixture) get(roomID string) *Snapshot {
	f.t.Helper()
	snapshot, err := f.m.Get(roomID)
	if err != nil {
		f.t.Fatalf("get: %v", err)
	}
	return snapshot
}

// waitEvent reads the next event of the given type, skipping the rest.
func waitEvent(t *testing.T, ch <-chan Event, kind EventType) Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case event, ok := <-ch:
			if !ok {
				t.Fatalf("event stream closed while waiting for %s", kind)
			}
			if event.Type == kind {
				return event
			}
		case <-time.After(5 * time.Millisecond):
		}
	}
	t.Fatalf("no %s event", kind)
	return Event{}
}

func drain(ch <-chan Event) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func TestCreateJoinLeaveLifecycle(t *testing.T) {
	f := newFixture(t, nil)
	host := guest("host-1")
	room := f.create("party", ControlsEveryone, "sesame", host)
	if room.Host != host.ID || room.MemberCount != 1 || !room.HasPassword {
		t.Fatalf("created room = %+v", room)
	}
	if len(f.m.List()) != 1 {
		t.Fatalf("list = %d rooms", len(f.m.List()))
	}

	if _, err := f.m.Join(room.ID, guest("bob"), "wrong", ""); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := f.m.Join(room.ID, guest("bob"), "", ""); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("blank password: %v", err)
	}
	joined, err := f.m.Join(room.ID, guest("bob"), "sesame", "")
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if joined.MemberCount != 2 || len(joined.Members) != 2 {
		t.Fatalf("joined room = %+v", joined)
	}

	if _, err := f.m.Get("nope"); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("unknown room: %v", err)
	}

	// The first leave keeps the room; the last closes it, and closing is not an
	// error to the caller.
	if _, err := f.m.Leave(room.ID, "bob"); err != nil {
		t.Fatalf("leave: %v", err)
	}
	if _, err := f.m.Leave(room.ID, host.ID); err != nil {
		t.Fatalf("last leave: %v", err)
	}
	if _, err := f.m.Get(room.ID); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("closed room: %v", err)
	}
}

func TestEnqueueMixesQueuesFairly(t *testing.T) {
	f := newFixture(t, nil)
	host := guest("host-1")
	room := f.create("party", ControlsEveryone, "", host)
	if _, err := f.m.Join(room.ID, guest("bob"), "", ""); err != nil {
		t.Fatal(err)
	}

	events, cancel := f.m.Subscribe()
	defer cancel()

	a1 := f.track("a1", 100_000)
	a2 := f.track("a2", 100_000)
	b1 := f.track("b1", 100_000)
	b2 := f.track("b2", 100_000)

	f.enqueue(room.ID, host.ID, a1.ID, a2.ID)
	event := waitEvent(t, events, EventQueueUpdated)
	data := event.Data.(map[string]any)
	if data["memberId"] != host.ID {
		t.Fatalf("queue event names %v", data["memberId"])
	}
	if queue := data["memberQueue"].([]QueueItem); len(queue) != 2 {
		t.Fatalf("member queue = %d items", len(queue))
	}
	// The first song is up, waiting for the host's player.
	playback := waitEvent(t, events, EventPlayback).Data.(map[string]any)
	current := playback["current"].(*PlaybackView)
	if current == nil || current.Item.TrackID != a1.ID || current.Started {
		t.Fatalf("first song = %+v", current)
	}

	f.enqueue(room.ID, "bob", b1.ID, b2.ID)
	snapshot := f.get(room.ID)
	got := make([]string, 0, 4)
	for _, item := range snapshot.MasterQueue {
		got = append(got, item.Title)
	}
	want := []string{"a1", "b1", "a2", "b2"}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("play order = %v, want %v", got, want)
		}
	}
}

func TestHostStartsAdvancesAndIgnoresStrays(t *testing.T) {
	f := newFixture(t, nil)
	host := guest("host-1")
	room := f.create("party", ControlsEveryone, "", host)
	if _, err := f.m.Join(room.ID, guest("bob"), "", ""); err != nil {
		t.Fatal(err)
	}
	first := f.track("first", 200_000)
	second := f.track("second", 200_000)
	f.enqueue(room.ID, host.ID, first.ID, second.ID)

	// Nobody but the host can put the song on the clock.
	f.start(room.ID, "bob", first.ID, 0, 0)
	if current := f.get(room.ID).Current; current.Started {
		t.Fatal("a follower started the room")
	}

	snap := f.start(room.ID, host.ID, first.ID, 0, 190_000)
	current := snap.Current
	if !current.Started || current.Paused || current.PositionMs != 0 {
		t.Fatalf("started view = %+v", current)
	}
	if current.DurationMs != 190_000 {
		t.Fatalf("duration = %d, want the host's file", current.DurationMs)
	}

	f.clock.Advance(5 * time.Second)
	if got := f.get(room.ID).Current.PositionMs; got != 5000 {
		t.Fatalf("position after 5s = %d", got)
	}

	// A late end for a song the room has left says nothing.
	f.ended(room.ID, host.ID, second.ID)
	if got := f.get(room.ID).Current.Item.TrackID; got != first.ID {
		t.Fatalf("stray end moved the room to %s", got)
	}
	// A follower's end says nothing either.
	f.ended(room.ID, "bob", first.ID)
	if got := f.get(room.ID).Current.Item.TrackID; got != first.ID {
		t.Fatalf("follower ended the song")
	}

	// The host's file running out moves the room on, and the played item
	// leaves its owner's queue.
	snap = f.ended(room.ID, host.ID, first.ID)
	current = snap.Current
	if current == nil || current.Item.TrackID != second.ID || current.Started {
		t.Fatalf("next song = %+v", current)
	}
	if queue := snap.Queues[host.ID]; len(queue) != 1 || queue[0].TrackID != second.ID {
		t.Fatalf("host queue after advance = %+v", queue)
	}
	if len(snap.MasterQueue) != 1 || snap.MasterQueue[0].TrackID != second.ID {
		t.Fatalf("play order after advance = %+v", snap.MasterQueue)
	}

	// The last song ending leaves the room idle.
	snap = f.ended(room.ID, host.ID, second.ID)
	if snap.Current != nil {
		t.Fatalf("idle room still has a current: %+v", snap.Current)
	}
}

func TestControlPolicyGovernsTransport(t *testing.T) {
	f := newFixture(t, nil)
	host := guest("host-1")
	room := f.create("party", ControlsHost, "", host)
	if _, err := f.m.Join(room.ID, guest("bob"), "", ""); err != nil {
		t.Fatal(err)
	}
	track := f.track("only", 100_000)
	f.enqueue(room.ID, host.ID, track.ID)
	f.start(room.ID, host.ID, track.ID, 0, 0)

	if _, err := f.m.Pause(room.ID, "bob"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("follower pause under host control: %v", err)
	}
	if _, err := f.m.Pause(room.ID, host.ID); err != nil {
		t.Fatalf("host pause: %v", err)
	}

	// Under everyone-controls the same follower may.
	open := f.create("open", ControlsEveryone, "", guest("carol"))
	if _, err := f.m.Join(open.ID, guest("dave"), "", ""); err != nil {
		t.Fatal(err)
	}
	track2 := f.track("open song", 100_000)
	f.enqueue(open.ID, "carol", track2.ID)
	f.start(open.ID, "carol", track2.ID, 0, 0)
	if _, err := f.m.Pause(open.ID, "dave"); err != nil {
		t.Fatalf("follower pause under open control: %v", err)
	}
}

func TestPauseResumeSeekAndSeekPastEnd(t *testing.T) {
	f := newFixture(t, nil)
	host := guest("host-1")
	room := f.create("party", ControlsEveryone, "", host)
	track := f.track("song", 100_000)
	other := f.track("other", 100_000)
	f.enqueue(room.ID, host.ID, track.ID, other.ID)
	f.start(room.ID, host.ID, track.ID, 0, 100_000)

	f.clock.Advance(10 * time.Second)
	snap, err := f.m.Pause(room.ID, host.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Current.Paused || snap.Current.PositionMs != 10_000 {
		t.Fatalf("paused view = %+v", snap.Current)
	}
	// Time passes while paused and the room does not move.
	f.clock.Advance(30 * time.Second)
	if got := f.get(room.ID).Current.PositionMs; got != 10_000 {
		t.Fatalf("position while paused = %d", got)
	}

	snap, err = f.m.Resume(room.ID, host.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Current.Paused || snap.Current.PositionMs != 10_000 {
		t.Fatalf("resumed view = %+v", snap.Current)
	}
	f.clock.Advance(2 * time.Second)
	if got := f.get(room.ID).Current.PositionMs; got != 12_000 {
		t.Fatalf("position after resume = %d", got)
	}

	snap, err = f.m.Seek(room.ID, host.ID, 50_000)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Current.PositionMs != 50_000 {
		t.Fatalf("seek view = %+v", snap.Current)
	}
	if _, err := f.m.Seek(room.ID, host.ID, -1); !errors.Is(err, ErrInvalidSeek) {
		t.Fatalf("negative seek: %v", err)
	}

	// Seeking to the end asks for the next song.
	snap, err = f.m.Seek(room.ID, host.ID, 100_000)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Current == nil || snap.Current.Item.TrackID != other.ID {
		t.Fatalf("seek past end = %+v", snap.Current)
	}
}

func TestVotesAndAutoSkip(t *testing.T) {
	f := newFixture(t, func(cfg *config.Config) {
		cfg.ListenTogether.SkipThreshold = 2.0
		cfg.ListenTogether.MinVotersForSkip = 2
		cfg.ListenTogether.VoterFractionForSkip = 1.0
	})
	host := guest("host-1")
	room := f.create("party", ControlsEveryone, "", host)
	if _, err := f.m.Join(room.ID, guest("bob"), "", ""); err != nil {
		t.Fatal(err)
	}
	bad := f.track("bad", 100_000)
	good := f.track("good", 100_000)
	f.enqueue(room.ID, host.ID, bad.ID, good.ID)
	f.start(room.ID, host.ID, bad.ID, 0, 0)

	snap, err := f.m.Vote(context.Background(), room.ID, host.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Current == nil || snap.Current.MeanScore != 1 || snap.Current.Votes[host.ID] != 1 {
		t.Fatalf("vote view = %+v", snap.Current)
	}
	// One voter is not enough.
	if snap.Current.Item.TrackID != bad.ID {
		t.Fatal("one vote skipped the song")
	}

	if _, err := f.m.Vote(context.Background(), room.ID, "bob", 1); err != nil {
		t.Fatal(err)
	}
	// Two voters under the threshold: the song is dropped and the next one is
	// waiting for the host as usual.
	snap = f.get(room.ID)
	if snap.Current == nil || snap.Current.Item.TrackID != good.ID || snap.Current.Started {
		t.Fatalf("after auto-skip = %+v", snap.Current)
	}

	// A vote on nothing playing is refused.
	f.ended(room.ID, host.ID, good.ID)
	if _, err := f.m.Vote(context.Background(), room.ID, host.ID, 3); !errors.Is(err, ErrNoPlayback) {
		t.Fatalf("vote with nothing playing: %v", err)
	}
	// Votes are kept for stats.
	votes, err := f.db.TrackVotes(context.Background(), room.ID, bad.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(votes) != 2 {
		t.Fatalf("persisted votes = %d, want 2", len(votes))
	}
	if _, err := f.m.Vote(context.Background(), room.ID, host.ID, 9); !errors.Is(err, ErrInvalidVote) {
		t.Fatalf("bad score: %v", err)
	}
}

func TestQueueEditsAndOwnership(t *testing.T) {
	f := newFixture(t, nil)
	host := guest("host-1")
	room := f.create("party", ControlsEveryone, "", host)
	if _, err := f.m.Join(room.ID, guest("bob"), "", ""); err != nil {
		t.Fatal(err)
	}
	tracks := []*store.Track{
		f.track("t1", 1000), f.track("t2", 1000),
		f.track("t3", 1000), f.track("t4", 1000),
	}
	ids := []uuid.UUID{tracks[0].ID, tracks[1].ID, tracks[2].ID, tracks[3].ID}
	// An unknown track in a batch leaves the queue untouched.
	unknown := append([]uuid.UUID{uuid.New()}, ids[0])
	if _, err := f.m.EnqueueMany(context.Background(), room.ID, "bob", unknown); err == nil {
		t.Fatal("batch with an unknown track was accepted")
	}
	if len(f.get(room.ID).Queues["bob"]) != 0 {
		t.Fatal("failed batch changed the queue")
	}

	f.enqueue(room.ID, "bob", ids[0], ids[1], ids[2])
	snap := f.get(room.ID)
	bob := snap.Queues["bob"]
	if len(bob) != 3 {
		t.Fatalf("bob queue = %d", len(bob))
	}
	// Bob reorders his own queue.
	snap, err := f.m.Reorder(room.ID, "bob", []string{bob[2].ID, bob[0].ID, bob[1].ID})
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Queues["bob"][0].ID; got != bob[2].ID {
		t.Fatalf("reorder left %s first", got)
	}
	// The host may edit bob's queue; bob may not edit the host's.
	f.enqueue(room.ID, host.ID, ids[3])
	hostItem := f.get(room.ID).Queues[host.ID][0]
	if _, err := f.m.Remove(room.ID, "bob", hostItem.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("bob removed the host's item: %v", err)
	}
	if _, err := f.m.Remove(room.ID, host.ID, hostItem.ID); err != nil {
		t.Fatalf("host remove: %v", err)
	}
	if _, err := f.m.Clear(room.ID, "bob", host.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("bob cleared the host's queue: %v", err)
	}
	if _, err := f.m.Clear(room.ID, host.ID, "bob"); err != nil {
		t.Fatalf("host clear: %v", err)
	}
	if len(f.get(room.ID).Queues["bob"]) != 0 {
		t.Fatal("clear left items behind")
	}
}

func TestLeavePromotesHostAndDropsQueue(t *testing.T) {
	f := newFixture(t, nil)
	host := guest("host-1")
	room := f.create("party", ControlsEveryone, "", host)
	if _, err := f.m.Join(room.ID, guest("bob"), "", ""); err != nil {
		t.Fatal(err)
	}
	track := f.track("song", 100_000)
	f.enqueue(room.ID, host.ID, track.ID)
	f.start(room.ID, host.ID, track.ID, 0, 0)
	if _, err := f.m.Vote(context.Background(), room.ID, host.ID, 5); err != nil {
		t.Fatal(err)
	}

	events, cancel := f.m.Subscribe()
	defer cancel()

	snap, err := f.m.Leave(room.ID, host.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Host != "bob" {
		t.Fatalf("host after leave = %s, want bob", snap.Host)
	}
	if _, ok := snap.Queues[host.ID]; ok {
		t.Fatal("the leaver's queue survived")
	}
	if snap.Current == nil || snap.Current.Votes[host.ID] != 0 {
		t.Fatalf("the leaver's vote survived: %+v", snap.Current)
	}
	gone := waitEvent(t, events, EventQueueUpdated)
	if data := gone.Data.(map[string]any); data["gone"] != true {
		t.Fatalf("leave queue event = %+v", data)
	}
}

func TestSupersedesJoinTakesOverTheMembership(t *testing.T) {
	f := newFixture(t, nil)
	host := guest("host-1")
	room := f.create("party", ControlsEveryone, "", host)
	if _, err := f.m.Join(room.ID, guest("browser-1"), "", ""); err != nil {
		t.Fatal(err)
	}
	track := f.track("song", 100_000)
	f.enqueue(room.ID, "browser-1", track.ID)

	id := uuid.New()
	account := Member{ID: id.String(), UserID: &id, Name: "someone"}
	snap, err := f.m.Join(room.ID, account, "", "browser-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ghost := snap.Queues["browser-1"]; ghost {
		t.Fatal("the guest membership survived")
	}
	if len(snap.Queues[account.ID]) != 1 {
		t.Fatalf("the account did not take the queue over: %+v", snap.Queues)
	}
}

func TestSeqCountsEveryEvent(t *testing.T) {
	f := newFixture(t, nil)
	host := guest("host-1")
	room := f.create("party", ControlsEveryone, "", host)
	track := f.track("song", 100_000)
	f.enqueue(room.ID, host.ID, track.ID)
	snap := f.get(room.ID)
	if snap.Seq == 0 {
		t.Fatal("snapshot seq is zero after events")
	}

	events, cancel := f.m.Subscribe()
	defer cancel()
	f.start(room.ID, host.ID, track.ID, 0, 0)
	event := waitEvent(t, events, EventPlayback)
	if event.Seq != f.get(room.ID).Seq {
		t.Fatalf("event seq %d, snapshot seq %d", event.Seq, f.get(room.ID).Seq)
	}
}

func TestDisconnectGraceLeavesRooms(t *testing.T) {
	f := newFixture(t, nil)
	host := guest("host-1")
	room := f.create("party", ControlsEveryone, "", host)
	if _, err := f.m.Join(room.ID, guest("bob"), "", ""); err != nil {
		t.Fatal(err)
	}

	hasMember := func(id string) bool {
		for _, member := range f.get(room.ID).Members {
			if member.ID == id {
				return true
			}
		}
		return false
	}
	waitGone := func() bool {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if !hasMember("bob") {
				return true
			}
			time.Sleep(5 * time.Millisecond)
		}
		return false
	}

	// A socket closing is not a leave until the grace has passed.
	f.m.Connect("bob")
	f.m.Disconnect("bob")
	if !hasMember("bob") {
		t.Fatal("the member left the moment the socket closed")
	}
	if !waitGone() {
		t.Fatal("the disconnected member stayed in the room")
	}

	// A second tab, or a reconnect, keeps them there.
	if _, err := f.m.Join(room.ID, guest("bob"), "", ""); err != nil {
		t.Fatal(err)
	}
	f.m.Connect("bob")
	f.m.Connect("bob")
	f.m.Disconnect("bob")
	time.Sleep(50 * time.Millisecond)
	if !hasMember("bob") {
		t.Fatal("one of two sockets closing took the member out")
	}
	f.m.Disconnect("bob")
	if !waitGone() {
		t.Fatal("the last socket closing did not take the member out")
	}
}
