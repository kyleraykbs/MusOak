package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"codeberg.org/kyleraykbs/prismusic/pkg/client"
)

// mpvSocketTimeout bounds how long we wait for mpv to create its IPC socket.
const mpvSocketTimeout = 10 * time.Second

// mpvMessage is both an IPC reply and an event; mpv sends either on the socket.
type mpvMessage struct {
	RequestID       *int            `json:"request_id,omitempty"`
	Error           string          `json:"error,omitempty"`
	Data            json.RawMessage `json:"data,omitempty"`
	Event           string          `json:"event,omitempty"`
	Reason          string          `json:"reason,omitempty"`
	PlaylistEntryID int             `json:"playlist_entry_id,omitempty"`
}

// mpvProcess drives one mpv instance over its JSON IPC socket.
type mpvProcess struct {
	socket string
	logger *slog.Logger

	cmd  *exec.Cmd
	conn net.Conn

	mu      sync.Mutex
	nextID  int
	pending map[int]chan mpvMessage

	events chan mpvMessage
	closed chan struct{}
	once   sync.Once
}

// extraMpvArgs reads PRISM_MPV_ARGS, which headless setups use to pass
// "--ao=null --no-video" and similar.
func extraMpvArgs() []string {
	raw := strings.TrimSpace(os.Getenv("PRISM_MPV_ARGS"))
	if raw == "" {
		return nil
	}
	return strings.Fields(raw)
}

// startMpv launches mpv idle with an IPC socket and connects to it.
func startMpv(ctx context.Context, socketPath string, logger *slog.Logger) (*mpvProcess, error) {
	if logger == nil {
		logger = slog.Default()
	}
	_ = os.Remove(socketPath)

	args := []string{
		"--idle=yes",
		"--no-terminal",
		"--input-ipc-server=" + socketPath,
	}
	args = append(args, extraMpvArgs()...)

	cmd := exec.Command("mpv", args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("prism: start mpv: %w (is mpv installed?)", err)
	}

	process := &mpvProcess{
		socket:  socketPath,
		logger:  logger,
		cmd:     cmd,
		pending: make(map[int]chan mpvMessage),
		events:  make(chan mpvMessage, 64),
		closed:  make(chan struct{}),
	}

	if err := waitForSocket(ctx, socketPath); err != nil {
		_ = cmd.Process.Kill()
		return nil, err
	}
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("prism: connect to mpv: %w", err)
	}
	process.conn = conn

	go process.readLoop()
	go func() {
		_ = cmd.Wait()
		process.shutdown()
	}()
	return process, nil
}

func waitForSocket(ctx context.Context, path string) error {
	deadline := time.Now().Add(mpvSocketTimeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	return fmt.Errorf("prism: mpv did not create its IPC socket at %s", path)
}

func (m *mpvProcess) readLoop() {
	scanner := bufio.NewScanner(m.conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var message mpvMessage
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			m.logger.Debug("mpv: unparsable message", "error", err, "line", scanner.Text())
			continue
		}
		if message.RequestID != nil {
			m.mu.Lock()
			waiter, ok := m.pending[*message.RequestID]
			delete(m.pending, *message.RequestID)
			m.mu.Unlock()
			if ok {
				waiter <- message
			}
			continue
		}
		if message.Event == "" {
			continue
		}
		select {
		case m.events <- message:
		default:
			m.logger.Debug("mpv: dropping event", "event", message.Event)
		}
	}
	m.shutdown()
}

func (m *mpvProcess) shutdown() {
	m.once.Do(func() {
		close(m.closed)
		if m.conn != nil {
			_ = m.conn.Close()
		}
	})
}

// command sends an IPC command and waits for its reply.
func (m *mpvProcess) command(ctx context.Context, args ...any) (json.RawMessage, error) {
	m.mu.Lock()
	id := m.nextID
	m.nextID++
	waiter := make(chan mpvMessage, 1)
	m.pending[id] = waiter
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		delete(m.pending, id)
		m.mu.Unlock()
	}()

	payload, err := json.Marshal(map[string]any{"command": args, "request_id": id})
	if err != nil {
		return nil, err
	}
	if _, err := m.conn.Write(append(payload, '\n')); err != nil {
		return nil, fmt.Errorf("prism: mpv write: %w", err)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.closed:
		return nil, errors.New("prism: mpv exited")
	case message := <-waiter:
		if message.Error != "" && message.Error != "success" {
			return nil, fmt.Errorf("prism: mpv: %s", message.Error)
		}
		return message.Data, nil
	}
}

