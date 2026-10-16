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
	// line says while the rendition about to play is still being fetched. It is
	// nil for a player with no server to ask, which then says nothing about
	// downloads.
	mediaStatus func(ctx context.Context, variantID string) (*client.MediaStatus, error)

	interactive bool
	keys        *keyReader
	restoreTerm func()
	stopOnce    sync.Once
	stopped     chan struct{}

	// drawn serialises whole status lines.
	drawn sync.Mutex

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
	// download is what the rendition being fetched for the next track is doing,
	// rendered for the status line and empty when nothing is being fetched.
	download string
	// pending is the variant being fetched for the track about to play, whose
	// download the status line reports; empty when nothing is being fetched.
	pending string
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

// startTrack loads the first entry from its file.
func (p *player) startTrack(ctx context.Context, item playbackItem) error {
	if err := p.sink.StartFile(ctx, item.source, item.positionMs); err != nil {
		return err
	}
	p.mu.Lock()
	p.loaded++
	p.mu.Unlock()
	p.applyPendingSkip(ctx)
	return nil
}

// appendTrack queues the next entry behind whatever is playing.
func (p *player) appendTrack(ctx context.Context, item playbackItem) error {
	if err := p.sink.AppendFile(ctx, item.source); err != nil {
		return err
	}
	p.mu.Lock()
	p.loaded++
	p.mu.Unlock()
	p.applyPendingSkip(ctx)
	return nil
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
	p.paused = paused
	if position >= 0 && position < len(p.queue) {
		p.title = p.queue[position].Title
	}
	pending := p.pending
	if pending == "" {
		p.download = ""
	}
	p.mu.Unlock()

	if pending != "" {
		p.readDownload(readCtx, pending)
	}
}

// setPending names the rendition being fetched for the track about to play, so
// the status line reports that fetch while it runs; an empty id clears it.
func (p *player) setPending(variantID string) {
	p.mu.Lock()
	p.pending = variantID
	if variantID == "" {
		p.download = ""
	}
	p.mu.Unlock()
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
	if p.pending == variantID {
		p.download = label
	}
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
