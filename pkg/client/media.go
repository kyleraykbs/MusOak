package client

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// mediaIdleTimeout is how long a media transfer may go without a byte before
// it is treated as dead. The media GET deliberately drops the request timeout
// because a rendition is large, so without this a server that stalls mid-file
// leaves the copy waiting forever.
const mediaIdleTimeout = 30 * time.Second

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
	resp.Body = newIdleBody(resp.Body, mediaIdleTimeout)
	return resp, nil
}

// idleBody fails a read once the stream has been silent for the timeout: the
// watchdog closes the underlying body, which unblocks whatever read is
// waiting. Every byte that arrives pushes the deadline back, so it measures a
// stall, not how long the whole rendition takes.
type idleBody struct {
	body    io.ReadCloser
	timeout time.Duration
	watch   *time.Timer
}

func newIdleBody(body io.ReadCloser, timeout time.Duration) *idleBody {
	idle := &idleBody{body: body, timeout: timeout}
	idle.watch = time.AfterFunc(timeout, func() { _ = body.Close() })
	return idle
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if n > 0 {
		b.watch.Reset(b.timeout)
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.watch.Stop()
	return b.body.Close()
}

// StreamInfo is what a streamed answer says about itself: whether the server is
// streaming a variant it has not finished downloading, and the track's length
// when the server knows it. A stream carries no length of its own - no
// Content-Length, no ETag, no ranges - so this is where a client draws a
// timeline for audio whose own length is still unknown.
type StreamInfo struct {
	// Streaming is true when the answer is a source stream rather than the
	// stored rendition; a request for a finished file is not streamed.
	Streaming bool
	// DurationMs is the track's length in milliseconds, zero when the server
	// does not know it.
	DurationMs int64
}

// MediaStreamInfo asks what a variant's URL would answer and reads only the
// headers: whether it is streaming, and how long the track is. The audio is not
// read and the response is closed at once, so this is a probe for the numbers a
// stream cannot carry, not a transfer. startMs should be the position the
// stream will actually be played from, so the length describes the song it is
// playing.
func (c *Client) MediaStreamInfo(ctx context.Context, variantID string, startMs int64) (StreamInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.MediaURL(variantID, startMs), nil)
	if err != nil {
		return StreamInfo{}, err
	}
	c.decorate(req)

	// A stream starts a provider fetch, which must not inherit the short
	// request timeout: the header arrives with the source's first bytes, and a
	// slow provider is not a slow request.
	hc := *c.http
	hc.Timeout = 0
	resp, err := hc.Do(req)
	if err != nil {
		return StreamInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return StreamInfo{}, decodeError(resp)
	}

	info := StreamInfo{Streaming: resp.Header.Get("X-Musoak-Stream") != ""}
	if raw := strings.TrimSpace(resp.Header.Get("X-Musoak-Duration")); raw != "" {
		if ms, err := strconv.ParseInt(raw, 10, 64); err == nil && ms > 0 {
			info.DurationMs = ms
		}
	}
	return info, nil
}

