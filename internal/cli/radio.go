package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"codeberg.org/kyleraykbs/prismusic/pkg/client"
)

// cmdRadio starts a station from a song and optionally plays it.
func (a *App) cmdRadio(ctx context.Context, args []string) error {
	var (
		providerList string
		length       int
		save         bool
		play         bool
		room         bool
		seed         string
	)
	for i := 0; i < len(args); i++ {
		switch argument := args[i]; {
		case argument == "--providers" || argument == "-providers":
			if i+1 >= len(args) {
				return errors.New("prism: --providers needs a comma separated list")
			}
			providerList = args[i+1]
			i++
		case strings.HasPrefix(argument, "--providers="):
			providerList = strings.TrimPrefix(argument, "--providers=")
		case argument == "--length":
			if i+1 >= len(args) {
				return errors.New("prism: --length needs a number")
			}
			if _, err := fmt.Sscanf(args[i+1], "%d", &length); err != nil {
				return fmt.Errorf("prism: %q is not a length", args[i+1])
			}
			i++
		case argument == "--play":
			play = true
		case argument == "--room":
			room = true
		case argument == "--save":
			save = true
		case argument == "--no-save":
			save = false
		case strings.HasPrefix(argument, "-"):
			return fmt.Errorf("prism: unknown radio option %q", argument)
		case seed == "":
			seed = argument
		default:
			return fmt.Errorf("prism: unexpected radio argument %q", argument)
		}
	}
	if seed == "" {
		return errors.New("prism: usage: prism radio <track-id|index> [--providers a,b] [--length N] [--play|--room] [--no-save]")
	}

	item, err := a.resolveTrack(ctx, seed)
	if err != nil {
		return err
	}

	opts := client.RadioOptions{Length: length}
	if providerList != "" {
		for _, name := range strings.Split(providerList, ",") {
			if name = strings.TrimSpace(name); name != "" {
				opts.Providers = append(opts.Providers, name)
			}
		}
	}
	// Guests cannot save, so a station they ask for is not persisted.
	me, err := a.client.Me(ctx)
	if err != nil {
		return err
	}
	authenticated := me.Authenticated
	opts.Save = client.Bool(save && authenticated)

	result, err := a.client.Radio(ctx, item.TrackID, opts)
	if err != nil {
		return err
	}

	a.printf("radio for %s, from %s: %d track(s)\n", result.Seed.Title,
		strings.Join(orDefault(result.Providers, "no provider"), ", "), len(result.Tracks))
	for i, track := range result.Tracks {
		a.printf("%3d. %-44s %8s  %s\n", i, truncate(track.Title, 44),
			formatDuration(track.DurationMs), joinArtists(track.Artists))
	}
	for _, problem := range result.ProviderErrors {
		a.printf("  ! %s: %s\n", problem.Provider, problem.Error)
	}
	if result.Playlist != nil {
		a.printf("saved as playlist %q (%s)\n", result.Playlist.Name, result.Playlist.ID)
	}

	switch {
	case room:
		room, err := a.joinedRoom(ctx, true)
		if err != nil {
			return err
		}
		for _, track := range result.Tracks {
			if _, err := room.Queue(ctx, track.ID); err != nil {
				return err
			}
		}
		a.printf("queued %d track(s) into %s\n", len(result.Tracks), room.RoomID)
		return nil
	case play:
		a.state.Queue = a.state.Queue[:0]
		for _, track := range result.Tracks {
			a.state.Queue = append(a.state.Queue, queueItem{TrackID: track.ID, Title: track.Title})
		}
		if err := a.state.save(); err != nil {
			return err
		}
		return a.cmdPlay(ctx, nil)
	default:
		return nil
	}
}

func orDefault(values []string, fallback string) []string {
	if len(values) == 0 {
		return []string{fallback}
	}
	return values
}
