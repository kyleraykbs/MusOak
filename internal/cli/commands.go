package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"codeberg.org/kyleraykbs/prismusic/internal/api"
	"codeberg.org/kyleraykbs/prismusic/pkg/client"
)

func parseIndex(raw string) (int, error) {
	return strconv.Atoi(strings.TrimSpace(raw))
}

// --- search ----------------------------------------------------------------

func (a *App) cmdSearch(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("search", flag.ContinueOnError)
	set.SetOutput(a.out)
	limit := set.Int("limit", 10, "results per provider")
	if err := set.Parse(args); err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(set.Args(), " "))
	if query == "" {
		return errors.New("prism: search needs a query")
	}

	result, err := a.client.Search(ctx, query, *limit)
	if err != nil {
		return err
	}
	if len(result.Groups) == 0 {
		a.printf("no results for %q\n", query)
	}
	a.state.LastSearch = a.state.LastSearch[:0]
	for i, group := range result.Groups {
		providers := make([]string, 0, len(group.Variants))
		for _, variant := range group.Variants {
			label := variant.Provider
			if variant.Media.State == client.MediaReady {
				label += "*"
			}
			providers = append(providers, label)
		}
		sort.Strings(providers)
		a.printf("%3d. %-50s %8s  [%s]\n", i, truncate(group.Track.Title, 50),
			formatDuration(group.Track.DurationMs), strings.Join(providers, " "))
		a.printf("     %s\n", joinArtists(group.Track.Artists))
		a.state.LastSearch = append(a.state.LastSearch, queueItem{TrackID: group.Track.ID, Title: group.Track.Title})
	}
	for _, problem := range result.ProviderErrors {
		a.printf("  ! %s: %s\n", problem.Provider, problem.Error)
	}
	if err := a.state.save(); err != nil {
		return err
	}
	if len(result.Groups) > 0 {
		a.printf("\nqueue one with: prism queue add <index>\n")
	}
	return nil
}

func truncate(s string, width int) string {
	if len(s) <= width {
		return s
	}
	if width <= 1 {
		return s[:width]
	}
	return s[:width-1] + "…"
}

// --- queue -----------------------------------------------------------------

func (a *App) cmdQueue(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return a.queueList()
	}
	switch args[0] {
	case "list":
		return a.queueList()
	case "add":
		if len(args) < 2 {
			return errors.New("prism: queue add needs a track id or search index")
		}
		item, err := a.resolveTrack(ctx, args[1])
		if err != nil {
			return err
		}
		a.state.Queue = append(a.state.Queue, item)
		if err := a.state.save(); err != nil {
			return err
		}
		a.printf("queued %s (%d in queue)\n", item.Title, len(a.state.Queue))
		return nil
	case "rm", "remove":
		if len(args) < 2 {
			return errors.New("prism: queue rm needs an index")
		}
		index, err := parseIndex(args[1])
		if err != nil || index < 0 || index >= len(a.state.Queue) {
			return fmt.Errorf("prism: no queue entry %q", args[1])
		}
		item := a.state.Queue[index]
		a.state.Queue = append(a.state.Queue[:index], a.state.Queue[index+1:]...)
		if err := a.state.save(); err != nil {
			return err
		}
		a.printf("removed %s\n", item.Title)
		return nil
	case "clear":
		a.state.Queue = nil
		return a.state.save()
	case "play":
		return a.cmdPlay(ctx, nil)
	default:
		return fmt.Errorf("prism: unknown queue command %q", args[0])
	}
}

func (a *App) queueList() error {
	if len(a.state.Queue) == 0 {
		a.printf("the queue is empty\n")
		return nil
	}
	for i, item := range a.state.Queue {
		a.printf("%3d. %s\n", i, item.Title)
	}
	return nil
}

// --- library ---------------------------------------------------------------

