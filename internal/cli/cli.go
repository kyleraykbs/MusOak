// Package cli implements the prism command line client.
//
// It is a plain API client: with client.serverURL set it talks to a running
// daemon, and without it the CLI starts its own server in-process, over
// loopback. Either way the same config, the same store and the same commands
// apply, which is what makes a single binary a complete music setup.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"codeberg.org/kyleraykbs/prismusic/internal/api"
	"codeberg.org/kyleraykbs/prismusic/internal/config"
	"codeberg.org/kyleraykbs/prismusic/pkg/client"
)

// App is the CLI's runtime.
type App struct {
	cfg    *config.Config
	logger *slog.Logger
	client *client.Client
	cache  *client.Cache
	state  *sessionState
	out    io.Writer

	embedded *embeddedServer
}

// New builds the CLI, starting an embedded server unless the configuration
// points at a remote one.
func New(cfg *config.Config, logger *slog.Logger, out io.Writer) (*App, error) {
	if out == nil {
		out = os.Stdout
	}
	cache, err := client.OpenCache(cfg.Client.CacheDir)
	if err != nil {
		return nil, err
	}
	state, err := loadState(cfg.Client.CacheDir)
	if err != nil {
		return nil, err
	}

	app := &App{cfg: cfg, logger: logger, cache: cache, state: state, out: out}
	if serverURL := strings.TrimSpace(cfg.Client.ServerURL); serverURL != "" {
		app.client = client.New(serverURL, client.WithToken(state.Token))
		return app, nil
	}

	embedded, err := startEmbedded(cfg, logger)
	if err != nil {
		return nil, err
	}
	app.embedded = embedded
	app.client = client.New("http://"+embedded.addr, client.WithToken(state.Token))
	logger.Debug("prism: running its own server", "address", embedded.addr)
	return app, nil
}

// Close shuts down whatever the CLI started.
func (a *App) Close() error {
	if a.embedded == nil {
		return nil
	}
	return a.embedded.Close()
}

// Client exposes the API client, for tests.
func (a *App) Client() *client.Client { return a.client }

// Run dispatches one command line.
func (a *App) Run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		a.usage()
		return nil
	}
	command, rest := args[0], args[1:]
	switch command {
	case "search":
		return a.cmdSearch(ctx, rest)
	case "play":
		return a.cmdPlay(ctx, rest)
	case "queue":
		return a.cmdQueue(ctx, rest)
	case "library":
		return a.cmdLibrary(ctx, rest)
	case "fav", "favorite", "favorites":
		return a.cmdFavorites(ctx, rest)
	case "providers":
		return a.cmdProviders(ctx, rest)
	case "login":
		return a.cmdLogin(ctx, rest)
	case "logout":
		return a.cmdLogout(ctx, rest)
	case "me":
		return a.cmdMe(ctx, rest)
	case "room":
		return a.cmdRoom(ctx, rest)
	case "serve":
		return a.cmdServe(ctx, rest)
	case "help", "-h", "--help":
		a.usage()
		return nil
	default:
		a.usage()
		return fmt.Errorf("prism: unknown command %q", command)
	}
}

func (a *App) usage() {
	fmt.Fprint(a.out, `prism — music client for prismusicd

Usage:
  prism search <query>                 search every provider, merged per track
  prism play                           play the local queue, gaplessly
  prism queue add <track-id|index>     append to the queue
  prism queue list                     show the queue
  prism queue rm <index>               remove a queue entry
  prism queue clear                    empty the queue
  prism library import <file|dir>      add local files to the library
  prism fav add|list|rm <track-id|index>
  prism providers                      list providers
  prism providers rank <a,b,c>         set your provider preference
  prism login <username> [--register]  log in (password on stdin or --password)
  prism logout
  prism me                             show who you are on this server
  prism room create [--name N]         open a room (you host it)
  prism room list
  prism room join <room-id>            follow a room and play it
  prism room queue add <track-id|index>
  prism room vote <1-5> | skip | pause | resume | seek <ms> | now
  prism serve                          run the server in the foreground

The queue, the media cache and the login token live in client.cacheDir.
With client.serverURL empty, prism runs its own server in-process.
`)
}

// --- embedded server -------------------------------------------------------

// embeddedServer is a full prismusicd bound to loopback inside this process.
type embeddedServer struct {
	addr   string
	http   *http.Server
	server *api.Server
}

func startEmbedded(cfg *config.Config, logger *slog.Logger) (*embeddedServer, error) {
	server, err := api.New(cfg, logger)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = server.Close()
		return nil, fmt.Errorf("prism: bind embedded server: %w", err)
	}
	httpServer := &http.Server{Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("prism: embedded server stopped", "error", err)
		}
	}()
	return &embeddedServer{addr: listener.Addr().String(), http: httpServer, server: server}, nil
}

func (e *embeddedServer) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = e.http.Shutdown(ctx)
	return e.server.Close()
}

// --- local state -----------------------------------------------------------

// queueItem is one entry of the local queue.
type queueItem struct {
	TrackID string `json:"trackId"`
	Title   string `json:"title"`
}

// sessionState is what the CLI remembers between runs.
type sessionState struct {
	Token      string      `json:"token,omitempty"`
	Queue      []queueItem `json:"queue,omitempty"`
	LastSearch []queueItem `json:"lastSearch,omitempty"`
	// RoomID is the room this client last joined.
	RoomID string `json:"roomId,omitempty"`

	path string
}

// saveRoom records the room to rejoin.
func (s *sessionState) saveRoom(roomID string) error {
	s.RoomID = roomID
	return s.save()
}

func loadState(dir string) (*sessionState, error) {
	state := &sessionState{path: filepath.Join(dir, "state.json")}
	raw, err := os.ReadFile(state.path)
	if err != nil {
		if os.IsNotExist(err) {
			return state, nil
		}
		return nil, fmt.Errorf("prism: read state: %w", err)
	}
	if err := json.Unmarshal(raw, state); err != nil {
		return nil, fmt.Errorf("prism: parse %s: %w", state.path, err)
	}
	state.path = filepath.Join(dir, "state.json")
	return state, nil
}

func (s *sessionState) save() error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".part"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("prism: write state: %w", err)
	}
	return os.Rename(tmp, s.path)
}

// resolveTrack turns a track id, or an index into the last search, into a
// track.
func (a *App) resolveTrack(ctx context.Context, argument string) (queueItem, error) {
	if argument == "" {
		return queueItem{}, errors.New("prism: a track id or search index is required")
	}
	if index, err := parseIndex(argument); err == nil {
		if index < 0 || index >= len(a.state.LastSearch) {
			return queueItem{}, fmt.Errorf("prism: no search result %d; run \"prism search\" first", index)
		}
		return a.state.LastSearch[index], nil
	}
	track, err := a.client.Track(ctx, argument)
	if err != nil {
		return queueItem{}, err
	}
	return queueItem{TrackID: track.ID, Title: track.Title}, nil
}

// --- output helpers --------------------------------------------------------

func (a *App) printf(format string, args ...any) {
	fmt.Fprintf(a.out, format, args...)
}

func formatDuration(ms int64) string {
	if ms <= 0 {
		return "--:--"
	}
	total := ms / 1000
	return fmt.Sprintf("%d:%02d", total/60, total%60)
}

func joinArtists(artists []string) string {
	if len(artists) == 0 {
		return "unknown artist"
	}
	return strings.Join(artists, ", ")
}
