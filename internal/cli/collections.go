package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"codeberg.org/kyleraykbs/musoak/pkg/client"
)

// --- albums ----------------------------------------------------------------

func (a *App) cmdAlbum(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("musoak: usage: musoak album search|show|sync|queue|play <...>")
	}

	switch args[0] {
	case "search":
		if len(args) < 2 {
			return errors.New("musoak: usage: musoak album search <query>")
		}
		return a.albumSearch(ctx, strings.Join(args[1:], " "))

	case "show":
		if len(args) < 2 {
			return errors.New("musoak: usage: musoak album show <album-id|index>")
		}
		return a.albumShow(ctx, args[1])

	case "sync":
		if len(args) < 2 {
			return errors.New("musoak: usage: musoak album sync <album-id|index> [--providers a,b] [--resolve]")
		}
		return a.albumSync(ctx, args[1], args[2:])

	case "queue":
		if len(args) < 2 {
			return errors.New("musoak: usage: musoak album queue <album-id|index>")
		}
		return a.albumQueue(ctx, args[1])

	case "play":
		if len(args) < 2 {
			return errors.New("musoak: usage: musoak album play <album-id|index>")
		}
		if err := a.albumQueue(ctx, args[1]); err != nil {
			return err
		}
		a.state.Queue = nil
		// albumQueue appended into the queue; playing it means starting there.
		return a.cmdPlay(ctx, nil)

	default:
		return fmt.Errorf("musoak: unknown album command %q", args[0])
	}
}

func (a *App) albumSearch(ctx context.Context, query string) error {
	albums, problems, err := a.client.SearchAlbums(ctx, query, 10)
	if err != nil {
		return err
	}
	if len(albums) == 0 {
		a.printf("no albums for %q\n", query)
	}
	a.state.LastAlbums = a.state.LastAlbums[:0]
	for _, album := range albums {
		a.state.LastAlbums = append(a.state.LastAlbums, queueItem{TrackID: album.ID, Title: album.Title})
	}
	if err := a.state.save(); err != nil {
		return err
	}

	for i, album := range albums {
		year := album.Year
		if year == "" {
			year = "----"
		}
		a.printf("%3d. %-40s %s  %-22s [%s]\n", i, truncate(album.Title, 40), year,
			truncate(joinArtists(album.Artists), 22), strings.Join(album.Providers, " "))
	}
	for _, problem := range problems {
		a.printf("  ! %s: %s\n", problem.Provider, problem.Error)
	}
	a.printf("\nsync one with: musoak album sync <index>\n")
	return nil
}

// resolveAlbum accepts an album id or an index into the last album search.
func (a *App) resolveAlbum(argument string) (string, string, error) {
	argument = strings.TrimSpace(argument)
	if argument == "" {
		return "", "", errors.New("musoak: an album id or index is required")
	}
	if index, err := parseIndex(argument); err == nil {
		if index < 0 || index >= len(a.state.LastAlbums) {
			return "", "", fmt.Errorf("musoak: no album search result %d; run \"musoak album search\" first", index)
		}
		item := a.state.LastAlbums[index]
		return item.TrackID, item.Title, nil
	}
	if !looksLikeID(argument) {
		return "", "", fmt.Errorf("musoak: %q is not an album id or index", argument)
	}
	return argument, "", nil
}

func (a *App) albumShow(ctx context.Context, argument string) error {
	albumID, _, err := a.resolveAlbum(argument)
	if err != nil {
		return err
	}
	album, err := a.client.Album(ctx, albumID)
	if err != nil {
		return err
	}
	a.printAlbum(album, true)
	return nil
}

