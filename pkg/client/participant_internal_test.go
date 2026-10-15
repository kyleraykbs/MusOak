package client

import (
	"context"
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

// A fetch on the reconcile loop freezes event processing, drift correction and
// every command for as long as the server takes to fetch from its provider and
// hand over the whole file. reconcile must act only on what is already on disk
// and leave the fetching to warm.
func TestReconcileDoesNotFetchOnTheLoop(t *testing.T) {
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/tracks/t1/variants", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"variants": []map[string]any{{"id": "v1", "downloadable": true}},
		})
	})
	mux.HandleFunc("/api/v1/media/v1/download", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(MediaStatus{VariantID: "v1", State: MediaReady})
	})
	// The download the old code performed on the loop; this one does not
	// finish while the test is watching.
	mux.HandleFunc("/api/v1/media/v1", func(w http.ResponseWriter, r *http.Request) {
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

	item := QueueItem{ID: "item-1", TrackID: "t1", DurationMs: 300_000}
	participant.adopt(&Room{
		ID: "r1", Seq: 1, ServerNowMs: time.Now().UnixMilli(),
		Current: &RoomPlayback{Item: item, Started: true, DurationMs: 300_000},
	})

	done := make(chan struct{})
	go func() {
		participant.reconcile(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("reconcile blocked on a fetch; it must act only on what is already on disk")
	}
	if played := sink.played(); len(played) != 0 {
		t.Fatalf("reconcile played a song that was never fetched: %+v", played)
	}
}
