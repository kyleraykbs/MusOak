package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// fetchServer is a server for one song whose rendition the server has not
// stored yet: the rendition listing, the download this client starts, and the
// file it finally gets. The file's bytes wait for the test, so the test decides
// when the download lands; a corrupt body is what a transfer that fails its
// checksum looks like.
type fetchServer struct {
	server  *httptest.Server
	variant string

	mu      sync.Mutex
	listed  int
	good    []byte
	served  []byte
	release chan struct{}
	landed  sync.Once
}

func newFetchServer(t *testing.T) *fetchServer {
	t.Helper()
	good := []byte("opus-bytes")
	s := &fetchServer{
		variant: "v1",
		good:    good,
		served:  good,
		release: make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/tracks/t1/variants", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.listed++
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"variants": []map[string]any{{
				"id": s.variant, "provider": "p", "downloadable": true,
			}},
		})
	})
	mux.HandleFunc("/api/v1/media/"+s.variant+"/download", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(MediaStatus{VariantID: s.variant, State: MediaReady, DurationMs: 300_000})
	})
	mux.HandleFunc("/api/v1/media/"+s.variant, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-s.release:
		case <-r.Context().Done():
			return
		}
		s.mu.Lock()
		served := append([]byte(nil), s.served...)
		etag := sha256Hex(s.good)
		s.mu.Unlock()
		w.Header().Set("ETag", `"`+etag+`"`)
		_, _ = w.Write(served)
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
func (s *fetchServer) land() { s.landed.Do(func() { close(s.release) }) }

// corrupt makes the media GET answer with bytes that do not match the hash it
// advertised, which is a transfer that failed rather than a file that landed.
func (s *fetchServer) corrupt() {
	s.mu.Lock()
	s.served = []byte("these are not the bytes that were promised")
	s.mu.Unlock()
}

// repair makes the media GET answer with the promised bytes again.
func (s *fetchServer) repair() {
	s.mu.Lock()
	s.served = append([]byte(nil), s.good...)
	s.mu.Unlock()
}

func (s *fetchServer) listedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listed
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// fetchRoom is a participant following a room on such a server, with a sink
// that records what it is handed.
func fetchRoom(t *testing.T) (*Participant, *fetchServer, *idleSink) {
	t.Helper()
	s := newFetchServer(t)
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

// A song whose rendition the server has not stored yet is fetched first, then
// played from the file: the client holds the file, and the server refuses to
// serve a rendition it has not stored.
func TestARenditionNotOnDiskIsFetchedThenPlayed(t *testing.T) {
	participant, server, sink := fetchRoom(t)
	item := QueueItem{ID: "item-1", TrackID: "t1", Title: "Song", DurationMs: 300_000}
	participant.adopt(atRoom(item, 45_000))

	participant.reconcile(context.Background())

	// Nothing is played while the file is still coming: there is no stream to
	// hand a sink, and the file is what the room's beat is waiting for.
	time.Sleep(200 * time.Millisecond)
	if played := sink.played(); len(played) != 0 {
		t.Fatalf("a song was played before its file landed: %+v", played)
	}

	server.land()
	slot := waitForPlays(t, sink, 1)[0]
	if slot.TrackID != item.TrackID || slot.Title != item.Title {
		t.Errorf("slot = %+v, want the room's song", slot)
	}
	if want := participant.cache.Path("v1"); slot.Path != want {
		t.Errorf("path = %q, want the fetched file %q", slot.Path, want)
	}
	if slot.VariantID != "v1" {
		t.Errorf("variant = %q, want the rendition the server offered", slot.VariantID)
	}
	if slot.PositionMs < 45_000 || slot.PositionMs > 47_000 {
		t.Errorf("began at %d, want the room's position (45000)", slot.PositionMs)
	}
	// The length the file does not yet report on its own comes from the room.
	if slot.DurationMs != 300_000 {
		t.Errorf("length = %d, want the room's 300000", slot.DurationMs)
	}
}

// Nothing on the reconcile loop may wait for a byte. The loop is what reads
// room events, corrects drift and answers commands; a fetch that never finishes
// must not hold it for a moment.
func TestReconcileNeverWaitsForTheFile(t *testing.T) {
	participant, server, sink := fetchRoom(t)
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

	// And the song is played the moment the file lands, from the file.
	server.land()
	slot := waitForPlays(t, sink, 1)[0]
	if want := participant.cache.Path("v1"); slot.Path != want {
		t.Fatalf("path = %q, want the fetched file %q", slot.Path, want)
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

// A fetch that fails must not take the room's song with it and must not be
// skipped: nothing is played, the room is still on that song, and the next beat
// tries again - and the file is what ends the wait.
func TestAFailedFetchIsRetriedNotSkipped(t *testing.T) {
	participant, server, sink := fetchRoom(t)
	server.corrupt()
	item := QueueItem{ID: "item-1", TrackID: "t1", Title: "Song", DurationMs: 300_000}
	participant.adopt(atRoom(item, 0))

	participant.reconcile(context.Background())
	time.Sleep(300 * time.Millisecond)

	if played := sink.played(); len(played) != 0 {
		t.Fatalf("a song was played after its fetch failed: %+v", played)
	}
	if _, ok := participant.cache.Lookup("v1"); ok {
		t.Error("a transfer that failed its checksum was cached")
	}

	// The loop carries on: the room is still on the song, and a later beat
	// fetches it again now that the bytes are whole.
	server.repair()
	server.land()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(sink.played()) == 0 {
		participant.reconcile(context.Background())
		time.Sleep(100 * time.Millisecond)
	}
	slot := waitForPlays(t, sink, 1)[0]
	if slot.TrackID != item.TrackID {
		t.Fatalf("the room's song was skipped for another: %+v", slot)
	}
	if want := participant.cache.Path("v1"); slot.Path != want {
		t.Errorf("path = %q, want the fetched file %q", slot.Path, want)
	}
}

// The listing is asked for once per track, not once per beat: the answer is
// remembered while the file is being fetched.
func TestTheRenditionChoiceIsRemembered(t *testing.T) {
	participant, server, sink := fetchRoom(t)
	item := QueueItem{ID: "item-1", TrackID: "t1", Title: "Song", DurationMs: 300_000}
	participant.adopt(atRoom(item, 0))

	for range 4 {
		participant.reconcile(context.Background())
		time.Sleep(30 * time.Millisecond)
	}
	if got := server.listedCount(); got > 1 {
		t.Errorf("the rendition listing was asked for %d times, want it resolved once", got)
	}
	server.land()
	waitForPlays(t, sink, 1)
}
