// Package api owns the HTTP surface of the server: REST endpoints, the media
// file server and the WebSocket event stream.
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"codeberg.org/kyleraykbs/prismusic/internal/config"
	"codeberg.org/kyleraykbs/prismusic/internal/provider"
	"codeberg.org/kyleraykbs/prismusic/internal/provider/ytmusic"
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
	providers *provider.Registry
}

// New builds the server and the enabled providers.
func New(cfg *config.Config, logger *slog.Logger) (*Server, error) {
	s := &Server{
		cfg:       cfg,
		logger:    logger,
		mux:       http.NewServeMux(),
		providers: provider.NewRegistry(logger, ProviderTimeout),
	}
	s.registerProviders()
	s.logProviderDeps()
	s.routes()
	return s, nil
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
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
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
