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

// playerSink simulates a player that can pause, seek and report where it is,
// which is what the follower rule needs to be exercised end to end.
type playerSink struct {
	mu       sync.Mutex
	plays    []client.SinkTrack
	seeks    []int64
	finished int
	stopped  int
	position int64
	started  time.Time
	paused   bool
	pausedAt int64
}

func (s *playerSink) Play(ctx context.Context, track client.SinkTrack) error {
	s.mu.Lock()
	s.plays = append(s.plays, track)
	s.position = track.PositionMs
	s.started = time.Now()
	s.paused = false
	s.mu.Unlock()

	for {
		s.mu.Lock()
		position, paused := s.at(time.Now())
		s.mu.Unlock()
		if !paused && track.DurationMs > 0 && position >= track.DurationMs {
			s.mu.Lock()
			s.finished++
			s.mu.Unlock()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (s *playerSink) at(now time.Time) (int64, bool) {
	if s.paused {
		return s.pausedAt, true
	}
	return s.position + now.Sub(s.started).Milliseconds(), false
}

func (s *playerSink) Stop(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped++
	return nil
}

func (s *playerSink) Pause(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.paused {
		s.pausedAt, _ = s.at(time.Now())
		s.paused = true
	}
	return nil
}

func (s *playerSink) Resume(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.paused {
		s.position = s.pausedAt
		s.started = time.Now()
		s.paused = false
	}
	return nil
}

func (s *playerSink) Seek(ctx context.Context, positionMs int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seeks = append(s.seeks, positionMs)
	s.position = positionMs
	s.started = time.Now()
	if s.paused {
		s.pausedAt = positionMs
	}
	return nil
}

func (s *playerSink) PositionMs(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	position, _ := s.at(time.Now())
	return position, nil
}

func (s *playerSink) recorded() []client.SinkTrack {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]client.SinkTrack(nil), s.plays...)
}

func (s *playerSink) seekCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seeks)
}

func (s *playerSink) currentPosition() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	position, _ := s.at(time.Now())
	return position
}

func (s *playerSink) stopCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

type participantClient struct {
	api         *client.Client
	room        *client.RoomClient
	sink        *playerSink
	participant *client.Participant
}

// importTrack renders a tone and imports it under the given title.
func importTrack(t *testing.T, ctx context.Context, apiClient *client.Client, title string, seconds float64) (string, string, client.Imported) {
	t.Helper()
	path := tone(t, t.TempDir(), title, seconds)
	result, err := apiClient.ImportPath(ctx, path)
	if err != nil {
		t.Fatalf("ImportPath: %v", err)
	}
	if len(result.Imported) != 1 {
		t.Fatalf("imported %d entries, want 1", len(result.Imported))
	}
	return path, result.Imported[0].Track.ID, result.Imported[0]
}

// roomClients builds n clients that each hold their own rendition of the same
// recording. Every client chooses its own version: the room assigns nothing.
func roomClients(t *testing.T, ctx context.Context, apiClient *client.Client, title string, lengths []float64) (*client.Client, []*participantClient, string) {
	t.Helper()
	var (
		imported []client.Imported
		paths    []string
	)
	var trackID string
	for _, length := range lengths {
		path, id, entry := importTrack(t, ctx, apiClient, title, length)
		if trackID == "" {
			trackID = id
		} else if id != trackID {
			t.Fatalf("rendition landed on track %s, want them merged onto %s", id, trackID)
		}
		imported = append(imported, entry)
		paths = append(paths, path)
	}

	clients := make([]*participantClient, len(imported))
	for i := range clients {
		clients[i] = &participantClient{
			api:  client.New(apiClient.BaseURL()),
			sink: &playerSink{},
		}
	}
	host, err := clients[0].api.CreateRoom(ctx, "test room", "everyone")
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	clients[0].room = host
	for i := 1; i < len(clients); i++ {
		joined, err := clients[i].api.JoinRoom(ctx, host.RoomID)
		if err != nil {
			t.Fatalf("JoinRoom: %v", err)
		}
		clients[i].room = joined
	}
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
		c.participant = client.NewParticipant(c.room, cache, c.sink, slog.New(slog.DiscardHandler))
	}
	return apiClient, clients, trackID
}

func runParticipants(t *testing.T, ctx context.Context, clients []*participantClient) func() {
	t.Helper()
	runCtx, stop := context.WithCancel(ctx)
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
	return func() {
		stop()
		runs.Wait()
	}
}

