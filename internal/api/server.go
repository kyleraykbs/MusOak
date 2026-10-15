// Package api owns the HTTP surface of the server: REST endpoints, the media
// file server and the WebSocket event stream.
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kyleraykbs/MusOak/internal/artwork"
	"github.com/kyleraykbs/MusOak/internal/auth"
	"github.com/kyleraykbs/MusOak/internal/config"
	"github.com/kyleraykbs/MusOak/internal/library"
	"github.com/kyleraykbs/MusOak/internal/lyrics"
	"github.com/kyleraykbs/MusOak/internal/match"
	"github.com/kyleraykbs/MusOak/internal/media"
	"github.com/kyleraykbs/MusOak/internal/prefetch"
	"github.com/kyleraykbs/MusOak/internal/provider"
	"github.com/kyleraykbs/MusOak/internal/provider/spotify"
	"github.com/kyleraykbs/MusOak/internal/provider/youtube"
	"github.com/kyleraykbs/MusOak/internal/provider/ytmusic"
	"github.com/kyleraykbs/MusOak/internal/radio"
	"github.com/kyleraykbs/MusOak/internal/ranking"
	"github.com/kyleraykbs/MusOak/internal/rooms"
	"github.com/kyleraykbs/MusOak/internal/store"
)

// ShutdownTimeout bounds graceful shutdown after the context is cancelled.
const ShutdownTimeout = 10 * time.Second

// ProviderTimeout bounds each provider's share of a fan-out search.
const ProviderTimeout = 15 * time.Second

// Server is the HTTP server. Everything the daemon does is reachable through
// it; the CLI and any bot are plain API clients.
type Server struct {
	cfg       *config.Config
	logger    *slog.Logger
	mux       *http.ServeMux
	store     *store.DB
	providers *provider.Registry
	media     *media.Manager
	matcher   *match.Matcher
	auth      *auth.Service
	ranking   *ranking.Service
	rooms     *rooms.Manager
	radio     *radio.Service
	library   *library.Service
	artwork   *artwork.Fetcher
	lyrics    *lyrics.Service

	searchLimiter *limiter
	loginLimiter  *limiter

	// searches holds the last few answers to the same question and shares the
	// one on its way, so typing, tab-switching and two people asking at once
	// cost one provider call between them.
	searches *searchAnswers

	// imports and archives are the long jobs a client watches: playlists being
	// imported, and playlists being written out as zips.
	imports  *jobRegistry[playlistImport]
	archives *jobRegistry[playlistArchive]

	// prefetcher keeps the playlists downloaded; nil when it is turned off.
	prefetcher *prefetch.Prefetcher
}

// New builds the server: it opens the store and instantiates the enabled
// providers and the media pipeline.
func New(cfg *config.Config, logger *slog.Logger) (*Server, error) {
	trustedProxies, err := cfg.TrustedProxyNets()
	if err != nil {
		return nil, err
	}

	db, err := store.Open(filepath.Join(cfg.StorageDir, "musoak.db"))
	if err != nil {
		return nil, err
	}

	s := &Server{
		cfg:       cfg,
		logger:    logger,
		mux:       http.NewServeMux(),
		store:     db,
		providers: provider.NewRegistry(logger, ProviderTimeout),
		searches:  newSearchAnswers(SearchAnswerTTL, 256),
	}
	quotaBytes := int64(cfg.Media.QuotaMB) * 1024 * 1024
	s.media = media.New(filepath.Join(cfg.StorageDir, "media"), quotaBytes, db, s.providers, logger)
	s.matcher = match.New(db, s.providers, cfg.Match.Threshold, logger)
	s.auth = auth.New(cfg, db, logger)
	s.ranking = ranking.New(db, cfg.DefaultProviderOrder, logger)
	s.rooms = rooms.NewManager(cfg, db, logger)
	// The server is the room's clock: the manager advances and republishes
	// every room once a second until the server closes.
	s.rooms.Start()
	s.radio = radio.New(db, s.providers, s.matcher, logger)
	s.library = library.New(db, s.providers, s.matcher, logger)
	s.artwork = artwork.New(filepath.Join(cfg.StorageDir, "artwork"), db, logger)
	s.lyrics = lyrics.New(db, lyrics.NewLRCLIB(), logger)

	// One search burst may be as wide as a handful of keystrokes; login bursts
	// stay tight because each attempt costs an argon2id hash.
	s.searchLimiter = newLimiter(cfg.RateLimit.SearchPerMinute, 10, trustedProxies...)
	s.loginLimiter = newLimiter(cfg.RateLimit.LoginPerMinute, 3, trustedProxies...)

	// Jobs are remembered for a while after they end, so a client that polls
	// every second still sees how one turned out.
	s.imports = newJobRegistry(importTTL, func(job *playlistImport) time.Time { return job.ended })
	s.archives = newJobRegistry(archiveTTL, func(archive *playlistArchive) time.Time { return archive.ended })

	s.startPrefetch()

	s.registerProviders()
	// The ranking order must cover every enabled provider, even one the
	// configured default order predates.
	s.ranking.SetEnabledProviders(s.providers.Names())
	s.logProviderDeps()
	s.routes()
	return s, nil
}

