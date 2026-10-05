package rooms

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/musoak/internal/config"
	"codeberg.org/kyleraykbs/musoak/internal/match"
	"codeberg.org/kyleraykbs/musoak/internal/provider"
	"codeberg.org/kyleraykbs/musoak/internal/ranking"
	"codeberg.org/kyleraykbs/musoak/internal/store"
)

// manualClock drives the room timeline in tests: nothing sleeps.
type manualClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*manualTimer
}

type manualTimer struct {
	mu      sync.Mutex
	at      time.Time
	fn      func()
	stopped bool
	done    bool
}

func newManualClock() *manualClock {
	return &manualClock{now: time.Unix(1_700_000_000, 0)}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) AfterFunc(d time.Duration, fn func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &manualTimer{at: c.now.Add(d), fn: fn}
	c.timers = append(c.timers, timer)
	return timer
}

// Advance moves time forward and fires everything that became due.
func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	var due []func()
	live := c.timers[:0]
	for _, timer := range c.timers {
		if fn, ok := timer.alarm(now); ok {
			due = append(due, fn)
			continue
		}
		if timer.live() {
			live = append(live, timer)
		}
	}
	c.timers = live
	c.mu.Unlock()

	for _, fn := range due {
		fn()
	}
}

func (t *manualTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped || t.done {
		return false
	}
	t.stopped = true
	return true
}

func (t *manualTimer) alarm(now time.Time) (func(), bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped || t.done || t.at.After(now) {
		return nil, false
	}
	t.done = true
	return t.fn, true
}

func (t *manualTimer) live() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.stopped && !t.done
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
	registry := provider.NewRegistry(logger, time.Second)
	matcher := match.New(db, registry, cfg.Match.Threshold, logger)
	rank := ranking.New(db, cfg.DefaultProviderOrder, logger)
	manager := NewManager(cfg, db, matcher, rank, logger)
	clock := newManualClock()
	manager.SetClock(clock)

	t.Cleanup(func() {
		manager.Close()
		_ = db.Close()
	})
	return &fixture{t: t, m: manager, clock: clock, db: db, cfg: cfg}
}

type variantSpec struct {
	provider        string
	providerTrackID string
	durationMs      int64
	downloadable    bool
}

