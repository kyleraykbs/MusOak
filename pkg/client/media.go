package client

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// mediaResponse performs the media GET and returns the raw response, so
// callers can see the ETag as well as the body.
func (c *Client) mediaResponse(ctx context.Context, variantID string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v1/media/"+variantID, nil)
	if err != nil {
		return nil, err
	}
	c.decorate(req)

	// Media transfers must not inherit the short request timeout.
	hc := *c.http
	hc.Timeout = 0
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		return nil, decodeError(resp)
	}
	return resp, nil
}

// Sink plays a rendition. The CLI's sink drives a local player; a Discord
// bot's sink streams the file into a voice channel. Nothing in this package
// cares which.
type Sink interface {
	// Play starts playback and returns when this client's own file has played
	// out, or ctx ends. It must not loop or pad; the participant handles the
	// room's timeline.
	Play(ctx context.Context, track SinkTrack) error
	// Stop abandons playback immediately.
	Stop(ctx context.Context) error
}

// Pauser is implemented by sinks that can pause in place.
type Pauser interface {
	Pause(ctx context.Context) error
	Resume(ctx context.Context) error
}

// Seeker is implemented by sinks that can jump to a position.
type Seeker interface {
	Seek(ctx context.Context, positionMs int64) error
}

// Positioner is implemented by sinks that report where they are; the
// participant uses it to correct drift with small seeks.
type Positioner interface {
	PositionMs(ctx context.Context) (int64, error)
}

// SinkTrack is one slot of playback handed to a Sink.
type SinkTrack struct {
	TrackID   string
	VariantID string
	Path      string
	// DurationMs is the length of this client's own file.
	DurationMs int64
	// TimelineMs is how long the room spends on this slot: a shorter file is
	// followed by silence, a longer one is cut at the timeline.
	TimelineMs int64
	// PositionMs is where to start, non-zero when joining a track in flight.
	PositionMs int64
	// StartedAtServerMs is the room's start instant, on the server clock.
	StartedAtServerMs int64
	// OffsetMs is reserved for per-variant alignment (different masters have
	// different intros); it is always zero today.
	OffsetMs int64
}

// Participant options.
const (
	// defaultClockSamples is how many ping samples one sync round takes.
	defaultClockSamples = 5
	// defaultResyncEvery is how often the clock is re-synced.
	defaultResyncEvery = 15 * time.Second
	// defaultDriftToleranceMs is how far a client may be from the room's
	// position before it seeks. Two seconds is the whole sync rule: a client
	// that is within it is close enough, and a song that ends slightly early
	// or late for somebody is not worth a jump.
	defaultDriftToleranceMs = 2000
	// prefetchAhead is how many of the room's upcoming songs this member keeps
	// ready. The room prepares nothing; a client downloading the next few
	// songs itself is what makes an advance cost only the time to switch
	// files.
	prefetchAhead = 3
)

// Participant follows a room on behalf of one member.
//
// It plays the room's current song and keeps itself on the room's position: if
// the song the room is on is not the one playing, it loads it; if it is, it
// seeks when it is more than two seconds away. It picks its own rendition of
// every song, because the room does not assign one. The host's participant also
// reports the two things the room cannot know otherwise — that its player has
// begun a song (`/started`) and that its file has run out (`/ended`).
type Participant struct {
	room   *RoomClient
	cache  *Cache
	sink   Sink
	logger *slog.Logger

	clockSamples   int
	resyncEvery    time.Duration
	driftTolerance int64

	// OnEvent observes every room event, for UIs that want to follow along.
	OnEvent func(Event)
	// OnTrack observes each playback slot this participant starts.
	OnTrack func(SinkTrack)

	mu       sync.Mutex
	offsetMs int64
	// room state, kept current from the snapshot and the event stream. It is
	// what the follower rule runs against.
	host     string
	current  *RoomPlayback
	playing  *localPlayback
	warming  map[string]bool
	lastSeq  int64
	started  bool
	reported string // the item id whose start the host has already reported
}

// localPlayback is the song this participant has on its sink.
type localPlayback struct {
	itemID     string
	trackID    string
	entry      CacheEntry
	paused     bool
	playing    bool
	cancel     context.CancelFunc
	generation int
}

// NewParticipant returns a participant for a room.
func NewParticipant(room *RoomClient, cache *Cache, sink Sink, logger *slog.Logger) *Participant {
	if logger == nil {
		logger = slog.Default()
	}
	return &Participant{
		room:           room,
		cache:          cache,
		sink:           sink,
		logger:         logger,
		clockSamples:   defaultClockSamples,
		resyncEvery:    defaultResyncEvery,
		driftTolerance: defaultDriftToleranceMs,
		warming:        map[string]bool{},
	}
}