func (a *App) cmdLibrary(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return errors.New("prism: usage: prism library import <file|dir>")
	}
	if args[0] != "import" {
		return fmt.Errorf("prism: unknown library command %q", args[0])
	}

	path := args[1]
	info, err := os.Stat(path)
	if err != nil {
		return err
	}

	var result *client.ImportResult
	if info.IsDir() {
		result, err = a.client.ImportDir(ctx, path)
	} else {
		result, err = a.client.ImportPath(ctx, path)
	}
	if err != nil {
		return err
	}

	for _, importedEntry := range result.Imported {
		a.printf("imported %s (%s) as %s\n", importedEntry.Track.Title,
			formatDuration(importedEntry.Variant.Media.DurationMs), importedEntry.Variant.Provider)

		// Keep the local copy in the cache, tagged with its track, so playback
		// never re-downloads it and rooms hear about the copy we already have.
		if importedEntry.Variant.Media.State != client.MediaReady {
			continue
		}
		entry, err := a.cache.Fetch(ctx, a.client, importedEntry.Variant.ID)
		if err != nil {
			a.printf("  ! could not cache %s: %v\n", importedEntry.Variant.ID, err)
			continue
		}
		entry.TrackID = importedEntry.Track.ID
		if err := a.cache.Put(entry); err != nil {
			a.printf("  ! could not index %s: %v\n", importedEntry.Variant.ID, err)
		}
	}
	for _, failure := range result.Failures {
		a.printf("  ! %s\n", failure)
	}
	if len(result.Imported) == 0 && len(result.Failures) == 0 {
		a.printf("nothing to import under %s\n", path)
	}
	return nil
}

// --- favorites -------------------------------------------------------------

func (a *App) cmdFavorites(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "list" {
		tracks, err := a.client.Favorites(ctx)
		if err != nil {
			return err
		}
		if len(tracks) == 0 {
			a.printf("no favorites yet\n")
			return nil
		}
		for _, track := range tracks {
			a.printf("%s  %-50s %8s  %s\n", track.ID, truncate(track.Title, 50),
				formatDuration(track.DurationMs), joinArtists(track.Artists))
		}
		return nil
	}

	switch args[0] {
	case "add":
		if len(args) < 2 {
			return errors.New("prism: fav add needs a track id or search index")
		}
		item, err := a.resolveTrack(ctx, args[1])
		if err != nil {
			return err
		}
		if err := a.client.AddFavorite(ctx, item.TrackID); err != nil {
			return err
		}
		a.printf("favorited %s\n", item.Title)
		return nil
	case "rm", "remove":
		if len(args) < 2 {
			return errors.New("prism: fav rm needs a track id or search index")
		}
		item, err := a.resolveTrack(ctx, args[1])
		if err != nil {
			return err
		}
		if err := a.client.RemoveFavorite(ctx, item.TrackID); err != nil {
			return err
		}
		a.printf("unfavorited %s\n", item.Title)
		return nil
	default:
		return fmt.Errorf("prism: unknown fav command %q", args[0])
	}
}

// --- providers -------------------------------------------------------------

func (a *App) cmdProviders(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "rank" {
		if len(args) < 2 {
			ranking, err := a.client.Ranking(ctx)
			if err != nil {
				return err
			}
			a.printf("your ranking:   %s\n", strings.Join(ranking.Ranking, ", "))
			a.printf("effective:      %s\n", strings.Join(ranking.Effective, ", "))
			a.printf("aggregate:      %s\n", strings.Join(ranking.Aggregate, ", "))
			a.printf("default:        %s\n", strings.Join(ranking.Default, ", "))
			return nil
		}
		providers := strings.Split(args[1], ",")
		for i := range providers {
			providers[i] = strings.TrimSpace(providers[i])
		}
		ranking, err := a.client.SetRanking(ctx, providers)
		if err != nil {
			return err
		}
		a.printf("effective order: %s\n", strings.Join(ranking.Effective, ", "))
		return nil
	}

	providers, err := a.client.Providers(ctx)
	if err != nil {
		return err
	}
	for _, provider := range providers {
		marks := make([]string, 0, 2)
		if provider.Capabilities.Search {
			marks = append(marks, "search")
		}
		if provider.Capabilities.Download {
			marks = append(marks, "download")
		} else {
			marks = append(marks, "metadata-only")
		}
		a.printf("%-10s %s\n", provider.Name, strings.Join(marks, ", "))
		for _, dependency := range provider.MissingDeps {
			a.printf("  ! missing %s\n", dependency)
		}
	}
	return nil
}

// --- accounts --------------------------------------------------------------

func (a *App) cmdLogin(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("login", flag.ContinueOnError)
	set.SetOutput(a.out)
	password := set.String("password", "", "password (read from stdin when empty)")
	register := set.Bool("register", false, "create the account instead of logging in")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() < 1 {
		return errors.New("prism: usage: prism login <username> [--register]")
	}
	username := set.Arg(0)

	secret := *password
	if secret == "" {
		line, err := readPassword(a.out)
		if err != nil {
			return err
		}
		secret = line
	}

	var (
		auth *client.Auth
		err  error
	)
	if *register {
		auth, err = a.client.Register(ctx, username, secret)
	} else {
		auth, err = a.client.Login(ctx, username, secret)
	}
	if err != nil {
		return err
	}
	a.state.Token = auth.Token
	if err := a.state.save(); err != nil {
		return err
	}
	a.printf("logged in as %s\n", auth.User.Username)
	return nil
}

