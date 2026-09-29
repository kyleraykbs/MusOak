package client_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"codeberg.org/kyleraykbs/prismusic/internal/api"
	"codeberg.org/kyleraykbs/prismusic/internal/config"
	"codeberg.org/kyleraykbs/prismusic/pkg/client"
)

// newServer boots a real server (in-memory transport, on-disk store in a temp
// dir) and returns a client for it.
func newServer(t *testing.T, mutate func(*config.Config)) *client.Client {
	t.Helper()
	cfg := config.Default()
	cfg.StorageDir = t.TempDir()
	cfg.Providers.YTMusic.Enabled = false
	cfg.Providers.Spotify.Enabled = false
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

	var longest int64
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
		if slot.TimelineMs > longest {
			longest = slot.TimelineMs
		}
	}

	// The room's timeline is the longest rendition; the shorter files pad with
	// silence rather than ending the room's slot early.
	var shortest int64 = 1 << 40
	for _, slot := range slots {
		if slot.DurationMs < shortest {
			shortest = slot.DurationMs
		}
		if slot.TimelineMs != longest {
			t.Errorf("client knew a timeline of %d, want the longest rendition %d", slot.TimelineMs, longest)
		}
	}
	if shortest >= longest {
		t.Fatalf("expected one rendition shorter than the timeline: %d vs %d", shortest, longest)
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

	peer := client.New(apiClient.BaseURL())
	room, err := peer.CreateRoom(ctx, "pair", "everyone")
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	self := client.New(apiClient.BaseURL())
	joined, err := self.JoinRoom(ctx, room.RoomID)
	if err != nil {
		t.Fatalf("JoinRoom: %v", err)
	}

	sink := &fakeSink{}
	participant := client.NewParticipant(joined, cache, sink, slog.New(slog.DiscardHandler))

	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- participant.Run(runCtx) }()
	time.Sleep(300 * time.Millisecond)

	if _, err := room.Queue(ctx, entry.Track.ID); err != nil {
		t.Fatalf("Queue: %v", err)
	}

	// The peer never reports ready, so the readiness timeout (not this client)
	// decides when the track starts; our participant then plays its local copy
	// from the room position.
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
	if len(ranking.Effective) == 0 || ranking.Effective[0] != "local" {
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
