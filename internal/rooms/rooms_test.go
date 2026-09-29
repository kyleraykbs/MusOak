package rooms

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/prismusic/internal/config"
	"codeberg.org/kyleraykbs/prismusic/internal/match"
	"codeberg.org/kyleraykbs/prismusic/internal/provider"
	"codeberg.org/kyleraykbs/prismusic/internal/ranking"
	"codeberg.org/kyleraykbs/prismusic/internal/store"
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

// TestThreeClientsStaySynchronizedAndSkipOnVotes is the Block 9 acceptance
// test: three clients with renditions of 180s, 179s and 182s start together,
// the shortest one runs out early and pads with silence, a vote-driven skip
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
	snapshot, err := f.m.Create("party", ControlsEveryone, host)
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
		if _, err := f.m.Join(roomID, Member{ID: client.id, Name: client.id}); err != nil {
			t.Fatalf("Join: %v", err)
		}
	}

	if _, err := f.m.Enqueue(ctx, roomID, host.ID, track1.ID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := f.m.Enqueue(ctx, roomID, host.ID, track2.ID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Nothing starts until everyone has downloaded and reported.
	pending := f.get(roomID)
	if f.current(pending).StartedAtMs != 0 {
		t.Fatal("track started before everyone was ready")
	}
	if len(f.current(pending).Awaiting) != 3 {
		t.Fatalf("awaiting = %v, want all three members", f.current(pending).Awaiting)
	}

	for _, client := range clients {
		if _, err := f.m.Ready(roomID, client.id, track1.ID, client.variantID, client.durationMs); err != nil {
			t.Fatalf("Ready(%s): %v", client.id, err)
		}
	}

	started := f.get(roomID)
	current := f.current(started)
	if current.Item.TrackID != track1.ID {
		t.Fatalf("current item = %s, want track one", current.Item.TrackID)
	}
	if current.StartedAtMs == 0 {
		t.Fatal("track did not start once everyone was ready")
	}
	if want := f.clock.Now().UnixMilli(); current.StartedAtMs != want {
		t.Errorf("startedAt = %d, want %d", current.StartedAtMs, want)
	}
	if current.TimelineMs != 182_000 {
		t.Errorf("timeline = %d, want the longest rendition (182000)", current.TimelineMs)
	}
	if current.PositionMs != 0 {
		t.Errorf("position = %d, want 0 at start", current.PositionMs)
	}
	if len(current.Awaiting) != 0 || len(current.CatchingUp) != 0 {
		t.Errorf("awaiting = %v, catchingUp = %v; want nobody", current.Awaiting, current.CatchingUp)
	}

	// The 179s rendition runs out while the room still has 3s to go: that
	// member pads with silence instead of ending the track early.
	shortEndsAt := current.StartedAtMs + 179_000
	roomEndsAt := current.StartedAtMs + current.TimelineMs
	if shortEndsAt >= roomEndsAt {
		t.Fatalf("expected the shortest rendition to end first: %d vs %d", shortEndsAt, roomEndsAt)
	}

	f.clock.Advance(179 * time.Second)
	midway := f.get(roomID)
	if midway.Current.Item.TrackID != track1.ID {
		t.Fatal("room advanced before the timeline ended")
	}
	if midway.Current.PositionMs != 179_000 {
		t.Errorf("position = %d, want 179000", midway.Current.PositionMs)
	}

	// A vote-driven skip: every member votes, the mean is below the threshold.
	f.clock.Advance(time.Second)
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

	// The next track runs to its end and the room goes idle.
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

func TestReadinessTimeoutStartsWithoutLaggards(t *testing.T) {
	f := newFixture(t, func(cfg *config.Config) {
		cfg.ListenTogether.ReadyTimeoutSeconds = 30
	})
	ctx := context.Background()

	track, variants := f.trackWithVariants("Song",
		variantSpec{provider: "local", providerTrackID: "a", durationMs: 200_000, downloadable: true},
	)

	snapshot, err := f.m.Create("party", ControlsEveryone, Member{ID: "host"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Join(snapshot.ID, Member{ID: "laggard"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Enqueue(ctx, snapshot.ID, "host", track.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Ready(snapshot.ID, "host", track.ID, variants[0].ID, 200_000); err != nil {
		t.Fatal(err)
	}

	before := f.get(snapshot.ID)
	if f.current(before).StartedAtMs != 0 {
		t.Fatal("track started before the readiness timeout")
	}
	if len(f.current(before).Awaiting) != 1 {
		t.Fatalf("awaiting = %v, want the laggard", f.current(before).Awaiting)
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

			snapshot, err := f.m.Create("party", ControlsEveryone, Member{ID: "m0"})
			if err != nil {
				t.Fatal(err)
			}
			members := []string{"m0"}
			for i := 1; i < tt.members; i++ {
				id := "m" + string(rune('0'+i))
				if _, err := f.m.Join(snapshot.ID, Member{ID: id}); err != nil {
					t.Fatal(err)
				}
				members = append(members, id)
			}
			if _, err := f.m.Enqueue(ctx, snapshot.ID, "m0", track.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.m.Enqueue(ctx, snapshot.ID, "m0", next.ID); err != nil {
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
	snapshot, err := f.m.Create("party", ControlsHost, Member{ID: "host"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Join(snapshot.ID, Member{ID: "guest"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Enqueue(ctx, snapshot.ID, "guest", track.ID); err != nil {
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

	snapshot, err := f.m.Create("party", ControlsEveryone, Member{ID: "host"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Enqueue(ctx, snapshot.ID, "host", track.ID); err != nil {
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

	if _, err := f.m.Join(snapshot.ID, Member{ID: "late"}); err != nil {
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

func TestRoomClosesWhenEverybodyLeaves(t *testing.T) {
	f := newFixture(t, nil)

	snapshot, err := f.m.Create("party", ControlsEveryone, Member{ID: "host"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Join(snapshot.ID, Member{ID: "guest"}); err != nil {
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
