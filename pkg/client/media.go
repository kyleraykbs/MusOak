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
	// Play starts playback and returns when the local file finishes or ctx ends.
	// The participant advances to the next song itself when it returns.
	Play(ctx context.Context, track SinkTrack) error
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

// Positioner reports the local playhead for drift correction against the room's
// position.
type Positioner interface {
	PositionMs(ctx context.Context) (int64, error)
}

// SinkTrack is one file handed to a Sink.
type SinkTrack struct {
	TrackID    string
	VariantID  string
	Path       string
	DurationMs int64
	PositionMs int64
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

// Participant follows a room on behalf of one member. The server is the room's
// clock: this client plays the room's current song, corrects itself to the
// room's position when it drifts more than two seconds, and advances to the
// next song on its own when its own file ends. It never reports a position; the
// server's once-a-second state is only ever a correction.
type Participant struct {
	room   *RoomClient
	cache  *Cache
	sink   Sink
	logger *slog.Logger

	clockSamples   int
	resyncEvery    time.Duration
	driftTolerance int64

	OnEvent func(Event)
	OnTrack func(SinkTrack)

	mu       sync.Mutex
	offsetMs int64
	current  *RoomPlayback
	master   []QueueItem
	// played names items this client has advanced past locally. A server state
	// that still names one of them is a beat behind the client, not a
	// correction to apply.
	played map[string]bool
	// loadLatencyMs is the smoothed time a load takes, from asking for the file
	// to the sink being ready; it is added to the room's position when starting
	// a load so the audible start lands where the room will be, not where it was.
	loadLatencyMs int64

	playing         *localPlayback
	warming         map[string]bool
	lastSeq         int64
	advancingItemID string
}

// localPlayback is the song this participant has on its sink.
type localPlayback struct {
	itemID     string
	trackID    string
	entry      CacheEntry
	paused     bool
	playing    bool
	ended      bool
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
		played:         map[string]bool{},
		warming:        map[string]bool{},
	}
}

// Room exposes the room client this participant follows.
func (p *Participant) Room() *RoomClient { return p.room }

// MemberID is who this participant is in the room.
func (p *Participant) MemberID() string { return p.room.MemberID }

// ErrRoomClosed is what Run returns when the room closed under it. It is
// distinct from a plain nil, which means the connection to the room dropped:
// the room is still there and following it again is worthwhile.
var ErrRoomClosed = errors.New("musoak: the room closed")

// Run follows the room until ctx ends, the room closes, or the connection to
// it drops.
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
				return ErrRoomClosed
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
// adopt takes a newer room snapshot, never replacing a socket event that
// arrived after the request began.
func (p *Participant) adopt(room *Room) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if room == nil || (p.lastSeq != 0 && room.Seq != 0 && room.Seq < p.lastSeq) {
		return
	}
	p.current = room.Current
	p.master = append([]QueueItem(nil), room.MasterQueue...)
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
		if previous != nil && (data.Current == nil || previous.Item.ID != data.Current.Item.ID) {
			p.master = dropQueueItem(p.master, previous.Item.ID)
		}
		p.current = data.Current
		changed := previous == nil || data.Current == nil || previous.Item.ID != data.Current.Item.ID
		p.mu.Unlock()
		if changed {
			go p.warmMaster(ctx)
		}
		return true

	case EventQueueUpdated:
		// The change carries the room's own order, and taking it saves a
		// snapshot per edit: a burst of adds - somebody shuffling a playlist
		// into the room - used to become a burst of whole-room fetches, which
		// is enough to back the event socket up until the server drops it.
		var data QueueEvent
		if err := event.DecodeData(&data); err == nil && data.Master != nil {
			p.mu.Lock()
			p.master = append([]QueueItem(nil), data.Master...)
			p.mu.Unlock()
			// The order moved, so the songs behind the one playing did too:
			// fetch them now, or the transition pays for the download. A song
			// queued mid-track used to wait for the advance to be fetched at
			// all, which is exactly the wait it was supposed to avoid.
			go p.warmMaster(ctx)
			return true
		}
		if snapshot, err := p.room.Snapshot(ctx); err == nil {
			p.adopt(snapshot)
			go p.warmMaster(ctx)
		} else {
			p.logger.Warn("participant: refresh room queue", "error", err)
		}
		return true

	case EventMemberJoined, EventMemberLeft:
		if snapshot, err := p.room.Snapshot(ctx); err == nil {
			p.adopt(snapshot)
			go p.warmMaster(ctx)
		} else {
			p.logger.Warn("participant: refresh room queue", "error", err)
		}
		return true

	case EventHostChanged:
		// Who leads decides who may command the room, not what plays: nothing
		// here follows the host's player any more.
		return false
	}
	return false
}

