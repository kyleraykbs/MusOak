// Command musoak is the MusOak client: search, queue, play and listen
// together. With client.serverURL empty it runs its own server in-process.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"codeberg.org/kyleraykbs/musoak/internal/cli"
	"codeberg.org/kyleraykbs/musoak/internal/config"
)

func main() {
	configPath := flag.String("config", "", "path to config file (default: $MUSOAK_CONFIG, else $XDG_CONFIG_HOME/musoak/config.json)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: musoak [--config FILE] <command> [arguments]\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg, err := config.Load(config.ResolvePath(*configPath))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	app, err := cli.New(cfg, logger, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() {
		if err := app.Close(); err != nil {
			logger.Error("musoak: shutdown", "error", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := app.Run(ctx, flag.Args()); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
