package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// idleSink is a player that holds the file it was given until its context
// ends: it never finishes on its own, so a test controls when a song ends.
type idleSink struct {
	mu       sync.Mutex
	plays    []SinkTrack
	seeks    []int64
	position int64
}

// setPosition says where the sink has got to, as a real player reports it.
func (s *idleSink) setPosition(positionMs int64) {
	s.mu.Lock()
	s.position = positionMs
	s.mu.Unlock()
}

func (s *idleSink) Play(ctx context.Context, track SinkTrack) error {
	s.mu.Lock()
	s.plays = append(s.plays, track)
	s.mu.Unlock()
	<-ctx.Done()
	return ctx.Err()
}

func (s *idleSink) Stop(ctx context.Context) error { return nil }

// The follower rule needs a playhead and a way to move it.
func (s *idleSink) PositionMs(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.position, nil
}

func (s *idleSink) Seek(ctx context.Context, positionMs int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seeks = append(s.seeks, positionMs)
	s.position = positionMs
	return nil
}

func (s *idleSink) seeked() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.seeks...)
}

func (s *idleSink) played() []SinkTrack {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SinkTrack(nil), s.plays...)
}

// A client reads ahead of what it is playing, so its own advance comes before
// the room's. When the two then agree, and a later beat names the finished
// song again, the client must not load it - that is the jump back into a song
// with two minutes left of it.
func TestAPlayedItemIsNeverLoadedAgain(t *testing.T) {
	cache, err := OpenCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	entry := func(id string) CacheEntry {
		return CacheEntry{
			VariantID:  "v-" + id,
			TrackID:    "t-" + id,
			Path:       filepath.Join(dir, id+".opus"),
			DurationMs: 300_000,
		}
	}
	for _, id := range []string{"item-1", "item-2"} {
		// The file has to exist: the cache only serves what is on disk.
		if err := os.WriteFile(entry(id).Path, []byte("opus"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := cache.Put(entry(id)); err != nil {
			t.Fatal(err)
		}
	}

	sink := &idleSink{}
	// The client is never dialled: the cache answers every lookup.
	room := &RoomClient{client: New("http://127.0.0.1:9"), RoomID: "r1", MemberID: "m1"}
	participant := NewParticipant(room, cache, sink, slog.New(slog.DiscardHandler))

	item := func(id string) QueueItem {
		return QueueItem{ID: id, TrackID: "t-" + id, DurationMs: 300_000}
	}
	one, two := item("item-1"), item("item-2")

	// This client played the first song and advanced past it on its own.
	participant.adopt(&Room{
		ID: "r1", Seq: 1, ServerNowMs: 1,
		Current:     &RoomPlayback{Item: one, Started: true, DurationMs: 300_000},
		MasterQueue: []QueueItem{one, two},
	})
	participant.setLocal(&localPlayback{
		itemID: one.ID, trackID: one.TrackID, entry: entry(one.ID),
	})
	participant.advanceLocal(context.Background(), one.ID)

	// The room agrees with this client on the new song...
	participant.adopt(&Room{
		ID: "r1", Seq: 2, ServerNowMs: 2,
		Current:     &RoomPlayback{Item: two, Started: true, DurationMs: 300_000},
		MasterQueue: []QueueItem{two},
	})
	participant.reconcile(context.Background())

	// ...and a stale beat names the finished song again, two minutes in.
	participant.adopt(&Room{
		ID: "r1", Seq: 3, ServerNowMs: 3,
		Current:     &RoomPlayback{Item: one, Started: true, PositionMs: 120_000, DurationMs: 300_000},
		MasterQueue: []QueueItem{one, two},
	})
	participant.reconcile(context.Background())

	// Give the loads a moment to reach the sink, then check that nothing
	// loaded the finished song again. (Loading the *second* song is the
	// legitimate one this client did on its own.)
	time.Sleep(300 * time.Millisecond)
	for _, play := range sink.played() {
		if play.TrackID == one.TrackID {
			t.Fatalf("the finished song was loaded again: %+v", sink.played())
		}
	}
}

// Somebody dragging the web's seek bar moves the room's position by minutes in
// one beat. That is a seek, not a broken comparison, and the bot follows it -
// the guard against an absurd drift is for a disagreement that will not go
// away, and it used to swallow the seek as well.
func TestARoomSeekIsFollowedHoweverFar(t *testing.T) {
	cache, err := OpenCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	entry := CacheEntry{
		VariantID: "v1", TrackID: "t1",
		Path: filepath.Join(dir, "song.opus"), DurationMs: 300_000,
	}
	if err := os.WriteFile(entry.Path, []byte("opus"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cache.Put(entry); err != nil {
		t.Fatal(err)
	}

	sink := &idleSink{}
	room := &RoomClient{client: New("http://127.0.0.1:9"), RoomID: "r1", MemberID: "m1"}
	participant := NewParticipant(room, cache, sink, slog.New(slog.DiscardHandler))
	item := QueueItem{ID: "item-1", TrackID: "t1", DurationMs: 300_000}
	participant.setLocal(&localPlayback{itemID: item.ID, trackID: item.TrackID, entry: entry, playing: true})

	// The room plays from the start, and this client is with it. The stamps are
	// the real clock's, because the participant extrapolates from its own.
	start := time.Now().UnixMilli()
	participant.adopt(&Room{
		ID: "r1", Seq: 1, ServerNowMs: start,
		Current: &RoomPlayback{Item: item, Started: true, PositionMs: 0, AtMs: start, DurationMs: 300_000},
	})
	participant.reconcile(context.Background())
	if seeks := sink.seeked(); len(seeks) != 0 {
		t.Fatalf("seeks before the room moved: %v", seeks)
	}

	// Ninety seconds later somebody seeks the room forward by a minute and a
	// half: the room's position jumps, the client's does not.
	seeksAt := time.Now().UnixMilli()
	participant.adopt(&Room{
		ID: "r1", Seq: 2, ServerNowMs: seeksAt,
		Current: &RoomPlayback{Item: item, Started: true, PositionMs: 90_000, AtMs: seeksAt, DurationMs: 300_000},
	})
	participant.reconcile(context.Background())

	seeks := sink.seeked()
	if len(seeks) == 0 {
		t.Fatal("the room was seeked and the client did not follow")
	}
	if got := seeks[len(seeks)-1]; got < 89_000 || got > 93_000 {
		t.Fatalf("seeked to %d, want the room's new position", got)
	}
}

// streamServer is a server for one song whose file the server has not finished
// preparing: the rendition listing, the stream it answers the URL with, and the
// download this client performs behind it. The file's own bytes are released by
// the test, so the test decides when the download lands.
type streamServer struct {
	server  *httptest.Server
	variant string
	// state is what the rendition listing says its file is. Anything but
	// "ready" means the server is still preparing it, which is what makes the
	// URL a stream.
	state string

	mu sync.Mutex
	// listed counts the rendition listings, land releases the file's bytes, and
	// fileBody is what the download is answered with - a test may put a
	// truncated transfer there, which is a download that fails its checksum.
	listed   int
	release  chan struct{}
	landed   sync.Once
	fileBody []byte
}

// corruptFile makes the download answer with bytes that do not match the hash it
// advertised, which is a transfer that failed rather than a file that landed.
func (s *streamServer) corruptFile() {
	s.mu.Lock()
	s.fileBody = []byte("these are not the bytes that were promised")
	s.mu.Unlock()
}

func newStreamServer(t *testing.T, variant, state string) *streamServer {
	t.Helper()
	s := &streamServer{
		variant: variant, state: state,
		release:  make(chan struct{}),
		fileBody: []byte("opus-bytes"),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/tracks/t1/variants", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.listed++
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"variants": []map[string]any{{
				"id": variant, "provider": "p", "downloadable": true,
				"media": map[string]any{"state": state},
			}},
		})
	})
	mux.HandleFunc("/api/v1/media/"+variant+"/download", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(MediaStatus{VariantID: variant, State: MediaReady, DurationMs: 300_000})
	})
	// The file itself: a GET without a start offset is the download this client
	// performs, and it waits for the test to say the bytes are here. A GET with
	// one is the stream the sink is handed - the client never reads it, because
	// its sink is a fake.
	// The hash the download advertises is the one for the whole file: a
	// transfer whose bytes do not match it is a failed download, which is what
	// a test that corrupts the body is asking for.
	etag := fmt.Sprintf("%q", sha256Hex(s.fileBody))
	mux.HandleFunc("/api/v1/media/"+variant, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("start") != "" {
			w.Header().Set("X-Musoak-Stream", "1")
			w.Header().Set("X-Musoak-Duration", "300000")
			_, _ = w.Write([]byte("stream"))
			return
		}
		select {
		case <-s.release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("ETag", etag)
		s.mu.Lock()
		body := s.fileBody
		s.mu.Unlock()
		_, _ = w.Write(body)
	})
	s.server = httptest.NewServer(mux)
	t.Cleanup(s.server.Close)
	// The file's bytes are released after the server's own cleanup is
	// registered, so it runs first: a download still waiting for them is what
	// some of these tests are about, and a handler left waiting would hold the
	// server's Close open for ever.
	t.Cleanup(s.land)
	return s
}