func dropQueueItem(queue []QueueItem, itemID string) []QueueItem {
	for i, item := range queue {
		if item.ID == itemID {
			return append(queue[:i], queue[i+1:]...)
		}
	}
	return queue
}

// reconcile is the playback rule, run on room events and once a second. It is
// correction-only: between the server's beats this client's own player runs the
// room's order, and the server's state is applied only where the two disagree.
func (p *Participant) reconcile(ctx context.Context) {
	p.mu.Lock()
	current := p.current
	local := p.playing
	var (
		localPlaying, localPaused, localEnded bool
		localItemID                           string
	)
	if local != nil {
		localPlaying, localPaused, localEnded = local.playing, local.paused, local.ended
		localItemID = local.itemID
	}
	p.mu.Unlock()

	if current == nil {
		p.stop(ctx)
		return
	}
	item := current.Item

	// A paused room holds its file where it is. The next beat after a resume
	// puts this client back on the room's song and position.
	if current.Paused {
		if local != nil && localPlaying && !localPaused {
			p.pause(ctx, local, true)
		}
		return
	}

	if local == nil || localItemID != item.ID {
		if p.hasPlayed(item.ID) {
			// This client ran past that song on its own and the server has not
			// caught up yet: its state is a beat behind, not a correction.
			return
		}
		p.load(ctx, current, p.startAt(current))
		return
	}
	// Server and client agree on the song: any item this client advanced past
	// locally is behind both of them now.
	p.forgetPlayed()

	if localEnded {
		p.advanceLocal(ctx, item.ID)
		return
	}
	if !localPlaying {
		p.load(ctx, current, p.startAt(current))
		return
	}
	if localPaused {
		p.pause(ctx, local, false)
		return
	}
	p.correctDrift(ctx, local, current.PositionAt(p.serverNow()))
}

// startAt is where a load should start so it lands on the room's position: the
// place the room will be once the load has taken its smoothed time.
func (p *Participant) startAt(current *RoomPlayback) int64 {
	p.mu.Lock()
	latency := p.loadLatencyMs
	p.mu.Unlock()
	return current.PositionAt(p.serverNow() + latency)
}

// load puts the room's song on the sink at the given position, fetching the file
// first when this client does not hold one. If a fresh file, the load is timed
// from issue through to the sink actually playing and the estimate is smoothed,
// so the next load can be started in time to land on the room's beat.
func (p *Participant) load(ctx context.Context, current *RoomPlayback, expected int64) {
	item := current.Item
	p.mu.Lock()
	local := p.playing
	sameFile := local != nil && local.itemID == item.ID
	p.mu.Unlock()

	var issuedAt time.Time
	if !sameFile {
		issuedAt = time.Now()
		entry, err := p.ensureTrack(ctx, item.TrackID)
		if err != nil {
			p.logger.Warn("participant: could not load the room's song", "track", item.TrackID, "error", err)
			return
		}
		p.setLocal(&localPlayback{itemID: item.ID, trackID: item.TrackID, entry: entry})
		local = p.local()
	}
	p.play(ctx, local, item, expected, issuedAt)
}

