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
	trimMedia := flag.Bool("trim-media", false, "cut the trailing silence off every stored rendition, then exit")
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

	// A library fetched before the transcode cut trailing silence still has it,
	// and a room plays to the end of the file. This is the one-off pass that
	// brings the files that are already here up to what a download gets now.
	if *trimMedia {
		trimmed, total, err := server.TrimMedia(ctx, func(done, total int) {
			if done%50 == 0 || done == total {
				logger.Info("trimming media", "done", done, "of", total)
			}
		})
		if err != nil {
			logger.Error("trim failed", "error", err, "trimmed", trimmed, "of", total)
			os.Exit(1)
		}
		logger.Info("media trimmed", "trimmed", trimmed, "of", total)
		return
	}

	if err := server.Run(ctx); err != nil {
		logger.Error("server failed", "error", err)
		os.Exit(1)
	}
}
