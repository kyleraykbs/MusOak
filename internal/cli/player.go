package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/kyleraykbs/MusOak/pkg/client"
)

const (
	// statusInterval is how often the status line is redrawn.
	statusInterval = 200 * time.Millisecond
	// volumeStep is how much one volume key press moves the level.
	volumeStep = 5
)

// player drives the queue through mpv and, on a terminal, keeps a status line
// with keyboard controls:
//
//	k / ↑   volume up        j / ↓   volume down
//	l / →   next track       h / ←   previous track
//	space   pause/resume     q       quit
type player struct {
	out   io.Writer
	sink  *MpvSink
	queue []queueItem

	// mediaStatus reads one variant's download state, which is what the status
	// line says while the playing track is still coming down. It is nil for a
	// player with no server to ask, which then says nothing about downloads.
	mediaStatus func(ctx context.Context, variantID string) (*client.MediaStatus, error)

	// streamInfo reads what a variant's stream says about itself, the length
	// above all: a stream carries none of its own, and the status line needs
	// one for the song that is arriving. It is nil for a player with no server
	// to ask.
	streamInfo func(ctx context.Context, variantID string, startMs int64) (client.StreamInfo, error)

	interactive bool
	keys        *keyReader
	restoreTerm func()
	stopOnce    sync.Once
	stopped     chan struct{}

	// drawn serialises whole status lines.
	drawn sync.Mutex

	// items are the queue entries handed to mpv, in playlist order. position
	// indexes into them, which is how a landed copy is matched to the track
	// currently playing.
	items []playbackItem

	mu          sync.Mutex
	loaded      int
	pendingSkip int
	position    int
	count       int
	title       string
	positionMs  int64
	durationMs  int64
	volume      int
	paused      bool
	message     string
	// download is what the playing entry's download is doing, rendered for the
	// status line and empty when nothing is being fetched.
	download string
}

// newPlayer returns a player for the queue.
func newPlayer(out io.Writer, sink *MpvSink, queue []queueItem) *player {
	return &player{
		out:      out,
		sink:     sink,
		queue:    queue,
		position: -1,
		stopped:  make(chan struct{}),
	}
}

// start enters raw mode and begins handling keys. Without a terminal on stdin
// and stdout, playback stays non-interactive.
func (p *player) start(ctx context.Context) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return
	}
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return
	}
	p.restoreTerm = func() {
		_ = term.Restore(fd, oldState)
		fmt.Fprint(p.out, "\r\n")
	}
	p.interactive = true
	p.keys = newKeyReader(bufio.NewReader(os.Stdin))

	fmt.Fprintln(p.out, "controls: k/↑ volume up · j/↓ volume down · l/→ next · h/← previous · space pause · q quit")

	go p.keyLoop(ctx)
	go p.statusLoop(ctx)
}

// stop leaves raw mode; safe to call twice.
func (p *player) stop() {
	p.stopOnce.Do(func() {
		if p.restoreTerm != nil {
			p.restoreTerm()
		}
		close(p.stopped)
	})
}

// quit stops playback and ends wait().
func (p *player) quit() {
	p.stop()
}

func (p *player) done() <-chan struct{} { return p.stopped }

func (p *player) keyLoop(ctx context.Context) {
	for {
		k := p.keys.next(statusInterval)
		if k == keyNone {
			select {
			case <-ctx.Done():
				return
			case <-p.stopped:
				return
			default:
				continue
			}
		}
		p.handle(ctx, k)
	}
}

func (p *player) statusLoop(ctx context.Context) {
	ticker := time.NewTicker(statusInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stopped:
			return
		case <-ticker.C:
			p.refresh(ctx)
			p.draw()
		}
	}
}

// handle applies one key press.
func (p *player) handle(ctx context.Context, k key) {
	switch k {
	case keyQuit:
		_ = p.sink.Stop(ctx)
		p.quit()
	case keyVolumeUp:
		p.changeVolume(ctx, volumeStep)
	case keyVolumeDown:
		p.changeVolume(ctx, -volumeStep)
	case keyPause:
		p.togglePause(ctx)
	case keyNext:
		p.skip(ctx, 1)
	case keyPrevious:
		p.skip(ctx, -1)
	}
}

func (p *player) changeVolume(ctx context.Context, delta int) {
	current, err := p.sink.process.volume(ctx)
	if err != nil {
		p.setMessage(fmt.Sprintf("volume unavailable: %v", err))
		return
	}
	next := current + delta
	if next < 0 {
		next = 0
	}
	if next > 100 {
		next = 100
	}
	if err := p.sink.process.setVolume(ctx, next); err != nil {
		p.setMessage(fmt.Sprintf("volume unavailable: %v", err))
		return
	}

	p.mu.Lock()
	p.volume = next
	p.message = ""
	p.mu.Unlock()
	p.draw()
}

