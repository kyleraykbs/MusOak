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

// manualClock holds time still so positions are arithmetic, not waits; the
// Manager reads it for every stamp. A test moves it with Advance and then calls
// tick, which is what the real loop does once a second.
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

// Advance moves the clock forward without running a beat.
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

// tick runs one beat of every room's clock, which is what the Manager's own
// loop does once a second.
func (f *fixture) tick() { f.m.tick(f.clock.Now().UnixMilli()) }

// advance moves the clock forward and runs one beat.
func (f *fixture) advance(d time.Duration) {
	f.clock.Advance(d)
	f.tick()
}

func (f *fixture) get(roomID string) *Snapshot {
	f.t.Helper()
	snapshot, err := f.m.Get(roomID)
	if err != nil {
		f.t.Fatalf("get: %v", err)
	}
	return snapshot
}

// attach gives a member a live event socket: the room plays to a listener and
// the tick is willing to move it on.
func (f *fixture) attach(memberID string) { f.m.Connect(memberID) }

// detach takes a member's last socket away, leaving them a member: a listener
// whose tab closed, not somebody who left.
func (f *fixture) detach(memberID string) { f.m.Disconnect(memberID) }

// longGrace keeps a detached member from being reaped mid-test, so a test can
// hold a room in the "nobody listening" state it is about.
func (f *fixture) longGrace() { f.m.grace = time.Hour }

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
	// Enqueue starts the room: the server is the clock now, so a room with a
	// queue and somebody in it plays - there is no host sync to wait for.
	state := f.get(room.ID)
	if state.Current == nil {
		t.Fatal("the room did not start on its own enqueue")
	}
	if state.Current.Item.ID != state.MasterQueue[0].ID {
		t.Fatalf("the room started on something other than the first queued item: %+v", state.Current.Item)
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

// A room with somebody listening starts its first queued song by itself, and
// the tick moves it on when the song's length elapses.
func TestTheServerStartsAndAdvancesTheRoom(t *testing.T) {
	f := newFixture(t, nil)
	host := guest("host-1")
	room := f.create("party", ControlsEveryone, "", host)
	f.attach(host.ID)
	first := f.track("first", 30_000)
	second := f.track("second", 30_000)
	f.enqueue(room.ID, host.ID, first.ID, second.ID)

	state := f.get(room.ID)
	if state.Current == nil || state.Current.Item.TrackID != first.ID {
		t.Fatalf("the room did not start its queue: %+v", state.Current)
	}
	if !state.Current.Started || state.Current.Paused {
		t.Fatalf("the current song is not running: %+v", state.Current)
	}
	if state.Current.PositionMs != 0 {
		t.Fatalf("current position = %d, want 0", state.Current.PositionMs)
	}

	f.advance(29 * time.Second)
	state = f.get(room.ID)
	if state.Current.Item.TrackID != first.ID {
		t.Fatalf("the room moved on before the song was over")
	}
	if state.Current.PositionMs != 29_000 {
		t.Fatalf("position after 29s = %d", state.Current.PositionMs)
	}

	f.advance(2 * time.Second)
	state = f.get(room.ID)
	if state.Current == nil || state.Current.Item.TrackID != second.ID {
		t.Fatalf("the room did not move on: %+v", state.Current)
	}
	if !state.Current.Started || state.Current.Paused {
		t.Fatalf("the next song is not running: %+v", state.Current)
	}
	if len(state.MasterQueue) != 1 || state.MasterQueue[0].TrackID != second.ID {
		t.Fatalf("queue after an advance = %+v", state.MasterQueue)
	}
	// The played item left its owner's queue as well as the play order.
	if len(state.Queues[host.ID]) != 1 || state.Queues[host.ID][0].TrackID != second.ID {
		t.Fatalf("played item remained in the queue: %+v", state.Queues[host.ID])
	}
}

// The play order keeps its place when an item is dropped: the room plays one
// track per member per pass, and item ids tell two copies of one track apart.
func TestAdvanceKeepsRoundRobinPhaseAndDropsFromQueue(t *testing.T) {
	f := newFixture(t, nil)
	host := guest("host-1")
	room := f.create("party", ControlsEveryone, "", host)
	if _, err := f.m.Join(room.ID, guest("bob"), "", ""); err != nil {
		t.Fatal(err)
	}
	f.attach(host.ID)
	first := f.track("first", 10_000)
	repeated := f.track("repeated", 10_000)
	middle := f.track("middle", 10_000)
	f.enqueue(room.ID, host.ID, first.ID, repeated.ID, repeated.ID)
	f.enqueue(room.ID, "bob", middle.ID)

	state := f.get(room.ID)
	want := []string{"first", "middle", "repeated", "repeated"}
	if len(state.MasterQueue) != len(want) {
		t.Fatalf("mixed queue size = %d, want %d", len(state.MasterQueue), len(want))
	}
	for i, title := range want {
		if state.MasterQueue[i].Title != title {
			t.Fatalf("mixed queue = %+v, want title %q at %d", state.MasterQueue, title, i)
		}
	}
	firstRepeat, secondRepeat := state.MasterQueue[2], state.MasterQueue[3]
	if firstRepeat.ID == secondRepeat.ID || firstRepeat.TrackID != secondRepeat.TrackID {
		t.Fatalf("duplicate queue entries = %+v, %+v", firstRepeat, secondRepeat)
	}

	f.advance(11 * time.Second)
	if got := f.get(room.ID).Current.Item.TrackID; got != middle.ID {
		t.Fatalf("second song = %s", got)
	}
	f.advance(11 * time.Second)
	if got := f.get(room.ID).Current.Item.ID; got != firstRepeat.ID {
		t.Fatalf("third song = %s, want the first copy", got)
	}
	f.advance(11 * time.Second)
	if got := f.get(room.ID).Current.Item.ID; got != secondRepeat.ID {
		t.Fatalf("fourth song = %s, want the second copy", got)
	}
	f.advance(11 * time.Second)
	state = f.get(room.ID)
	if state.Current != nil || len(state.MasterQueue) != 0 {
		t.Fatalf("idle room after the last song = %+v", state)
	}
}

// The tick republishes the position every beat, so a client that missed an
// event is corrected by the next one; the played item's queue rides that beat.
func TestTickPublishesTheHeartbeatAndTheDroppedQueue(t *testing.T) {
	f := newFixture(t, nil)
	host := guest("host-1")
	room := f.create("party", ControlsEveryone, "", host)
	f.attach(host.ID)
	first := f.track("first", 10_000)
	second := f.track("second", 10_000)
	f.enqueue(room.ID, host.ID, first.ID, second.ID)

	events, cancel := f.m.Subscribe()
	defer cancel()

	// A beat with nothing to change is still a heartbeat.
	f.advance(1 * time.Second)
	beat := waitEvent(t, events, EventPlayback)
	data := beat.Data.(map[string]any)
	if view := data["current"].(*PlaybackView); view == nil || view.PositionMs != 1_000 {
		t.Fatalf("heartbeat view = %+v", data["current"])
	}

	// The beat that ends the song carries the queue it dropped.
	f.advance(10 * time.Second)
	dropped := waitEvent(t, events, EventQueueUpdated)
	if qdata := dropped.Data.(map[string]any); qdata["memberId"] != host.ID {
		t.Fatalf("advance queue event = %+v", qdata)
	}
	if view := f.get(room.ID).Current; view == nil || view.Item.TrackID != second.ID {
		t.Fatalf("the beat did not start the next song: %+v", view)
	}
}

// A song whose length the server never learned still moves the room on, after
// the fallback window rather than never.
func TestUnknownDurationUsesTheFallbackWindow(t *testing.T) {
	f := newFixture(t, nil)
	host := guest("host-1")
	room := f.create("party", ControlsEveryone, "", host)
	f.attach(host.ID)
	unknown := &store.Track{Title: "no length"}
	if err := f.db.CreateTrack(context.Background(), unknown); err != nil {
		t.Fatal(err)
	}
	next := f.track("next", 10_000)
	f.enqueue(room.ID, host.ID, unknown.ID, next.ID)

	f.advance(time.Duration(unknownDurationFallbackMs-1000) * time.Millisecond)
	if got := f.get(room.ID).Current.Item.TrackID; got != unknown.ID {
		t.Fatalf("the room moved on early: %s", got)
	}
	f.advance(2 * time.Second)
	if got := f.get(room.ID).Current.Item.TrackID; got != next.ID {
		t.Fatalf("the fallback window never elapsed: %s", got)
	}
}

// A room with somebody in it keeps its own time, socket or no socket: a browser
// that dropped its connection must not stop the music for the people in the
// room. (The case this replaces - a room nobody is listening to is left alone -
// is gone: a member with no socket is removed after the grace, and a room with
// no members is closed, so "nobody to play to" is not a state a live room is in.
// The queue is still never spent on an empty room, because an empty room is not
// a room.)
func TestServerKeepsTimeWithASocketDown(t *testing.T) {
	f := newFixture(t, nil)
	host := guest("host-1")
	room := f.create("party", ControlsEveryone, "", host)
	first := f.track("first", 30_000)
	second := f.track("second", 30_000)
	f.enqueue(room.ID, host.ID, first.ID, second.ID)

	f.advance(31 * time.Second)
	state := f.get(room.ID)
	if state.Current == nil {
		t.Fatal("a room with somebody in it never started")
	}
	// 30-second songs, 31 seconds on: the first one has finished and the room
	// has moved on by itself, with no socket ever registered.
	if got := state.Current.Item.TrackID; got != second.ID {
		t.Fatalf("the room did not move on by itself: %s", got)
	}
}

// A queue change carries the room's play order with it: the order is the
// server's to know, and a client that works one out for itself can come out with
// a different answer the moment a queue is edited - which is a client playing
// one song while the room plays another.
func TestAQueueChangeCarriesThePlayOrder(t *testing.T) {
	f := newFixture(t, nil)
	host := guest("host-1")
	room := f.create("party", ControlsEveryone, "", host)
	events, cancel := f.m.Subscribe()
	defer cancel()

	first := f.track("first", 60_000)
	second := f.track("second", 60_000)
	f.enqueue(room.ID, host.ID, first.ID, second.ID)

	event := waitEvent(t, events, EventQueueUpdated)
	data := event.Data.(map[string]any)
	master, ok := data["master"].([]QueueItem)
	if !ok {
		t.Fatalf("a queue change carried no play order: %+v", data)
	}
	if len(master) != 2 || master[0].TrackID != first.ID || master[1].TrackID != second.ID {
		t.Fatalf("the order sent is not the room's: %+v", master)
	}
}

// An open room nothing is playing in closes itself once it has been that way for
// the idle window.
func TestIdleRoomClosesItself(t *testing.T) {
	f := newFixture(t, nil)
	f.m.idleAfter = time.Minute
	host := guest("host-1")
	room := f.create("party", ControlsEveryone, "", host)
	f.attach(host.ID)

	f.advance(30 * time.Second)
	if _, err := f.m.Get(room.ID); err != nil {
		t.Fatalf("the room closed early: %v", err)
	}
	f.advance(31 * time.Second)
	if _, err := f.m.Get(room.ID); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("an idle room did not close itself: %v", err)
	}
}