// Sink plays a rendition. The CLI's sink drives a local player; a Discord
// bot's sink streams the file into a voice channel. Nothing in this package
// cares which.
type Sink interface {
	// Play starts what the track names and returns when it finishes or ctx ends:
	// a file this client holds, or the server's own answer for a rendition it
	// does not - a stream, which begins at the position in the track rather than
	// being seeked to it, or a stored file served over HTTP, which is seeked.
	// The participant advances to the next song itself when it returns, and a
	// stream-taking-over handover arrives as one more Play of the same track.
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

// SinkTrack is one rendition handed to a Sink: a file this client holds, or,
// when it does not hold it yet, the server's own answer for it.
type SinkTrack struct {
	TrackID   string
	VariantID string
	// Title is the song's name, for a sink that says what it is playing.
	Title string
	// Path is what to play: the file on disk, or a URL of the server's.
	Path string
	// DurationMs is the song's length: the file's own once it is here, and the
	// length the room reports while the rendition is still arriving.
	DurationMs int64
	// PositionMs is where in the song playback should begin. A file is played
	// from there; a stream already begins there - the URL says so - because a
	// stream has no length to seek within.
	PositionMs int64
	// Streaming is true when Path is the server's stream of a rendition it has
	// not finished preparing: a copy of the source arriving now, marked
	// X-Musoak-Stream, with no length of its own and no ranges. A sink must not
	// seek one - PositionMs is where in the song it begins, not where to jump
	// to - and must not read its end as the song ending, because the file
	// behind it takes over the moment it lands.
	Streaming bool
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

	// warmConcurrency is how many prefetches may run at once.
	warmConcurrency = 2

	// maxResyncBackoff caps how long a repeated clock-resync failure waits
	// before trying again, so a server that is down is not asked once every
	// resyncEvery forever.
	maxResyncBackoff = 2 * time.Minute

	// streamGap is the least time between two requests for one song's stream. A
	// stream request buys the provider's own start-up, so a stream is not
	// re-requested on every beat even when the drift rule would like one.
	streamGap = 2 * time.Second
	// streamRetryStep and streamRetryMax bound the wait before a stream that
	// would not play is asked for again. The download behind it is usually the
	// answer, and the waits double, so a provider that is refusing is not asked
	// once a beat while its file is still coming.
	streamRetryStep = 500 * time.Millisecond
	streamRetryMax  = 10 * time.Second
	// startWatchLimit is how long a load is watched for the sink to start
	// playing before its cost is left unmeasured, and startWatchStream is the
	// same for a stream: nothing that has not started by then is going to. A
	// stream is given longer because its start is somebody else's provider
	// beginning to send, not this machine opening a file.
	startWatchLimit  = 5 * time.Second
	startWatchStream = 10 * time.Second
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
	// resyncBackoff is how long the next clock-resync attempt waits after a
	// run of failures; zero means the normal interval.
	resyncBackoff time.Duration

	OnEvent func(Event)
	OnTrack func(SinkTrack)

	mu       sync.Mutex
	offsetMs int64
	current  *RoomPlayback
	master   []QueueItem
	// played names items this client has advanced past locally, for the life
	// of the participant. A server state that still names one of them is a
	// beat behind the client, not a correction to apply - and it must stay
	// that way: this client reads ahead of what it is playing, so its own
	// advance comes before the room's, and a memory that was cleared once the
	// two agreed let a later stale beat load the finished song again, at the
	// finished song's position. Queue entries are unique and play once, so
	// remembering them costs nothing and forbids the jump back.
	played map[string]bool
	// loadLatencyMs is the smoothed time a load takes, from asking for the file
	// to the sink being ready; it is added to the room's position when starting
	// a load so the audible start lands where the room will be, not where it was.
	loadLatencyMs int64
	// streamLatencyMs is the same estimate for a stream, and a separate one on
	// purpose: a file is this device reading one, while a stream is the
	// provider starting to send it, and a file load scheduled a stream's two
	// seconds ahead would start the song two seconds past the room.
	streamLatencyMs int64
	// variants is the rendition this member chose per track - and whether the
	// server is still preparing it - so a stream re-requested on a seek costs
	// no round trip, and a track whose file never lands is not resolved again
	// on every beat.
	variants map[string]rendition
	// absurdSaid is whether the too-large-to-correct warning has been said for
	// the song playing.
	absurdSaid bool
	// lastExpected and lastItemID are the room's position and song at the
	// previous correction check: a jump in the room's own position between two
	// of them is the room being seeked, which is to be followed however far,
	// while a standing disagreement is what the guard is for.
	lastExpected int64
	lastItemID   string

	playing *localPlayback
	warming map[string]bool
	// warmSlots bounds how many prefetches run at once: a queue burst —
	// somebody shuffling a playlist into the room — is one warm request per
	// event, and letting them all fetch together starves the event loop that
	// has to keep reading the socket.
	warmSlots       chan struct{}
	lastSeq         int64
	advancingItemID string
}

// localPlayback is the song this participant has on its sink.
type localPlayback struct {
	itemID  string
	trackID string
	// entry is the rendition in hand. Its VariantID is the one being played and
	// fetched; its Path is the file once that file is here, and is empty while
	// the sink holds the server's own answer instead.
	entry      CacheEntry
	paused     bool
	playing    bool
	ended      bool
	cancel     context.CancelFunc
	generation int

	// durationMs is how long the song runs. A rendition with no file here
	// cannot say, so this is the figure the room reports, and it is what a
	// streamed timeline and the follower rule are drawn from until the file
	// lands and its own length can be read.
	durationMs int64
	// streaming is true while the sink holds the server's stream of a rendition
	// that is not here yet. A stream is not a file: it has no length to seek
	// within, so moving it means asking for it at another position again, and
	// its end is the provider's bytes stopping rather than the song being over.
	streaming bool
	// streamStartMs is where in the song the stream's bytes begin - the
	// ?start= its URL carries. A sink reports a stream's playhead relative to
	// that beginning, so this is what turns it into a position in the song.
	streamStartMs int64
	// streamNextAt is the earliest another stream of this song may be asked
	// for, and streamTries counts the attempts that would not play: each
	// request costs the provider's start-up, so they are spaced, and a run of
	// failures backs off.
	streamNextAt time.Time
	streamTries  int
	// resolving is whether the rendition is being chosen right now, and
	// fetching whether its file is being downloaded: both are goroutines this
	// participant owns, never work the reconcile loop waits for.
	resolving bool
	fetching  bool
}

// length is how long the song runs: the file's own length once it is here, and
// the length the room reports while the rendition is still arriving.
func (l *localPlayback) length() int64 {
	if l.entry.Path != "" && l.entry.DurationMs > 0 {
		return l.entry.DurationMs
	}
	return l.durationMs
}

// rendition is what this member will play and fetch for one track: the variant
// it chose, and whether the server is still preparing it.
type rendition struct {
	variantID string
	// streaming is whether the server is still preparing this variant. One it
	// already holds is answered with the stored file - a file over HTTP, which
	// a player seeks to a position - where one it is still preparing is a
	// stream, beginning at the position its request names.
	streaming bool
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
		variants:       map[string]rendition{},
		warmSlots:      make(chan struct{}, warmConcurrency),
	}
}

