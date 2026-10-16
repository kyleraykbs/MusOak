package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kyleraykbs/MusOak/pkg/client"
)

// The member's line says the two things a listener needs: the song and where in
// it they are. What is playing is always a file this member holds - the
// participant fetches a rendition before it plays it - so there is never a
// download to report mid-song.
func TestRoomLineSaysTheSongAndWhereTheMemberIs(t *testing.T) {
	line := func(title, where, length string) string {
		return fmt.Sprintf("%-44s %s/%s", title, where, length)
	}

	cases := []struct {
		name string
		what roomStatus
		want string
	}{
		{
			name: "a song playing",
			what: roomStatus{title: "Song One", positionMs: 83_000, durationMs: 225_000},
			want: line("Song One", "1:23", "3:45"),
		},
		{
			name: "a length the room never learned",
			what: roomStatus{title: "Song One", positionMs: 1_000},
			want: line("Song One", "0:01", "--:--"),
		},
		{
			name: "a song the room has not started says so instead of a time",
			what: roomStatus{},
			want: line("waiting for the room", "--:--", "--:--"),
		},
		{
			name: "a title too long for the line is shortened, not wrapped",
			what: roomStatus{
				title: "A Song Whose Title Is Much Longer Than The Line", positionMs: 1_000, durationMs: 60_000,
			},
			// The "…" is three bytes where one character was measured, so this
			// one runs into the time rather than keeping the column - the same
			// shortening the queue player's line has always had.
			want: "A Song Whose Title Is Much Longer Than The … 0:01/1:00",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := roomLine(testCase.what); got != testCase.want {
				t.Errorf("roomLine = %q, want %q", got, testCase.want)
			}
		})
	}
}

// One line per change, without a terminal to redraw: a log wants the songs and
// the moments the rendition changed, not a carriage return per status read.
func TestRoomPlayerSaysOneLinePerChange(t *testing.T) {
	out := &lockedBuffer{}
	player := newRoomPlayer(out, nil)

	song := func(trackID, variantID, title string, positionMs int64) client.SinkTrack {
		return client.SinkTrack{
			TrackID: trackID, VariantID: variantID, Title: title,
			Path: "/cache/" + variantID + ".opus", PositionMs: positionMs, DurationMs: 225_000,
		}
	}

	player.track(song("t1", "v1", "Song One", 45_000))
	// The same song reloaded further in is the same song being played, not
	// news.
	player.track(song("t1", "v1", "Song One", 46_000))
	// The next song is news.
	player.track(song("t2", "v2", "Song Two", 0))

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("said %d lines, want 2:\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[0], "Song One") {
		t.Errorf("the first line does not name the song: %q", lines[0])
	}
	if !strings.Contains(lines[1], "Song Two") {
		t.Errorf("the next song was not said: %q", lines[1])
	}
}

// On a terminal the line is redrawn in place, and only when it says something
// new: the tick is faster than the line changes, so a redraw that changes
// nothing is a write nobody can see.
func TestRoomPlayerRedrawsOnlyWhatChanged(t *testing.T) {
	out := &lockedBuffer{}
	player := newRoomPlayer(out, nil)
	player.interactive = true

	player.track(client.SinkTrack{
		TrackID: "t1", VariantID: "v1", Title: "Song One",
		PositionMs: 45_000, DurationMs: 225_000,
	})
	first := out.String()
	if !strings.Contains(first, "\r\033[K") {
		t.Fatalf("the line was not drawn in place: %q", first)
	}
	if !strings.Contains(first, "Song One") {
		t.Errorf("the line does not name the song: %q", first)
	}

	// The same line again: nothing is written.
	player.draw()
	if got := out.String(); got != first {
		t.Errorf("an unchanged line was drawn again: %q", got)
	}

	// The position moving is a change.
	player.mu.Lock()
	player.position = 46_000
	player.mu.Unlock()
	player.draw()
	if got := out.String(); got == first {
		t.Errorf("a moved position was not drawn: %q", got)
	}
}

// tickingSink is a player that can say where it is and nothing else.
type tickingSink struct{ position int64 }

func (s tickingSink) Play(ctx context.Context, track client.SinkTrack) error { return nil }
func (s tickingSink) Stop(ctx context.Context) error                         { return nil }
func (s tickingSink) PositionMs(ctx context.Context) (int64, error)          { return s.position, nil }

// The line follows the member's own playhead, which is what a listener hears,
// and the length the room reported.
func TestRoomPlayerDrawsTheMembersOwnPlayhead(t *testing.T) {
	participant := client.NewParticipant(&client.RoomClient{RoomID: "r1"}, nil, tickingSink{position: 42_000}, nil)
	player := newRoomPlayer(io.Discard, participant)
	player.track(client.SinkTrack{
		TrackID: "t1", VariantID: "v1", Title: "Song", DurationMs: 225_000,
	})

	player.refresh(context.Background())

	line := player.line()
	if !strings.Contains(line, "0:42/3:45") {
		t.Errorf("line = %q, want the member's own playhead against the room's length", line)
	}
}

