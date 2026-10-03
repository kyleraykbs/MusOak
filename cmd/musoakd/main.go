// Command musoakd runs the MusOak server.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"codeberg.org/kyleraykbs/musoak/internal/api"
	"codeberg.org/kyleraykbs/musoak/internal/config"
)

func main() {
	configPath := flag.String("config", "", "path to config file (default: $MUSOAK_CONFIG, else $XDG_CONFIG_HOME/musoak/config.json)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg, err := config.Load(config.ResolvePath(*configPath))
	if err != nil {
		logger.Error("invalid config", "error", err)
		os.Exit(1)
	}

	server, err := api.New(cfg, logger)
	if err != nil {
		logger.Error("startup failed", "error", err)
		os.Exit(1)
	}
	defer func() {
		if err := server.Close(); err != nil {
			logger.Error("close failed", "error", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := server.Run(ctx); err != nil {
		logger.Error("server failed", "error", err)
		os.Exit(1)
	}
}