// SetDriftTolerance sets how far this client may sit from the room's position
// before it seeks back, in milliseconds.
//
// It belongs to the caller's pipeline: a client that keeps a second of audio
// buffered ahead of the wire is a second behind by design, and correcting that
// is correcting the buffer, not the playback. A caller that buffers should set
// this past its buffer - otherwise every beat is a correction.
func (p *Participant) SetDriftTolerance(ms int64) {
	if ms < 0 {
		ms = 0
	}
	p.mu.Lock()
	p.driftTolerance = ms
	p.mu.Unlock()
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
	// A failed estimate at boot is not a reason to refuse the room: the offset
	// only has to be close for the follower rule, and the resync fixes it. The
	// old code returned here, so one failed dial meant the participant never
	// started at all.
	if offset, err := p.room.client.ClockOffset(ctx, p.clockSamples); err != nil {
		p.logger.Warn("participant: initial clock sync failed; starting with the local clock", "error", err)
	} else {
		p.setOffset(offset)
	}

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
	resync := time.NewTimer(p.resyncEvery)
	defer resync.Stop()

	for {
		select {
		case <-ctx.Done():
			p.stop(ctx)
			return ctx.Err()
		case event, ok := <-events:
			if !ok {
				// The connection to the room dropped; the room did not. What
				// this client is playing keeps playing - the socket is how the
				// room talks to it, not what it is listening to - and the
				// caller reconnects and follows again. Stopping the sink here
				// is what turned a blink into a hole in the music: a second of
				// song, then silence until the follower came back.
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
			resync.Reset(p.resyncClock(ctx))
		}
	}
}

// resyncClock re-estimates the offset and returns how long to wait before the
// next attempt. A failed dial keeps the last good offset - the estimate only
// has to be close - and backs off, instead of dialling the socket every
// resyncEvery while the server is unreachable.
func (p *Participant) resyncClock(ctx context.Context) time.Duration {
	offset, err := p.room.client.ClockOffset(ctx, 3)
	if err == nil {
		p.setOffset(offset)
		p.mu.Lock()
		p.resyncBackoff = 0
		p.mu.Unlock()
		return p.resyncEvery
	}
	p.logger.Warn("participant: clock resync failed; keeping the last offset", "error", err)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.resyncBackoff == 0 {
		p.resyncBackoff = p.resyncEvery
	} else {
		p.resyncBackoff *= 2
	}
	if p.resyncBackoff > maxResyncBackoff {
		p.resyncBackoff = maxResyncBackoff
	}
	return p.resyncBackoff
}

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
		// The live ctx, not a fresh background one: a dead server must not
		// freeze the loop past the caller's deadline - or ignore Ctrl-C - for
		// as long as the request timeout.
		if snapshot, err := p.room.Snapshot(ctx); err == nil {
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
		p.load(ctx, current, p.startAt(current, p.willStream(item.TrackID)))
		return
	}
	if localEnded {
		p.advanceLocal(ctx, item.ID)
		return
	}
	if !localPlaying {
		p.load(ctx, current, p.startAt(current, p.willStream(item.TrackID)))
		return
	}
	if localPaused {
		p.pause(ctx, local, false)
		return
	}
	p.correctDrift(ctx, local, current)
}