func readPassword(out interface{ Write([]byte) (int, error) }) (string, error) {
	fmt.Fprint(out, "password: ")
	var line string
	if _, err := fmt.Fscanln(os.Stdin, &line); err != nil {
		return "", fmt.Errorf("prism: read password: %w", err)
	}
	return line, nil
}

func (a *App) cmdLogout(ctx context.Context, args []string) error {
	if a.state.Token != "" {
		if err := a.client.Logout(ctx); err != nil && !client.IsUnauthorized(err) {
			return err
		}
	}
	a.state.Token = ""
	if err := a.state.save(); err != nil {
		return err
	}
	a.printf("logged out\n")
	return nil
}

func (a *App) cmdMe(ctx context.Context, args []string) error {
	me, err := a.client.Me(ctx)
	if err != nil {
		return err
	}
	switch {
	case me.Authenticated && me.User != nil:
		a.printf("%s (%s)\n", me.User.Username, me.User.ID)
	case me.Guest:
		a.printf("guest (this server allows anonymous reading)\n")
	default:
		a.printf("not logged in; the server requires a login\n")
	}
	a.printf("server: requireLogin=%v registrationOpen=%v\n", me.RequireLogin, me.RegistrationOpen)
	return nil
}

// --- serve -----------------------------------------------------------------

// cmdServe runs the server in the foreground. With client.serverURL empty the
// CLI already embeds one, so `prism serve` exists for running that server on
// its own, on the configured listen address.
func (a *App) cmdServe(ctx context.Context, args []string) error {
	server, err := api.New(a.cfg, a.logger)
	if err != nil {
		return err
	}
	defer func() {
		if err := server.Close(); err != nil {
			a.logger.Error("prism: closing the server", "error", err)
		}
	}()

	a.printf("serving on %s (storage %s)\n", a.cfg.Listen, a.cfg.StorageDir)
	return server.Run(ctx)
}

// --- rooms -----------------------------------------------------------------

func (a *App) cmdRoom(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return a.roomList(ctx)
	}
	switch args[0] {
	case "list", "ls":
		return a.roomList(ctx)
	case "create":
		set := flag.NewFlagSet("room create", flag.ContinueOnError)
		set.SetOutput(a.out)
		name := set.String("name", "", "room name")
		controls := set.String("controls", "everyone", "who may control playback: host or everyone")
		if err := set.Parse(args[1:]); err != nil {
			return err
		}
		room, err := a.client.CreateRoom(ctx, *name, *controls)
		if err != nil {
			return err
		}
		a.printf("room %s (%s) created\n", room.RoomID, *controls)
		return nil
	case "join":
		if len(args) < 2 {
			return errors.New("prism: usage: prism room join <room-id>")
		}
		return a.roomJoin(ctx, args[1])
	case "leave":
		room, err := a.joinedRoom(ctx, true)
		if err != nil {
			return err
		}
		if err := room.Leave(ctx); err != nil {
			return err
		}
		a.printf("left %s\n", room.RoomID)
		return nil
	case "queue":
		if len(args) < 3 || args[1] != "add" {
			return errors.New("prism: usage: prism room queue add <track-id|index>")
		}
		item, err := a.resolveTrack(ctx, args[2])
		if err != nil {
			return err
		}
		room, err := a.joinedRoom(ctx, true)
		if err != nil {
			return err
		}
		if _, err := room.Queue(ctx, item.TrackID); err != nil {
			return err
		}
		a.printf("queued %s in %s\n", item.Title, room.RoomID)
		return nil
	case "vote", "skip", "pause", "resume", "seek", "remove", "now", "reorder":
		return a.roomControl(ctx, args)
	default:
		return fmt.Errorf("prism: unknown room command %q", args[0])
	}
}

func (a *App) roomList(ctx context.Context) error {
	rooms, err := a.client.Rooms(ctx)
	if err != nil {
		return err
	}
	if len(rooms) == 0 {
		a.printf("no rooms\n")
		return nil
	}
	for _, room := range rooms {
		current := "-"
		if room.Current != nil {
			current = room.Current.Item.Title
		}
		a.printf("%s  %-20s %2d member(s)  now: %s\n", room.ID, truncate(room.Name, 20), len(room.Members), current)
	}
	return nil
}

