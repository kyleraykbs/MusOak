package client

import (
	"context"
	"errors"
	"fmt"
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
	// defaultDriftToleranceMs is the drift worth correcting with a small seek.
	defaultDriftToleranceMs = 150
	// prefetchAhead is how many of the room's upcoming songs this member keeps
	// ready. The server prepares the one it is about to play and assigns a
	// rendition of it; the songs after that are nobody's job until they are
	// reached, and a room moves faster than a download does.
	prefetchAhead = 3
)

// Participant follows a room on behalf of one member: it downloads what the
// server assigns, reports readiness with the length of its own rendition, and
// plays along the server's timeline.
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

	mu          sync.Mutex
	offsetMs    int64
	assignments map[string]CacheEntry
	// warming is the songs being fetched ahead right now, so the same one is
	// not asked for twice while it is on its way.
	warming  map[string]bool
	playback *playbackState
}

type playbackState struct {
	trackID    string
	variantID  string
	startedAt  int64
	timelineMs int64
	positionMs int64
	paused     bool
	cancel     context.CancelFunc
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
		assignments:    map[string]CacheEntry{},
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

	// A room may already be playing when we arrive.
	if snapshot, err := p.room.Snapshot(ctx); err == nil && snapshot.Current != nil && snapshot.Current.StartedAtMs != 0 {
		p.adoptSnapshot(ctx, snapshot)
	}

	drift := time.NewTicker(time.Second)
	defer drift.Stop()
	resync := time.NewTicker(p.resyncEvery)
	defer resync.Stop()

	for {
		select {
		case <-ctx.Done():
			_ = p.stopPlayback(ctx)
			return ctx.Err()
		case event, ok := <-events:
			if !ok {
				_ = p.stopPlayback(ctx)
				return nil
			}
			if err := p.handle(ctx, event); err != nil {
				if errors.Is(err, errRoomClosed) {
					_ = p.stopPlayback(ctx)
					return nil
				}
				p.logger.Warn("participant: event failed", "type", event.Type, "error", err)
			}
		case <-drift.C:
			p.correctDrift(ctx)
		case <-resync.C:
			if offset, err := p.room.client.ClockOffset(ctx, 3); err == nil {
				p.setOffset(offset)
			}
		}
	}
}

var errRoomClosed = errors.New("room closed")

func (p *Participant) handle(ctx context.Context, event Event) error {
	if p.OnEvent != nil {
		p.OnEvent(event)
	}

	switch event.Type {
	case EventRoomClosed:
		return errRoomClosed

	case EventTrackPrepared:
		var data TrackPrepared
		if err := event.DecodeData(&data); err != nil {
			return err
		}
		return p.prepare(ctx, data)

	case EventTrackStarted:
		var data TrackStarted
		if err := event.DecodeData(&data); err != nil {
			return err
		}
		return p.start(ctx, data)

	case EventTrackSkipped:
		return p.stopPlayback(ctx)

	case EventPaused:
		if pauser, ok := p.sink.(Pauser); ok {
			p.mu.Lock()
			state := p.playback
			p.mu.Unlock()
			if state != nil && !state.paused {
				state.paused = true
				return pauser.Pause(ctx)
			}
		}
		return nil

	case EventResumed:
		if pauser, ok := p.sink.(Pauser); ok {
			p.mu.Lock()
			state := p.playback
			p.mu.Unlock()
			if state != nil && state.paused {
				state.paused = false
				return pauser.Resume(ctx)
			}
		}
		return nil

	case EventSeeked:
		var data struct {
			PositionMs int64 `json:"positionMs"`
			StartedAt  int64 `json:"startedAt"`
		}
		if err := event.DecodeData(&data); err != nil {
			return err
		}
		p.mu.Lock()
		if p.playback != nil {
			p.playback.positionMs = data.PositionMs
			p.playback.startedAt = data.StartedAt
		}
		p.mu.Unlock()
		if seeker, ok := p.sink.(Seeker); ok {
			return seeker.Seek(ctx, data.PositionMs)
		}
		return nil
	}
	return nil
}