// Room exposes the room client this participant follows.
func (p *Participant) Room() *RoomClient { return p.room }

// MemberID is who this participant is in the room.
func (p *Participant) MemberID() string { return p.room.MemberID }

// Run follows the room until ctx ends or the room closes.
func (p *Participant) Run(ctx context.Context) error {
	offset, err := p.room.client.ClockOffset(ctx, p.clockSamples)
	if err != nil {
		return err
	}
	p.setOffset(offset)

	events, err := p.room.Events(ctx)
	if err != nil {
		return err
	}

	// A room may already be playing when we arrive: adopt what it holds.
	if snapshot, err := p.room.Snapshot(ctx); err == nil {
		p.adopt(snapshot)
		p.warmAhead(ctx, snapshot.Pending())
		p.reconcile(ctx)
	}

	drift := time.NewTicker(time.Second)
	defer drift.Stop()
	resync := time.NewTicker(p.resyncEvery)
	defer resync.Stop()

	for {
		select {
		case <-ctx.Done():
			p.stop(ctx)
			return ctx.Err()
		case event, ok := <-events:
			if !ok {
				p.stop(ctx)
				return nil
			}
			if event.Type == EventRoomClosed {
				p.stop(ctx)
				return nil
			}
			if p.apply(ctx, event) {
				p.reconcile(ctx)
			}
		case <-drift.C:
			p.reconcile(ctx)
		case <-resync.C:
			if offset, err := p.room.client.ClockOffset(ctx, 3); err == nil {
				p.setOffset(offset)
			}
		}
	}
}

// adopt takes the room as the snapshot has it.
func (p *Participant) adopt(room *Room) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if room == nil {
		return
	}
	p.host = room.Host
	p.current = room.Current
	p.lastSeq = room.Seq
}

// apply folds one event into the room state, reporting whether anything the
// follower rule cares about changed. A gap in the event counter refetches the
// snapshot, which is the way back from a missed event.
func (p *Participant) apply(ctx context.Context, event Event) bool {
	if p.OnEvent != nil {
		p.OnEvent(event)
	}

	p.mu.Lock()
	if p.lastSeq != 0 && event.Seq != 0 && event.Seq != p.lastSeq+1 {
		seq := p.lastSeq
		p.mu.Unlock()
		p.logger.Warn("participant: missed room events, resyncing", "last_seq", seq, "event_seq", event.Seq)
		if snapshot, err := p.room.Snapshot(context.Background()); err == nil {
			p.adopt(snapshot)
		}
		return true
	}
	if event.Seq != 0 {
		p.lastSeq = event.Seq
	}
	p.mu.Unlock()

	switch event.Type {
	case EventPlayback:
		var data PlaybackEvent
		if err := event.DecodeData(&data); err != nil {
			p.logger.Warn("participant: bad playback event", "error", err)
			return false
		}
		p.mu.Lock()
		previous := p.current
		p.current = data.Current
		p.mu.Unlock()
		changed := previous == nil || data.Current == nil || previous.Item.ID != data.Current.Item.ID
		if changed {
			// A new song is up: keep the next few on disk while it plays.
			go p.warmRoom(ctx)
		}
		return true

	case EventHostChanged:
		var data HostChangedEvent
		if err := event.DecodeData(&data); err != nil {
			return false
		}
		p.mu.Lock()
		p.host = data.Host
		p.mu.Unlock()
		return true
	}
	return false
}

// reconcile is the whole follower rule, run on every event and once a second.
func (p *Participant) reconcile(ctx context.Context) {
	p.mu.Lock()
	current := p.current
	host := p.host
	p.mu.Unlock()

	if current == nil {
		p.stop(ctx)
		return
	}
	item := current.Item
	isHost := host == p.room.MemberID
	expected := current.PositionAt(p.serverNow())

	local := p.local()
	if local == nil || local.itemID != item.ID {
		// The room is on a different song: load it. A follower holds it until
		// the host has begun it; the host is the one who begins it.
		entry, err := p.ensureTrack(ctx, item.TrackID)
		if err != nil {
			p.logger.Warn("participant: could not load the room's song", "track", item.TrackID, "error", err)
			return
		}
		p.setLocal(&localPlayback{itemID: item.ID, trackID: item.TrackID, entry: entry})
		local = p.local()
	}

	if !current.Started {
		if !isHost {
			return // loaded and holding; the host is what starts the song
		}
		if !local.playing {
			p.play(ctx, local, current, expected, true)
		}
		return
	}

	if !current.Paused {
		if !local.playing {
			p.play(ctx, local, current, expected, isHost)
			return
		}
		if local.paused {
			if pauser, ok := p.sink.(Pauser); ok {
				if err := pauser.Resume(ctx); err != nil {
					p.logger.Warn("participant: resume", "error", err)
				}
			}
			p.setPaused(local, false)
			return
		}
		p.correctDrift(ctx, local, expected)
		return
	}

	// Not started, or paused: hold where the room is.
	if local.playing && !local.paused {
		if pauser, ok := p.sink.(Pauser); ok {
			if err := pauser.Pause(ctx); err != nil {
				p.logger.Warn("participant: pause", "error", err)
			}
		}
		p.setPaused(local, true)
	}
}