// A paused song is a room going nowhere and closes like an idle one; a room that
// is playing is not idle and does not.
func TestPlayingRoomIsNotIdleButAPausedOneIs(t *testing.T) {
	f := newFixture(t, nil)
	f.m.idleAfter = time.Minute
	host := guest("host-1")
	room := f.create("party", ControlsEveryone, "", host)
	song := f.track("song", 600_000)
	f.attach(host.ID)
	f.enqueue(room.ID, host.ID, song.ID)

	f.advance(2 * time.Minute)
	if _, err := f.m.Get(room.ID); err != nil {
		t.Fatalf("a playing room closed itself: %v", err)
	}

	if _, err := f.m.Pause(room.ID, host.ID); err != nil {
		t.Fatal(err)
	}
	f.advance(59 * time.Second)
	if _, err := f.m.Get(room.ID); err != nil {
		t.Fatalf("a paused room closed early: %v", err)
	}
	f.advance(2 * time.Second)
	if _, err := f.m.Get(room.ID); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("a paused room did not close itself: %v", err)
	}
}

// Using a room puts its idle clock back: a room people are still filling is not
// one to close under them.
func TestUsingARoomPutsTheIdleClockBack(t *testing.T) {
	f := newFixture(t, nil)
	f.m.idleAfter = time.Minute
	host := guest("host-1")
	room := f.create("party", ControlsEveryone, "", host)
	f.attach(host.ID)
	song := f.track("song", 30_000)

	f.advance(50 * time.Second)
	f.enqueue(room.ID, host.ID, song.ID)
	f.advance(50 * time.Second)
	if _, err := f.m.Get(room.ID); err != nil {
		t.Fatalf("the room closed although somebody was still queueing: %v", err)
	}
	f.advance(time.Minute)
	if _, err := f.m.Get(room.ID); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("the room never closed after the idle window: %v", err)
	}
}