// prepare downloads this member's assigned rendition and reports readiness.
func (p *Participant) prepare(ctx context.Context, data TrackPrepared) error {
	variantID, ok := data.Variants[p.room.MemberID]
	if !ok || variantID == "" {
		// The server picked nothing for us; it will retry on the next event.
		return nil
	}

	// A rendition of this track we already hold is reported as-is: no
	// download, and the room learns the duration we will really play.
	if entry, ok := p.cache.LookupTrack(data.Item.TrackID); ok {
		p.remember(data.Item.TrackID, entry)
		_, err := p.room.Ready(ctx, data.Item.TrackID, entry.VariantID, entry.DurationMs)
		return err
	}

	entry, err := p.cache.Fetch(ctx, p.room.client, variantID)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", variantID, err)
	}
	entry.TrackID = data.Item.TrackID
	p.remember(data.Item.TrackID, entry)

	if _, err := p.room.Ready(ctx, data.Item.TrackID, entry.VariantID, entry.DurationMs); err != nil {
		return err
	}

	// If the room is already playing this track — we joined late, or the server
	// assigned our rendition after the start — play from the room position.
	if snapshot, err := p.room.Snapshot(ctx); err == nil {
		// The songs after this one are worth having too: the server prepares
		// them one at a time, and a skip should not wait on a download.
		p.warmAhead(ctx, snapshot, data.Item.ID)
		if current := snapshot.Current; current != nil {
			if current.Item.TrackID == data.Item.TrackID && current.StartedAtMs != 0 {
				p.mu.Lock()
				alreadyPlaying := p.playback != nil && p.playback.trackID == data.Item.TrackID
				p.mu.Unlock()
				if !alreadyPlaying {
					p.adoptSnapshot(ctx, snapshot)
				}
			}
		}
	}
	return nil
}

// warmAhead gets the room's next few songs onto disk, skipping the one being
// prepared, which is already on its way.
//
// These are not readiness reports: the server decides what is prepared and
// when, and this only means the file is already here by then. The room's own
// order is what to follow, because that is what will be played.
func (p *Participant) warmAhead(ctx context.Context, snapshot *Room, preparingItemID string) {
	if snapshot == nil {
		return
	}
	currentID := preparingItemID
	if snapshot.Current != nil {
		currentID = snapshot.Current.Item.ID
	}
	ahead := 0
	for _, item := range snapshot.Queue {
		if item.ID == currentID || item.TrackID == "" {
			continue
		}
		p.warm(ctx, item.TrackID)
		ahead++
		if ahead == prefetchAhead {
			return
		}
	}
}

// warm fetches one song the room will reach, unless this member already holds
// it or somebody is already fetching it. Nothing waits on it: a song that
// cannot be fetched says so when it is reached.
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
		if _, err := p.fetchAny(ctx, trackID); err != nil {
			p.logger.Debug("participant: could not warm an upcoming song", "track", trackID, "error", err)
		}
	}()
}

// fetchAny downloads some rendition of a track nobody has assigned this member
// yet. Which one matters less than having one: the room plays whatever a member
// reports, and the rendition the server assigns later is used as it is when the
// file is already here.
func (p *Participant) fetchAny(ctx context.Context, trackID string) (CacheEntry, error) {
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
	// The index is what says which track a file belongs to, so that the next
	// look for this song finds it.
	entry.TrackID = trackID
	if err := p.cache.Put(entry); err != nil {
		return CacheEntry{}, err
	}
	return entry, nil
}

// claimWarm reports whether this song should be fetched here, taking the claim
// when it should.
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

func (p *Participant) start(ctx context.Context, started TrackStarted) error {
	entry, ok := p.assignment(started.Item.TrackID)
	if !ok {
		// We missed the preparation (we joined late, or we were catching up):
		// fetch our assigned rendition now and play from the room position.
		variantID := started.Variants[p.room.MemberID]
		if variantID == "" {
			return nil
		}
		fetched, err := p.cache.Fetch(ctx, p.room.client, variantID)
		if err != nil {
			return fmt.Errorf("catch up on %s: %w", variantID, err)
		}
		fetched.TrackID = started.Item.TrackID
		p.remember(started.Item.TrackID, fetched)
		entry = fetched
	}

	position := p.serverNow() - started.StartedAt
	if position < 0 {
		position = 0
	}
	if started.TimelineMs > 0 && position >= started.TimelineMs {
		return nil // this track is already over
	}

	slot := SinkTrack{
		TrackID:           started.Item.TrackID,
		VariantID:         entry.VariantID,
		Path:              entry.Path,
		DurationMs:        entry.DurationMs,
		TimelineMs:        started.TimelineMs,
		PositionMs:        position,
		StartedAtServerMs: started.StartedAt,
	}
	if p.OnTrack != nil {
		p.OnTrack(slot)
	}

	playCtx, cancel := context.WithCancel(ctx)
	p.mu.Lock()
	if p.playback != nil && p.playback.cancel != nil {
		p.playback.cancel()
	}
	p.playback = &playbackState{
		trackID:    slot.TrackID,
		variantID:  slot.VariantID,
		startedAt:  slot.StartedAtServerMs,
		timelineMs: slot.TimelineMs,
		positionMs: position,
		cancel:     cancel,
	}
	p.mu.Unlock()

	go p.play(playCtx, slot)
	return nil
}