func (p *player) togglePause(ctx context.Context) {
	paused, err := p.sink.process.pauseState(ctx)
	if err != nil {
		return
	}
	if err := p.sink.process.setPause(ctx, !paused); err != nil {
		return
	}
	p.mu.Lock()
	p.paused = !paused
	p.mu.Unlock()
	p.draw()
}

// skip moves to the next or previous entry. A track that is still downloading
// is remembered and skipped to as soon as it is queued, so no key press is
// swallowed.
func (p *player) skip(ctx context.Context, direction int) {
	position, err := p.sink.process.playlistPosition(ctx)
	if err != nil {
		return
	}
	count, err := p.sink.process.playlistCount(ctx)
	if err != nil {
		return
	}

	target := position + direction
	if target < 0 {
		return
	}
	if target >= count {
		p.mu.Lock()
		p.pendingSkip += direction
		p.message = "next track is still downloading"
		p.mu.Unlock()
		p.draw()
		return
	}

	if direction > 0 {
		_ = p.sink.process.nextEntry(ctx)
	} else {
		_ = p.sink.process.previousEntry(ctx)
	}
	p.mu.Lock()
	p.message = ""
	p.mu.Unlock()
	p.refresh(ctx)
	p.draw()
}

// startTrack loads the first entry. When its local copy is not on disk yet it
// is streamed from its URL, and the copy is swapped in when it lands.
func (p *player) startTrack(ctx context.Context, item playbackItem) error {
	if err := p.sink.StartFile(ctx, item.source, item.seekMs()); err != nil {
		return err
	}
	index := p.addItem(item)
	p.loaded++
	p.watchDuration(ctx, index, item)
	p.watchLocal(ctx, index, item.ready)
	p.applyPendingSkip(ctx)
	return nil
}

// appendTrack queues the next entry behind whatever is playing. A copy that
// landed before its turn is already the local path; otherwise mpv streams the
// URL and the copy is swapped in once that entry is the one playing.
func (p *player) appendTrack(ctx context.Context, item playbackItem) error {
	if err := p.sink.AppendFile(ctx, item.source); err != nil {
		return err
	}
	index := p.addItem(item)
	p.loaded++
	p.watchDuration(ctx, index, item)
	p.watchLocal(ctx, index, item.ready)
	p.applyPendingSkip(ctx)
	return nil
}

// seekMs is where mpv should start the source. A stream already begins where
// the listener asked - the server starts the source there - so mpv plays it
// from zero; a local file is seeked to the position.
func (i playbackItem) seekMs() int64 {
	if i.ready != nil {
		return 0
	}
	return i.positionMs
}

// streamed reports whether the entry is still the stream, not yet the file on
// disk. mpv's playhead is then relative to where the stream began, and the
// entry's position is that beginning.
func (i playbackItem) streamed() bool {
	return i.ready != nil && i.source != i.localPath
}

// watchDuration learns a streamed entry's length from the stream's own header
// and records it, so the status line can draw a timeline for a song whose bytes
// have no length of their own. It only runs for a streaming entry: a local file
// reports its length through mpv, and a length already known needs no probe.
func (p *player) watchDuration(ctx context.Context, index int, item playbackItem) {
	if item.ready == nil || item.durationMs > 0 || p.streamInfo == nil {
		return
	}
	go func() {
		info, err := p.streamInfo(ctx, item.variantID, item.positionMs)
		if err != nil || info.DurationMs <= 0 {
			return
		}
		p.mu.Lock()
		if index < len(p.items) {
			p.items[index].durationMs = info.DurationMs
		}
		p.mu.Unlock()
		p.draw()
	}()
}

// addItem records an entry in playlist order and returns its index.
func (p *player) addItem(item playbackItem) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.items = append(p.items, item)
	return len(p.items) - 1
}

// watchLocal hands mpv the local copy of a track once its download lands,
// resuming from the position the streaming one reached and keeping the pause
// state. It only swaps when that entry is the one actually playing; a copy
// that arrives for a queued entry is left for its own turn (refresh picks it
// up the moment it starts playing).
func (p *player) watchLocal(ctx context.Context, index int, ready <-chan client.CacheEntry) {
	if ready == nil {
		return
	}
	go func() {
		entry, ok := <-ready
		if !ok {
			return
		}

		p.mu.Lock()
		if index < len(p.items) {
			p.items[index].localPath = entry.Path
		}
		p.mu.Unlock()

		// Only the entry that is playing may be swapped, and it is matched by
		// variant id: a copy for a different item must never replace the track
		// the listener is hearing.
		position, err := p.sink.process.playlistPosition(ctx)
		if err != nil || position < 0 {
			return
		}
		p.mu.Lock()
		var playingVariant string
		if position < len(p.items) {
			playingVariant = p.items[position].variantID
		}
		p.mu.Unlock()
		if playingVariant == "" || playingVariant != entry.VariantID {
			return
		}
		p.swapToLocal(ctx, position)
	}()
}

