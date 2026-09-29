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

	"codeberg.org/kyleraykbs/prismusic/internal/auth"
	"codeberg.org/kyleraykbs/prismusic/internal/config"
	"codeberg.org/kyleraykbs/prismusic/internal/library"
	"codeberg.org/kyleraykbs/prismusic/internal/match"
	"codeberg.org/kyleraykbs/prismusic/internal/media"
	"codeberg.org/kyleraykbs/prismusic/internal/provider"
	"codeberg.org/kyleraykbs/prismusic/internal/provider/spotify"
	"codeberg.org/kyleraykbs/prismusic/internal/provider/ytmusic"
	"codeberg.org/kyleraykbs/prismusic/internal/radio"
	"codeberg.org/kyleraykbs/prismusic/internal/ranking"
	"codeberg.org/kyleraykbs/prismusic/internal/rooms"
	"codeberg.org/kyleraykbs/prismusic/internal/store"
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

	searchLimiter *limiter
	loginLimiter  *limiter
}

// New builds the server: it opens the store and instantiates the enabled
// providers and the media pipeline.
func New(cfg *config.Config, logger *slog.Logger) (*Server, error) {
	db, err := store.Open(filepath.Join(cfg.StorageDir, "prismusic.db"))
	if err != nil {
		return nil, err
	}

	s := &Server{
		cfg:       cfg,
		logger:    logger,
		mux:       http.NewServeMux(),
		store:     db,
		providers: provider.NewRegistry(logger, ProviderTimeout),
	}
	quotaBytes := int64(cfg.Media.QuotaMB) * 1024 * 1024
	s.media = media.New(filepath.Join(cfg.StorageDir, "media"), quotaBytes, db, s.providers, logger)
	s.matcher = match.New(db, s.providers, cfg.Match.Threshold, logger)
	s.auth = auth.New(cfg, db, logger)
	s.ranking = ranking.New(db, cfg.DefaultProviderOrder, logger)
	s.rooms = rooms.NewManager(cfg, db, s.matcher, s.ranking, logger)
	s.radio = radio.New(db, s.providers, s.matcher, logger)
	s.library = library.New(db, s.providers, s.matcher, logger)

	// One search burst may be as wide as a handful of keystrokes; login bursts
	// stay tight because each attempt costs an argon2id hash.
	s.searchLimiter = newLimiter(cfg.RateLimit.SearchPerMinute, 10)
	s.loginLimiter = newLimiter(cfg.RateLimit.LoginPerMinute, 3)

	s.registerProviders()
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
		s.providers.Register(ytmusic.New(s.logger))
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
	return s.Serve(ctx, listener)
}

// Serve serves HTTP on listener until ctx is cancelled, then shuts down
// gracefully. It is the single place the server serves from, so every caller
// gets the same middleware stack.
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	srv := &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
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