// TestTheHostStartsAndFollowersFollow is the whole contract: the room holds a
// song until the host's player begins it, everybody plays the same song at the
// same position, and a room seek pulls a follower that has drifted back.
func TestTheHostStartsAndFollowersFollow(t *testing.T) {
	apiClient := newServer(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	// Different lengths: each client plays its own version, and nobody waits
	// for anybody.
	apiClient, clients, trackID := roomClients(t, ctx, apiClient, "Same Song.opus", []float64{8.0, 7.0})

	// Let the participants attach their event sockets before the room starts.
	stop := runParticipants(t, ctx, clients)
	defer stop()
	time.Sleep(300 * time.Millisecond)

	if _, err := clients[0].room.Queue(ctx, trackID); err != nil {
		t.Fatalf("Queue: %v", err)
	}

	waitFor(t, 15*time.Second, "everybody to start playing", func() bool {
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
	for i, slot := range slots {
		if slot.TrackID != trackID {
			t.Fatalf("client %d played %s, want %s", i, slot.TrackID, trackID)
		}
		if slot.PositionMs > 2000 {
			t.Errorf("client %d began %d ms in; the room had just started", i, slot.PositionMs)
		}
	}

	// The room's clock is the host's file, and the host's report is what ran it.
	snapshot, err := clients[0].room.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snapshot.Current == nil || !snapshot.Current.Started {
		t.Fatalf("the room never started: %+v", snapshot.Current)
	}
	if snapshot.Current.DurationMs != slots[0].DurationMs {
		t.Errorf("room length = %d, want the host's file %d",
			snapshot.Current.DurationMs, slots[0].DurationMs)
	}

	// A seek four seconds in is more than the two-second tolerance: the
	// follower must move.
	if _, err := clients[0].room.Seek(ctx, 4000); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	waitFor(t, 10*time.Second, "the follower to seek back onto the room", func() bool {
		return clients[1].sink.seekCount() > 0
	})
	waitFor(t, 5*time.Second, "both clients to sit within the tolerance", func() bool {
		room, err := clients[1].room.Snapshot(ctx)
		if err != nil || room.Current == nil {
			return false
		}
		want := room.Current.PositionAt(room.ServerNowMs)
		got := clients[1].sink.currentPosition()
		drift := got - want
		if drift < 0 {
			drift = -drift
		}
		return drift <= 2500
	})
}

// TestTheHostsFileEndingMovesTheSongOn: the host's participant plays the room's
// queue to the end, reporting each file running out, and the room advances with
// it while followers follow.
func TestTheHostsFileEndingMovesTheSongOn(t *testing.T) {
	apiClient := newServer(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	apiClient, clients, first := roomClients(t, ctx, apiClient, "First Song.opus", []float64{0.7, 0.7})
	// A second recording, so the queue has somewhere to advance to.
	_, secondTrack, _ := importTrack(t, ctx, apiClient, "Second Song.opus", 0.7)

	stop := runParticipants(t, ctx, clients)
	defer stop()
	time.Sleep(300 * time.Millisecond)

	if _, err := clients[0].room.Queue(ctx, first); err != nil {
		t.Fatalf("Queue first: %v", err)
	}
	if _, err := clients[0].room.Queue(ctx, secondTrack); err != nil {
		t.Fatalf("Queue second: %v", err)
	}

	// The host's first file ends, which advances the room, and its second file
	// ends, which leaves the room idle.
	waitFor(t, 20*time.Second, "the host to play through both songs", func() bool {
		return len(clients[0].sink.recorded()) >= 2
	})
	waitFor(t, 20*time.Second, "the room to go idle", func() bool {
		room, err := clients[0].room.Snapshot(ctx)
		return err == nil && room.Current == nil
	})
	// The follower played both too.
	if played := clients[1].sink.recorded(); len(played) < 2 || played[1].TrackID != secondTrack {
		t.Fatalf("follower played %d slots (%+v)", len(played), played)
	}
}

// TestAShortFollowersFileDoesNotEndTheSong: a follower whose own copy is
// shorter than the host's goes quiet when its file ends and the song plays on.
func TestAShortFollowersFileDoesNotEndTheSong(t *testing.T) {
	apiClient := newServer(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	apiClient, clients, trackID := roomClients(t, ctx, apiClient, "Long Song.opus", []float64{3.0, 0.5})

	stop := runParticipants(t, ctx, clients)
	defer stop()
	time.Sleep(300 * time.Millisecond)

	if _, err := clients[0].room.Queue(ctx, trackID); err != nil {
		t.Fatalf("Queue: %v", err)
	}
	waitFor(t, 15*time.Second, "both to start", func() bool {
		return len(clients[0].sink.recorded()) > 0 && len(clients[1].sink.recorded()) > 0
	})
	// The follower's short file runs out first.
	waitFor(t, 10*time.Second, "the follower's file to finish", func() bool {
		clients[1].sink.mu.Lock()
		defer clients[1].sink.mu.Unlock()
		return clients[1].sink.finished > 0
	})
	time.Sleep(500 * time.Millisecond)

	room, err := clients[0].room.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if room.Current == nil || room.Current.Item.TrackID != trackID {
		t.Fatalf("the follower's file ending moved the room on: %+v", room.Current)
	}
	if room.Current.Paused {
		t.Fatal("the room paused itself")
	}
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