func (a *App) roomControl(ctx context.Context, args []string) error {
	room, err := a.joinedRoom(ctx, true)
	if err != nil {
		return err
	}
	switch args[0] {
	case "vote":
		if len(args) < 2 {
			return errors.New("prism: usage: prism room vote <1-5>")
		}
		score, err := parseIndex(args[1])
		if err != nil {
			return fmt.Errorf("prism: vote must be 1-5")
		}
		snapshot, err := room.Vote(ctx, score)
		if err != nil {
			return err
		}
		a.printf("voted %d; mean %.2f (%d votes)\n", score, snapshot.Current.MeanScore, len(snapshot.Current.Votes))
		return nil
	case "skip":
		_, err := room.Skip(ctx)
		return err
	case "pause":
		_, err := room.Pause(ctx)
		return err
	case "resume":
		_, err := room.Resume(ctx)
		return err
	case "seek":
		if len(args) < 2 {
			return errors.New("prism: usage: prism room seek <milliseconds>")
		}
		position, err := parseIndex(args[1])
		if err != nil {
			return fmt.Errorf("prism: seek needs milliseconds")
		}
		if _, err := room.Seek(ctx, int64(position)); err != nil {
			return err
		}
		return nil
	case "now":
		snapshot, err := room.Snapshot(ctx)
		if err != nil {
			return err
		}
		a.printRoom(snapshot)
		return nil
	default:
		return fmt.Errorf("prism: unknown room command %q", args[0])
	}
}

// roomJoin follows a room with mpv until the context ends.
func (a *App) roomJoin(ctx context.Context, roomID string) error {
	if err := a.state.saveRoom(roomID); err != nil {
		return err
	}
	room, err := a.client.JoinRoom(ctx, roomID)
	if err != nil {
		return err
	}

	sink, err := NewMpvSink(ctx, a.socketPath(), a.logger)
	if err != nil {
		return err
	}
	defer sink.Close()

	participant := client.NewParticipant(room, a.cache, sink, a.logger)
	participant.OnTrack = func(track client.SinkTrack) {
		a.printf("playing %s (from %s, %s in, room slot %s)\n", track.VariantID[:8],
			track.Path, formatDuration(track.PositionMs), formatDuration(track.TimelineMs))
	}
	participant.OnEvent = func(event client.Event) {
		if event.Type == client.EventTrackPrepared || event.Type == client.EventTrackSkipped {
			a.logger.Debug("room event", "type", event.Type)
		}
	}

	a.printf("joined %s as %s; Ctrl-C to leave\n", roomID, room.MemberID)
	return participant.Run(ctx)
}

// joinedRoom returns the room this CLI is in, re-joining when needed.
func (a *App) joinedRoom(ctx context.Context, required bool) (*client.RoomClient, error) {
	if roomID := a.state.RoomID; roomID != "" {
		room, err := a.client.JoinRoom(ctx, roomID)
		if err == nil {
			return room, nil
		}
		if !client.IsNotFound(err) {
			return nil, err
		}
		a.state.RoomID = ""
		_ = a.state.save()
	}
	if required {
		return nil, errors.New("prism: not in a room; use \"prism room join <room-id>\"")
	}
	return nil, nil
}

func (a *App) printRoom(room *client.Room) {
	a.printf("room %s (%s), %d member(s)\n", room.ID, room.Name, len(room.Members))
	for i, item := range room.Queue {
		a.printf("  queue %2d. %s\n", i, item.Title)
	}
	if room.Current == nil {
		a.printf("  nothing playing\n")
		return
	}
	current := room.Current
	a.printf("  playing: %s  %s / %s%s\n", current.Item.Title,
		formatDuration(current.PositionMs), formatDuration(current.TimelineMs), pausedSuffix(current.Paused))
	if len(current.Votes) > 0 {
		a.printf("  votes: %d, mean %.2f (skip below %.1f)\n",
			len(current.Votes), current.MeanScore, room.Skip.SkipThreshold)
	}
	if len(current.Awaiting) > 0 {
		a.printf("  waiting for: %s\n", strings.Join(current.Awaiting, ", "))
	}
	if len(current.CatchingUp) > 0 {
		a.printf("  catching up: %s\n", strings.Join(current.CatchingUp, ", "))
	}
}

func pausedSuffix(paused bool) string {
	if paused {
		return "  [paused]"
	}
	return ""
}

func (a *App) socketPath() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("prism-mpv-%d.sock", os.Getpid()))
}