// play starts one slot: it plays this client's file from where the room is and
// reports the start when this client is the host.
func (p *Participant) play(ctx context.Context, local *localPlayback, current *RoomPlayback, expected int64, isHost bool) {
	slot := SinkTrack{
		TrackID:    local.trackID,
		VariantID:  local.entry.VariantID,
		Path:       local.entry.Path,
		DurationMs: local.entry.DurationMs,
		PositionMs: expected,
	}
	if p.OnTrack != nil {
		p.OnTrack(slot)
	}

	playCtx, cancel := context.WithCancel(ctx)
	p.mu.Lock()
	if local.cancel != nil {
		local.cancel()
	}
	local.cancel = cancel
	local.playing = true
	local.paused = false
	local.generation++
	generation := local.generation
	item := current.Item
	p.mu.Unlock()

	if isHost && !current.Started {
		// The host's player is what starts the room's clock. Report where its
		// file actually is, once per song.
		p.mu.Lock()
		already := p.reported == item.ID
		if !already {
			p.reported = item.ID
		}
		p.mu.Unlock()
		if !already {
			if _, err := p.room.Started(ctx, item.TrackID, expected, local.entry.DurationMs); err != nil {
				p.logger.Warn("participant: report start", "error", err)
			}
		}
	}

	go func() {
		err := p.sink.Play(playCtx, slot)
		finished := playCtx.Err() == nil
		p.mu.Lock()
		same := p.playing != nil && p.playing.generation == generation
		if same {
			p.playing.playing = false
		}
		p.mu.Unlock()
		if err != nil && finished {
			p.logger.Warn("participant: playback failed", "variant", slot.VariantID, "error", err)
		}
		if !finished || !same {
			return
		}
		// This client's file has played out. The host's file running out is
		// what moves the room on; anybody else's just goes quiet.
		//
		// A player that stops on its own is only a song ending when it stopped
		// at the song's end: an end-file event left over from the file before
		// this one stops the wait a moment after a load, and reporting that
		// would cut the song for everybody.
		if isHost && p.playedToTheEnd(ctx, slot) {
			if _, err := p.room.Ended(ctx, item.ID, item.TrackID); err != nil {
				p.logger.Warn("participant: report end", "error", err)
			}
		}
	}()
}

// playedToTheEnd reports whether the file really ran out: a slot whose length
// is known must have been played to (near) it, not merely stopped.
func (p *Participant) playedToTheEnd(ctx context.Context, slot SinkTrack) bool {
	if slot.DurationMs <= 0 {
		return true
	}
	positioner, ok := p.sink.(Positioner)
	if !ok {
		return true
	}
	position, err := positioner.PositionMs(ctx)
	if err != nil {
		return true
	}
	return position >= slot.DurationMs-1000
}

// correctDrift nudges the sink back onto the room's position.
func (p *Participant) correctDrift(ctx context.Context, local *localPlayback, expected int64) {
	positioner, ok := p.sink.(Positioner)
	seeker, canSeek := p.sink.(Seeker)
	if !ok || !canSeek {
		return
	}
	if local.entry.DurationMs > 0 && expected >= local.entry.DurationMs {
		// This client's file is over; the room is still on it. Wait for the
		// room to move, which is what "a song ends slightly early" means.
		return
	}
	got, err := positioner.PositionMs(ctx)
	if err != nil {
		return
	}
	drift := got - expected
	if drift < 0 {
		drift = -drift
	}
	if drift <= p.driftTolerance {
		return
	}
	p.logger.Debug("participant: correcting drift", "drift_ms", got-expected)
	if err := seeker.Seek(ctx, expected); err != nil {
		p.logger.Warn("participant: drift correction failed", "error", err)
	}
}

