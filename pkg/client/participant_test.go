package client_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"codeberg.org/kyleraykbs/musoak/internal/api"
	"codeberg.org/kyleraykbs/musoak/internal/config"
	"codeberg.org/kyleraykbs/musoak/pkg/client"
)

// newServer boots a real server (in-memory transport, on-disk store in a temp
// dir) and returns a client for it.
func newServer(t *testing.T, mutate func(*config.Config)) *client.Client {
	t.Helper()
	cfg := config.Default()
	cfg.StorageDir = t.TempDir()
	cfg.Providers.YTMusic.Enabled = false
	cfg.Providers.Spotify.Enabled = false
	// The plain YouTube provider would reach the network; a test wants none of it.
	cfg.Providers.YouTube.Enabled = false
	if mutate != nil {
		mutate(cfg)
	}

	server, err := api.New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(func() {
		httpServer.Close()
		if err := server.Close(); err != nil {
			t.Errorf("server.Close: %v", err)
		}
	})
	return client.New(httpServer.URL)
}

// tone renders a sine wave of the given length and returns its path.
func tone(t *testing.T, dir, name string, seconds float64) string {
	t.Helper()
	dst := filepath.Join(dir, name)
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration="+strconv.FormatFloat(seconds, 'f', 2, 64),
		"-c:a", "libopus", "-f", "opus", dst)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Skipf("ffmpeg unavailable: %v: %s", err, stderr.String())
	}
	return dst
}

// fakeSink simulates a player: it "plays" for as long as this client's own
// file lasts, and records every slot it was handed.
type fakeSink struct {
	mu      sync.Mutex
	plays   []client.SinkTrack
	stopped int
}

func (s *fakeSink) Play(ctx context.Context, track client.SinkTrack) error {
	s.mu.Lock()
	s.plays = append(s.plays, track)
	s.mu.Unlock()

	remaining := track.DurationMs - track.PositionMs
	if remaining < 0 {
		remaining = 0
	}
	timer := time.NewTimer(time.Duration(remaining) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *fakeSink) Stop(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped++
	return nil
}

func (s *fakeSink) recorded() []client.SinkTrack {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]client.SinkTrack(nil), s.plays...)
}

func (s *fakeSink) stopCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

type participantClient struct {
	api         *client.Client
	room        *client.RoomClient
	sink        *fakeSink
	participant *client.Participant
}