// Close releases the server's resources.
func (s *Server) Close() error {
	if s.rooms != nil {
		s.rooms.Close()
	}
	if s.store == nil {
		return nil
	}
	return s.store.Close()
}

// registerProviders instantiates every provider the configuration enables.
func (s *Server) registerProviders() {
	if s.cfg.Providers.YTMusic.Enabled {
		s.providers.Register(ytmusic.New(s.logger, ytmusic.Options{
			CookiesFromBrowser: s.cfg.Providers.YTMusic.CookiesFromBrowser,
			CookiesFile:        s.cfg.Providers.YTMusic.CookiesFile,
		}))
	}
	if s.cfg.Providers.YouTube.Enabled {
		s.providers.Register(youtube.New(s.logger, youtube.Options{
			CookiesFromBrowser: s.cfg.Providers.YouTube.CookiesFromBrowser,
			CookiesFile:        s.cfg.Providers.YouTube.CookiesFile,
		}))
	}
	if s.cfg.Providers.Spotify.Enabled {
		if s.cfg.Providers.Spotify.ClientID == "" || s.cfg.Providers.Spotify.ClientSecret == "" {
			s.logger.Warn("spotify is enabled without credentials; it will fail on use",
				"hint", "set providers.spotify.clientId and providers.spotify.clientSecret")
		}
		s.providers.Register(spotify.New(
			s.cfg.Providers.Spotify.ClientID,
			s.cfg.Providers.Spotify.ClientSecret,
			s.logger,
		))
	}
	// A Spotify playlist is importable whether or not Spotify is one of this
	// server's providers: its public playlists are read from the embed page,
	// which needs no account, and those names are all an import needs to find
	// the same songs somewhere playable. Credentials, when the config has them,
	// are used - the API answers with more than the embed does.
	if !s.cfg.Providers.Spotify.Enabled {
		s.providers.RegisterBrowsing("spotify", func() (provider.Provider, error) {
			return spotify.New(s.cfg.Providers.Spotify.ClientID, s.cfg.Providers.Spotify.ClientSecret, s.logger), nil
		})
	}
}

// logProviderDeps reports missing external tooling at startup instead of on
// the first search.
func (s *Server) logProviderDeps() {
	for name, missing := range s.providers.MissingDeps() {
		sort.Strings(missing)
		for _, bin := range missing {
			s.logger.Warn("provider dependency missing",
				"provider", name, "binary", bin, "hint", depHint(bin))
		}
	}
}

func depHint(bin string) string {
	switch {
	case strings.Contains(bin, "python3"):
		return "install python3 with the ytmusicapi package"
	case strings.Contains(bin, "yt-dlp"):
		return "install yt-dlp; it breaks periodically, so keep it easy to update"
	case strings.Contains(bin, "ffmpeg") || strings.Contains(bin, "ffprobe"):
		return "install ffmpeg (it provides both ffmpeg and ffprobe)"
	}
	return "install it and restart"
}

// Handler exposes the fully wrapped HTTP handler (rate limits, auth, routes).
func (s *Server) Handler() http.Handler { return s.handler() }

// handler composes the middleware stack exactly once per call site: the tests,
// the CLI's embedded server and Run all serve this, so none of them can
// accidentally bypass authentication or the rate limits.
func (s *Server) handler() http.Handler {
	return s.withRateLimits(s.withAuth(s.mux))
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// Run serves on the configured listen address until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return err
	}
	// The playlists are kept downloaded for as long as the server runs. It is
	// started here rather than in New so that a test's server stays quiet.
	s.StartPrefetch(ctx)
	return s.Serve(ctx, listener)
}

// Serve serves HTTP on listener until ctx is cancelled, then shuts down
// gracefully. It is the single place the server serves from, so every caller
// gets the same middleware stack.
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	srv := &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// A keep-alive connection that falls silent is closed rather than held
		// open in case the client sends another request.
		IdleTimeout: 2 * time.Minute,
		// Reading a request is the one fetch that can otherwise stall a handler
		// for ever: a client that opens a request and dribbles its body holds
		// the goroutine reading it. The bound is generous enough for the
		// largest upload - the body is JSON and runs to ninety-six megabytes -
		// and still finite. WriteTimeout is deliberately unset: an audio stream
		// is one long write, and a deadline on it would cut the song short.
		ReadTimeout: 5 * time.Minute,
	}

	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("listening", "addr", listener.Addr().String(), "storage_dir", s.cfg.StorageDir)
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	s.logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	s.logger.Info("stopped")
	return nil
}
