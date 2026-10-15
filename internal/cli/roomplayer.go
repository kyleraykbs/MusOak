package cli

import (
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

// roomPlayer is the one line a room member has while it follows a room: the song
// the room is on, where this member is in it, and what the rendition in hand is
// doing - the server's own stream of it arriving while its download runs behind
// it, or the file once it has landed.
//
// It is what makes a member's wait visible instead of silence: a song being heard
// from the provider while the file is still coming, the download's own stage and
// numbers, and the moment the file takes over. On a terminal the line is redrawn
// in place, where a position moving and a label changing are worth watching;
// anywhere else one line is printed per change, so piping the command gives a
// readable log rather than a carriage return per status read.
type roomPlayer struct {
	out  io.Writer
	part *client.Participant
	// media reads one variant's download state, and is nil for a member with no
	// server to ask, which then says nothing about downloads.
	media func(ctx context.Context, variantID string) (*client.MediaStatus, error)

	// interactive is whether the line may be redrawn in place, which is what a
	// terminal is for.
	interactive bool

	mu   sync.Mutex
	slot client.SinkTrack
	// position is where this member's own player is in the song.
	position int64
	// download is what the playing rendition's download is doing, already
	// rendered, and empty when there is nothing being fetched.
	download string
	// reading is whether a download status read is in flight, so the tick does
	// not pile them up, and lastRead is when the last one was asked for.
	reading  bool
	lastRead time.Time
	// printedKey and printedAt are what the last line said and where in the song
	// it was said, which is how a repeated line is left out of a log.
	printedKey string
	printedAt  int64

	// drawn serialises whole lines, and shown is the line last drawn so a tick
	// that changes nothing is not written twice.
	drawn sync.Mutex
	shown string

	// said serialises the printed lines and what they said. The participant
	// hands a slot over from more than one goroutine, so two lines could
	// otherwise be written for one change - or none for two.
	said sync.Mutex

	stopped chan struct{}
	once    sync.Once
}

// newRoomPlayer returns a player for one member's playback.
func newRoomPlayer(out io.Writer, part *client.Participant, media func(ctx context.Context, variantID string) (*client.MediaStatus, error)) *roomPlayer {
	return &roomPlayer{out: out, part: part, media: media, stopped: make(chan struct{})}
}

// start begins the line, and the tick that keeps it true: the position moves
// while a song plays, and a download's numbers move while it is fetched.
func (r *roomPlayer) start(ctx context.Context) {
	r.interactive = term.IsTerminal(int(os.Stdout.Fd()))
	go r.run(ctx)
}

// stop ends the line and leaves the terminal the way it was found.
func (r *roomPlayer) stop() {
	r.once.Do(func() {
		close(r.stopped)
		r.drawn.Lock()
		defer r.drawn.Unlock()
		if r.interactive {
			// Leave the row for whatever is said next rather than stranding a
			// half-drawn line under the prompt. Held with the drawing lock, so
			// this clear is the last thing written to that row.
			fmt.Fprint(r.out, "\r\033[K")
		}
	})
}

// stopping reports whether the line has been ended, so nothing is written after
// the row has been given back.
func (r *roomPlayer) stopping() bool {
	select {
	case <-r.stopped:
		return true
	default:
		return false
	}
}

func (r *roomPlayer) run(ctx context.Context) {
	ticker := time.NewTicker(statusInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stopped:
			return
		case <-ticker.C:
			r.refresh(ctx)
			r.draw()
		}
	}
}

// track is what the participant hands its sink, as it hands it: a new song, the
// stream of the one already playing asked for again, or the file taking over
// from it. It must not wait for anything - the participant calls it from its own
// loop and from the goroutines that resolve and fetch - so it records the answer
// and draws it, and nothing else.
func (r *roomPlayer) track(slot client.SinkTrack) {
	r.mu.Lock()
	r.slot = slot
	if !slot.Streaming {
		// The file is what is playing now, so there is no download left to
		// report; a label from the stream it took over from is stale.
		r.download = ""
	}
	r.mu.Unlock()

	if r.interactive {
		r.draw()
		return
	}
	r.print(slot)
}