// land lets the file's bytes arrive, which is the download finishing.
func (s *streamServer) land() { s.landed.Do(func() { close(s.release) }) }

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// streamingRoom is a participant following a room on such a server, with a sink
// that records what it is handed.
func streamingRoom(t *testing.T, state string) (*Participant, *streamServer, *idleSink) {
	t.Helper()
	s := newStreamServer(t, "v1", state)
	cache, err := OpenCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sink := &idleSink{}
	room := &RoomClient{client: New(s.server.URL), RoomID: "r1", MemberID: "m1"}
	participant := NewParticipant(room, cache, sink, slog.New(slog.DiscardHandler))
	return participant, s, sink
}

// atRoom stamps a room playing one song from positionMs, on the real clock the
// participant extrapolates from.
func atRoom(item QueueItem, positionMs int64) *Room {
	now := time.Now().UnixMilli()
	return &Room{
		ID: "r1", Seq: 1, ServerNowMs: now,
		Current: &RoomPlayback{
			Item: item, Started: true, PositionMs: positionMs, AtMs: now, DurationMs: item.DurationMs,
		},
		MasterQueue: []QueueItem{item},
	}
}

// waitForPlays waits until a sink has been handed at least n slots.
func waitForPlays(t *testing.T, sink *idleSink, n int) []SinkTrack {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if played := sink.played(); len(played) >= n {
			return played
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the sink was handed %d slots, want %d", len(sink.played()), n)
	return nil
}

// A song the server has not prepared yet is played as the server's own stream,
// at the room's position: the URL carries the offset, because a stream that has
// not finished has no length to seek within, and the player is told the song's
// length from the room rather than from bytes that do not exist yet.
func TestASongThatIsNotHereIsStreamedAtTheRoomPosition(t *testing.T) {
	participant, _, sink := streamingRoom(t, MediaDownloading)
	item := QueueItem{ID: "item-1", TrackID: "t1", Title: "Song", DurationMs: 300_000}
	participant.adopt(atRoom(item, 45_000))

	participant.reconcile(context.Background())

	slot := waitForPlays(t, sink, 1)[0]
	if !slot.Streaming {
		t.Fatalf("the song was not handed over as a stream: %+v", slot)
	}
	if slot.TrackID != item.TrackID || slot.Title != item.Title {
		t.Errorf("slot = %+v, want the room's song", slot)
	}
	if slot.PositionMs < 45_000 || slot.PositionMs > 46_000 {
		t.Errorf("began at %d, want the room's position (45000)", slot.PositionMs)
	}
	if want := participant.Room().Client().MediaURL("v1", slot.PositionMs); slot.Path != want {
		t.Errorf("path = %q, want the stream URL %q", slot.Path, want)
	}
	if !strings.Contains(slot.Path, "start=") {
		t.Errorf("path = %q, want the stream to begin where the room is", slot.Path)
	}
	if slot.VariantID != "v1" {
		t.Errorf("variant = %q, want the rendition the server offered", slot.VariantID)
	}
	// The length a stream cannot carry comes from the room.
	if slot.DurationMs != 300_000 {
		t.Errorf("length = %d, want the room's 300000", slot.DurationMs)
	}
}

// A rendition the server already holds is a file over HTTP rather than a stream:
// it is seeked to the room's position, and its URL names the whole file.
func TestARenditionTheServerHoldsIsSeekedRatherThanStreamed(t *testing.T) {
	participant, _, sink := streamingRoom(t, MediaReady)
	item := QueueItem{ID: "item-1", TrackID: "t1", Title: "Song", DurationMs: 300_000}
	participant.adopt(atRoom(item, 45_000))

	participant.reconcile(context.Background())

	slot := waitForPlays(t, sink, 1)[0]
	if slot.Streaming {
		t.Fatalf("a stored file was handed over as a stream: %+v", slot)
	}
	if want := participant.Room().Client().MediaURL("v1", 0); slot.Path != want {
		t.Errorf("path = %q, want the stored file %q", slot.Path, want)
	}
	if slot.PositionMs < 45_000 || slot.PositionMs > 46_000 {
		t.Errorf("position = %d, want the room's position", slot.PositionMs)
	}
}

// Nothing on the reconcile loop may wait for a byte. The loop is what reads room
// events, corrects drift and answers commands; a download that never finishes
// must not hold it for a moment, and what the sink is handed is the stream while
// the file is still coming.
func TestReconcileNeverWaitsForTheFile(t *testing.T) {
	participant, _, sink := streamingRoom(t, MediaDownloading)
	item := QueueItem{ID: "item-1", TrackID: "t1", Title: "Song", DurationMs: 300_000}
	participant.adopt(atRoom(item, 0))

	for i := range 3 {
		done := make(chan struct{})
		go func() {
			participant.reconcile(context.Background())
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("reconcile %d blocked while the file was still being fetched", i)
		}
	}

	// And the song is heard in the meantime: the sink holds the server's stream.
	slot := waitForPlays(t, sink, 1)[0]
	if !slot.Streaming {
		t.Fatalf("the wait for the file was passed on to the listener: %+v", slot)
	}
}

// The round trip that chooses the rendition is not the loop's either: a server
// that never answers the listing must not hold up room events, and nothing may
// be played until it does answer.
func TestReconcileNeverWaitsForTheRendition(t *testing.T) {
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/tracks/t1/variants", func(w http.ResponseWriter, r *http.Request) {
		<-release
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	defer close(release)

	cache, err := OpenCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sink := &idleSink{}
	room := &RoomClient{client: New(server.URL), RoomID: "r1", MemberID: "m1"}
	participant := NewParticipant(room, cache, sink, slog.New(slog.DiscardHandler))
	item := QueueItem{ID: "item-1", TrackID: "t1", Title: "Song", DurationMs: 300_000}
	participant.adopt(atRoom(item, 0))

	done := make(chan struct{})
	go func() {
		participant.reconcile(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reconcile blocked on the rendition listing")
	}
	if played := sink.played(); len(played) != 0 {
		t.Fatalf("a song was played with no rendition chosen: %+v", played)
	}
}

// The handover must land where the music got to: the file is the whole song,
// while the stream began part-way in, so the offset it began at is added back to
// the sink's playhead before the file starts there.
func TestTheFileTakesOverWhereTheStreamReached(t *testing.T) {
	participant, server, sink := streamingRoom(t, MediaDownloading)
	item := QueueItem{ID: "item-1", TrackID: "t1", Title: "Song", DurationMs: 300_000}
	participant.adopt(atRoom(item, 45_000))

	participant.reconcile(context.Background())
	stream := waitForPlays(t, sink, 1)[0]

	// Three seconds of the stream have played.
	sink.setPosition(3_000)
	server.land()

	played := waitForPlays(t, sink, 2)
	file := played[1]
	if file.Streaming {
		t.Fatalf("the handover kept playing the stream: %+v", file)
	}
	if file.TrackID != item.TrackID {
		t.Errorf("the handover moved to another song: %+v", file)
	}
	if want := filepath.Join(participant.cache.Dir(), "v1.opus"); file.Path != want {
		t.Errorf("path = %q, want the file that landed at %q", file.Path, want)
	}
	if want := stream.PositionMs + 3_000; file.PositionMs < want-250 || file.PositionMs > want+250 {
		t.Errorf("the file began at %d, want where the stream got to (%d)", file.PositionMs, want)
	}
	if file.DurationMs != 300_000 {
		t.Errorf("length = %d, want the song's", file.DurationMs)
	}
}

// refusingSink is a player whose stream will not play at all - the 503 the
// server answers a third listener with, a provider that is refusing - and which
// says where it is, so the follower rule runs as it really would.
type refusingSink struct {
	mu       sync.Mutex
	plays    []SinkTrack
	position int64
}

func (s *refusingSink) Play(ctx context.Context, track SinkTrack) error {
	s.mu.Lock()
	s.plays = append(s.plays, track)
	s.mu.Unlock()
	if track.Streaming {
		return errors.New("musoak: server returned Service Unavailable (503)")
	}
	<-ctx.Done()
	return ctx.Err()
}

func (s *refusingSink) Stop(ctx context.Context) error { return nil }

func (s *refusingSink) PositionMs(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.position, nil
}

func (s *refusingSink) Seek(ctx context.Context, positionMs int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.position = positionMs
	return nil
}

func (s *refusingSink) played() []SinkTrack {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SinkTrack(nil), s.plays...)
}

// A stream that will not play is a wait, not a skipped song: it is asked for
// again while the download behind it is still coming, the waits doubling, and
// the file is what ends the wait. The room's next song is never reached by this
// client in the meantime - the room decides when a song is over.
func TestAStreamThatWillNotPlayIsWaitedForNotSkipped(t *testing.T) {
	server := newStreamServer(t, "v1", MediaDownloading)
	cache, err := OpenCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sink := &refusingSink{}
	room := &RoomClient{client: New(server.server.URL), RoomID: "r1", MemberID: "m1"}
	participant := NewParticipant(room, cache, sink, slog.New(slog.DiscardHandler))

	first := QueueItem{ID: "item-1", TrackID: "t1", Title: "First", DurationMs: 300_000}
	second := QueueItem{ID: "item-2", TrackID: "t2", Title: "Second", DurationMs: 300_000}
	participant.adopt(&Room{
		ID: "r1", Seq: 1, ServerNowMs: time.Now().UnixMilli(),
		Current: &RoomPlayback{
			Item: first, Started: true, AtMs: time.Now().UnixMilli(), DurationMs: first.DurationMs,
		},
		MasterQueue: []QueueItem{first, second},
	})

	// The room's beat, for a couple of seconds: every attempt fails at once, so
	// what these beats are is the wait between attempts.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		participant.reconcile(context.Background())
		time.Sleep(200 * time.Millisecond)
	}
	attempts := sink.played()
	if len(attempts) < 2 {
		t.Fatalf("the stream was asked for %d time(s); a refused one is asked again", len(attempts))
	}
	for _, attempt := range attempts {
		if attempt.TrackID != first.TrackID {
			t.Fatalf("the room's song was skipped for another: %+v", attempt)
		}
		if !attempt.Streaming {
			t.Fatalf("a file was played while the rendition was not here: %+v", attempt)
		}
	}
	if _, ok := cache.LookupTrack(first.TrackID); ok {
		t.Fatal("the file was here after all; this test is about the wait")
	}

	// The download lands, and the file is what ends the wait.
	server.land()
	file := waitForFilePlay(t, sink, first.TrackID)
	if want := filepath.Join(cache.Dir(), "v1.opus"); file.Path != want {
		t.Errorf("path = %q, want the file that landed at %q", file.Path, want)
	}
	for _, played := range sink.played() {
		if played.TrackID == second.TrackID {
			t.Fatalf("this client ran past a song the room was still on: %+v", sink.played())
		}
	}
}