// property reads a property as JSON.
func (m *mpvProcess) property(ctx context.Context, name string) (json.RawMessage, error) {
	return m.command(ctx, "get_property", name)
}

// positionMs reports mpv's playback position in milliseconds.
func (m *mpvProcess) positionMs(ctx context.Context) (int64, error) {
	raw, err := m.property(ctx, "time-pos")
	if err != nil {
		return 0, err
	}
	var seconds *float64
	if err := json.Unmarshal(raw, &seconds); err != nil || seconds == nil {
		return 0, nil // no file loaded
	}
	return int64(*seconds * 1000), nil
}

// loadReplace starts playing path, optionally from a position.
func (m *mpvProcess) loadReplace(ctx context.Context, path string, positionMs int64) error {
	args := []any{"loadfile", path, "replace"}
	if positionMs > 0 {
		args = append(args, "-1", fmt.Sprintf("start=+%.3f", float64(positionMs)/1000))
	}
	_, err := m.command(ctx, args...)
	return err
}

// loadAppend queues path after whatever is playing; playback continues without
// a gap when the current file ends.
func (m *mpvProcess) loadAppend(ctx context.Context, path string) error {
	_, err := m.command(ctx, "loadfile", path, "append-play")
	return err
}

// playlistCount is how many entries mpv has queued.
func (m *mpvProcess) playlistCount(ctx context.Context) (int, error) {
	raw, err := m.property(ctx, "playlist-count")
	if err != nil {
		return 0, err
	}
	var count int
	if err := json.Unmarshal(raw, &count); err != nil {
		return 0, err
	}
	return count, nil
}

// idle reports whether mpv has nothing left to play.
func (m *mpvProcess) idle(ctx context.Context) (bool, error) {
	raw, err := m.property(ctx, "idle-active")
	if err != nil {
		return false, err
	}
	var idle bool
	if err := json.Unmarshal(raw, &idle); err != nil {
		return false, err
	}
	return idle, nil
}

func (m *mpvProcess) setPause(ctx context.Context, paused bool) error {
	_, err := m.command(ctx, "set_property", "pause", paused)
	return err
}

func (m *mpvProcess) seek(ctx context.Context, positionMs int64) error {
	_, err := m.command(ctx, "seek", float64(positionMs)/1000, "absolute+exact")
	return err
}

func (m *mpvProcess) stop(ctx context.Context) error {
	_, err := m.command(ctx, "stop")
	return err
}

// waitForEnd blocks until the current file ends.
func (m *mpvProcess) waitForEnd(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-m.closed:
			return errors.New("prism: mpv exited")
		case event := <-m.events:
			if event.Event == "end-file" {
				return nil
			}
		}
	}
}

func (m *mpvProcess) close() {
	if m.cmd == nil || m.cmd.Process == nil {
		return
	}
	_, _ = m.command(context.Background(), "quit")
	done := make(chan struct{})
	go func() {
		_ = m.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = m.cmd.Process.Kill()
	}
	m.shutdown()
	_ = os.Remove(m.socket)
}

// MpvSink plays renditions through mpv. It is the CLI's client.Sink: a Discord
// bot would implement the same interface differently.
type MpvSink struct {
	process *mpvProcess
	logger  *slog.Logger
}

// NewMpvSink starts an mpv instance and returns a sink driving it.
func NewMpvSink(ctx context.Context, socketPath string, logger *slog.Logger) (*MpvSink, error) {
	process, err := startMpv(ctx, socketPath, logger)
	if err != nil {
		return nil, err
	}
	return &MpvSink{process: process, logger: logger}, nil
}

var _ client.Sink = (*MpvSink)(nil)
var _ client.Pauser = (*MpvSink)(nil)
var _ client.Seeker = (*MpvSink)(nil)
var _ client.Positioner = (*MpvSink)(nil)