// --- against a real mpv -----------------------------------------------------

// newMpvSink starts an mpv for a test. mpv is present in this environment, so
// these run rather than skip.
func newMpvSink(t *testing.T) (*MpvSink, context.Context) {
	t.Helper()
	requireMpv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	sink, err := NewMpvSink(ctx, filepath.Join(t.TempDir(), "mpv.sock"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewMpvSink: %v", err)
	}
	t.Cleanup(sink.Close)
	return sink, ctx
}

// A file that will not load is an error, not the song ending: a 404, a URL the
// server refused, a container mpv cannot read.
func TestMpvSinkReportsAFileThatWillNotPlay(t *testing.T) {
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such rendition", http.StatusNotFound)
	}))
	t.Cleanup(busy.Close)

	sink, ctx := newMpvSink(t)
	err := sink.Play(ctx, client.SinkTrack{TrackID: "t1", VariantID: "v1", Path: busy.URL})
	if err == nil {
		t.Fatal("a 404 was reported as the song finishing")
	}
	if !strings.Contains(err.Error(), "could not play") {
		t.Errorf("error = %v, want it to say mpv could not play it", err)
	}
}

// Handing the sink another file makes mpv emit end-file with reason "stop" for
// the one it was playing. That is the file being taken over, not this file
// ending: a reader that took it for the end would call the song over while the
// listener is still hearing it, and the next end-file would look like the one it
// waited for.
//
// Both files are long, so a reader that came back at all came back for the wrong
// reason: the replacement is still playing.
func TestMpvSinkIsNotFooledByAFileReplacedUnderIt(t *testing.T) {
	dir := t.TempDir()
	first := tone(t, dir, "first.opus", 30.0)
	second := tone(t, dir, "second.opus", 30.0)

	sink, ctx := newMpvSink(t)
	playCtx, cancelPlay := context.WithCancel(ctx)
	defer cancelPlay()

	played := make(chan error, 1)
	go func() {
		played <- sink.Play(playCtx, client.SinkTrack{
			TrackID: "t1", VariantID: "v1", Path: first, DurationMs: 30_000,
		})
	}()

	waitForCondition(t, ctx, "the first file to play", func() bool {
		position, err := sink.PositionMs(ctx)
		return err == nil && position > 0
	})

	// The file is replaced under the reader.
	if err := sink.StartFile(ctx, second, 0); err != nil {
		t.Fatalf("StartFile: %v", err)
	}
	select {
	case err := <-played:
		t.Fatalf("the replaced file's end was read as this file ending: %v", err)
	case <-time.After(time.Second):
	}
	waitForCondition(t, ctx, "the file that took over", func() bool {
		position, err := sink.PositionMs(ctx)
		return err == nil && position > 0 && position < 5_000
	})

	// Stopping it is not the end either: the reader is waiting for the file it
	// was handed, and only the context it runs under takes that away.
	if err := sink.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	cancelPlay()
	select {
	case err := <-played:
		if err == nil {
			t.Error("a stopped file was reported as running out")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the reader did not come back when its context ended")
	}
}

// A file that really runs out is still an end: the reason it ends for itself is
// the one the reader is waiting for.
func TestMpvSinkEndsWhenTheFileRunsOut(t *testing.T) {
	short := tone(t, t.TempDir(), "short.opus", 0.4)

	sink, ctx := newMpvSink(t)
	played := make(chan error, 1)
	go func() {
		played <- sink.Play(ctx, client.SinkTrack{
			TrackID: "t1", VariantID: "v1", Path: short, DurationMs: 400,
		})
	}()

	select {
	case err := <-played:
		if err != nil {
			t.Errorf("the file running out was reported as %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the file ran out and its end was never reported")
	}
}

// A file is seeked to the room's position: that is what the position in a slot
// means.
func TestMpvSinkSeeksAFileToThePosition(t *testing.T) {
	dir := t.TempDir()
	stored := tone(t, dir, "stored.opus", 10.0)

	sink, ctx := newMpvSink(t)
	go func() {
		_ = sink.Play(ctx, client.SinkTrack{
			TrackID: "t1", VariantID: "v1", Path: stored, PositionMs: 3_000, DurationMs: 10_000,
		})
	}()
	waitForCondition(t, ctx, "the file to play from the room's position", func() bool {
		position, err := sink.PositionMs(ctx)
		return err == nil && position >= 3_000 && position < 6_000
	})
}