// swapToLocal replaces whatever mpv is playing with the entry's local copy at
// the position it had reached, keeping the pause state. The caller has checked
// that the entry is the one playing.
func (p *player) swapToLocal(ctx context.Context, index int) {
	p.mu.Lock()
	var path string
	var offset int64
	if index < len(p.items) {
		item := p.items[index]
		path = item.localPath
		// A stream asked to begin at a position carries the rest of the song,
		// and mpv's playhead is relative to where that stream started. The
		// local copy is the whole song, so the playhead is shifted by the
		// offset the stream began at before it is seeked.
		if item.streamed() {
			offset = item.positionMs
		}
	}
	p.mu.Unlock()
	if path == "" {
		return
	}

	paused, err := p.sink.process.pauseState(ctx)
	if err != nil {
		return
	}
	where, _ := p.sink.process.positionMs(ctx)
	if err := p.sink.process.loadReplace(ctx, path, where+offset); err != nil {
		return
	}
	if paused {
		_ = p.sink.process.setPause(ctx, true)
	}

	p.mu.Lock()
	if index < len(p.items) {
		p.items[index].source = path
	}
	p.mu.Unlock()
}

// applyPendingSkip honours an earlier "next track is still downloading".
func (p *player) applyPendingSkip(ctx context.Context) {
	p.mu.Lock()
	pending := p.pendingSkip
	if pending > 0 {
		p.pendingSkip--
		p.message = ""
	}
	p.mu.Unlock()

	if pending > 0 {
		_ = p.sink.process.nextEntry(ctx)
	}
}

// refresh reads the playback state from mpv.
func (p *player) refresh(ctx context.Context) {
	readCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()

	position, positionErr := p.sink.process.playlistPosition(readCtx)
	count, countErr := p.sink.process.playlistCount(readCtx)
	where, _ := p.sink.process.positionMs(readCtx)
	duration, _ := p.sink.process.durationMs(readCtx)
	volume, volumeErr := p.sink.process.volume(readCtx)
	paused, _ := p.sink.process.pauseState(readCtx)

	p.mu.Lock()
	if positionErr == nil {
		p.position = position
	}
	if countErr == nil {
		p.count = count
	}
	if volumeErr == nil {
		p.volume = volume
	}
	p.positionMs = where
	p.durationMs = duration
	if position >= 0 && position < len(p.items) {
		item := p.items[position]
		if item.streamed() {
			// The stream began at the offset; its playhead is relative to it.
			p.positionMs = where + item.positionMs
		}
		if duration <= 0 {
			// A stream has no length for mpv to report; the header's figure is
			// what the timeline is drawn from until the local copy lands.
			p.durationMs = item.durationMs
		}
	}
	p.paused = paused
	if position >= 0 && position < len(p.queue) {
		p.title = p.queue[position].Title
	}
	// A local copy that landed while its entry was still queued behind another
	// track is handed over now, the moment that entry is the one playing.
	swap := positionErr == nil && position >= 0 && position < len(p.items) &&
		p.items[position].localPath != "" && p.items[position].source != p.items[position].localPath

	// The playing entry that has not landed locally is the only download the
	// status line can say anything about; anything else has nothing to report.
	waiting := ""
	if positionErr == nil && position >= 0 && position < len(p.items) {
		if item := p.items[position]; item.ready != nil && item.localPath == "" {
			waiting = item.variantID
		}
	}
	if waiting == "" {
		p.download = ""
	}
	p.mu.Unlock()

	if waiting != "" {
		p.readDownload(readCtx, waiting)
	}

	if swap {
		p.swapToLocal(ctx, position)
	}
}

// readDownload records what a variant's download is doing, so the status line
// says the stage and its numbers rather than leaving the listener guessing.
func (p *player) readDownload(ctx context.Context, variantID string) {
	if p.mediaStatus == nil {
		return
	}
	status, err := p.mediaStatus(ctx, variantID)
	label := ""
	if err == nil {
		label = downloadLabel(status)
	}
	p.mu.Lock()
	p.download = label
	p.mu.Unlock()
}

// wait blocks until mpv has played everything it was given, or the user quits.
func (p *player) wait(ctx context.Context) error {
	ticker := time.NewTicker(statusInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.stopped:
			return nil
		case <-ticker.C:
		}

		idle, err := p.sink.process.idle(ctx)
		if err != nil {
			return err
		}
		p.mu.Lock()
		allQueued := p.loaded >= len(p.queue)
		p.mu.Unlock()
		if idle && allQueued {
			return nil
		}
	}
}