// Play starts one slot and returns when this client's file has ended.
func (s *MpvSink) Play(ctx context.Context, track client.SinkTrack) error {
	if err := s.process.loadReplace(ctx, track.Path, track.PositionMs); err != nil {
		return err
	}
	return s.process.waitForEnd(ctx)
}

// Stop abandons playback.
func (s *MpvSink) Stop(ctx context.Context) error {
	return s.process.stop(ctx)
}

// Pause freezes playback in place.
func (s *MpvSink) Pause(ctx context.Context) error {
	return s.process.setPause(ctx, true)
}

// Resume continues playback.
func (s *MpvSink) Resume(ctx context.Context) error {
	return s.process.setPause(ctx, false)
}

// Seek jumps to a position.
func (s *MpvSink) Seek(ctx context.Context, positionMs int64) error {
	return s.process.seek(ctx, positionMs)
}

// PositionMs reports where mpv actually is, which is how the participant
// measures its drift.
func (s *MpvSink) PositionMs(ctx context.Context) (int64, error) {
	return s.process.positionMs(ctx)
}

// Close shuts mpv down.
func (s *MpvSink) Close() {
	s.process.close()
}

// --- transport controls (interactive playback) ------------------------------

// volume reports mpv's volume as a percentage.
func (m *mpvProcess) volume(ctx context.Context) (int, error) {
	raw, err := m.property(ctx, "volume")
	if err != nil {
		return 0, err
	}
	var level float64
	if err := json.Unmarshal(raw, &level); err != nil {
		return 0, nil
	}
	return int(level + 0.5), nil
}

func (m *mpvProcess) setVolume(ctx context.Context, volume int) error {
	_, err := m.command(ctx, "set_property", "volume", volume)
	return err
}

func (m *mpvProcess) pauseState(ctx context.Context) (bool, error) {
	raw, err := m.property(ctx, "pause")
	if err != nil {
		return false, err
	}
	var paused bool
	if err := json.Unmarshal(raw, &paused); err != nil {
		return false, nil
	}
	return paused, nil
}

// durationMs reports the length of the loaded file.
func (m *mpvProcess) durationMs(ctx context.Context) (int64, error) {
	raw, err := m.property(ctx, "duration")
	if err != nil {
		return 0, err
	}
	var seconds *float64
	if err := json.Unmarshal(raw, &seconds); err != nil || seconds == nil {
		return 0, nil
	}
	return int64(*seconds * 1000), nil
}

// playlistPosition is the index of the playing entry, or -1 when idle.
func (m *mpvProcess) playlistPosition(ctx context.Context) (int, error) {
	raw, err := m.property(ctx, "playlist-pos")
	if err != nil {
		return -1, err
	}
	var position *int
	if err := json.Unmarshal(raw, &position); err != nil || position == nil {
		return -1, nil
	}
	return *position, nil
}

func (m *mpvProcess) nextEntry(ctx context.Context) error {
	_, err := m.command(ctx, "playlist-next")
	return err
}

func (m *mpvProcess) previousEntry(ctx context.Context) error {
	_, err := m.command(ctx, "playlist-prev")
	return err
}

// --- queue playback (prism play) -------------------------------------------

// StartFile replaces the playlist with path and starts playing.
func (s *MpvSink) StartFile(ctx context.Context, path string, positionMs int64) error {
	return s.process.loadReplace(ctx, path, positionMs)
}

// AppendFile queues path after whatever is playing, so the next track starts
// the moment the current one ends.
func (s *MpvSink) AppendFile(ctx context.Context, path string) error {
	return s.process.loadAppend(ctx, path)
}

// PlaylistCount reports how many entries mpv has queued.
func (s *MpvSink) PlaylistCount(ctx context.Context) (int, error) {
	return s.process.playlistCount(ctx)
}

// WaitIdle blocks until mpv has played everything it was given.
func (s *MpvSink) WaitIdle(ctx context.Context) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		idle, err := s.process.idle(ctx)
		if err != nil {
			return err
		}
		if idle {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