func (a *App) printAlbum(album *client.Album, withTracks bool) {
	year := album.Year
	if year == "" {
		year = "----"
	}
	a.printf("%s\n  %s, %s  [%s]  %d track(s)\n", album.Title, joinArtists(album.Artists), year,
		strings.Join(album.Providers, " "), album.TrackCount)
	for _, variant := range album.Variants {
		a.printf("  %-10s %s (%s, %d track(s))\n", variant.Provider,
			truncate(variant.Title, 40), orDash(variant.Year), variant.TrackCount)
	}
	if !withTracks {
		return
	}
	if len(album.Tracks) == 0 {
		a.printf("  no tracklist yet; run \"musoak album sync %s\"\n", album.ID)
		return
	}
	for i, track := range album.Tracks {
		a.printf("%3d. %-44s %8s  %s\n", i, truncate(track.Title, 44),
			formatDuration(track.DurationMs), joinArtists(track.Artists))
	}
}

func orDash(value string) string {
	if value == "" {
		return "--"
	}
	return value
}

func (a *App) albumSync(ctx context.Context, argument string, flags []string) error {
	albumID, title, err := a.resolveAlbum(argument)
	if err != nil {
		return err
	}
	opts, err := parseSyncFlags(flags)
	if err != nil {
		return err
	}

	result, err := a.client.SyncAlbum(ctx, albumID, opts)
	if err != nil {
		return err
	}
	if title == "" {
		title = albumID
	}
	a.printf("synced %q from %s: %d new track(s), %d in the album\n",
		title, strings.Join(result.Providers, ","), result.Added, len(result.Tracks))
	for _, problem := range result.ProviderErrors {
		a.printf("  ! %s: %s\n", problem.Provider, problem.Error)
	}
	for i, track := range result.Tracks {
		a.printf("%3d. %-44s %8s  %s\n", i, truncate(track.Title, 44),
			formatDuration(track.DurationMs), joinArtists(track.Artists))
	}
	return nil
}

func (a *App) albumQueue(ctx context.Context, argument string) error {
	albumID, _, err := a.resolveAlbum(argument)
	if err != nil {
		return err
	}
	album, err := a.client.Album(ctx, albumID)
	if err != nil {
		return err
	}
	if len(album.Tracks) == 0 {
		return fmt.Errorf("musoak: %q has no tracklist yet; run \"musoak album sync %s\"", album.Title, album.ID)
	}
	for _, track := range album.Tracks {
		a.state.Queue = append(a.state.Queue, queueItem{TrackID: track.ID, Title: track.Title})
	}
	if err := a.state.save(); err != nil {
		return err
	}
	a.printf("queued %d track(s) from %q (%d in queue)\n", len(album.Tracks), album.Title, len(a.state.Queue))
	return nil
}

// --- artists ---------------------------------------------------------------

func (a *App) cmdArtist(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("musoak: usage: musoak artist search|show|sync <...>")
	}

	switch args[0] {
	case "search":
		if len(args) < 2 {
			return errors.New("musoak: usage: musoak artist search <query>")
		}
		return a.artistSearch(ctx, strings.Join(args[1:], " "))

	case "show":
		if len(args) < 2 {
			return errors.New("musoak: usage: musoak artist show <artist-id|index>")
		}
		return a.artistShow(ctx, args[1])

	case "sync":
		if len(args) < 2 {
			return errors.New("musoak: usage: musoak artist sync <artist-id|index> [--albums] [--resolve] [--providers a,b]")
		}
		return a.artistSync(ctx, args[1], args[2:])

	default:
		return fmt.Errorf("musoak: unknown artist command %q", args[0])
	}
}

func (a *App) artistSearch(ctx context.Context, query string) error {
	artists, problems, err := a.client.SearchArtists(ctx, query, 10)
	if err != nil {
		return err
	}
	if len(artists) == 0 {
		a.printf("no artists for %q\n", query)
	}
	a.state.LastArtists = a.state.LastArtists[:0]
	for _, artist := range artists {
		a.state.LastArtists = append(a.state.LastArtists, queueItem{TrackID: artist.ID, Title: artist.Name})
	}
	if err := a.state.save(); err != nil {
		return err
	}

	for i, artist := range artists {
		a.printf("%3d. %-40s [%s]\n", i, truncate(artist.Name, 40), strings.Join(artist.Providers, " "))
	}
	for _, problem := range problems {
		a.printf("  ! %s: %s\n", problem.Provider, problem.Error)
	}
	a.printf("\nsync one with: musoak artist sync <index> --albums\n")
	return nil
}

