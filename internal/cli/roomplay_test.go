package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kyleraykbs/MusOak/internal/config"
	"github.com/kyleraykbs/MusOak/pkg/client"
)

// The member's line says the three things a listener needs: the song, where in
// it they are, and what the rendition in hand is doing. A song being heard from
// the server's own stream while its file arrives is the one a listener cannot
// otherwise tell apart from one that is already here - the difference is what
// happens when they seek, or when the download finishes.
func TestRoomLineSaysTheSongAndWhatItsRenditionIsDoing(t *testing.T) {
	// line is the shape the renderer produces: the title in its column, then
	// where the member is, then whatever the rendition in hand is doing.
	line := func(title, where, length string, tail ...string) string {
		out := fmt.Sprintf("%-44s %s/%s", title, where, length)
		for _, extra := range tail {
			out += "  " + extra
		}
		return out
	}

	cases := []struct {
		name string
		what roomStatus
		want string
	}{
		{
			name: "a stream arriving while its file is fetched",
			what: roomStatus{
				title: "Song One", positionMs: 83_000, durationMs: 225_000,
				streaming: true, download: "downloading 3.1 of 8.4 MB",
			},
			want: line("Song One", "1:23", "3:45", "streaming while the file arrives", "downloading 3.1 of 8.4 MB"),
		},
		{
			name: "the file itself, once it has landed",
			what: roomStatus{title: "Song One", positionMs: 131_000, durationMs: 225_000},
			want: line("Song One", "2:11", "3:45"),
		},
		{
			name: "a stream with nothing countable to report",
			what: roomStatus{title: "Song One", positionMs: 1_000, durationMs: 225_000, streaming: true},
			want: line("Song One", "0:01", "3:45", "streaming while the file arrives"),
		},
		{
			name: "a download that failed says so where its progress was",
			what: roomStatus{
				title: "Song One", streaming: true,
				download: "the download failed: the provider refused",
			},
			want: line("Song One", "--:--", "--:--", "streaming while the file arrives", "the download failed: the provider refused"),
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

// A download that failed has to say so: a stage that stops moving without a
// word is the silence the line exists to end, and the song behind it is not
// coming.
func TestDownloadLabelSaysADownloadThatFailed(t *testing.T) {
	if got, want := downloadLabel(&client.MediaStatus{State: client.MediaFailed, Error: "the provider refused"}),
		"the download failed: the provider refused"; got != want {
		t.Errorf("downloadLabel = %q, want %q", got, want)
	}
	if got, want := downloadLabel(&client.MediaStatus{State: client.MediaFailed}), "the download failed"; got != want {
		t.Errorf("downloadLabel = %q, want %q", got, want)
	}
}

// One line per change, without a terminal to redraw: a log wants the songs and
// the moments the rendition changed, not a carriage return per status read.
func TestRoomPlayerSaysOneLinePerChange(t *testing.T) {
	out := &lockedBuffer{}
	player := newRoomPlayer(out, nil, nil)

	song := func(streaming bool, positionMs int64, path string) client.SinkTrack {
		return client.SinkTrack{
			TrackID: "t1", VariantID: "v1", Title: "Song One",
			Path: path, PositionMs: positionMs, DurationMs: 225_000, Streaming: streaming,
		}
	}

	// The stream, then the same stream asked for again further into the song -
	// which is the same song being played, not news.
	player.track(song(true, 45_000, "http://server/api/v1/media/v1?start=45000"))
	player.track(song(true, 46_000, "http://server/api/v1/media/v1?start=46000"))
	// The file taking over is news.
	player.track(song(false, 47_500, "/cache/v1.opus"))
	// So is the next song.
	next := song(true, 0, "http://server/api/v1/media/v2")
	next.TrackID, next.VariantID, next.Title = "t2", "v2", "Song Two"
	player.track(next)

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("said %d lines, want 3:\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[0], "streaming while the file arrives") {
		t.Errorf("the first line does not say the stream is what is playing: %q", lines[0])
	}
	if strings.Contains(lines[1], "streaming while the file arrives") {
		t.Errorf("the handover was still reported as a stream: %q", lines[1])
	}
	if !strings.Contains(lines[2], "Song Two") {
		t.Errorf("the next song was not said: %q", lines[2])
	}
}

// On a terminal the line is redrawn in place, and only when it says something
// new: the tick is faster than the line changes, so a redraw that changes
// nothing is a write nobody can see.
func TestRoomPlayerRedrawsOnlyWhatChanged(t *testing.T) {
	out := &lockedBuffer{}
	player := newRoomPlayer(out, nil, nil)
	player.interactive = true

	slot := client.SinkTrack{
		TrackID: "t1", VariantID: "v1", Title: "Song One",
		PositionMs: 45_000, DurationMs: 225_000, Streaming: true,
	}
	player.track(slot)
	first := out.String()
	if !strings.Contains(first, "\r\033[K") {
		t.Fatalf("the line was not drawn in place: %q", first)
	}
	if !strings.Contains(first, "streaming while the file arrives") {
		t.Errorf("the line does not say what is playing: %q", first)
	}

	// The same line again: nothing is written. The position has not moved and
	// nothing else about the song has changed.
	player.draw()
	if got := out.String(); got != first {
		t.Errorf("an unchanged line was drawn again: %q", got)
	}

	// The download's label changing is a change.
	player.mu.Lock()
	player.download = "downloading 3.1 of 8.4 MB"
	player.mu.Unlock()
	player.draw()
	if got := out.String(); !strings.Contains(got, "downloading 3.1 of 8.4 MB") {
		t.Errorf("the download was not drawn: %q", got)
	}
}

// The tick keeps the line true while a song plays: the position comes from the
// member's own player, and falls back to the room's own clock for a sink that
// cannot say where it is.
func TestRoomPlayerTicksThePositionAndTheDownload(t *testing.T) {
	out := &lockedBuffer{}
	player := newRoomPlayer(out, client.NewParticipant(&client.RoomClient{RoomID: "r1"}, nil, tickingSink{position: 1_000}, nil),
		func(ctx context.Context, variantID string) (*client.MediaStatus, error) {
			if variantID != "v1" {
				t.Errorf("asked about variant %q, want the one in hand", variantID)
			}
			return &client.MediaStatus{Stage: client.MediaStageConvert, Progress: 0.45}, nil
		})
	player.interactive = true
	player.track(client.SinkTrack{
		TrackID: "t1", VariantID: "v1", Title: "Song One",
		PositionMs: 45_000, DurationMs: 225_000, Streaming: true,
	})

	ctx := context.Background()
	player.refresh(ctx)
	waitForCondition(t, ctx, "the download's stage to be read", func() bool {
		player.mu.Lock()
		defer player.mu.Unlock()
		return player.download == "converting 45%"
	})
	if got := player.line(); !strings.Contains(got, "converting 45%") {
		t.Errorf("line = %q, want the download's stage in it", got)
	}
}

// tickingSink is a player that can say where it is and nothing else.
type tickingSink struct{ position int64 }

func (s tickingSink) Play(ctx context.Context, track client.SinkTrack) error { return nil }
func (s tickingSink) Stop(ctx context.Context) error                         { return nil }
func (s tickingSink) PositionMs(ctx context.Context) (int64, error)          { return s.position, nil }

// The line follows the member's own playhead, which is what a listener hears,
// and the length the room reported, which is the figure a rendition that is
// still arriving cannot carry.
func TestRoomPlayerDrawsTheMembersOwnPlayhead(t *testing.T) {
	participant := client.NewParticipant(&client.RoomClient{RoomID: "r1"}, nil, tickingSink{position: 42_000}, nil)
	player := newRoomPlayer(io.Discard, participant, nil)
	player.track(client.SinkTrack{
		TrackID: "t1", VariantID: "v1", Title: "Song", DurationMs: 225_000, Streaming: true,
	})

	player.refresh(context.Background())

	line := player.line()
	if !strings.Contains(line, "0:42/3:45") {
		t.Errorf("line = %q, want the member's own playhead against the room's length", line)
	}
}

// A status read that is still in flight is not asked again on the next tick: the
// line is drawn every 200ms and the server is asked once a second at most.
func TestRoomPlayerAsksAboutTheDownloadOnceAtATime(t *testing.T) {
	calls := make(chan struct{}, 16)
	player := newRoomPlayer(io.Discard,
		client.NewParticipant(&client.RoomClient{RoomID: "r1"}, nil, tickingSink{position: 1_000}, nil),
		func(ctx context.Context, variantID string) (*client.MediaStatus, error) {
			calls <- struct{}{}
			return &client.MediaStatus{Stage: client.MediaStageStore}, nil
		})
	player.track(client.SinkTrack{
		TrackID: "t1", VariantID: "v1", PositionMs: 1_000, DurationMs: 225_000, Streaming: true,
	})

	ctx := context.Background()
	for range 5 {
		player.refresh(ctx)
	}
	select {
	case <-calls:
	case <-time.After(time.Second):
		t.Fatal("the download was never asked about")
	}
	time.Sleep(50 * time.Millisecond)
	if extra := len(calls); extra > 0 {
		t.Errorf("the download was asked about %d more times in one tick", extra)
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

// streamOf answers a tone the way the server answers a rendition that is not
// downloaded yet: chunked, so the answer carries no length and no range to seek
// within, and marked as the stream it is.
func streamOf(t *testing.T, path string) http.Handler {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/ogg")
		w.Header().Set("X-Musoak-Stream", "1")
		w.Header().Set("X-Musoak-Duration", "225000")
		for offset := 0; offset < len(body); offset += 32 * 1024 {
			if _, err := w.Write(body[offset:min(offset+32*1024, len(body))]); err != nil {
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	})
}

// A stream that will not play is an error, not the song ending. The server
// answers a third listener of one rendition with 503 while its download runs,
// and a reader that took that for the end would report the song over while the
// room is still on it.
func TestMpvSinkReportsAStreamThatWillNotPlay(t *testing.T) {
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "media stream busy", http.StatusServiceUnavailable)
	}))
	t.Cleanup(busy.Close)

	sink, ctx := newMpvSink(t)
	err := sink.Play(ctx, client.SinkTrack{TrackID: "t1", VariantID: "v1", Path: busy.URL, Streaming: true})
	if err == nil {
		t.Fatal("a 503 was reported as the song finishing")
	}
	if !strings.Contains(err.Error(), "could not play") {
		t.Errorf("error = %v, want it to say mpv could not play it", err)
	}
}

// Handing the sink another file makes mpv emit end-file with reason "stop" for
// the one it was playing. That is the file being taken over - a stream handing
// over to the file that just landed - and not this file ending: a reader that
// took it for the end would call the song over while the listener is still
// hearing it, and the next end-file would look like the one it waited for.
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

	// The stream is taken over by the file that landed.
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

// A rendition the server already holds is a file over HTTP, and a file is seeked
// to the room's position: that is what the position in a slot means when what is
// being played is not a stream.
func TestMpvSinkSeeksAStoredRenditionToThePosition(t *testing.T) {
	dir := t.TempDir()
	stored := tone(t, dir, "stored.opus", 10.0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, stored)
	}))
	t.Cleanup(server.Close)

	sink, ctx := newMpvSink(t)
	go func() {
		_ = sink.Play(ctx, client.SinkTrack{
			TrackID: "t1", VariantID: "v1", Path: server.URL, PositionMs: 3_000, DurationMs: 10_000,
		})
	}()
	waitForCondition(t, ctx, "the file to play from the room's position", func() bool {
		position, err := sink.PositionMs(ctx)
		return err == nil && position >= 3_000 && position < 6_000
	})
}