// ensureTrack makes sure a rendition of the track is on disk locally. A copy
// this client already has is used as-is; otherwise the first downloadable
// rendition is fetched, which is this member's own choice of version.
func (p *Participant) ensureTrack(ctx context.Context, trackID string) (CacheEntry, error) {
	if entry, ok := p.cache.LookupTrack(trackID); ok {
		return entry, nil
	}
	variants, err := p.room.client.TrackVariants(ctx, trackID)
	if err != nil {
		return CacheEntry{}, err
	}
	variantID := ""
	for _, variant := range variants {
		if variant.Downloadable {
			variantID = variant.ID
			break
		}
	}
	if variantID == "" {
		return CacheEntry{}, errors.New("no downloadable rendition")
	}
	entry, err := p.cache.Fetch(ctx, p.room.client, variantID)
	if err != nil {
		return CacheEntry{}, err
	}
	entry.TrackID = trackID
	if err := p.cache.Put(entry); err != nil {
		return CacheEntry{}, err
	}
	return entry, nil
}

// warmRoom fetches the room and warms the songs behind the current one. It is
// best effort, and a failure just means the file is fetched when it is reached.
func (p *Participant) warmRoom(ctx context.Context) {
	snapshot, err := p.room.Snapshot(ctx)
	if err != nil {
		return
	}
	p.warmAhead(ctx, snapshot.Pending())
}

// warmAhead gets the room's next few songs onto disk. Nothing waits on it: a
// song that cannot be fetched says so when it is reached.
func (p *Participant) warmAhead(ctx context.Context, pending []QueueItem) {
	for i, item := range pending {
		if i == prefetchAhead {
			return
		}
		p.warm(ctx, item.TrackID)
	}
}

func (p *Participant) warm(ctx context.Context, trackID string) {
	if trackID == "" {
		return
	}
	if _, ok := p.cache.LookupTrack(trackID); ok {
		return
	}
	if !p.claimWarm(trackID) {
		return
	}
	go func() {
		defer p.releaseWarm(trackID)
		if _, err := p.ensureTrack(ctx, trackID); err != nil {
			p.logger.Debug("participant: could not warm an upcoming song", "track", trackID, "error", err)
		}
	}()
}

func (p *Participant) claimWarm(trackID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.warming[trackID] {
		return false
	}
	p.warming[trackID] = true
	return true
}

func (p *Participant) releaseWarm(trackID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.warming, trackID)
}

// Position reports where the room should be right now, on the server clock.
func (p *Participant) Position() int64 {
	p.mu.Lock()
	current := p.current
	p.mu.Unlock()
	return current.PositionAt(p.serverNow())
}

// LocalPosition reports where this client's own player is, when its sink can
// say so.
func (p *Participant) LocalPosition(ctx context.Context) (int64, bool) {
	positioner, ok := p.sink.(Positioner)
	if !ok {
		return 0, false
	}
	position, err := positioner.PositionMs(ctx)
	if err != nil {
		return 0, false
	}
	return position, true
}

// Vote scores the current track from 1 (bad) to 5 (great).
func (p *Participant) Vote(ctx context.Context, score int) error {
	_, err := p.room.Vote(ctx, score)
	return err
}

// StopPlayback stops this client's audio without leaving the room.
func (p *Participant) StopPlayback(ctx context.Context) error {
	p.stop(ctx)
	return nil
}

// stop abandons whatever this participant is playing.
func (p *Participant) stop(ctx context.Context) {
	p.mu.Lock()
	local := p.playing
	p.playing = nil
	p.mu.Unlock()
	if local != nil && local.cancel != nil {
		local.cancel()
	}
	if err := p.sink.Stop(ctx); err != nil {
		p.logger.Debug("participant: stop", "error", err)
	}
}

func (p *Participant) local() *localPlayback {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.playing
}

// setLocal installs a newly loaded song, abandoning whatever was playing.
func (p *Participant) setLocal(local *localPlayback) {
	p.mu.Lock()
	previous := p.playing
	p.playing = local
	p.mu.Unlock()
	if previous != nil && previous.cancel != nil {
		previous.cancel()
	}
}

func (p *Participant) setPaused(local *localPlayback, paused bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.playing != nil && p.playing.itemID == local.itemID {
		p.playing.paused = paused
	}
}

// serverNow is the server clock as this client sees it.
func (p *Participant) serverNow() int64 {
	p.mu.Lock()
	offset := p.offsetMs
	p.mu.Unlock()
	return time.Now().UnixMilli() + offset
}

func (p *Participant) setOffset(offset time.Duration) {
	p.mu.Lock()
	p.offsetMs = offset.Milliseconds()
	p.mu.Unlock()
}

// ClockOffset is the current estimate of (server clock - local clock).
func (p *Participant) ClockOffset() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return time.Duration(p.offsetMs) * time.Millisecond
}