// refresh reads where this member is, and asks what its download is doing while
// its rendition is still arriving.
func (r *roomPlayer) refresh(ctx context.Context) {
	// The position is a read of the player, which is bounded so a player that
	// has stopped answering cannot hold the line still: the room's clock is
	// what is drawn when it cannot be read, which is the position this member is
	// meant to sit on anyway.
	readCtx, cancel := context.WithTimeout(ctx, time.Second)
	position, ok := r.part.LocalPosition(readCtx)
	cancel()
	if !ok {
		position = r.part.Position()
	}
	r.mu.Lock()
	r.position = position
	slot := r.slot
	// Only a stream is asked about. It is the one case where the server still
	// has something to prepare, and so the only one with a stage and numbers to
	// report; a rendition the server already holds has nothing left to say, and
	// a file on disk has nothing coming at all.
	due := slot.Streaming && slot.VariantID != "" && r.media != nil &&
		!r.reading && time.Since(r.lastRead) >= time.Second
	if due {
		r.reading = true
	}
	r.mu.Unlock()

	if due {
		// The read is a round trip, so it is not made on the drawing loop: a
		// server that is slow must not stall the line.
		go r.readDownload(ctx, slot.VariantID)
	}
}

// readDownload records what a variant's download is doing, so the line says the
// stage and its numbers rather than leaving the listener guessing.
func (r *roomPlayer) readDownload(ctx context.Context, variantID string) {
	defer func() {
		r.mu.Lock()
		r.reading = false
		r.lastRead = time.Now()
		r.mu.Unlock()
	}()

	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	label := ""
	if status, err := r.media(readCtx, variantID); err == nil {
		label = downloadLabel(status)
	}

	r.mu.Lock()
	if r.slot.VariantID == variantID {
		r.download = label
	}
	r.mu.Unlock()
	r.draw()
}

// print says one line, when there is something new in it: a different song, the
// file taking over from a stream, or the same song started again. A stream asked
// for once more mid-song is not news, and neither is the same line twice.
func (r *roomPlayer) print(slot client.SinkTrack) {
	// The participant hands a slot over from more than one goroutine - the loop
	// that reconciles, and the goroutine a download finishes under - so a line
	// and the note of what it said are taken together.
	r.said.Lock()
	defer r.said.Unlock()
	if r.stopping() {
		return
	}

	key := roomKey(slot)
	r.mu.Lock()
	same := key == r.printedKey && slot.PositionMs >= r.printedAt-1_000
	r.mu.Unlock()
	if same {
		return
	}

	line := r.line()
	fmt.Fprintln(r.out, line)

	r.mu.Lock()
	r.printedKey, r.printedAt = key, slot.PositionMs
	r.mu.Unlock()
}

// roomKey is what makes two slots the same telling: the song, the rendition, and
// whether it is the server's stream or a file. The position is not part of it -
// a stream asked for again further into the song is the same song - so a
// restart is caught by the position going backwards instead.
func roomKey(slot client.SinkTrack) string {
	return slot.TrackID + "\x00" + slot.VariantID + "\x00" + strconv.FormatBool(slot.Streaming)
}

// draw writes the line in place, which is only ever done on a terminal.
func (r *roomPlayer) draw() {
	if !r.interactive {
		return
	}
	r.drawn.Lock()
	defer r.drawn.Unlock()
	if r.stopping() {
		return
	}
	line := r.line()
	if line == r.shown {
		return
	}
	r.shown = line
	fmt.Fprintf(r.out, "\r\033[K%s", line)
}

// line renders the whole status, which the tests check directly.
func (r *roomPlayer) line() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return roomLine(roomStatus{
		title:      r.slot.Title,
		positionMs: r.position,
		durationMs: r.slot.DurationMs,
		streaming:  r.slot.Streaming,
		download:   r.download,
	})
}

// roomStatus is what a member's line is drawn from.
type roomStatus struct {
	title      string
	positionMs int64
	durationMs int64
	// streaming is true while what is playing is the server's own stream of a
	// rendition that is not here yet: the song is being heard while its file is
	// fetched behind it.
	streaming bool
	// download is the rendition's own download, already rendered, and empty when
	// nothing is being fetched.
	download string
}

// roomLine renders the whole of a member's line: the song, where the member is
// in it, and what the rendition in hand is doing. The room's answer to the queue
// player's status line, and what the tests pin.
func roomLine(s roomStatus) string {
	title := s.title
	if title == "" {
		title = "waiting for the room"
	}
	// A length the room never learned stays "--:--": a bar drawn from nothing is
	// a bar that says the song is over.
	where, length := "--:--", "--:--"
	if s.positionMs > 0 {
		where = formatStatusTime(s.positionMs)
	}
	if s.durationMs > 0 {
		length = formatDuration(s.durationMs)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%-44s %s/%s", truncate(title, 44), where, length)
	if s.streaming {
		// The listener is not waiting for anything: the song is being heard from
		// the provider's own stream while the file arrives.
		b.WriteString("  streaming while the file arrives")
	}
	if s.download != "" {
		fmt.Fprintf(&b, "  %s", s.download)
	}
	return b.String()
}