// A stream is played from where its bytes begin. mpv cannot seek it - that is
// what having no length and no ranges means - so what carries the room's
// position from then on is the offset this client keeps, not a seek.
func TestMpvSinkPlaysAStreamFromItsOwnBeginning(t *testing.T) {
	dir := t.TempDir()
	stream := tone(t, dir, "stream.opus", 8.0)
	server := httptest.NewServer(streamOf(t, stream))
	t.Cleanup(server.Close)

	sink, ctx := newMpvSink(t)
	go func() {
		_ = sink.Play(ctx, client.SinkTrack{
			TrackID: "t1", VariantID: "v1", Path: server.URL,
			PositionMs: 45_000, DurationMs: 225_000, Streaming: true,
		})
	}()
	waitForCondition(t, ctx, "the stream to play from its own beginning", func() bool {
		position, err := sink.PositionMs(ctx)
		return err == nil && position > 0 && position < 3_000
	})
}

// streamRequests records what a member asked the server to stream, which is what
// "the sink was handed the stream at the room's position" looks like from the
// server's side.
type streamRequests struct {
	mu     sync.Mutex
	starts []int64
}

func (s *streamRequests) record(raw string) {
	ms, _ := strconv.ParseInt(raw, 10, 64)
	s.mu.Lock()
	s.starts = append(s.starts, ms)
	s.mu.Unlock()
}