// --- status line ------------------------------------------------------------

func (p *player) setMessage(message string) {
	p.mu.Lock()
	p.message = message
	p.mu.Unlock()
}

// draw prints the status line in place.
func (p *player) draw() {
	if !p.interactive {
		return
	}
	p.drawn.Lock()
	defer p.drawn.Unlock()
	fmt.Fprintf(p.out, "\r\033[K%s", p.statusLine())
}

// statusLine renders the whole status, which the tests check directly.
func (p *player) statusLine() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return formatStatus(status{
		position:   p.position,
		count:      p.count,
		title:      p.title,
		positionMs: p.positionMs,
		durationMs: p.durationMs,
		paused:     p.paused,
		volume:     p.volume,
		message:    p.message,
		download:   p.download,
	})
}

type status struct {
	position   int
	count      int
	title      string
	positionMs int64
	durationMs int64
	paused     bool
	volume     int
	message    string
	// download is the playing track's download, already rendered: at most one
	// of a percentage and a count ever appears in it.
	download string
}

func formatStatus(s status) string {
	marker := ">"
	if s.paused {
		marker = "||"
	}
	index := s.position + 1
	if s.position < 0 {
		index = 0
	}

	// A loaded track at position zero is "0:00"; only an empty player is
	// "--:--". The duration stays "--:--" until mpv reports it.
	where, length := "--:--", "--:--"
	if s.position >= 0 {
		where = formatStatusTime(s.positionMs)
		if s.durationMs > 0 {
			length = formatDuration(s.durationMs)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s [%d/%d] %-44s %s/%s",
		marker, index, s.count, truncate(s.title, 44),
		where, length)
	if s.download != "" {
		fmt.Fprintf(&b, "  %s", s.download)
	}
	fmt.Fprintf(&b, "  vol [%s] %3d%%", volumeBar(s.volume), s.volume)
	if s.message != "" {
		fmt.Fprintf(&b, "  %s", s.message)
	}
	return b.String()
}

// downloadLabel says what a variant's download is doing: the stage, and the
// number the stage can honestly give. progress is -1 when there is nothing to
// count, and then the stage's own count is shown rather than a fabricated
// percentage. It is empty when nothing is running.
func downloadLabel(s *client.MediaStatus) string {
	if s == nil {
		return ""
	}
	if s.State == client.MediaFailed {
		// A download that failed says so where its progress was: a stage that
		// stops moving without a word is the silence this line exists to end,
		// and the song behind it is not coming.
		if s.Error != "" {
			return "the download failed: " + s.Error
		}
		return "the download failed"
	}
	switch s.Stage {
	case client.MediaStageDownload:
		switch {
		case s.Progress >= 0 && s.TotalBytes > 0:
			return "downloading " + formatBytesOf(s.Bytes, s.TotalBytes)
		case s.Bytes > 0:
			return "downloading " + formatBytes(s.Bytes)
		default:
			return "downloading"
		}
	case client.MediaStageConvert:
		switch {
		case s.Progress >= 0:
			return fmt.Sprintf("converting %d%%", int(s.Progress*100+0.5))
		case s.Bytes > 0:
			// A conversion's numbers are milliseconds, not bytes.
			return "converting " + formatDuration(s.Bytes)
		default:
			return "converting"
		}
	case client.MediaStageStore:
		return "storing the file"
	default:
		return ""
	}
}

// formatBytes renders a size the one way this program writes them: megabytes to
// a decimal, kilobytes whole, and the last few hundred bytes as bytes.
func formatBytes(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	divisor, unit, decimals := byteScale(n)
	return strconv.FormatFloat(float64(n)/divisor, 'f', decimals, 64) + " " + unit
}

// formatBytesOf renders a count against its total, which is stated once in the
// scale the total calls for: "3.1 of 8.4 MB".
func formatBytesOf(done, total int64) string {
	divisor, unit, decimals := byteScale(total)
	number := func(n int64) string {
		return strconv.FormatFloat(float64(n)/divisor, 'f', decimals, 64)
	}
	return number(done) + " of " + number(total) + " " + unit
}

// byteScale is the unit a byte count is written in and the decimals it keeps.
// It is the only place a size is scaled, so two counts - a bar's filled and
// its total - can never come out in two different units.
func byteScale(n int64) (divisor float64, unit string, decimals int) {
	switch {
	case n >= 1<<20:
		return 1 << 20, "MB", 1
	case n >= 1<<10:
		return 1 << 10, "KB", 0
	default:
		return 1, "B", 0
	}
}

// formatStatusTime renders a playback time, where zero is a real position.
func formatStatusTime(ms int64) string {
	if ms <= 0 {
		return "0:00"
	}
	return formatDuration(ms)
}
