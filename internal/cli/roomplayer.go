package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/kyleraykbs/MusOak/pkg/client"
)

// roomPlayer is the one line a room member has while it follows a room: the song
// the room is on, and where this member is in it. The participant hands it a file
// only once the rendition is on disk, so the line never has a download to report
// mid-song; it says where the song has reached instead.
//
// On a terminal the line is redrawn in place, where a position moving is worth
// watching; anywhere else one line is printed per change, so piping the command
// gives a readable log rather than a carriage return per status read.
type roomPlayer struct {
	out  io.Writer
	part *client.Participant

	// interactive is whether the line may be redrawn in place, which is what a
	// terminal is for.
	interactive bool

	mu   sync.Mutex
	slot client.SinkTrack
	// position is where this member's own player is in the song.
	position int64
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
func newRoomPlayer(out io.Writer, part *client.Participant) *roomPlayer {
	return &roomPlayer{out: out, part: part, stopped: make(chan struct{})}
}

// start begins the line, and the tick that keeps it true: the position moves
// while a song plays.
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

// track is what the participant hands its sink, as it hands it: a new song, from
// the file it fetched. It must not wait for anything - the participant calls it
// from its own loop - so it records the answer and draws it, and nothing else.
func (r *roomPlayer) track(slot client.SinkTrack) {
	r.mu.Lock()
	r.slot = slot
	r.mu.Unlock()

	if r.interactive {
		r.draw()
		return
	}
	r.print(slot)
}

// refresh reads where this member is in the song.
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
	r.mu.Unlock()
}

// print says one line, when there is something new in it: a different song, or
// the same song started again. The same line twice is not news.
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

// roomKey is what makes two slots the same telling: the song and the rendition.
// The position is not part of it, so a restart is caught by the position going
// backwards instead.
func roomKey(slot client.SinkTrack) string {
	return slot.TrackID + "\x00" + slot.VariantID
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
	})
}

// roomStatus is what a member's line is drawn from.
type roomStatus struct {
	title      string
	positionMs int64
	durationMs int64
}

// roomLine renders the whole of a member's line: the song and where the member
// is in it. The room's answer to the queue player's status line, and what the
// tests pin.
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

	return fmt.Sprintf("%-44s %s/%s", truncate(title, 44), where, length)
}
