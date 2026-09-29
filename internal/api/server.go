// Package api owns the HTTP surface of the server: REST endpoints, the media
// file server and the WebSocket event stream.
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"codeberg.org/kyleraykbs/prismusic/internal/auth"
	"codeberg.org/kyleraykbs/prismusic/internal/config"
	"codeberg.org/kyleraykbs/prismusic/internal/match"
	"codeberg.org/kyleraykbs/prismusic/internal/media"
	"codeberg.org/kyleraykbs/prismusic/internal/provider"
	"codeberg.org/kyleraykbs/prismusic/internal/provider/ytmusic"
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
	s.media = media.New(filepath.Join(cfg.StorageDir, "media"), db, s.providers, logger)
	s.matcher = match.New(db, s.providers, cfg.Match.Threshold, logger)
	s.auth = auth.New(cfg, db, logger)
	s.ranking = ranking.New(db, cfg.DefaultProviderOrder, logger)
	s.rooms = rooms.NewManager(cfg, db, s.matcher, s.ranking, logger)

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

// Handler exposes the HTTP handler for tests.
func (s *Server) Handler() http.Handler { return s.withAuth(s.mux) }

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /api/v1/providers", s.handleProviders)
	s.mux.HandleFunc("GET /api/v1/search", s.handleSearch)
	s.mux.HandleFunc("GET /api/v1/tracks/{trackId}", s.handleTrack)
	s.mux.HandleFunc("GET /api/v1/tracks/{trackId}/variants", s.handleTrackVariants)
	s.mux.HandleFunc("POST /api/v1/tracks/{trackId}/resolve", s.handleTrackResolve)
	s.mux.HandleFunc("GET /api/v1/media/{variantId}", s.handleMediaFile)
	s.mux.HandleFunc("GET /api/v1/media/{variantId}/status", s.handleMediaStatus)
	s.mux.HandleFunc("POST /api/v1/media/{variantId}/download", s.handleMediaDownload)
	s.mux.HandleFunc("POST /api/v1/library/import", s.handleLibraryImport)
	s.mux.HandleFunc("POST /api/v1/auth/register", s.handleRegister)
	s.mux.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	s.mux.HandleFunc("POST /api/v1/auth/logout", s.handleLogout)
	s.mux.HandleFunc("GET /api/v1/me", s.handleMe)
	s.mux.HandleFunc("GET /api/v1/me/favorites", s.handleFavoritesList)
	s.mux.HandleFunc("POST /api/v1/me/favorites", s.handleFavoriteAdd)
	s.mux.HandleFunc("DELETE /api/v1/me/favorites/{trackId}", s.handleFavoriteRemove)
	s.mux.HandleFunc("GET /api/v1/me/providers/ranking", s.handleRankingGet)
	s.mux.HandleFunc("PUT /api/v1/me/providers/ranking", s.handleRankingPut)
	s.mux.HandleFunc("GET /api/v1/clock", s.handleClock)
	s.mux.HandleFunc("GET /api/v1/ws", s.handleWS)
	s.mux.HandleFunc("GET /api/v1/rooms", s.handleRoomList)
	s.mux.HandleFunc("POST /api/v1/rooms", s.handleRoomCreate)
	s.mux.HandleFunc("GET /api/v1/rooms/{roomId}", s.handleRoomGet)
	s.mux.HandleFunc("POST /api/v1/rooms/{roomId}/join", s.handleRoomJoin)
	s.mux.HandleFunc("POST /api/v1/rooms/{roomId}/leave", s.handleRoomLeave)
	s.mux.HandleFunc("POST /api/v1/rooms/{roomId}/queue", s.handleRoomEnqueue)
	s.mux.HandleFunc("DELETE /api/v1/rooms/{roomId}/queue/{itemId}", s.handleRoomRemove)
	s.mux.HandleFunc("POST /api/v1/rooms/{roomId}/queue/reorder", s.handleRoomReorder)
	s.mux.HandleFunc("POST /api/v1/rooms/{roomId}/pause", s.handleRoomPause)
	s.mux.HandleFunc("POST /api/v1/rooms/{roomId}/resume", s.handleRoomResume)
	s.mux.HandleFunc("POST /api/v1/rooms/{roomId}/skip", s.handleRoomSkip)
	s.mux.HandleFunc("POST /api/v1/rooms/{roomId}/seek", s.handleRoomSeek)
	s.mux.HandleFunc("POST /api/v1/rooms/{roomId}/vote", s.handleRoomVote)
	s.mux.HandleFunc("POST /api/v1/rooms/{roomId}/ready", s.handleRoomReady)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// Run serves HTTP until ctx is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.Listen,
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("listening", "addr", srv.Addr, "storage_dir", s.cfg.StorageDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
