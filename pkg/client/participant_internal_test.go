package client

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// idleSink is a player that holds the file it was given until its context
// ends: it never finishes on its own, so a test controls when a song ends.
type idleSink struct {
	mu    sync.Mutex
	plays []SinkTrack
}

func (s *idleSink) Play(ctx context.Context, track SinkTrack) error {
	s.mu.Lock()
	s.plays = append(s.plays, track)
	s.mu.Unlock()
	<-ctx.Done()
	return ctx.Err()
}

func (s *idleSink) Stop(ctx context.Context) error { return nil }

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