// TestParticipantsFollowTheRoom is Block 9's acceptance scenario driven
// entirely through the SDK: three clients hold renditions of different
// lengths, start together on the server's timeline, the shortest one pads with
// silence instead of ending the slot early, and a unanimous low vote skips the
// track.
func TestParticipantsFollowTheRoom(t *testing.T) {
	apiClient := newServer(t, func(cfg *config.Config) {
		cfg.ListenTogether.ReadyTimeoutSeconds = 5
		// All three clients must vote before the skip fires, so the test can
		// vote one by one and still observe the unanimous result.
		cfg.ListenTogether.MinVotersForSkip = 3
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Three renditions of one recording: same name, different lengths. The
	// server matches them onto a single canonical track.
	files := t.TempDir()
	var (
		imported []client.Imported
		paths    []string
	)
	for _, length := range []float64{0.5, 0.4, 0.6} {
		path := tone(t, files, "Song One.opus", length)
		result, err := apiClient.ImportPath(ctx, path)
		if err != nil {
			t.Fatalf("ImportPath: %v", err)
		}
		if len(result.Imported) != 1 {
			t.Fatalf("imported %d entries, want 1", len(result.Imported))
		}
		imported = append(imported, result.Imported[0])
		paths = append(paths, path)
	}

	trackID := imported[0].Track.ID
	for i, entry := range imported {
		if entry.Track.ID != trackID {
			t.Fatalf("rendition %d landed on track %s, want them merged onto %s",
				i, entry.Track.ID, trackID)
		}
		if entry.Variant.Media.State != client.MediaReady {
			t.Fatalf("rendition %d is %s, want ready", i, entry.Variant.Media.State)
		}
	}

	// Every client already holds one rendition locally, like a CLI with a warm
	// cache: no downloads, and each reports its own duration to the room.
	clients := make([]*participantClient, len(imported))
	for i := range clients {
		clients[i] = &participantClient{
			api:  client.New(apiClient.BaseURL()),
			sink: &fakeSink{},
		}
	}

	// The host opens the room; the others join as guests.
	room, err := clients[0].api.CreateRoom(ctx, "test room", "everyone")
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	clients[0].room = room
	for i := 1; i < len(clients); i++ {
		joined, err := clients[i].api.JoinRoom(ctx, room.RoomID)
		if err != nil {
			t.Fatalf("JoinRoom: %v", err)
		}
		clients[i].room = joined
	}
	// Each participant gets a cache holding its own rendition of the track.
	for i, c := range clients {
		cache, err := client.OpenCache(filepath.Join(t.TempDir(), "cache"))
		if err != nil {
			t.Fatalf("OpenCache: %v", err)
		}
		if err := cache.Put(client.CacheEntry{
			VariantID:  imported[i].Variant.ID,
			TrackID:    trackID,
			Path:       paths[i],
			DurationMs: imported[i].Variant.Media.DurationMs,
		}); err != nil {
			t.Fatalf("cache.Put: %v", err)
		}
		if _, ok := cache.LookupTrack(trackID); !ok {
			t.Fatal("cache does not report the rendition it was given")
		}
		c.participant = client.NewParticipant(c.room, cache, c.sink, slog.New(slog.DiscardHandler))
	}

	runCtx, stopRuns := context.WithCancel(ctx)
	defer stopRuns()
	var runs sync.WaitGroup
	for _, c := range clients {
		runs.Add(1)
		go func(c *participantClient) {
			defer runs.Done()
			if err := c.participant.Run(runCtx); err != nil && runCtx.Err() == nil {
				t.Errorf("participant ended: %v", err)
			}
		}(c)
	}

	// Let the participants attach their event sockets before the room starts.
	time.Sleep(300 * time.Millisecond)

	if _, err := clients[0].room.Queue(ctx, trackID); err != nil {
		t.Fatalf("Queue: %v", err)
	}

	waitFor(t, 10*time.Second, "everybody to start playing", func() bool {
		for _, c := range clients {
			if len(c.sink.recorded()) == 0 {
				return false
			}
		}
		return true
	})

	slots := make([]client.SinkTrack, 0, len(clients))
	for _, c := range clients {
		slots = append(slots, c.sink.recorded()[0])
	}

	var shortest int64 = 1 << 40
	var longest int64
	timeline := slots[0].TimelineMs
	for i, slot := range slots {
		if slot.TrackID != trackID {
			t.Fatalf("client %d played %s, want %s", i, slot.TrackID, trackID)
		}
		if slot.StartedAtServerMs != slots[0].StartedAtServerMs {
			t.Errorf("client %d started at %d, want %d (synchronized start)",
				i, slot.StartedAtServerMs, slots[0].StartedAtServerMs)
		}
		if slot.PositionMs > 200 {
			t.Errorf("client %d began %d ms in; the room had just started", i, slot.PositionMs)
		}
		if slot.DurationMs < shortest {
			shortest = slot.DurationMs
		}
		if slot.DurationMs > longest {
			longest = slot.DurationMs
		}
		if slot.TimelineMs != timeline {
			t.Errorf("client knew a timeline of %d, want every client to agree on %d", slot.TimelineMs, timeline)
		}
	}

	// The room's timeline is the host's rendition: the host is the room's clock,
	// so their copy is what the room runs for, and everybody else follows them.
	if timeline != slots[0].DurationMs {
		t.Errorf("timeline = %d, want the host's rendition %d", timeline, slots[0].DurationMs)
	}
	if shortest >= longest {
		t.Fatalf("expected renditions of different lengths: %d vs %d", shortest, longest)
	}

	for _, c := range clients {
		if err := c.participant.Vote(ctx, 1); err != nil {
			t.Fatalf("Vote: %v", err)
		}
	}

	waitFor(t, 10*time.Second, "the vote to skip the track", func() bool {
		snapshot, err := clients[0].room.Snapshot(ctx)
		if err != nil {
			return false
		}
		return snapshot.Current == nil && len(snapshot.Queue) == 0
	})

	waitFor(t, 10*time.Second, "clients to stop playing", func() bool {
		for _, c := range clients {
			if c.sink.stopCount() == 0 {
				return false
			}
		}
		return true
	})

	// The room kept every rendition as a variant of one track.
	variants, err := clients[0].api.TrackVariants(ctx, trackID)
	if err != nil {
		t.Fatalf("TrackVariants: %v", err)
	}
	if len(variants) != len(imported) {
		t.Errorf("variants = %d, want %d", len(variants), len(imported))
	}

	stopRuns()
	runs.Wait()
}

// TestParticipantReportsLocalRendition checks the no-download path: a client
// that already has the track never asks the server for media, and the room
// learns the duration of the copy it will really play.
func TestParticipantReportsLocalRendition(t *testing.T) {
	apiClient := newServer(t, func(cfg *config.Config) {
		cfg.ListenTogether.ReadyTimeoutSeconds = 5
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	files := t.TempDir()
	path := tone(t, files, "Local Song.opus", 0.4)
	result, err := apiClient.ImportPath(ctx, path)
	if err != nil {
		t.Fatalf("ImportPath: %v", err)
	}
	entry := result.Imported[0]

	cache, err := client.OpenCache(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Put(client.CacheEntry{
		VariantID:  entry.Variant.ID,
		TrackID:    entry.Track.ID,
		Path:       path,
		DurationMs: entry.Variant.Media.DurationMs,
	}); err != nil {
		t.Fatal(err)
	}

	// This client opens the room, so it is the host: the room waits for its
	// file, and reporting ready is what starts the track.
	self := client.New(apiClient.BaseURL())
	room, err := self.CreateRoom(ctx, "pair", "everyone")
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}

	sink := &fakeSink{}
	participant := client.NewParticipant(room, cache, sink, slog.New(slog.DiscardHandler))

	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- participant.Run(runCtx) }()
	time.Sleep(300 * time.Millisecond)

	if _, err := room.Queue(ctx, entry.Track.ID); err != nil {
		t.Fatalf("Queue: %v", err)
	}

	// The host's own word starts the track, and the file it plays is the one it
	// already had: nothing is downloaded, and the room learns the length of the
	// copy that will really be heard.
	waitFor(t, 10*time.Second, "the cached rendition to play", func() bool {
		return len(sink.recorded()) > 0
	})

	slot := sink.recorded()[0]
	if slot.VariantID != entry.Variant.ID {
		t.Errorf("played variant %s, want the cached %s", slot.VariantID, entry.Variant.ID)
	}
	if slot.Path != path {
		t.Errorf("played %s, want the local copy %s", slot.Path, path)
	}
	if slot.DurationMs != entry.Variant.Media.DurationMs {
		t.Errorf("duration = %d, want %d", slot.DurationMs, entry.Variant.Media.DurationMs)
	}

	stop()
	<-done
}

// TestParticipantWarmsTheRoomsNextSongs checks that a member keeps the room's
// upcoming songs on disk rather than only the one it is playing. The server
// prepares a track at a time, and a room moves faster than a download: without
// this, a skip waits for one.
func TestParticipantWarmsTheRoomsNextSongs(t *testing.T) {
	apiClient := newServer(t, func(cfg *config.Config) {
		cfg.ListenTogether.ReadyTimeoutSeconds = 5
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Five local songs: one to play, the three after it, and one beyond the
	// window for the queue to be longer than what is fetched.
	files := t.TempDir()
	tracks := make([]string, 0, 5)
	for i := range 5 {
		path := tone(t, files, fmt.Sprintf("Ahead %d.opus", i), 0.4)
		result, err := apiClient.ImportPath(ctx, path)
		if err != nil {
			t.Fatalf("ImportPath: %v", err)
		}
		tracks = append(tracks, result.Imported[0].Track.ID)
	}

	cache, err := client.OpenCache(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}

	peer := client.New(apiClient.BaseURL())
	room, err := peer.CreateRoom(ctx, "ahead", "everyone")
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	self := client.New(apiClient.BaseURL())
	joined, err := self.JoinRoom(ctx, room.RoomID)
	if err != nil {
		t.Fatalf("JoinRoom: %v", err)
	}

	participant := client.NewParticipant(joined, cache, &fakeSink{}, slog.New(slog.DiscardHandler))
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- participant.Run(runCtx) }()
	time.Sleep(300 * time.Millisecond)

	for _, trackID := range tracks {
		if _, err := room.Queue(ctx, trackID); err != nil {
			t.Fatalf("Queue: %v", err)
		}
	}

	// The room prepares the first song; the three that follow it should be on
	// disk before anybody asks for them.
	waitFor(t, 20*time.Second, "the room's next three songs to be ready", func() bool {
		for _, trackID := range tracks[1:4] {
			if _, ok := cache.LookupTrack(trackID); !ok {
				return false
			}
		}
		return true
	})

	// And no further: the room is still on its first song, so the one after
	// the window is not this member's to fetch yet.
	if _, ok := cache.LookupTrack(tracks[4]); ok {
		t.Error("fetched a song beyond the room's next three")
	}

	stop()
	<-done
}

// TestClockOffsetIsStable checks the NTP-style clock estimate.
func TestClockOffsetIsStable(t *testing.T) {
	apiClient := newServer(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	offset, err := apiClient.ClockOffset(ctx, 5)
	if err != nil {
		t.Fatalf("ClockOffset: %v", err)
	}
	if offset > time.Second || offset < -time.Second {
		t.Errorf("offset = %v; the server clock is this machine's clock", offset)
	}
	now, err := apiClient.Clock(ctx)
	if err != nil {
		t.Fatalf("Clock: %v", err)
	}
	if delta := now - time.Now().UnixMilli(); delta > 2000 || delta < -2000 {
		t.Errorf("server clock differs by %d ms", delta)
	}
}

// TestAuthFlowOverSDK covers register, me, favorites and ranking through the
// public client.
func TestAuthFlowOverSDK(t *testing.T) {
	apiClient := newServer(t, nil)
	ctx := context.Background()

	auth, err := apiClient.Register(ctx, "sdk-user", "hunter2hunter2")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if auth.Token == "" || auth.User.Username != "sdk-user" {
		t.Fatalf("auth = %+v", auth)
	}
	if apiClient.Token() != auth.Token {
		t.Error("client did not keep the session token")
	}

	me, err := apiClient.Me(ctx)
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if !me.Authenticated || me.User == nil || me.User.Username != "sdk-user" {
		t.Fatalf("me = %+v", me)
	}

	files := t.TempDir()
	path := tone(t, files, "Fav.opus", 0.3)
	result, err := apiClient.ImportPath(ctx, path)
	if err != nil {
		t.Fatalf("ImportPath: %v", err)
	}
	trackID := result.Imported[0].Track.ID

	if err := apiClient.AddFavorite(ctx, trackID); err != nil {
		t.Fatalf("AddFavorite: %v", err)
	}
	favorites, err := apiClient.Favorites(ctx)
	if err != nil {
		t.Fatalf("Favorites: %v", err)
	}
	if len(favorites) != 1 || favorites[0].ID != trackID {
		t.Fatalf("favorites = %+v", favorites)
	}
	if err := apiClient.RemoveFavorite(ctx, trackID); err != nil {
		t.Fatalf("RemoveFavorite: %v", err)
	}
	if favorites, _ := apiClient.Favorites(ctx); len(favorites) != 0 {
		t.Errorf("favorites = %+v, want empty", favorites)
	}

	ranking, err := apiClient.SetRanking(ctx, []string{"local", "ytmusic"})
	if err != nil {
		t.Fatalf("SetRanking: %v", err)
	}
	// The caller's own order is kept, behind the two reserved slots.
	if len(ranking.Effective) < 3 || ranking.Effective[0] != "self" ||
		ranking.Effective[1] != "uploaded" || ranking.Effective[2] != "local" {
		t.Fatalf("ranking = %+v", ranking)
	}

	if err := apiClient.Logout(ctx); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if apiClient.Token() != "" {
		t.Error("client kept the token after logout")
	}
	if _, err := apiClient.Favorites(ctx); !client.IsUnauthorized(err) {
		t.Errorf("Favorites after logout: err = %v, want unauthorized", err)
	}
}

func waitFor(t *testing.T, timeout time.Duration, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