func (s *streamRequests) all() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.starts...)
}

// waitForStreams waits until at least n streams have been asked for, and says
// what positions they asked to begin at.
func waitForStreams(t *testing.T, ctx context.Context, streams *streamRequests, n int) []int64 {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if asked := streams.all(); len(asked) >= n {
			return asked
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for %d stream(s): %v", n, ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatalf("only %d stream(s) were asked for, want %d", len(streams.all()), n)
	return nil
}

// waitForOutput waits until the member has said something.
func waitForOutput(t *testing.T, ctx context.Context, out *lockedBuffer, want string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if said := out.String(); strings.Contains(said, want) {
			return said
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for %q: %v (the member said:\n%s)", want, ctx.Err(), out.String())
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatalf("the member never said %q; it said:\n%s", want, out.String())
	return ""
}

// waitForHandover waits for a line about the song that no longer says the stream
// is what is playing: the file has taken over.
func waitForHandover(t *testing.T, ctx context.Context, out *lockedBuffer) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		streamed := false
		for _, line := range strings.Split(out.String(), "\n") {
			if strings.Contains(line, "streaming while the file arrives") {
				streamed = true
				continue
			}
			if streamed && strings.TrimSpace(line) != "" {
				return line
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for the file to take over: %v", ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatalf("the file never took over; the member said:\n%s", out.String())
	return ""
}

// The whole path a room member takes on a song it does not hold: the room's song
// is heard from the server's own stream, beginning where the room is, a room seek
// is followed by asking for the stream again at the new position - a stream has
// nothing to seek within - and the file takes over the moment the download lands.
//
// It runs through the CLI with real mpv. The server here is one whose rendition
// is still being prepared, which is what a song fetched from a provider looks
// like before it lands: it streams the source while the download runs behind it.
func TestARoomMemberHearsASongItDoesNotHoldWhileItArrives(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	host, _ := testApp(t, nil)
	dir := t.TempDir()
	stored := tone(t, dir, "Room Song.opus", 60.0)
	if err := host.Run(ctx, []string{"library", "import", dir}); err != nil {
		t.Fatalf("library import: %v", err)
	}
	entries := host.cache.Entries()
	if len(entries) != 1 {
		t.Fatalf("the import left %d renditions in the host's cache, want 1", len(entries))
	}
	trackID, variantID := entries[0].TrackID, entries[0].VariantID

	if err := host.Run(ctx, []string{"room", "create", "--name", "listeners"}); err != nil {
		t.Fatalf("room create: %v", err)
	}
	roomID := host.state.RoomID
	if err := host.Run(ctx, []string{"room", "queue", "add", trackID}); err != nil {
		t.Fatalf("room queue add: %v", err)
	}

	// The host is a member, which is all a room needs to run its own clock: the
	// song is under way before the member joins, so the position the stream is
	// asked to begin at is a number a test can hold on to.
	waitForCondition(t, ctx, "the room to be a couple of seconds into its song", func() bool {
		rooms, err := host.Client().Rooms(ctx)
		if err != nil {
			return false
		}
		for _, room := range rooms {
			if room.ID == roomID && room.Current != nil && room.Current.Item.TrackID == trackID {
				return room.Current.PositionAt(room.ServerNowMs) > 2_000
			}
		}
		return false
	})

	// Everything the member sees is the server's own answer, except that this
	// rendition is still being prepared - so the URL it is handed is the
	// source's stream - and the file's bytes are held until the test says the
	// download is done, which is the wait a provider's song really has.
	streams := &streamRequests{}
	fileGate := make(chan struct{})
	var landed sync.Once
	land := func() { landed.Do(func() { close(fileGate) }) }
	defer land()
	handler := host.embedded.server.Handler()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/variants"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"variants": []map[string]any{{
				"id": variantID, "trackId": trackID, "provider": "local", "downloadable": true,
				"title": "Room Song", "durationMs": 60_000,
				"media": map[string]any{"state": client.MediaDownloading},
			}}})
		case r.URL.Path == "/api/v1/media/"+variantID && r.URL.Query().Get("start") != "":
			streams.record(r.URL.Query().Get("start"))
			streamOf(t, stored).ServeHTTP(w, r)
		case r.URL.Path == "/api/v1/media/"+variantID:
			// The download this member performs: it is not finished until the
			// test says so.
			select {
			case <-fileGate:
			case <-r.Context().Done():
				return
			}
			handler.ServeHTTP(w, r)
		default:
			handler.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(proxy.Close)

	cfg := config.Default()
	cfg.StorageDir = t.TempDir()
	cfg.Client.CacheDir = t.TempDir()
	cfg.Client.ServerURL = proxy.URL
	out := &lockedBuffer{}
	member, err := New(cfg, slog.New(slog.DiscardHandler), out)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = member.Close() })

	memberCtx, leave := context.WithCancel(ctx)
	defer leave()
	go func() { _ = member.Run(memberCtx, []string{"room", "join", roomID}) }()

	// The sink was handed the stream at the room's position, not at zero: the
	// member is joining a song that is already under way.
	if asked := waitForStreams(t, ctx, streams, 1); asked[0] < 2_000 {
		t.Errorf("the stream was asked to begin at %d, want the room's position", asked[0])
	}
	// And the member's own line says what is happening.
	waitForOutput(t, ctx, out, "streaming while the file arrives")

	// A seek is the room moving by minutes, and a stream cannot be seeked: the
	// only way to follow it is to ask for the stream again where the room is.
	if err := host.Run(ctx, []string{"room", "seek", "20000"}); err != nil {
		t.Fatalf("room seek: %v", err)
	}
	asked := waitForStreams(t, ctx, streams, 2)
	if last := asked[len(asked)-1]; last < 19_000 {
		t.Errorf("after the room's seek the stream began at %d, want the room's new position", last)
	}

	// The download lands, and the file takes over from the stream.
	land()
	waitForCondition(t, ctx, "the rendition in the member's own cache", func() bool {
		_, ok := member.cache.Lookup(variantID)
		return ok
	})
	if line := waitForHandover(t, ctx, out); !strings.Contains(line, "1:") {
		t.Errorf("the line the file took over on says %q, want where the song had reached", line)
	}
}