// watchStart times a load through to the sink actually playing: from the moment
// the load was issued to the first position the sink reports at or past the
// start. The sink's own start latency is what the room's boundary has to absorb,
// and it is not observable any other way through the Sink interface.
func (p *Participant) watchStart(ctx context.Context, issuedAt time.Time, expected int64, local *localPlayback, generation int) {
	positioner, ok := p.sink.(Positioner)
	if !ok {
		return
	}
	target := expected + 50
	if target < 50 {
		target = 50
	}
	// Nothing that has not started in five seconds is going to: the estimate
	// keeps its old value rather than waiting longer.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		same := p.playing == local && p.playing.generation == generation
		p.mu.Unlock()
		if !same {
			return
		}
		if position, err := positioner.PositionMs(ctx); err == nil && position >= target {
			p.measureLoad(time.Since(issuedAt))
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// measureLoad folds one load's duration into the smoothed estimate. A load from
// cache costs only the sink's own start, so the estimate settles on what
// starting a song from this device actually takes.
func (p *Participant) measureLoad(took time.Duration) {
	sample := took.Milliseconds()
	if sample < 0 || sample > 10_000 {
		// A wild sample is a stalled fetch, not a load time: leave the estimate
		// alone rather than skewing it for the next songs.
		return
	}
	p.mu.Lock()
	if p.loadLatencyMs == 0 {
		p.loadLatencyMs = sample
	} else {
		p.loadLatencyMs = (p.loadLatencyMs*3 + sample) / 4
	}
	p.mu.Unlock()
}

// play starts the local file at the given position. The sink runs it as an
// ordinary queue entry: when it ends, this client moves on by itself. A fresh
// load (issuedAt non-zero) is watched through to the sink playing so its cost is
// measured; an already-loaded file has nothing to measure.
func (p *Participant) play(ctx context.Context, local *localPlayback, item QueueItem, expected int64, issuedAt time.Time) {
	if local == nil {
		return
	}
	if expected < 0 {
		expected = 0
	}
	slot := SinkTrack{
		TrackID: local.trackID, VariantID: local.entry.VariantID, Path: local.entry.Path,
		DurationMs: local.entry.DurationMs, PositionMs: expected,
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
	local.ended = false
	local.generation++
	generation := local.generation
	p.mu.Unlock()

	go func() {
		err := p.sink.Play(playCtx, slot)
		finished := playCtx.Err() == nil
		p.mu.Lock()
		same := p.playing == local && p.playing.generation == generation
		if same {
			p.playing.playing = false
			p.playing.ended = finished
		}
		p.mu.Unlock()
		if err != nil && finished {
			p.logger.Warn("participant: playback failed", "variant", slot.VariantID, "error", err)
		}
		if finished && same {
			// The file ran out, so the next song starts here rather than on the
			// server's next beat. The server's state corrects this client if the
			// room disagrees.
			p.advanceLocal(ctx, item.ID)
		}
	}()

	if !issuedAt.IsZero() {
		if _, ok := p.sink.(Positioner); ok {
			go p.watchStart(playCtx, issuedAt, expected, local, generation)
		} else {
			// No playhead to watch: the load's own cost is all there is to time.
			p.measureLoad(time.Since(issuedAt))
		}
	}
}

// advanceLocal moves to the next item of the room's own play order when this
// client's file ends, without waiting for the server. The item played is
// remembered so a server state that still names it is not taken as a correction.
func (p *Participant) advanceLocal(ctx context.Context, itemID string) {
	p.mu.Lock()
	if p.playing == nil || p.playing.itemID != itemID || p.advancingItemID == itemID {
		p.mu.Unlock()
		return
	}
	p.advancingItemID = itemID
	p.played[itemID] = true
	p.master = dropQueueItem(p.master, itemID)
	next, found := QueueItem{}, false
	if len(p.master) > 0 {
		next, found = p.master[0], true
	}
	p.mu.Unlock()

	if !found {
		p.clearAdvancing(itemID)
		p.stop(ctx)
		return
	}
	p.load(ctx, &RoomPlayback{Item: next, DurationMs: next.DurationMs, Started: true}, 0)
	p.clearAdvancing(itemID)
}

func (p *Participant) hasPlayed(itemID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.played[itemID]
}

// forgetPlayed drops the local-advance memory once the server has caught up.
func (p *Participant) forgetPlayed() {
	p.mu.Lock()
	if len(p.played) > 0 {
		p.played = map[string]bool{}
	}
	p.mu.Unlock()
}

// pause pauses or resumes the local sink in place.
func (p *Participant) pause(ctx context.Context, local *localPlayback, paused bool) {
	pauser, ok := p.sink.(Pauser)
	if !ok {
		return
	}
	var err error
	if paused {
		err = pauser.Pause(ctx)
	} else {
		err = pauser.Resume(ctx)
	}
	if err != nil {
		p.logger.Warn("participant: pause", "error", err)
	}
	p.setPaused(local, paused)
}

func (p *Participant) clearAdvancing(itemID string) {
	p.mu.Lock()
	if p.advancingItemID == itemID {
		p.advancingItemID = ""
	}
	p.mu.Unlock()
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

// warmMaster warms the songs behind the one playing from the order this
// client already holds, so a queue change costs no round trip to act on.
func (p *Participant) warmMaster(ctx context.Context) {
	p.mu.Lock()
	pending := append([]QueueItem(nil), p.master...)
	p.mu.Unlock()
	p.warmAhead(ctx, pending)
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