// willStream reports whether this member's next load of the track will be the
// server's stream of it rather than a file. It is what picks the start
// estimate, so it answers from the rendition this member already chose when
// there is one, and otherwise from whether anything is on disk.
func (p *Participant) willStream(trackID string) bool {
	if _, ok := p.cache.LookupTrack(trackID); ok {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if chosen, ok := p.variants[trackID]; ok {
		return chosen.streaming
	}
	// Nothing here and nothing chosen yet: what the server answers with is a
	// stream unless it turns out to hold the file already.
	return true
}

// startAt is where a load should start so it lands on the room's position: the
// place the room will be once the load has taken its smoothed time. A stream
// and a file do not take the same time to start, so each is drawn from its own
// estimate.
func (p *Participant) startAt(current *RoomPlayback, stream bool) int64 {
	p.mu.Lock()
	latency := p.loadLatencyMs
	if stream {
		latency = p.streamLatencyMs
	}
	p.mu.Unlock()
	return current.PositionAt(p.serverNow() + latency)
}

// load puts the room's song on the sink at the given position. A rendition this
// member already holds is played from its file; one it does not is handed to
// the sink as the server's own answer for it - its stream, beginning where the
// room is - so the song is heard in the time the provider takes to start
// sending instead of after the whole download, with the file fetched behind it
// and taken over the moment it lands (see playFromServer).
//
// Nothing here fetches, resolves or waits: it runs on the event and drift loop,
// and both the server's own fetch and this client's download belong to
// goroutines this participant owns. A download on this loop is what froze it -
// seconds to minutes in which no event, no drift correction and no command is
// read, and the socket back-fills until the server drops the member.
//
// If a fresh file, the load is timed from issue through to the sink actually
// playing and the estimate is smoothed, so the next load can be started in time
// to land on the room's beat.
func (p *Participant) load(ctx context.Context, current *RoomPlayback, expected int64) {
	item := current.Item
	p.mu.Lock()
	local := p.playing
	p.mu.Unlock()

	if local != nil && local.itemID == item.ID {
		if local.entry.Path == "" {
			// The same song, still not here: it is asked for again rather than
			// loaded, because a stream is what the sink will play.
			p.playFromServer(ctx, local, current, expected)
			return
		}
		p.play(ctx, local, item, expected, time.Time{})
		return
	}

	if entry, ok := p.cache.LookupTrack(item.TrackID); ok {
		issuedAt := time.Now()
		p.setLocal(&localPlayback{
			itemID: item.ID, trackID: item.TrackID, entry: entry,
			durationMs: roomLength(current),
		})
		p.play(ctx, p.local(), item, expected, issuedAt)
		return
	}

	// Not here yet: the sink hears the server's stream of it while the file
	// arrives, which is the whole point of not waiting for the download.
	p.setLocal(&localPlayback{
		itemID: item.ID, trackID: item.TrackID, durationMs: roomLength(current),
	})
	p.playFromServer(ctx, p.local(), current, expected)
}

// roomLength is how long the room's current song runs, as the room reports it:
// the playback's own figure, else the queue entry's.
func roomLength(current *RoomPlayback) int64 {
	if current == nil {
		return 0
	}
	if current.DurationMs > 0 {
		return current.DurationMs
	}
	return current.Item.DurationMs
}

// playFromServer puts the room's song on the sink from the server's own answer
// for it - its stream of a rendition that is not downloaded yet, or the file it
// holds - and has the file fetched behind it so the sink can take it over when
// it lands. The song is heard now rather than when the download finishes.
//
// It never waits for anything: asking the sink to play a URL is one message to
// it, and both the round trip that chooses the rendition and the transfer that
// fetches it run in goroutines this participant owns. The reconcile loop only
// arranges them.
//
// expected is where in the song this member should begin. A stream starts at
// the position its request names rather than seeking to it, so that is where
// the room will be when the audio actually starts.
func (p *Participant) playFromServer(ctx context.Context, local *localPlayback, current *RoomPlayback, expected int64) {
	if local == nil {
		return
	}
	item := current.Item
	// Nothing to hear past the end of the song: the room is about to move on,
	// and a fetch that begins at the last second of a track plays nothing.
	if length := local.length(); length > 0 && expected >= length {
		return
	}

	p.mu.Lock()
	switch {
	case p.playing != local, local.entry.Path != "":
		// The song moved on under this, or the file landed while it was being
		// arranged.
		p.mu.Unlock()
		return
	case time.Now().Before(local.streamNextAt):
		// A stream was asked for too recently to ask again: each request buys
		// the provider's own start-up, so they are spaced rather than made
		// every beat.
		p.mu.Unlock()
		return
	}
	if local.entry.VariantID != "" {
		// The rendition is already this member's: this is the same song asked
		// for again, at the position the caller wants.
		local.arrange(expected)
		variantID := local.entry.VariantID
		p.mu.Unlock()
		// Timed from the ask: what is measured is how long this rendition takes
		// to start being heard, which is the provider beginning to send it.
		p.play(ctx, local, item, expected, time.Now())
		p.fetchBehind(ctx, local, item, variantID)
		return
	}
	if local.resolving {
		p.mu.Unlock()
		return
	}
	local.resolving = true
	p.mu.Unlock()

	// Which rendition this member plays and fetches is one round trip away, and
	// the loop is not the place to spend it.
	go func() {
		chosen, err := p.chooseRendition(ctx, item.TrackID)
		// The room has moved while the rendition was being chosen: begin where
		// it will be once the sink has it, not where it was when the loop asked.
		// Drawn before the lock, because the estimate is read under it.
		start := int64(0)
		if err == nil {
			start = p.startAt(current, chosen.streaming)
		}

		p.mu.Lock()
		local.resolving = false
		if err != nil {
			// Nothing to hand the sink: the room's next beat tries again, and
			// the wait doubles while it keeps failing.
			local.streamTries++
			local.streamNextAt = time.Now().Add(retryWait(local.streamTries))
			p.mu.Unlock()
			p.logger.Warn("participant: no rendition to play the room's song from",
				"track", item.TrackID, "error", err)
			return
		}
		if p.playing != local || local.entry.Path != "" {
			p.mu.Unlock()
			return
		}
		local.entry.VariantID = chosen.variantID
		local.streaming = chosen.streaming
		local.arrange(start)
		p.mu.Unlock()

		// Timed from the ask, so the next stream of this member can be asked
		// for at the position the room will be at when it starts: what is
		// measured is the provider's own start-up, not the round trip that
		// chose the rendition.
		p.play(ctx, local, item, start, time.Now())
		p.fetchBehind(ctx, local, item, chosen.variantID)
	}()
}

// arrange records how the sink will be asked to play this song on the next
// play: a stream begins at the position its URL names, a file the server holds
// is seeked to the position like any other, and either way the next request for
// this song is spaced - a stream request buys the provider's own start-up.
//
// The caller holds p.mu.
func (local *localPlayback) arrange(expected int64) {
	local.streamNextAt = time.Now().Add(streamGap)
	if local.streaming {
		local.streamStartMs = expected
		return
	}
	local.streamStartMs = 0
}

// retryWait is how long the nth attempt at something that would not play waits:
// half a second, doubling to ten, which is about the time a download behind a
// stream needs to land.
func retryWait(tries int) time.Duration {
	wait := streamRetryStep
	for i := 1; i < tries && wait < streamRetryMax; i++ {
		wait *= 2
	}
	return min(wait, streamRetryMax)
}

// watchStart times a load through to the sink actually playing: from the moment
// the load was issued to the first position the sink reports at or past the
// start. The sink's own start latency is what the room's boundary has to absorb,
// and it is not observable any other way through the Sink interface. A stream
// and a file are timed into their own estimates, because they are not the same
// quantity.
func (p *Participant) watchStart(ctx context.Context, issuedAt time.Time, expected int64, local *localPlayback, generation int) {
	positioner, ok := p.sink.(Positioner)
	if !ok {
		return
	}
	target := expected + 50
	if target < 50 {
		target = 50
	}
	p.mu.Lock()
	limit := startWatchLimit
	if local.streaming {
		limit = startWatchStream
	}
	p.mu.Unlock()
	// Nothing that has not started in that long is going to: the estimate keeps
	// its old value rather than waiting longer.
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		same := p.playing == local && p.playing.generation == generation
		var stream bool
		var startOffset int64
		if same {
			stream, startOffset = local.streaming, local.streamStartMs
		}
		p.mu.Unlock()
		if !same {
			return
		}
		// A stream's playhead is relative to the bytes the URL began at, so the
		// offset it started at is added back: what is timed is the sink reaching
		// the position in the song it was asked for.
		if position, err := positioner.PositionMs(ctx); err == nil && position+startOffset >= target {
			p.measureLoad(time.Since(issuedAt), stream)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// measureLoad folds one load's duration into the smoothed estimate for what it
// timed. A load from cache costs only the sink's own start, so the estimate
// settles on what starting a song from this device actually takes; a stream
// costs the provider starting to send it, which is a different and much larger
// figure - a file load scheduled a stream's two seconds ahead would start the
// song two seconds past the room.
func (p *Participant) measureLoad(took time.Duration, stream bool) {
	sample := took.Milliseconds()
	if sample < 0 || sample > 10_000 {
		// A wild sample is a stalled fetch, not a load time: leave the estimate
		// alone rather than skewing it for the next songs.
		return
	}
	p.mu.Lock()
	estimate := &p.loadLatencyMs
	if stream {
		estimate = &p.streamLatencyMs
	}
	if *estimate == 0 {
		*estimate = sample
	} else {
		*estimate = (*estimate*3 + sample) / 4
	}
	p.mu.Unlock()
}

// play hands the sink the song at the given position and runs it as an ordinary
// queue entry: a file this member holds, or the server's own answer for the
// rendition - a stream of a source the server is still fetching, or the file it
// holds, which is a file over HTTP. A fresh load (issuedAt non-zero) is watched
// through to the sink playing so its cost is measured; an already-loaded file
// has nothing to measure.
func (p *Participant) play(ctx context.Context, local *localPlayback, item QueueItem, expected int64, issuedAt time.Time) {
	if local == nil {
		return
	}
	if expected < 0 {
		expected = 0
	}
	source, streaming := local.entry.Path, false
	if source == "" {
		if local.entry.VariantID == "" {
			// No rendition chosen yet: there is nothing to hand a sink.
			return
		}
		source, streaming = p.room.client.MediaURL(local.entry.VariantID, local.streamStartMs), local.streaming
	}
	slot := SinkTrack{
		TrackID: local.trackID, VariantID: local.entry.VariantID, Title: item.Title,
		Path: source, DurationMs: local.length(), PositionMs: expected, Streaming: streaming,
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
		stream := false
		if same {
			stream = local.streaming
			local.playing = false
			// A file that ran out is the song over, and this client moves on by
			// itself - the room's next beat corrects it if the room disagrees.
			// A stream that ran out is the provider's bytes stopping: the room
			// is the only one that says when the song is over, so a stream is
			// asked for again rather than run past, and one that would not play
			// at all is waited for rather than skipped.
			local.ended = finished && !stream
			if stream {
				local.streamTries++
				local.streamNextAt = time.Now().Add(retryWait(local.streamTries))
			}
		}
		p.mu.Unlock()
		if err != nil && finished {
			p.logger.Warn("participant: playback failed", "variant", slot.VariantID, "error", err)
		}
		if finished && same && !stream {
			p.advanceLocal(ctx, item.ID)
		}
	}()

	if !issuedAt.IsZero() {
		if _, ok := p.sink.(Positioner); ok {
			go p.watchStart(playCtx, issuedAt, expected, local, generation)
		} else {
			// No playhead to watch: the load's own cost is all there is to time.
			p.measureLoad(time.Since(issuedAt), streaming)
		}
	}
}

// fetchBehind gets the rendition a stream is playing onto disk, so the sink can
// be handed the file the moment it lands. The transfer is detached from the
// caller's context - a skip, a room change or quitting must not strand a
// half-written file, exactly as the server detaches its own transfer from the
// request that started it - and it is the download the stream already started on
// the server's side that this joins.
func (p *Participant) fetchBehind(ctx context.Context, local *localPlayback, item QueueItem, variantID string) {
	p.mu.Lock()
	if local.fetching || p.playing != local {
		p.mu.Unlock()
		return
	}
	local.fetching = true
	p.mu.Unlock()

	go func() {
		defer func() {
			p.mu.Lock()
			local.fetching = false
			p.mu.Unlock()
		}()
		entry, err := p.fetch(context.WithoutCancel(ctx), variantID, item.TrackID)
		if err != nil {
			if entry.Path == "" {
				p.logger.Warn("participant: could not fetch the rendition being streamed",
					"track", item.TrackID, "variant", variantID, "error", err)
				return
			}
			// The file is here and playable; only indexing it failed.
			p.logger.Warn("participant: index the fetched rendition", "track", item.TrackID, "error", err)
		}
		p.handOver(ctx, local, item, entry)
	}()
}

// handOver moves the sink from what the server was sending onto the file that
// just landed, at the position the music had reached. The file is the whole song
// while a stream began part-way in, so the offset the stream began at is added
// back to the sink's playhead before the file starts there: the handover is the
// same moment of the song rather than a jump in it.
func (p *Participant) handOver(ctx context.Context, local *localPlayback, item QueueItem, entry CacheEntry) {
	// Read the playhead first: the sink has to be aiming at the file from where
	// the stream is now, and the stream is still playing while this is read.
	position := p.playhead(ctx, local)

	p.mu.Lock()
	if p.playing != local || local.entry.Path != "" {
		// The member has moved on, or the file is already what is playing.
		p.mu.Unlock()
		return
	}
	wasPaused := local.paused
	local.entry = entry
	local.streaming = false
	local.streamStartMs = 0
	local.streamNextAt = time.Time{}
	p.mu.Unlock()

	p.play(ctx, local, item, position, time.Time{})
	if wasPaused {
		p.pause(ctx, local, true)
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

// correctDrift nudges the sink back onto the room's position. A file is seeked
// there; a stream, which has no length and no ranges to seek within, is asked
// for again at the room's position, which is the only move it has.
func (p *Participant) correctDrift(ctx context.Context, local *localPlayback, current *RoomPlayback) {
	positioner, ok := p.sink.(Positioner)
	seeker, canSeek := p.sink.(Seeker)
	if !ok {
		return
	}
	expected := current.PositionAt(p.serverNow())
	// Was the room's own position jumped between this check and the last one?
	// The room advances about a second per check, so anything more is somebody
	// seeking it — the web's bar, another listener, the song being restarted —
	// and that is followed however far it went, in either direction. Recorded
	// before any early return, because the check that saw the room at its old
	// position is exactly the one to compare against. Without this, a seek looks
	// like the broken position the guard below exists for, and the client
	// ignores it.
	p.mu.Lock()
	previous, previousItem := p.lastExpected, p.lastItemID
	p.lastExpected, p.lastItemID = expected, local.itemID
	start, streaming, nextAt, tolerance := local.streamStartMs, local.streaming, local.streamNextAt, p.driftTolerance
	p.mu.Unlock()
	roomJumped := previousItem == local.itemID && absMs(expected-previous) > roomSeekJumpMs
	if roomJumped {
		p.logger.Info("participant: the room was seeked; following it",
			"from_ms", previous, "to_ms", expected)
	}

	if length := local.length(); length > 0 && expected >= length {
		// This client's file is over; the room is still on it. Wait for the
		// room to move, which is what "a song ends slightly early" means.
		return
	}
	raw, err := positioner.PositionMs(ctx)
	if err != nil {
		return
	}
	// A stream's playhead is relative to the bytes its URL began at: every
	// position here is a position in the song.
	got := raw + start
	drift := absMs(got - expected)
	if drift <= tolerance {
		return
	}
	// A drift this large is not a file that runs at the wrong speed: it is two
	// positions that do not describe the same thing — a different item, or a
	// clock that moved under both of them. Moving the audio to it lands
	// somewhere worse and the next beat moves it again, so say it once and wait
	// for the room to move instead of tearing the song apart every second.
	if drift > absurdDriftMs && !roomJumped {
		p.sayAbsurdDrift(got, expected)
		return
	}
	if streaming {
		if raw <= 0 {
			// The stream has not played a byte yet: what the first beats measure
			// is the provider starting to send, not a drift, and asking for it
			// again only starts the wait over.
			return
		}
		if time.Now().Before(nextAt) {
			// A request buys the provider's start-up, so a correction waits its
			// turn instead of being made on every beat.
			return
		}
		p.logger.Debug("participant: the stream is off the room; asking for it again",
			"drift_ms", got-expected, "got_ms", got, "expected_ms", expected)
		p.playFromServer(ctx, local, current, p.startAt(current, true))
		return
	}
	if !canSeek {
		return
	}
	p.logger.Debug("participant: correcting drift",
		"drift_ms", got-expected, "got_ms", got, "expected_ms", expected)
	if err := seeker.Seek(ctx, expected); err != nil {
		p.logger.Warn("participant: drift correction failed", "error", err)
	}
}

// absMs is how far apart two positions are, whichever way round they came.
func absMs(ms int64) int64 {
	if ms < 0 {
		return -ms
	}
	return ms
}

// sinkPosition is where in the song this member's own player is: the sink's
// playhead, with the offset a stream began at added back. A stream's playhead is
// relative to the bytes it was handed, and every position this package compares
// is a position in the song.
func (p *Participant) sinkPosition(ctx context.Context, local *localPlayback) (int64, bool) {
	positioner, ok := p.sink.(Positioner)
	if !ok {
		return 0, false
	}
	raw, err := positioner.PositionMs(ctx)
	if err != nil {
		return 0, false
	}
	p.mu.Lock()
	start := int64(0)
	if local != nil {
		start = local.streamStartMs
	}
	p.mu.Unlock()
	return raw + start, true
}

// playhead is where in the song this member is, for the purpose of taking over
// from what it is playing: its own player where the sink can say, and the room's
// position - the clock this member is meant to sit on - where it cannot.
func (p *Participant) playhead(ctx context.Context, local *localPlayback) int64 {
	if position, ok := p.sinkPosition(ctx, local); ok {
		return position
	}
	return p.Position()
}

// absurdDriftMs is how far apart two positions have to be before correcting
// them stops being about playback and starts being about a broken comparison.
const absurdDriftMs = 60_000

// roomSeekJumpMs is how far the room's own position may move between two
// correction checks before it is somebody seeking rather than time passing.
const roomSeekJumpMs = 2_000

// sayAbsurdDrift reports a drift too large to be a drift, once per song: a
// warning every beat for a condition that is not going away is noise, and the
// number is what matters.
func (p *Participant) sayAbsurdDrift(got, expected int64) {
	p.mu.Lock()
	said := p.absurdSaid
	p.absurdSaid = true
	p.mu.Unlock()
	if said {
		return
	}
	p.logger.Warn("participant: drift far too large to correct; waiting for the room to move",
		"drift_ms", got-expected, "got_ms", got, "expected_ms", expected)
}

// ensureTrack makes sure a rendition of the track is on disk locally, so a song
// reached later does not pay for its download then. A copy this client already
// has is used as-is; otherwise the rendition it chose for that track is
// fetched, which is this member's own choice of version.
func (p *Participant) ensureTrack(ctx context.Context, trackID string) (CacheEntry, error) {
	if entry, ok := p.cache.LookupTrack(trackID); ok {
		return entry, nil
	}
	chosen, err := p.chooseRendition(ctx, trackID)
	if err != nil {
		return CacheEntry{}, err
	}
	return p.fetch(ctx, chosen.variantID, trackID)
}

// chooseRendition is the rendition this member will play and fetch for a track:
// the copy in hand, else the first downloadable rendition the server offers,
// which is this member's own choice of version. It reports whether the server is
// still preparing that rendition, because a stream begins at the position its
// request names where the stored file a server already holds is a file over
// HTTP that a player seeks. The answer is remembered per track, so a stream
// re-requested on a correction costs no round trip.
func (p *Participant) chooseRendition(ctx context.Context, trackID string) (rendition, error) {
	if entry, ok := p.cache.LookupTrack(trackID); ok {
		return rendition{variantID: entry.VariantID}, nil
	}
	p.mu.Lock()
	chosen, ok := p.variants[trackID]
	p.mu.Unlock()
	if ok {
		return chosen, nil
	}

	variants, err := p.room.client.TrackVariants(ctx, trackID)
	if err != nil {
		return rendition{}, err
	}
	chosen, ok = pickRendition(variants)
	if !ok {
		return rendition{}, errors.New("musoak: no downloadable rendition")
	}
	p.mu.Lock()
	p.variants[trackID] = chosen
	p.mu.Unlock()
	return chosen, nil
}

// pickRendition is the first downloadable rendition of a track, and whether the
// server is still preparing it: one it already holds is answered with the stored
// file, and one it is still fetching is a stream.
func pickRendition(variants []Variant) (rendition, bool) {
	for _, variant := range variants {
		if variant.Downloadable {
			return rendition{variantID: variant.ID, streaming: variant.Media.State != MediaReady}, true
		}
	}
	return rendition{}, false
}

// fetch downloads a variant and indexes it under its track, so the next play of
// that track is a file rather than a stream. The entry comes back either way: a
// failure to index a file that landed still leaves a playable file behind, which
// is why callers check the path and not only the error.
func (p *Participant) fetch(ctx context.Context, variantID, trackID string) (CacheEntry, error) {
	entry, err := p.cache.Fetch(ctx, p.room.client, variantID)
	if err != nil {
		return CacheEntry{}, err
	}
	entry.TrackID = trackID
	if err := p.cache.Put(entry); err != nil {
		return entry, err
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
		select {
		case p.warmSlots <- struct{}{}:
			defer func() { <-p.warmSlots }()
		case <-ctx.Done():
			return
		}
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

// LocalPosition reports where this client's own player is in the song, when its
// sink can say so. A stream's playhead is relative to where that stream began,
// so the offset is added back: the answer is a position in the song, which is
// the only kind of position this package talks about.
func (p *Participant) LocalPosition(ctx context.Context) (int64, bool) {
	return p.sinkPosition(ctx, p.local())
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
	p.absurdSaid = false
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