// trackWithVariants creates a canonical track with one variant per spec.
func (f *fixture) trackWithVariants(title string, specs ...variantSpec) (*store.Track, []store.Variant) {
	f.t.Helper()
	ctx := context.Background()
	track := &store.Track{Title: title, DurationMs: 180_000}
	if err := f.db.CreateTrack(ctx, track); err != nil {
		f.t.Fatal(err)
	}
	for _, spec := range specs {
		variant := &store.Variant{
			TrackID:         track.ID,
			Provider:        spec.provider,
			ProviderTrackID: spec.providerTrackID,
			Title:           title,
			Artists:         []string{"Artist"},
			DurationMs:      spec.durationMs,
			Downloadable:    spec.downloadable,
		}
		if err := f.db.CreateVariant(ctx, variant); err != nil {
			f.t.Fatal(err)
		}
	}
	variants, err := f.db.VariantsForTrack(ctx, track.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return track, variants
}

// assigned waits for the room to hand this member a rendition. Preparing runs
// off the enqueue, so the snapshot is empty for a moment after it.
func (f *fixture) assigned(roomID, memberID string) string {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if current := f.get(roomID).Current; current != nil {
			if id := current.Variants[memberID]; id != "" {
				return id
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	f.t.Fatal("the room never assigned a rendition")
	return ""
}

// enqueue adds a track and waits for the room to assign renditions when the
// track is the one being prepared. Assignment runs off the lock, and a track
// does not start without it, so a test that reports readiness has to let it
// finish first.
func (f *fixture) enqueue(ctx context.Context, roomID, memberID string, trackID uuid.UUID) (*Snapshot, error) {
	f.t.Helper()
	snapshot, err := f.m.Enqueue(ctx, roomID, memberID, trackID)
	if err != nil {
		return snapshot, err
	}
	if current := f.get(roomID).Current; current == nil || current.Item.TrackID != trackID {
		return snapshot, nil // queued behind something else: nothing is being prepared
	}
	f.prepared(roomID)
	return snapshot, nil
}

// prepared waits for the renditions of the item the room is preparing. The
// assignment runs off the lock, and a track does not start without it.
func (f *fixture) prepared(roomID string) {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if current := f.get(roomID).Current; current != nil && current.Prepared {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	f.t.Fatal("the room never prepared the track")
}

func (f *fixture) current(snapshot *Snapshot) *PlaybackView {
	f.t.Helper()
	if snapshot.Current == nil {
		f.t.Fatal("room has no current playback")
	}
	return snapshot.Current
}

func (f *fixture) get(roomID string) *Snapshot {
	f.t.Helper()
	snapshot, err := f.m.Get(roomID)
	if err != nil {
		f.t.Fatalf("Get: %v", err)
	}
	return snapshot
}

// track creates one canonical track with a single local rendition.
func (f *fixture) track(title string) *store.Track {
	f.t.Helper()
	track, _ := f.trackWithVariants(title, variantSpec{
		provider: "local", providerTrackID: title, durationMs: 180_000, downloadable: true,
	})
	return track
}

func queueTitles(items []QueueItem) []string {
	titles := make([]string, 0, len(items))
	for _, item := range items {
		titles = append(titles, item.Title)
	}
	return titles
}

// TestThreeClientsStaySynchronizedAndSkipOnVotes is the Block 9 acceptance
// test: three clients with renditions of 180s, 179s and 182s start together on
// the host's word, the room runs as long as the host's copy, a vote-driven skip
// advances the room, and the next track plays to its end.
func TestThreeClientsStaySynchronizedAndSkipOnVotes(t *testing.T) {
	f := newFixture(t, func(cfg *config.Config) {
		// Every member must have voted before the skip can fire, so the test
		// can vote one by one.
		cfg.ListenTogether.MinVotersForSkip = 3
	})
	ctx := context.Background()

	events, cancel := f.m.Subscribe()
	defer cancel()

	track1, variants := f.trackWithVariants("Song One",
		variantSpec{provider: "local", providerTrackID: "a", durationMs: 180_000, downloadable: true},
		variantSpec{provider: "ytmusic", providerTrackID: "b", durationMs: 179_000, downloadable: true},
		variantSpec{provider: "other", providerTrackID: "c", durationMs: 182_000, downloadable: true},
	)
	track2, variants2 := f.trackWithVariants("Song Two",
		variantSpec{provider: "local", providerTrackID: "d", durationMs: 180_000, downloadable: true},
	)

	host := Member{ID: "host", Name: "Host"}
	snapshot, err := f.m.Create("party", ControlsEveryone, "", host)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	roomID := snapshot.ID

	clients := []struct {
		id         string
		variantID  uuid.UUID
		durationMs int64
	}{
		{id: "host", variantID: variants[0].ID, durationMs: 180_000},
		{id: "second", variantID: variants[1].ID, durationMs: 179_000},
		{id: "third", variantID: variants[2].ID, durationMs: 182_000},
	}
	for _, client := range clients[1:] {
		if _, err := f.m.Join(roomID, Member{ID: client.id, Name: client.id}, "", ""); err != nil {
			t.Fatalf("Join: %v", err)
		}
	}

	if _, err := f.enqueue(ctx, roomID, host.ID, track1.ID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := f.enqueue(ctx, roomID, host.ID, track2.ID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Nothing starts until the host's file is here: the host is the room's
	// clock, and the room waits for them rather than for everybody.
	pending := f.get(roomID)
	if f.current(pending).StartedAtMs != 0 {
		t.Fatal("track started before the host was ready")
	}
	if len(f.current(pending).Awaiting) != 1 || f.current(pending).Awaiting[0] != host.ID {
		t.Fatalf("awaiting = %v, want the host alone", f.current(pending).Awaiting)
	}

	// The other two have not reported at all; the host's word is enough.
	if _, err := f.m.Ready(roomID, host.ID, track1.ID, variants[0].ID, 180_000); err != nil {
		t.Fatalf("Ready(host): %v", err)
	}

	started := f.get(roomID)
	current := f.current(started)
	if current.Item.TrackID != track1.ID {
		t.Fatalf("current item = %s, want track one", current.Item.TrackID)
	}
	if current.StartedAtMs == 0 {
		t.Fatal("track did not start once the host was ready")
	}
	if want := f.clock.Now().UnixMilli(); current.StartedAtMs != want {
		t.Errorf("startedAt = %d, want %d", current.StartedAtMs, want)
	}
	// The host's copy sets the length: 180s, whatever the others hold.
	if current.TimelineMs != 180_000 {
		t.Errorf("timeline = %d, want the host's rendition (180000)", current.TimelineMs)
	}
	if current.PositionMs != 0 {
		t.Errorf("position = %d, want 0 at start", current.PositionMs)
	}
	// The two who have not reported are catching up, not being waited for.
	if len(current.Awaiting) != 0 {
		t.Errorf("awaiting = %v, want nobody", current.Awaiting)
	}
	if len(current.CatchingUp) != 2 {
		t.Errorf("catchingUp = %v, want the two who have not reported", current.CatchingUp)
	}

	// Everybody's song ends when the host's does.
	hostEndsAt := current.StartedAtMs + 180_000
	roomEndsAt := current.StartedAtMs + current.TimelineMs
	if hostEndsAt != roomEndsAt {
		t.Fatalf("the room's end should be the host's file: %d vs %d", hostEndsAt, roomEndsAt)
	}

	// Midway, and before the host's file runs out, so what advances the room
	// next is the vote and not the end of the track.
	f.clock.Advance(100 * time.Second)
	midway := f.get(roomID)
	if midway.Current.Item.TrackID != track1.ID {
		t.Fatal("room advanced before the timeline ended")
	}
	if midway.Current.PositionMs != 100_000 {
		t.Errorf("position = %d, want 100000", midway.Current.PositionMs)
	}

	// A vote-driven skip: every member votes, the mean is below the threshold.
	scores := []int{1, 1, 2}
	for i, client := range clients {
		if _, err := f.m.Vote(ctx, roomID, client.id, scores[i]); err != nil {
			t.Fatalf("Vote(%s, %d): %v", client.id, scores[i], err)
		}
	}

	skipped := f.get(roomID)
	after := f.current(skipped)
	if after.Item.TrackID != track2.ID {
		t.Fatalf("current item = %s, want the next track after the skip", after.Item.TrackID)
	}
	if after.StartedAtMs != 0 {
		t.Error("the next track must wait for readiness")
	}
	if skipped.Queue == nil || len(skipped.Queue) != 0 {
		t.Errorf("queue = %v, want empty", skipped.Queue)
	}

	// Votes outlive the room, for later stats.
	count, mean, err := f.db.TrackVoteStats(ctx, track1.ID)
	if err != nil {
		t.Fatalf("TrackVoteStats: %v", err)
	}
	if count != 3 {
		t.Errorf("stored votes = %d, want 3", count)
	}
	if mean < 1.32 || mean > 1.34 {
		t.Errorf("mean = %v, want ~1.333", mean)
	}

	// The next track runs to its end and the room goes idle. Its renditions are
	// assigned off the lock, and it does not start until they are.
	f.prepared(roomID)
	variant2 := variants2[0]
	for _, client := range clients {
		if _, err := f.m.Ready(roomID, client.id, track2.ID, variant2.ID, 180_000); err != nil {
			t.Fatalf("Ready: %v", err)
		}
	}
	second := f.current(f.get(roomID))
	if second.Item.TrackID != track2.ID || second.StartedAtMs == 0 {
		t.Fatalf("second track did not start: %+v", second)
	}
	if second.TimelineMs != 180_000 {
		t.Errorf("timeline = %d, want the only rendition (180000)", second.TimelineMs)
	}
	f.clock.Advance(180 * time.Second)

	idle := f.get(roomID)
	if idle.Current != nil {
		t.Fatalf("room still has a current track: %+v", idle.Current)
	}

	// The event stream told the whole story.
	seen := map[EventType]int{}
	var skipReason string
	for {
		select {
		case event := <-events:
			seen[event.Type]++
			if event.Type == EventTrackSkipped {
				if data, ok := event.Data.(map[string]any); ok {
					skipReason, _ = data["reason"].(string)
				}
			}
			continue
		default:
		}
		break
	}
	if seen[EventMemberJoined] != 3 {
		t.Errorf("member_joined events = %d, want 3", seen[EventMemberJoined])
	}
	if seen[EventTrackStarted] != 2 {
		t.Errorf("track_started events = %d, want 2", seen[EventTrackStarted])
	}
	if seen[EventTrackSkipped] != 1 || skipReason != "votes" {
		t.Errorf("track_skipped events = %d, reason = %q", seen[EventTrackSkipped], skipReason)
	}
}

// TestRoomLengthIsTheHostsFile is the room's length: as long as the host's copy
// of the track. The host is the room's clock, so a member holding a shorter or
// longer copy follows them rather than moving the room.
func TestRoomLengthIsTheHostsFile(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()

	track, variants := f.trackWithVariants("Comforter",
		variantSpec{provider: "local", providerTrackID: "a", durationMs: 254_000, downloadable: true},
		variantSpec{provider: "ytmusic", providerTrackID: "b", durationMs: 431_000, downloadable: true},
	)

	// The host holds the longer copy and the other member the shorter one: the
	// room runs on the host's 431s, not the shorter 254s.
	snapshot, err := f.m.Create("party", ControlsEveryone, "", Member{ID: "host", Name: "Host"})
	if err != nil {
		t.Fatal(err)
	}
	roomID := snapshot.ID
	if _, err := f.m.Join(roomID, Member{ID: "second", Name: "Second"}, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.enqueue(ctx, roomID, "host", track.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Ready(roomID, "host", track.ID, variants[1].ID, 431_000); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Ready(roomID, "second", track.ID, variants[0].ID, 254_000); err != nil {
		t.Fatal(err)
	}

	current := f.current(f.get(roomID))
	if current.StartedAtMs == 0 {
		t.Fatal("the track did not start")
	}
	if current.TimelineMs != 431_000 {
		t.Fatalf("timeline = %d, want the host's file (431000), not the other member's 254000", current.TimelineMs)
	}

	// The host switching to a shorter copy moves the end of the track, and the
	// room says so, so every client's clock follows.
	events, cancel := f.m.Subscribe()
	defer cancel()
	if _, err := f.m.Ready(roomID, "host", track.ID, variants[0].ID, 100_000); err != nil {
		t.Fatal(err)
	}
	if got := f.current(f.get(roomID)).TimelineMs; got != 100_000 {
		t.Fatalf("timeline = %d, want the switched file (100000)", got)
	}
	deadline := time.After(5 * time.Second)
	announced := false
	for !announced {
		select {
		case event := <-events:
			if event.Type != EventReadyState {
				continue
			}
			data, ok := event.Data.(map[string]any)
			if !ok {
				continue
			}
			if got, ok := data["timelineMs"].(int64); ok && got == 100_000 {
				announced = true
			}
		case <-deadline:
			t.Fatal("the room never announced the new length")
		}
	}

	// Another member leaving does not move the host's end.
	if _, err := f.m.Leave(roomID, "second"); err != nil {
		t.Fatal(err)
	}
	if got := f.current(f.get(roomID)).TimelineMs; got != 100_000 {
		t.Fatalf("timeline = %d, want the host's switched file (100000)", got)
	}
}

func TestRoomLengthBeforeTheQueuerMeasures(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()

	kyle := &store.User{Username: "kyle", PasswordHash: "x"}
	if err := f.db.CreateUser(ctx, kyle); err != nil {
		t.Fatal(err)
	}
	// Their own copy of the song is the shorter one, and their order says so.
	if err := f.db.SetRanking(ctx, kyle.ID, []string{"local"}); err != nil {
		t.Fatal(err)
	}
	track, variants := f.trackWithVariants("Comforter",
		variantSpec{provider: "local", providerTrackID: "a", durationMs: 254_000, downloadable: true},
		variantSpec{provider: "ytmusic", providerTrackID: "b", durationMs: 431_000, downloadable: true},
	)

	snapshot, err := f.m.Create("party", ControlsEveryone, "", Member{ID: "host", Name: "Kyle", UserID: &kyle.ID})
	if err != nil {
		t.Fatal(err)
	}
	roomID := snapshot.ID
	if _, err := f.m.Join(roomID, Member{ID: "second", Name: "Second"}, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.enqueue(ctx, roomID, "host", track.ID); err != nil {
		t.Fatal(err)
	}

	// The other member's file is measured; the queuer's is not yet.
	if _, err := f.m.Ready(roomID, "second", track.ID, variants[1].ID, 431_000); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Ready(roomID, "host", track.ID, variants[0].ID, 0); err != nil {
		t.Fatal(err)
	}

	current := f.current(f.get(roomID))
	if current.StartedAtMs == 0 {
		t.Fatal("the track did not start")
	}
	if current.TimelineMs != 254_000 {
		t.Fatalf("timeline = %d, want the host's rendition (254000), not the other member's 431000", current.TimelineMs)
	}
	// A member who has reported is carried by the report, not by the variants
	// map: that is what the view is telling clients.
	var plays uuid.UUID
	for _, report := range current.Ready {
		if report.MemberID == "host" {
			plays = report.VariantID
		}
	}
	if plays != variants[0].ID {
		t.Fatalf("host plays %s, want %s", plays, variants[0].ID)
	}
}

// TestHostSuccessionAndReturn: the room always has a host. The member who has
// been in it longest takes over when the host leaves, and the room's owner
// takes it back when they come home.
func TestHostSuccessionAndReturn(t *testing.T) {
	f := newFixture(t, nil)

	snapshot, err := f.m.Create("party", ControlsEveryone, "", Member{ID: "kyle", Name: "Kyle"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	roomID := snapshot.ID
	for _, id := range []string{"second", "third"} {
		if _, err := f.m.Join(roomID, Member{ID: id, Name: id}, "", ""); err != nil {
			t.Fatalf("Join(%s): %v", id, err)
		}
	}

	// The host leaves: whoever has been in the room longest takes over. second
	// joined before third, and that is the whole of the rule.
	after, err := f.m.Leave(roomID, "kyle")
	if err != nil {
		t.Fatalf("Leave: %v", err)
	}
	if after.Host != "second" {
		t.Fatalf("host = %q, want the longest-standing member", after.Host)
	}

	// Somebody arriving later does not displace them.
	if _, err := f.m.Join(roomID, Member{ID: "fourth"}, "", ""); err != nil {
		t.Fatalf("Join: %v", err)
	}
	if got := f.get(roomID).Host; got != "second" {
		t.Fatalf("host = %q, want it to stay with the longest-standing member", got)
	}

	// The owner comes back, and the room is theirs again - without the member
	// who held it in the meantime being dropped.
	back, err := f.m.Join(roomID, Member{ID: "kyle", Name: "Kyle"}, "", "")
	if err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	if back.Host != "kyle" {
		t.Fatalf("host = %q, want the owner back in charge", back.Host)
	}
	found := false
	for _, member := range back.Members {
		if member.ID == "second" {
			found = true
		}
	}
	if !found {
		t.Error("the member who led the room was dropped when the owner returned")
	}
}

// TestHostFollowsTheAccount: a member id belongs to a browser, so a host who
// reloads or opens another tab arrives as somebody new. The room belongs to the
// account, and the account leads it.
func TestHostFollowsTheAccount(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()

	kyle := &store.User{Username: "kyle", PasswordHash: "x"}
	if err := f.db.CreateUser(ctx, kyle); err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.m.Create("party", ControlsEveryone, "", Member{ID: "tab-one", Name: "Kyle", UserID: &kyle.ID})
	if err != nil {
		t.Fatal(err)
	}
	roomID := snapshot.ID
	if _, err := f.m.Join(roomID, Member{ID: "guest"}, "", ""); err != nil {
		t.Fatal(err)
	}

	// The same account, a new tab: it replaces the old member and leads.
	rejoined, err := f.m.Join(roomID, Member{ID: "tab-two", Name: "Kyle", UserID: &kyle.ID}, "", "tab-one")
	if err != nil {
		t.Fatal(err)
	}
	if rejoined.Host != "tab-two" {
		t.Fatalf("host = %q, want the account's new tab", rejoined.Host)
	}
	for _, member := range rejoined.Members {
		if member.ID == "tab-one" {
			t.Error("the superseded tab is still a member")
		}
	}

	// A guest in the room is not the owner, and joining does not take the room
	// from them.
	guest, err := f.m.Join(roomID, Member{ID: "guest"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if guest.Host != "tab-two" {
		t.Fatalf("host = %q, want the account still leading", guest.Host)
	}
}

// TestTheHostsWordStartsTheRoom: the host is the room's clock, so the room
// begins on their word and everybody else catches up to wherever it has got to.
func TestTheHostsWordStartsTheRoom(t *testing.T) {
	f := newFixture(t, func(cfg *config.Config) {
		cfg.ListenTogether.ReadyTimeoutSeconds = 30
	})
	ctx := context.Background()

	track, variants := f.trackWithVariants("Song",
		variantSpec{provider: "local", providerTrackID: "a", durationMs: 200_000, downloadable: true},
	)

	snapshot, err := f.m.Create("party", ControlsEveryone, "", Member{ID: "host"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Join(snapshot.ID, Member{ID: "laggard"}, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.enqueue(ctx, snapshot.ID, "host", track.ID); err != nil {
		t.Fatal(err)
	}

	// The host's file is the one the room waits for, and nobody else's.
	before := f.current(f.get(snapshot.ID))
	if len(before.Awaiting) != 1 || before.Awaiting[0] != "host" {
		t.Fatalf("awaiting = %v, want the host alone", before.Awaiting)
	}

	if _, err := f.m.Ready(snapshot.ID, "host", track.ID, variants[0].ID, 200_000); err != nil {
		t.Fatal(err)
	}

	current := f.current(f.get(snapshot.ID))
	if current.StartedAtMs == 0 {
		t.Fatal("the host's readiness did not start the track")
	}
	if current.TimelineMs != 200_000 {
		t.Errorf("timeline = %d, want the host's rendition", current.TimelineMs)
	}
	// The laggard is not being waited for: they join wherever the song is.
	if len(current.CatchingUp) != 1 || current.CatchingUp[0] != "laggard" {
		t.Errorf("catchingUp = %v, want [laggard]", current.CatchingUp)
	}

	// A late report turns a laggard into a normal member.
	if _, err := f.m.Ready(snapshot.ID, "laggard", track.ID, variants[0].ID, 200_000); err != nil {
		t.Fatal(err)
	}
	if len(f.current(f.get(snapshot.ID)).CatchingUp) != 0 {
		t.Error("catching up member still listed as lagging")
	}
}

// TestReadinessTimeoutStartsWithoutLaggards: with the host sitting the song out
// there is nobody leading it, so the room is the room again, and the window is
// what ends the wait for the members who have not reported.
func TestReadinessTimeoutStartsWithoutLaggards(t *testing.T) {
	f := newFixture(t, func(cfg *config.Config) {
		cfg.ListenTogether.ReadyTimeoutSeconds = 30
	})
	ctx := context.Background()

	track, variants := f.trackWithVariants("Song",
		variantSpec{provider: "local", providerTrackID: "a", durationMs: 200_000, downloadable: true},
	)

	snapshot, err := f.m.Create("party", ControlsEveryone, "", Member{ID: "host"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Join(snapshot.ID, Member{ID: "second"}, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Join(snapshot.ID, Member{ID: "third"}, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.enqueue(ctx, snapshot.ID, "host", track.ID); err != nil {
		t.Fatal(err)
	}
	// The host is not playing this one, so nothing leads the room - and one of
	// the two left is not a quorum either.
	if _, err := f.m.SetOut(snapshot.ID, "host", true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Ready(snapshot.ID, "second", track.ID, variants[0].ID, 200_000); err != nil {
		t.Fatal(err)
	}

	before := f.get(snapshot.ID)
	if f.current(before).StartedAtMs != 0 {
		t.Fatal("track started before the readiness timeout")
	}
	if len(f.current(before).Awaiting) != 1 || f.current(before).Awaiting[0] != "third" {
		t.Fatalf("awaiting = %v, want the member who has not reported", f.current(before).Awaiting)
	}

	f.clock.Advance(30 * time.Second)

	after := f.get(snapshot.ID)
	current := f.current(after)
	if current.StartedAtMs == 0 {
		t.Fatal("the readiness timeout did not start the track")
	}
	if current.TimelineMs != 200_000 {
		t.Errorf("timeline = %d, want the one reported rendition", current.TimelineMs)
	}
}

func TestSkipRule(t *testing.T) {
	tests := []struct {
		name     string
		members  int
		votes    []int
		wantSkip bool
		mutate   func(*config.Config)
	}{
		{name: "mean below threshold with enough voters", members: 3, votes: []int{1, 2}, wantSkip: true},
		{name: "mean equal to threshold is not below", members: 3, votes: []int{2, 2}, wantSkip: false},
		{name: "high scores keep the track", members: 3, votes: []int{4, 5, 3}, wantSkip: false},
		{name: "too few voters", members: 6, votes: []int{1}, wantSkip: false},
		{name: "fraction of the room too small", members: 6, votes: []int{1, 1}, wantSkip: false},
		{name: "neutral counts towards the mean", members: 3, votes: []int{1, 3, 3}, wantSkip: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, func(cfg *config.Config) {
				cfg.ListenTogether.MinVotersForSkip = 2
				cfg.ListenTogether.VoterFractionForSkip = 0.5
				cfg.ListenTogether.SkipThreshold = 2.0
				if tt.mutate != nil {
					tt.mutate(cfg)
				}
			})
			ctx := context.Background()

			track, variants := f.trackWithVariants("Song",
				variantSpec{provider: "local", providerTrackID: "a", durationMs: 300_000, downloadable: true},
			)
			next, _ := f.trackWithVariants("Next",
				variantSpec{provider: "local", providerTrackID: "b", durationMs: 300_000, downloadable: true},
			)

			snapshot, err := f.m.Create("party", ControlsEveryone, "", Member{ID: "m0"})
			if err != nil {
				t.Fatal(err)
			}
			members := []string{"m0"}
			for i := 1; i < tt.members; i++ {
				id := "m" + string(rune('0'+i))
				if _, err := f.m.Join(snapshot.ID, Member{ID: id}, "", ""); err != nil {
					t.Fatal(err)
				}
				members = append(members, id)
			}
			if _, err := f.enqueue(ctx, snapshot.ID, "m0", track.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.enqueue(ctx, snapshot.ID, "m0", next.ID); err != nil {
				t.Fatal(err)
			}
			for _, id := range members {
				if _, err := f.m.Ready(snapshot.ID, id, track.ID, variants[0].ID, 300_000); err != nil {
					t.Fatal(err)
				}
			}

			for _, score := range tt.votes {
				if _, err := f.m.Vote(ctx, snapshot.ID, members[len(f.current(f.get(snapshot.ID)).Votes)], score); err != nil {
					t.Fatalf("Vote: %v", err)
				}
			}

			current := f.current(f.get(snapshot.ID))
			skipped := current.Item.TrackID == next.ID
			if skipped != tt.wantSkip {
				t.Errorf("skipped = %v, want %v (votes %v, members %d)", skipped, tt.wantSkip, tt.votes, tt.members)
			}
		})
	}
}

func TestHostOnlyControls(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()

	track, variants := f.trackWithVariants("Song",
		variantSpec{provider: "local", providerTrackID: "a", durationMs: 300_000, downloadable: true},
	)
	snapshot, err := f.m.Create("party", ControlsHost, "", Member{ID: "host"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Join(snapshot.ID, Member{ID: "guest"}, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.enqueue(ctx, snapshot.ID, "guest", track.ID); err != nil {
		t.Fatalf("anyone may enqueue: %v", err)
	}
	if _, err := f.m.Ready(snapshot.ID, "host", track.ID, variants[0].ID, 300_000); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Ready(snapshot.ID, "guest", track.ID, variants[0].ID, 300_000); err != nil {
		t.Fatal(err)
	}

	if _, err := f.m.Pause(snapshot.ID, "guest"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("guest pause: err = %v, want ErrForbidden", err)
	}

	paused, err := f.m.Pause(snapshot.ID, "host")
	if err != nil {
		t.Fatalf("host pause: %v", err)
	}
	if !f.current(paused).Paused || f.current(paused).PositionMs != 0 {
		t.Fatalf("paused state = %+v", f.current(paused))
	}

	// Time passes; a paused room does not move and does not advance.
	f.clock.Advance(60 * time.Second)
	stillPaused := f.get(snapshot.ID)
	if !f.current(stillPaused).Paused || f.current(stillPaused).PositionMs != 0 {
		t.Errorf("paused position moved: %+v", f.current(stillPaused))
	}

	resumed, err := f.m.Resume(snapshot.ID, "host")
	if err != nil {
		t.Fatalf("host resume: %v", err)
	}
	if f.current(resumed).Paused {
		t.Error("room still paused after resume")
	}
	f.clock.Advance(10 * time.Second)
	if got := f.current(f.get(snapshot.ID)).PositionMs; got != 10_000 {
		t.Errorf("position after resume = %d, want 10000", got)
	}

	// Seek moves the room and reschedules its end.
	if _, err := f.m.Seek(snapshot.ID, "host", 299_500); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	f.clock.Advance(time.Second)
	after := f.get(snapshot.ID)
	if after.Current != nil {
		t.Errorf("seeking past the end should advance the room: %+v", f.current(after))
	}
}

func TestJoiningDuringPlaybackAssignsARendition(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()

	track, variants := f.trackWithVariants("Song",
		variantSpec{provider: "local", providerTrackID: "a", durationMs: 300_000, downloadable: true},
	)

	snapshot, err := f.m.Create("party", ControlsEveryone, "", Member{ID: "host"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.enqueue(ctx, snapshot.ID, "host", track.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Ready(snapshot.ID, "host", track.ID, variants[0].ID, 300_000); err != nil {
		t.Fatal(err)
	}
	if current := f.current(f.get(snapshot.ID)); current.StartedAtMs == 0 {
		t.Fatal("the track did not start for the host alone")
	}

	// A member that arrives while the track plays must still be told what to
	// play, rather than sitting in the room with nothing assigned.
	events, cancel := f.m.Subscribe()
	defer cancel()

	if _, err := f.m.Join(snapshot.ID, Member{ID: "late"}, "", ""); err != nil {
		t.Fatalf("Join: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for {
		select {
		case event := <-events:
			if event.Type != EventTrackPrepared {
				continue
			}
			data, ok := event.Data.(map[string]any)
			if !ok {
				continue
			}
			assignments, ok := data["variants"].(map[string]uuid.UUID)
			if !ok {
				t.Fatalf("track_prepared carried %T, want map[string]uuid.UUID", data["variants"])
			}
			assigned, ok := assignments["late"]
			if !ok {
				continue // an earlier prepare for the other members
			}
			if assigned != variants[0].ID {
				t.Fatalf("late joiner was assigned %s, want %s", assigned, variants[0].ID)
			}
			current := f.current(f.get(snapshot.ID))
			if current.Variants["late"] != assigned.String() {
				t.Errorf("the room does not show the late joiner's rendition: %+v", current.Variants)
			}
			return
		case <-deadline:
			t.Fatal("the late joiner never received a rendition assignment")
		}
	}
}

// TestRoomAssignsTheMembersOwnUpload is the reserved slot rule applied where a
// room decides a rendition. Both copies are downloadable and the provider's
// copy is the room's only other option, so the order is what decides: a member
// whose order puts their own upload first is handed that copy, and one whose
// order puts it last is handed the provider's.
func TestRoomAssignsTheMembersOwnUpload(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()

	kyle := &store.User{Username: "kyle", PasswordHash: "x"}
	if err := f.db.CreateUser(ctx, kyle); err != nil {
		t.Fatal(err)
	}
	track, variants := f.trackWithVariants("Song",
		variantSpec{provider: "ytmusic", providerTrackID: "o1", durationMs: 180_000, downloadable: true},
	)
	provider := variants[0]

	upload := &store.Upload{UserID: kyle.ID, Filename: "mine.opus"}
	mine := &store.Variant{TrackID: track.ID, Title: "Song", Artists: []string{"Artist"}, DurationMs: 180_000}
	media := &store.MediaFile{Path: "mine.opus", SHA256: "abc", DurationMs: 180_000, Bytes: 10}
	if err := f.db.CreateUpload(ctx, upload, mine, media); err != nil {
		t.Fatal(err)
	}

	if err := f.db.SetRanking(ctx, kyle.ID, []string{ranking.SlotSelf, "ytmusic"}); err != nil {
		t.Fatal(err)
	}

	snapshot, err := f.m.Create("party", ControlsEveryone, "", Member{ID: "kyle", UserID: &kyle.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.enqueue(ctx, snapshot.ID, "kyle", track.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.assigned(snapshot.ID, "kyle"); got != mine.ID.String() {
		t.Fatalf("assigned %s, want the member's own upload %s", got, mine.ID)
	}

	// The same room and the same track, with the order the other way round.
	if err := f.db.SetRanking(ctx, kyle.ID, []string{"ytmusic", ranking.SlotSelf}); err != nil {
		t.Fatal(err)
	}
	second, err := f.m.Create("party2", ControlsEveryone, "", Member{ID: "kyle", UserID: &kyle.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.enqueue(ctx, second.ID, "kyle", track.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.assigned(second.ID, "kyle"); got != provider.ID.String() {
		t.Fatalf("assigned %s, want the provider's copy %s", got, provider.ID)
	}
}

// TestSittingOutTakesAMemberOutOfTheRoomsLength: the room runs on the host's
// copy, so another member sitting the song out changes nothing about its length
// - what it changes is who the room waits for. A host who sits one out is the
// case that still moves the end of the track, because the room then falls back
// to the room as a whole.
func TestSittingOutTakesAMemberOutOfTheRoomsLength(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()

	track, variants := f.trackWithVariants("Comforter",
		variantSpec{provider: "local", providerTrackID: "a", durationMs: 254_000, downloadable: true},
		variantSpec{provider: "ytmusic", providerTrackID: "b", durationMs: 431_000, downloadable: true},
	)

	snapshot, err := f.m.Create("party", ControlsEveryone, "", Member{ID: "host", Name: "Host"})
	if err != nil {
		t.Fatal(err)
	}
	roomID := snapshot.ID
	if _, err := f.m.Join(roomID, Member{ID: "second", Name: "Second"}, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.enqueue(ctx, roomID, "host", track.ID); err != nil {
		t.Fatal(err)
	}

	// The short copy's owner steps out, and is not waited for.
	if _, err := f.m.SetOut(roomID, "second", true); err != nil {
		t.Fatal(err)
	}
	out := f.get(roomID)
	for _, member := range out.Members {
		if member.ID == "second" && !member.Out {
			t.Fatalf("members = %+v, want the second one marked out", out.Members)
		}
	}
	if _, err := f.m.Ready(roomID, "host", track.ID, variants[1].ID, 431_000); err != nil {
		t.Fatal(err)
	}

	current := f.current(f.get(roomID))
	if current.StartedAtMs == 0 {
		t.Fatal("the room waited for a member who is sitting it out")
	}
	if current.TimelineMs != 431_000 {
		t.Fatalf("timeline = %d, want the host's file (431000)", current.TimelineMs)
	}
	if len(current.Awaiting) != 0 {
		t.Errorf("awaiting = %v, want nobody", current.Awaiting)
	}

	// Back in, with a shorter copy: the host's end is still the host's.
	if _, err := f.m.SetOut(roomID, "second", false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Ready(roomID, "second", track.ID, variants[0].ID, 254_000); err != nil {
		t.Fatal(err)
	}
	if got := f.current(f.get(roomID)).TimelineMs; got != 431_000 {
		t.Fatalf("timeline = %d, want the host's file still (431000)", got)
	}

	// The host sits this one out: nobody leads it now, so the room falls back to
	// the shortest file it has left.
	if _, err := f.m.SetOut(roomID, "host", true); err != nil {
		t.Fatal(err)
	}
	if got := f.current(f.get(roomID)).TimelineMs; got != 254_000 {
		t.Fatalf("timeline = %d, want the shortest file left (254000)", got)
	}
}

func TestRoomClosesWhenEverybodyLeaves(t *testing.T) {
	f := newFixture(t, nil)

	snapshot, err := f.m.Create("party", ControlsEveryone, "", Member{ID: "host"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Join(snapshot.ID, Member{ID: "guest"}, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Leave(snapshot.ID, "host"); err != nil {
		t.Fatalf("Leave: %v", err)
	}

	remaining := f.get(snapshot.ID)
	if remaining.Host != "guest" {
		t.Errorf("host = %q, want the promoted guest", remaining.Host)
	}
	if _, err := f.m.Leave(snapshot.ID, "guest"); err != nil {
		t.Fatalf("last leave: %v (leaving a room that closes is still a success)", err)
	}
	if rooms := f.m.List(); len(rooms) != 0 {
		t.Errorf("rooms = %d, want none", len(rooms))
	}
	if _, err := f.m.Get(snapshot.ID); !errors.Is(err, ErrRoomNotFound) {
		t.Errorf("the closed room is still there: %v", err)
	}
}

// TestMasterQueueInterleavesMemberQueues is the per-member queue acceptance
// test: two members' own queues interleave fairly — one track per member per
// pass, in join order, each member's own order kept (A1 B1 A2 B2) — the
// master recomputes live as either edits, and remove/reorder/clear only ever
// touch the caller's queue.
func TestMasterQueueInterleavesMemberQueues(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()

	events, cancel := f.m.Subscribe()
	defer cancel()

	snapshot, err := f.m.Create("party", ControlsEveryone, "", Member{ID: "a", Name: "A"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	roomID := snapshot.ID
	if _, err := f.m.Join(roomID, Member{ID: "b", Name: "B"}, "", ""); err != nil {
		t.Fatalf("Join: %v", err)
	}

	tracks := map[string]*store.Track{}
	for _, title := range []string{"A1", "A2", "B1", "B2", "B3"} {
		tracks[title] = f.track(title)
	}
	enqueue := func(member, title string) {
		f.t.Helper()
		if _, err := f.enqueue(ctx, roomID, member, tracks[title].ID); err != nil {
			f.t.Fatalf("Enqueue(%s %s): %v", member, title, err)
		}
	}

	enqueue("a", "A1") // the room takes A1 into playback right away
	enqueue("a", "A2")
	enqueue("b", "B1")
	enqueue("b", "B2")

	state := f.get(roomID)
	if got, want := queueTitles(state.MasterQueue), []string{"A1", "B1", "A2", "B2"}; !slices.Equal(got, want) {
		t.Errorf("masterQueue = %v, want %v", got, want)
	}
	if got, want := queueTitles(state.Queues["a"]), []string{"A1", "A2"}; !slices.Equal(got, want) {
		t.Errorf("a's queue = %v, want %v", got, want)
	}
	if got, want := queueTitles(state.Queues["b"]), []string{"B1", "B2"}; !slices.Equal(got, want) {
		t.Errorf("b's queue = %v, want %v", got, want)
	}
	// The room plays masterQueue: its head is what is on.
	if got := f.current(state).Item.Title; got != "A1" {
		t.Errorf("current = %s, want A1", got)
	}
	// "queue" keeps its old meaning: the play order after the current track.
	if got, want := queueTitles(state.Queue), []string{"B1", "A2", "B2"}; !slices.Equal(got, want) {
		t.Errorf("queue = %v, want %v", got, want)
	}

	// B reorders their own queue: only B's order moves, and the master mix
	// recomputes around it.
	bItems := state.Queues["b"]
	reordered, err := f.m.Reorder(roomID, "b", []string{bItems[1].ID, bItems[0].ID})
	if err != nil {
		t.Fatalf("Reorder: %v", err)
	}
	if got, want := queueTitles(reordered.Queues["b"]), []string{"B2", "B1"}; !slices.Equal(got, want) {
		t.Errorf("b's queue after reorder = %v, want %v", got, want)
	}
	if got, want := queueTitles(reordered.Queues["a"]), []string{"A1", "A2"}; !slices.Equal(got, want) {
		t.Errorf("a's queue after b's reorder = %v, want %v", got, want)
	}
	if got, want := queueTitles(reordered.MasterQueue), []string{"A1", "B2", "A2", "B1"}; !slices.Equal(got, want) {
		t.Errorf("masterQueue after reorder = %v, want %v", got, want)
	}

	// B drops one of their own items; A's queue does not move.
	removed, err := f.m.Remove(roomID, "b", reordered.Queues["b"][0].ID) // B2
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got, want := queueTitles(removed.Queues["b"]), []string{"B1"}; !slices.Equal(got, want) {
		t.Errorf("b's queue after remove = %v, want %v", got, want)
	}
	if got, want := queueTitles(removed.Queues["a"]), []string{"A1", "A2"}; !slices.Equal(got, want) {
		t.Errorf("a's queue after b's remove = %v, want %v", got, want)
	}
	if got, want := queueTitles(removed.MasterQueue), []string{"A1", "B1", "A2"}; !slices.Equal(got, want) {
		t.Errorf("masterQueue after remove = %v, want %v", got, want)
	}

	// B queues up again, then clears their own queue.
	enqueue("b", "B3")
	cleared, err := f.m.Clear(roomID, "b", "")
	if err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if len(cleared.Queues["b"]) != 0 {
		t.Errorf("b's queue after clear = %v, want empty", queueTitles(cleared.Queues["b"]))
	}
	if got, want := queueTitles(cleared.Queues["a"]), []string{"A1", "A2"}; !slices.Equal(got, want) {
		t.Errorf("a's queue after b's clear = %v, want %v", got, want)
	}
	if got, want := queueTitles(cleared.MasterQueue), []string{"A1", "A2"}; !slices.Equal(got, want) {
		t.Errorf("masterQueue after clear = %v, want %v", got, want)
	}

	// The host may act on any queue.
	enqueue("b", "B3")
	if _, err := f.m.Clear(roomID, "a", "b"); err != nil {
		t.Fatalf("host clear of b's queue: %v", err)
	}
	enqueue("b", "B2")
	withB := f.get(roomID)
	var b2 QueueItem
	for _, item := range withB.Queues["b"] {
		if item.Title == "B2" {
			b2 = item
		}
	}
	if _, err := f.m.Remove(roomID, "a", b2.ID); err != nil {
		t.Fatalf("host remove of b's item: %v", err)
	}
	if len(f.get(roomID).Queues["b"]) != 0 {
		t.Error("host remove did not empty b's queue")
	}

	// A member may not touch another member's queue.
	withA := f.get(roomID)
	aItemID := withA.Queues["a"][0].ID
	if _, err := f.m.Remove(roomID, "b", aItemID); !errors.Is(err, ErrForbidden) {
		t.Errorf("b removing a's item: err = %v, want ErrForbidden", err)
	}
	aOrder := make([]string, 0, len(withA.Queues["a"]))
	for i := len(withA.Queues["a"]) - 1; i >= 0; i-- {
		aOrder = append(aOrder, withA.Queues["a"][i].ID)
	}
	if _, err := f.m.Reorder(roomID, "b", aOrder); !errors.Is(err, ErrForbidden) {
		t.Errorf("b reordering a's queue: err = %v, want ErrForbidden", err)
	}
	if _, err := f.m.Clear(roomID, "b", "a"); !errors.Is(err, ErrForbidden) {
		t.Errorf("b clearing a's queue: err = %v, want ErrForbidden", err)
	}

	// queue_updated tells everyone both queues and the fair master mix.
	var last map[string]any
	for {
		select {
		case event := <-events:
			if event.Type == EventQueueUpdated {
				last, _ = event.Data.(map[string]any)
			}
			continue
		default:
		}
		break
	}
	if last == nil {
		t.Fatal("no queue_updated event was published")
	}
	queues, ok := last["queues"].(map[string][]QueueItem)
	if !ok {
		t.Fatalf("queue_updated carries %T for queues, want map[string][]QueueItem", last["queues"])
	}
	master, ok := last["masterQueue"].([]QueueItem)
	if !ok {
		t.Fatalf("queue_updated carries %T for masterQueue, want []QueueItem", last["masterQueue"])
	}
	if _, ok := last["queue"]; !ok {
		t.Error("queue_updated lost the queue key existing clients follow")
	}
	if got, want := queueTitles(queues["a"]), []string{"A1", "A2"}; !slices.Equal(got, want) {
		t.Errorf("event queues[a] = %v, want %v", got, want)
	}
	if len(queues["b"]) != 0 {
		t.Errorf("event queues[b] = %v, want empty", queueTitles(queues["b"]))
	}
	if got, want := queueTitles(master), []string{"A1", "A2"}; !slices.Equal(got, want) {
		t.Errorf("event masterQueue = %v, want %v", got, want)
	}
}

// TestRoomPlaysMasterQueueInOrder walks a room through its mix: the playback
// follows the master queue, one track per member per pass.
func TestRoomPlaysMasterQueueInOrder(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()

	snapshot, err := f.m.Create("party", ControlsEveryone, "", Member{ID: "a"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	roomID := snapshot.ID
	if _, err := f.m.Join(roomID, Member{ID: "b"}, "", ""); err != nil {
		t.Fatalf("Join: %v", err)
	}

	tracks := map[string]*store.Track{}
	for _, title := range []string{"A1", "A2", "B1", "B2"} {
		tracks[title] = f.track(title)
	}
	for _, enqueue := range []struct{ member, title string }{
		{"a", "A1"}, {"a", "A2"}, {"b", "B1"}, {"b", "B2"},
	} {
		if _, err := f.enqueue(ctx, roomID, enqueue.member, tracks[enqueue.title].ID); err != nil {
			t.Fatalf("Enqueue(%s %s): %v", enqueue.member, enqueue.title, err)
		}
	}

	var played []string
	for {
		state := f.get(roomID)
		if state.Current == nil {
			break
		}
		played = append(played, state.Current.Item.Title)
		if _, err := f.m.Skip(roomID, "a"); err != nil {
			t.Fatalf("Skip after %s: %v", played[len(played)-1], err)
		}
	}
	if want := []string{"A1", "B1", "A2", "B2"}; !slices.Equal(played, want) {
		t.Errorf("played = %v, want %v", played, want)
	}
}

// TestTransportEventsNameTheMember pins what a client needs to say "Sam
// paused": every transport event carries the member who drove it. The room's
// own decisions - a vote, a track running out - carry nobody.
func TestTransportEventsNameTheMember(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	events, cancel := f.m.Subscribe()
	defer cancel()

	snapshot, err := f.m.Create("party", ControlsEveryone, "", Member{ID: "kyle", Name: "Kyle"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	roomID := snapshot.ID
	if _, err := f.m.Join(roomID, Member{ID: "sam", Name: "Sam"}, "", ""); err != nil {
		t.Fatalf("Join: %v", err)
	}
	track, _ := f.trackWithVariants("Song",
		variantSpec{provider: "local", providerTrackID: "a", durationMs: 180_000, downloadable: true},
	)
	if _, err := f.enqueue(ctx, roomID, "kyle", track.ID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	for _, member := range []string{"kyle", "sam"} {
		variantID, err := uuid.Parse(f.assigned(roomID, member))
		if err != nil {
			t.Fatalf("assigned(%s): %v", member, err)
		}
		if _, err := f.m.Ready(roomID, member, track.ID, variantID, 180_000); err != nil {
			t.Fatalf("Ready(%s): %v", member, err)
		}
	}

	if _, err := f.m.Pause(roomID, "sam"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if _, err := f.m.Seek(roomID, "sam", 30_000); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if _, err := f.m.Skip(roomID, "sam"); err != nil {
		t.Fatalf("Skip: %v", err)
	}

	want := map[EventType]string{
		EventPaused:       "Sam",
		EventSeeked:       "Sam",
		EventTrackSkipped: "Sam",
	}
	seen := map[EventType]bool{}
	deadline := time.After(5 * time.Second)
	for len(seen) < len(want) {
		select {
		case event := <-events:
			name, ok := want[event.Type]
			if !ok {
				continue
			}
			data, _ := event.Data.(map[string]any)
			by, _ := data["by"].(map[string]any)
			if got, _ := by["name"].(string); got != name {
				t.Errorf("%s: by.name = %q, want %q", event.Type, got, name)
			}
			seen[event.Type] = true
		case <-deadline:
			t.Fatalf("saw %v of the transport events, want %v", seen, want)
		}
	}
}

// TestTheHostsEndMovesTheRoomOn: the host is the room's clock, so their file
// reaching its end is the song reaching its end - and it does not matter what
// length the room had worked out in advance. Anybody else's file ending says
// nothing.
func TestTheHostsEndMovesTheRoomOn(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()

	track, variants := f.trackWithVariants("Song",
		variantSpec{provider: "local", providerTrackID: "a", durationMs: 300_000, downloadable: true},
	)
	next, _ := f.trackWithVariants("Next",
		variantSpec{provider: "local", providerTrackID: "b", durationMs: 300_000, downloadable: true},
	)

	snapshot, err := f.m.Create("party", ControlsEveryone, "", Member{ID: "host"})
	if err != nil {
		t.Fatal(err)
	}
	roomID := snapshot.ID
	if _, err := f.m.Join(roomID, Member{ID: "guest"}, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.enqueue(ctx, roomID, "host", track.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.enqueue(ctx, roomID, "host", next.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Ready(roomID, "host", track.ID, variants[0].ID, 300_000); err != nil {
		t.Fatal(err)
	}
	if f.current(f.get(roomID)).StartedAtMs == 0 {
		t.Fatal("the track did not start")
	}

	// Somebody who is not the room's clock saying so changes nothing.
	if _, err := f.m.Ended(roomID, "guest", track.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.current(f.get(roomID)).Item.TrackID; got != track.ID {
		t.Fatalf("current = %s, want the same song: a guest's file ending says nothing", got)
	}

	// An end that arrives late names the song the client has already moved on
	// to. The room is playing something else, and a song is not over because a
	// file that is no longer playing has stopped: this is what cut songs short.
	if _, err := f.m.Ended(roomID, "host", next.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.current(f.get(roomID)).Item.TrackID; got != track.ID {
		t.Fatalf("current = %s, want the same song: that end was for another one", got)
	}

	// The host's own file ending, naming the song that is playing, is the end of
	// the song - well before the length the room worked out.
	if _, err := f.m.Ended(roomID, "host", track.ID); err != nil {
		t.Fatal(err)
	}
	after := f.get(roomID)
	if after.Current == nil || after.Current.Item.TrackID != next.ID {
		t.Fatalf("current = %+v, want the next song", after.Current)
	}
}

// TestRoomPassword gates joining. The password lives in the manager and is
// never part of any state clients see.
func TestRoomPassword(t *testing.T) {
	f := newFixture(t, nil)

	snapshot, err := f.m.Create("party", ControlsEveryone, "sesame", Member{ID: "host"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !snapshot.HasPassword {
		t.Error("a room with a password must report hasPassword")
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sesame") {
		t.Errorf("the snapshot leaks the password: %s", raw)
	}

	if _, err := f.m.Join(snapshot.ID, Member{ID: "guest"}, "open", ""); !errors.Is(err, ErrWrongPassword) {
		t.Errorf("join with the wrong password: err = %v, want ErrWrongPassword", err)
	}
	if _, err := f.m.Join(snapshot.ID, Member{ID: "guest"}, "", ""); !errors.Is(err, ErrWrongPassword) {
		t.Errorf("join without a password: err = %v, want ErrWrongPassword", err)
	}
	joined, err := f.m.Join(snapshot.ID, Member{ID: "guest"}, "sesame", "")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if joined.MemberCount != 2 {
		t.Errorf("memberCount = %d, want 2", joined.MemberCount)
	}

	open, err := f.m.Create("open", ControlsEveryone, "", Member{ID: "host"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if open.HasPassword {
		t.Error("a room without a password must not report hasPassword")
	}
	if _, err := f.m.Join(open.ID, Member{ID: "guest"}, "anything", ""); err != nil {
		t.Errorf("joining an open room: %v", err)
	}
}

// TestMemberEventsCarryMemberCount: member_joined and member_left announce how
// big the room is, and a member who leaves takes their queue with them.
func TestMemberEventsCarryMemberCount(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()

	events, cancel := f.m.Subscribe()
	defer cancel()

	snapshot, err := f.m.Create("party", ControlsEveryone, "", Member{ID: "host"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	roomID := snapshot.ID
	if _, err := f.m.Join(roomID, Member{ID: "guest"}, "", ""); err != nil {
		t.Fatalf("Join: %v", err)
	}

	track := f.track("Song")
	hostTrack := f.track("Host Song")
	if _, err := f.enqueue(ctx, roomID, "host", hostTrack.ID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := f.enqueue(ctx, roomID, "guest", track.ID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := f.m.Leave(roomID, "guest"); err != nil {
		t.Fatalf("Leave: %v", err)
	}

	state := f.get(roomID)
	if _, ok := state.Queues["guest"]; ok {
		t.Error("a member who left still has a queue")
	}
	if got, want := queueTitles(state.MasterQueue), []string{"Host Song"}; !slices.Equal(got, want) {
		t.Errorf("masterQueue = %v, want the leaver's items gone (%v)", got, want)
	}

	joined, left := map[int]int{}, map[int]int{}
	for {
		select {
		case event := <-events:
			data, _ := event.Data.(map[string]any)
			switch event.Type {
			case EventMemberJoined:
				if count, ok := data["memberCount"].(int); ok {
					joined[count]++
				} else {
					t.Errorf("member_joined carries no memberCount: %v", data)
				}
			case EventMemberLeft:
				if count, ok := data["memberCount"].(int); ok {
					left[count]++
				} else {
					t.Errorf("member_left carries no memberCount: %v", data)
				}
			}
			continue
		default:
		}
		break
	}
	if joined[1] != 1 || joined[2] != 1 {
		t.Errorf("member_joined memberCounts = %v, want one 1 and one 2", joined)
	}
	if left[1] != 1 {
		t.Errorf("member_left memberCounts = %v, want one 1", left)
	}
}

func TestDisconnectLeavesOnlyOnTheLastSocket(t *testing.T) {
	f := newFixture(t, nil)
	host := Member{ID: "member-1", Name: "kyle"}
	room, err := f.m.Create("kitchen", ControlsEveryone, "", host)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	memberOf := func() bool {
		snapshot, err := f.m.Get(room.ID)
		if err != nil {
			// The room closes when its last member leaves, which is also "not a
			// member" as far as this is asking.
			return false
		}
		for _, member := range snapshot.Members {
			if member.ID == host.ID {
				return true
			}
		}
		return false
	}

	// Two tabs on one account are one listener: closing one window must not take
	// the member out of the room the other is still listening in.
	f.m.Connect(host.ID)
	f.m.Connect(host.ID)
	f.m.Disconnect(host.ID)
	if !memberOf() {
		t.Fatal("a second tab was still open, so the member should still be in the room")
	}

	f.m.Disconnect(host.ID)
	// The grace has not passed: a client that dropped its socket to reconnect
	// is not a member who left.
	if !memberOf() {
		t.Fatal("a disconnect is not a leave until the grace has passed")
	}

	f.m.leaveEveryRoom(host.ID)
	if memberOf() {
		t.Fatal("the last socket closing should have taken the member out")
	}
}

// A browser that joined as a guest and has since signed in is the same person.
// The account's membership takes over the guest's queue and the guest's
// membership goes: without that the room shows them twice, once under the name
// their browser made up, and whatever they queued belongs to the ghost.
func TestJoinTakesOverTheGuestMembership(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()

	room, err := f.m.Create("party", ControlsEveryone, "", Member{ID: "host"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := f.m.Join(room.ID, Member{ID: "browser-1", Name: "web"}, "", ""); err != nil {
		t.Fatalf("guest join: %v", err)
	}
	track, _ := f.trackWithVariants("Song", variantSpec{
		provider: "ytmusic", providerTrackID: "yt-1", durationMs: 180_000, downloadable: true,
	})
	if _, err := f.enqueue(ctx, room.ID, "browser-1", track.ID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	joined, err := f.m.Join(room.ID, Member{ID: "user-9", Name: "Ada"}, "", "browser-1")
	if err != nil {
		t.Fatalf("account join: %v", err)
	}

	if len(joined.Members) != 2 {
		t.Fatalf("members = %+v, want the host and the account", joined.Members)
	}
	for _, member := range joined.Members {
		if member.ID == "browser-1" {
			t.Errorf("the guest membership is still there: %+v", member)
		}
	}
	if mine := joined.Queues["user-9"]; len(mine) != 1 {
		t.Errorf("the account's queue = %+v, want the song the guest queued", mine)
	}
	if mine := joined.Queues["browser-1"]; len(mine) != 0 {
		t.Errorf("the guest still holds a queue: %+v", mine)
	}
}

// nextPrepared waits for the renditions of the song the room has prepared
// behind the one playing. Assignment runs off the lock, exactly as it does for
// the current song.
func (f *fixture) nextPrepared(roomID string) {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.m.mu.Lock()
		room := f.m.rooms[roomID]
		ready := room != nil && room.next != nil && room.next.prepared
		f.m.mu.Unlock()
		if ready {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	f.t.Fatal("the room never prepared the next song")
}

// The room prepares the song after this one while this one plays, and takes a
// member's readiness for it. That is what makes the advance cost nothing: a
// download that only begins when the song ends is what the readiness timeout
// gets spent waiting on, and a room that has been told the next file is here
// has nothing to wait for.
func TestRoomStartsThePreparedSongWithoutWaiting(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()

	snapshot, err := f.m.Create("party", ControlsEveryone, "", Member{ID: "a"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	roomID := snapshot.ID
	first := f.track("First")
	second := f.track("Second")
	if _, err := f.enqueue(ctx, roomID, "a", first.ID); err != nil {
		t.Fatalf("Enqueue(first): %v", err)
	}
	if _, err := f.enqueue(ctx, roomID, "a", second.ID); err != nil {
		t.Fatalf("Enqueue(second): %v", err)
	}

	// The first song starts once the member says its file is here.
	if _, err := f.m.Ready(roomID, "a", first.ID, uuid.Nil, 180_000); err != nil {
		t.Fatalf("Ready(first): %v", err)
	}
	if got := f.current(f.get(roomID)).Item.Title; got != "First" {
		t.Fatalf("playing = %q, want First", got)
	}

	// While it plays, the room names what comes next, and the member says that
	// one is here too - which is the whole point of naming it early.
	state := f.get(roomID)
	if state.Next == nil || state.Next.TrackID != second.ID {
		t.Fatalf("next = %+v, want Second", state.Next)
	}
	f.nextPrepared(roomID)
	if _, err := f.m.Ready(roomID, "a", second.ID, uuid.Nil, 180_000); err != nil {
		t.Fatalf("Ready(second): %v", err)
	}

	// The first song ends. The second starts on that instant, not after the
	// readiness timeout: nothing is left to wait for.
	f.clock.Advance(180 * time.Second)

	state = f.get(roomID)
	current := state.Current
	if current == nil || current.Item.TrackID != second.ID {
		t.Fatalf("after the first song the room is on %+v, want Second", current)
	}
	if current.StartedAtMs == 0 {
		t.Error("the prepared song did not start")
	}
	if state.Next != nil {
		t.Errorf("next = %+v after the advance, want none", state.Next)
	}
}

// Without being told, the room waits: the prepared song is known but nobody has
// said its file is here, which is what the timeout is for.
func TestRoomWaitsWhenNobodyIsReadyAhead(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()

	snapshot, err := f.m.Create("party", ControlsEveryone, "", Member{ID: "a"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	roomID := snapshot.ID
	first := f.track("First")
	second := f.track("Second")
	if _, err := f.enqueue(ctx, roomID, "a", first.ID); err != nil {
		t.Fatalf("Enqueue(first): %v", err)
	}
	if _, err := f.enqueue(ctx, roomID, "a", second.ID); err != nil {
		t.Fatalf("Enqueue(second): %v", err)
	}
	if _, err := f.m.Ready(roomID, "a", first.ID, uuid.Nil, 180_000); err != nil {
		t.Fatalf("Ready(first): %v", err)
	}

	f.clock.Advance(180 * time.Second)

	current := f.get(roomID).Current
	if current == nil || current.Item.TrackID != second.ID {
		t.Fatalf("the room should be preparing Second, got %+v", current)
	}
	if current.StartedAtMs != 0 {
		t.Error("nothing said the second file was here; the room should still be waiting")
	}
}

// The room asks for the renditions of the song it has prepared, so that a
// client's own warming is a bonus rather than the only thing standing between
// the room and a download. A member running an old frontend cannot warm at all,
// and used to cost the room the whole readiness timeout.
func TestRoomWarmsWhatItIsAboutToPlay(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()

	var mu sync.Mutex
	warmed := map[uuid.UUID]bool{}
	f.m.SetWarm(func(id uuid.UUID) {
		mu.Lock()
		defer mu.Unlock()
		warmed[id] = true
	})

	snapshot, err := f.m.Create("party", ControlsEveryone, "", Member{ID: "a"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	roomID := snapshot.ID
	first, _ := f.trackWithVariants("First", variantSpec{
		provider: "local", providerTrackID: "First", durationMs: 180_000, downloadable: true,
	})
	second, secondVariants := f.trackWithVariants("Second", variantSpec{
		provider: "local", providerTrackID: "Second", durationMs: 180_000, downloadable: true,
	})
	if _, err := f.enqueue(ctx, roomID, "a", first.ID); err != nil {
		t.Fatalf("Enqueue(first): %v", err)
	}
	if _, err := f.enqueue(ctx, roomID, "a", second.ID); err != nil {
		t.Fatalf("Enqueue(second): %v", err)
	}
	if _, err := f.m.Ready(roomID, "a", first.ID, uuid.Nil, 180_000); err != nil {
		t.Fatalf("Ready(first): %v", err)
	}
	f.nextPrepared(roomID)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := warmed[secondVariants[0].ID]
		mu.Unlock()
		if got {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Error("the room never asked for the prepared song's rendition")
}

// The host's readiness is what starts the room. Nobody else's is needed, which
// is what waiting for the slowest connection in the room used to cost.
func TestTheHostStartsTheRoomAlone(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()

	snapshot, err := f.m.Create("party", ControlsEveryone, "", Member{ID: "a"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	roomID := snapshot.ID
	for _, id := range []string{"b", "c", "d"} {
		if _, err := f.m.Join(roomID, Member{ID: id}, "", ""); err != nil {
			t.Fatalf("Join(%s): %v", id, err)
		}
	}
	track := f.track("Song")
	if _, err := f.enqueue(ctx, roomID, "a", track.ID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// The host alone. b, c and d have not reported, and are not waited for.
	if _, err := f.m.Ready(roomID, "a", track.ID, uuid.Nil, 180_000); err != nil {
		t.Fatalf("Ready(host): %v", err)
	}
	current := f.current(f.get(roomID))
	if current.StartedAtMs == 0 {
		t.Error("the host's readiness did not start the room")
	}
	if len(current.CatchingUp) != 3 {
		t.Errorf("catchingUp = %v, want the three who have not reported", current.CatchingUp)
	}
}

// TestRoomWaitsForTheHostWhateverTheWindow: the room waits for the host, so
// another member reporting is not enough to start it - and the window does not
// start it either. The window is the backstop for a room with nobody to wait
// for, not a way to begin a song the host cannot hear.
func TestRoomWaitsForTheHostWhateverTheWindow(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()

	snapshot, err := f.m.Create("party", ControlsEveryone, "", Member{ID: "a"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	roomID := snapshot.ID
	if _, err := f.m.Join(roomID, Member{ID: "b"}, "", ""); err != nil {
		t.Fatalf("Join: %v", err)
	}
	track := f.track("Song")
	if _, err := f.enqueue(ctx, roomID, "a", track.ID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// b's file is here. The room is waiting on a, who leads it.
	if _, err := f.m.Ready(roomID, "b", track.ID, uuid.Nil, 180_000); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if got := f.current(f.get(roomID)).StartedAtMs; got != 0 {
		t.Fatal("the room started without the host")
	}

	// The window passes and the room is still waiting: the host's file is what
	// starts the song.
	f.clock.Advance(6 * time.Second)
	if got := f.current(f.get(roomID)).StartedAtMs; got != 0 {
		t.Fatal("the window started the song without the host")
	}

	// The host's word is what starts it.
	if _, err := f.m.Ready(roomID, "a", track.ID, uuid.Nil, 180_000); err != nil {
		t.Fatalf("Ready(host): %v", err)
	}
	if got := f.current(f.get(roomID)).StartedAtMs; got == 0 {
		t.Error("the host's readiness did not start the room")
	}
}

// A member with one song should not wait behind two of somebody else's: the
// round-robin is one item per member per pass, so a short queue is placed by
// its pass, not by its length.
func TestMasterQueueWithASingleItemFromAMember(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()

	snapshot, err := f.m.Create("party", ControlsEveryone, "", Member{ID: "a"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	roomID := snapshot.ID
	if _, err := f.m.Join(roomID, Member{ID: "b"}, "", ""); err != nil {
		t.Fatalf("Join: %v", err)
	}
	a1, a2, a3 := f.track("A1"), f.track("A2"), f.track("A3")
	b1 := f.track("B1")
	for _, enqueue := range []struct {
		member string
		track  *store.Track
	}{{"a", a1}, {"a", a2}, {"a", a3}, {"b", b1}} {
		if _, err := f.enqueue(ctx, roomID, enqueue.member, enqueue.track.ID); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}
	got := queueTitles(f.get(roomID).MasterQueue)
	want := []string{"A1", "B1", "A2", "A3"}
	if !slices.Equal(got, want) {
		t.Errorf("master queue = %v, want %v", got, want)
	}
}

// A member who queued a single song joins the pass as soon as the playing one
// is done - they must not wait behind the rest of another member's long queue.
func TestSingleItemMemberJoinsTheNextPass(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()

	snapshot, err := f.m.Create("party", ControlsEveryone, "", Member{ID: "kube"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	roomID := snapshot.ID
	if _, err := f.m.Join(roomID, Member{ID: "lain"}, "", ""); err != nil {
		t.Fatalf("Join: %v", err)
	}
	for _, title := range []string{"K1", "K2", "K3", "K4"} {
		if _, err := f.enqueue(ctx, roomID, "kube", f.track(title).ID); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}
	// Two of Kube's play before Lain queues anything.
	for i := 0; i < 2; i++ {
		if _, err := f.m.Skip(roomID, "kube"); err != nil {
			t.Fatalf("Skip: %v", err)
		}
	}
	if _, err := f.enqueue(ctx, roomID, "lain", f.track("L1").ID); err != nil {
		t.Fatalf("Enqueue lain: %v", err)
	}

	got := queueTitles(f.get(roomID).MasterQueue)
	want := []string{"K3", "L1", "K4"}
	if !slices.Equal(got, want) {
		t.Errorf("master queue = %v, want %v", got, want)
	}
}