// waitForFilePlay waits until a sink has been handed the file for a track.
func waitForFilePlay(t *testing.T, sink *refusingSink, trackID string) SinkTrack {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, played := range sink.played() {
			if played.TrackID == trackID && !played.Streaming {
				return played
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the file for %s never reached the sink: %+v", trackID, sink.played())
	return SinkTrack{}
}

// A download that fails must not take the song with it: what the listener is
// hearing is the stream, the room is still on that song, and the file simply
// never lands. Nothing stops, nothing is skipped, and nothing half-finished is
// passed off as the rendition.
func TestAFailedDownloadDoesNotTakeTheStreamWithIt(t *testing.T) {
	participant, server, sink := streamingRoom(t, MediaDownloading)
	server.corruptFile()
	item := QueueItem{ID: "item-1", TrackID: "t1", Title: "Song", DurationMs: 300_000}
	participant.adopt(atRoom(item, 45_000))

	participant.reconcile(context.Background())
	stream := waitForPlays(t, sink, 1)[0]
	if !stream.Streaming {
		t.Fatalf("the song was not handed over as a stream: %+v", stream)
	}

	// The transfer fails its checksum, so the file never lands.
	server.land()
	time.Sleep(500 * time.Millisecond)

	if played := sink.played(); len(played) != 1 {
		t.Fatalf("a failed download changed what is playing: %+v", played)
	}
	if participant.local() == nil {
		t.Fatal("the song was dropped when its download failed")
	}
	if _, ok := participant.cache.Lookup("v1"); ok {
		t.Error("a transfer that failed its checksum was cached")
	}

	// And the loop carries on with it.
	participant.reconcile(context.Background())
	if played := sink.played(); len(played) != 1 {
		t.Fatalf("the loop moved the song on after a failed download: %+v", played)
	}
}