func TestPausedRoomPositionDoesNotAdvance(t *testing.T) {
	f := newFixture(t, nil)
	host := guest("host-1")
	room := f.create("party", ControlsEveryone, "", host)
	song := f.track("song", 30_000)
	f.attach(host.ID)
	f.enqueue(room.ID, host.ID, song.ID)
	f.advance(5 * time.Second)
	if _, err := f.m.Pause(room.ID, host.ID); err != nil {
		t.Fatal(err)
	}
	f.advance(10 * time.Minute)
	state := f.get(room.ID)
	if state.Current == nil || !state.Current.Paused || state.Current.PositionMs != 5_000 {
		t.Fatalf("paused room moved: %+v", state.Current)
	}
	if _, err := f.m.Resume(room.ID, host.ID); err != nil {
		t.Fatal(err)
	}
	f.advance(2 * time.Second)
	state = f.get(room.ID)
	if state.Current == nil || state.Current.PositionMs != 7_000 {
		t.Fatalf("resumed room position = %+v", state.Current)
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
	f.attach(host.ID)
	f.enqueue(room.ID, host.ID, track.ID)

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
	f.attach("carol")
	f.enqueue(open.ID, "carol", track2.ID)
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
	f.attach(host.ID)
	f.enqueue(room.ID, host.ID, track.ID, other.ID)

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
	f.attach(host.ID)
	f.enqueue(room.ID, host.ID, bad.ID, good.ID)

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
	// running in its place.
	snap = f.get(room.ID)
	if snap.Current == nil || snap.Current.Item.TrackID != good.ID || snap.Current.Paused {
		t.Fatalf("after auto-skip = %+v", snap.Current)
	}

	// A vote on nothing playing is refused.
	empty := f.create("empty", ControlsEveryone, "", guest("eve"))
	if _, err := f.m.Vote(context.Background(), empty.ID, "eve", 3); !errors.Is(err, ErrNoPlayback) {
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
	f.attach(host.ID)
	f.enqueue(room.ID, host.ID, track.ID)
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
	f.attach(host.ID)
	f.tick()
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

// A queued row carries the artist's name beside the id: the list shows the
// name under the title, and only the id can open the artist's page.
func TestAQueuedItemCarriesTheArtistNames(t *testing.T) {
	f := newFixture(t, nil)
	track := f.track("Xtal", 200_000)
	if err := f.db.SetTrackArtists(context.Background(), track.ID, []string{"Aphex Twin"}); err != nil {
		t.Fatalf("SetTrackArtists: %v", err)
	}
	room := f.create("room", ControlsEveryone, "", guest("kyle"))
	state := f.enqueue(room.ID, "kyle", track.ID)

	items := state.Queues["kyle"]
	if len(items) != 1 {
		t.Fatalf("queued %d items, want 1", len(items))
	}
	got := items[0]
	if len(got.Artists) != 1 || got.Artists[0] != "Aphex Twin" {
		t.Errorf("artists = %v, want [Aphex Twin]", got.Artists)
	}
	if len(got.ArtistIDs) != 1 {
		t.Errorf("artistIds = %v, want the id the menu opens", got.ArtistIDs)
	}
}