// play runs one slot: it plays this client's file and, if that file is shorter
// than the room's timeline, holds silence until the slot ends.
func (p *Participant) play(ctx context.Context, slot SinkTrack) {
	if err := p.sink.Play(ctx, slot); err != nil && !errors.Is(err, context.Canceled) && ctx.Err() == nil {
		p.logger.Warn("participant: playback failed", "variant", slot.VariantID, "error", err)
	}
	if ctx.Err() != nil {
		return
	}

	// Pad the rest of the slot with silence: never end the room's track early.
	deadline := time.Duration(slot.StartedAtServerMs+slot.TimelineMs-p.serverNow()) * time.Millisecond
	if deadline <= 0 {
		return
	}
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// adoptSnapshot resumes a track that was already playing when we joined.
func (p *Participant) adoptSnapshot(ctx context.Context, snapshot *Room) {
	current := snapshot.Current
	if current == nil {
		return
	}
	variantID := current.Variants[p.room.MemberID]
	for _, report := range current.Ready {
		if report.MemberID == p.room.MemberID {
			variantID = report.VariantID
		}
	}
	started := TrackStarted{Item: current.Item, StartedAt: current.StartedAtMs, TimelineMs: current.TimelineMs}
	if variantID != "" {
		started.Variants = map[string]string{p.room.MemberID: variantID}
	}
	if err := p.start(ctx, started); err != nil {
		p.logger.Warn("participant: could not join the running track", "error", err)
	}
}

// correctDrift nudges the sink back onto the room timeline.
func (p *Participant) correctDrift(ctx context.Context) {
	positioner, ok := p.sink.(Positioner)
	if !ok {
		return
	}
	p.mu.Lock()
	state := p.playback
	p.mu.Unlock()
	if state == nil || state.paused {
		return
	}

	want := p.serverNow() - state.startedAt
	if want < 0 || (state.timelineMs > 0 && want > state.timelineMs) {
		return
	}
	got, err := positioner.PositionMs(ctx)
	if err != nil {
		return
	}
	drift := got - want
	if drift < 0 {
		drift = -drift
	}
	if drift <= p.driftTolerance {
		return
	}
	if seeker, ok := p.sink.(Seeker); ok {
		p.logger.Debug("participant: correcting drift", "drift_ms", got-want)
		if err := seeker.Seek(ctx, want); err != nil {
			p.logger.Warn("participant: drift correction failed", "error", err)
		}
	}
}

// Position reports where the room should be right now, on the server clock.
func (p *Participant) Position() int64 {
	p.mu.Lock()
	state := p.playback
	p.mu.Unlock()
	if state == nil {
		return 0
	}
	if state.paused {
		return state.positionMs
	}
	return p.serverNow() - state.startedAt
}

// Vote scores the current track from 1 (bad) to 5 (great).
func (p *Participant) Vote(ctx context.Context, score int) error {
	_, err := p.room.Vote(ctx, score)
	return err
}

// StopPlayback stops this client's audio without leaving the room.
func (p *Participant) StopPlayback(ctx context.Context) error {
	return p.stopPlayback(ctx)
}

func (p *Participant) stopPlayback(ctx context.Context) error {
	p.mu.Lock()
	state := p.playback
	p.playback = nil
	p.mu.Unlock()
	if state != nil && state.cancel != nil {
		state.cancel()
	}
	return p.sink.Stop(ctx)
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

func (p *Participant) remember(trackID string, entry CacheEntry) {
	p.mu.Lock()
	p.assignments[trackID] = entry
	p.mu.Unlock()
}

func (p *Participant) assignment(trackID string) (CacheEntry, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.assignments[trackID]
	return entry, ok
}