func (a *App) resolveArtist(argument string) (string, string, error) {
	argument = strings.TrimSpace(argument)
	if argument == "" {
		return "", "", errors.New("musoak: an artist id or index is required")
	}
	if index, err := parseIndex(argument); err == nil {
		if index < 0 || index >= len(a.state.LastArtists) {
			return "", "", fmt.Errorf("musoak: no artist search result %d; run \"musoak artist search\" first", index)
		}
		item := a.state.LastArtists[index]
		return item.TrackID, item.Title, nil
	}
	if !looksLikeID(argument) {
		return "", "", fmt.Errorf("musoak: %q is not an artist id or index", argument)
	}
	return argument, "", nil
}

func (a *App) artistShow(ctx context.Context, argument string) error {
	artistID, _, err := a.resolveArtist(argument)
	if err != nil {
		return err
	}
	artist, err := a.client.Artist(ctx, artistID)
	if err != nil {
		return err
	}
	a.printf("%s  [%s]\n", artist.Name, strings.Join(artist.Providers, " "))
	for _, variant := range artist.Variants {
		a.printf("  %-10s %s\n", variant.Provider, variant.Name)
	}
	if len(artist.Albums) == 0 {
		a.printf("  no albums yet; run \"musoak artist sync %s --albums\"\n", artist.ID)
		return nil
	}
	for i, album := range artist.Albums {
		a.printf("%3d. %-40s %s  %d track(s)\n", i, truncate(album.Title, 40), orDash(album.Year), album.TrackCount)
	}
	return nil
}

func (a *App) artistSync(ctx context.Context, argument string, flags []string) error {
	artistID, name, err := a.resolveArtist(argument)
	if err != nil {
		return err
	}
	opts, err := parseSyncFlags(flags)
	if err != nil {
		return err
	}
	for _, flag := range flags {
		if flag == "--albums" {
			opts.SyncAlbums = true
		}
	}

	result, err := a.client.SyncArtist(ctx, artistID, opts)
	if err != nil {
		return err
	}
	if name == "" {
		name = artistID
	}
	a.printf("synced %q from %s: %d album(s), %d new track(s)\n",
		name, strings.Join(result.Providers, ","), len(result.Albums), result.Added)
	for _, problem := range result.ProviderErrors {
		a.printf("  ! %s: %s\n", problem.Provider, problem.Error)
	}
	for i, album := range result.Albums {
		a.printf("%3d. %-40s %s  %d track(s)\n", i, truncate(album.Title, 40), orDash(album.Year), album.TrackCount)
	}
	return nil
}

// parseSyncFlags reads the options album and artist syncs share.
func parseSyncFlags(flags []string) (client.SyncOptions, error) {
	var opts client.SyncOptions
	for i := 0; i < len(flags); i++ {
		switch argument := flags[i]; {
		case argument == "--resolve":
			opts.Resolve = true
		case argument == "--albums":
			opts.SyncAlbums = true
		case argument == "--providers":
			if i+1 >= len(flags) {
				return opts, errors.New("musoak: --providers needs a comma separated list")
			}
			opts.Providers = splitList(flags[i+1])
			i++
		case strings.HasPrefix(argument, "--providers="):
			opts.Providers = splitList(strings.TrimPrefix(argument, "--providers="))
		default:
			return opts, fmt.Errorf("musoak: unknown sync option %q", argument)
		}
	}
	return opts, nil
}

func splitList(raw string) []string {
	out := make([]string, 0)
	for _, field := range strings.Split(raw, ",") {
		if field = strings.TrimSpace(field); field != "" {
			out = append(out, field)
		}
	}
	return out
}
