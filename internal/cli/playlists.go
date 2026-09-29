package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"codeberg.org/kyleraykbs/prismusic/pkg/client"
)

// --- playlists --------------------------------------------------------------

// resolvePlaylist accepts a playlist id or a name the caller owns.
func (a *App) resolvePlaylist(ctx context.Context, argument string) (*client.Playlist, error) {
	argument = strings.TrimSpace(argument)
	if argument == "" {
		return nil, errors.New("prism: a playlist id or name is required")
	}
	if looksLikeID(argument) {
		detail, err := a.client.Playlist(ctx, argument)
		if err != nil {
			return nil, err
		}
		return &detail.Playlist, nil
	}
	return a.client.PlaylistByName(ctx, argument)
}

func looksLikeID(argument string) bool {
	if len(argument) != 36 {
		return false
	}
	for _, r := range argument {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r == '-':
		default:
			return false
		}
	}
	return true
}

func (a *App) cmdPlaylist(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return a.playlistList(ctx)
	}

	switch args[0] {
	case "list", "ls":
		return a.playlistList(ctx)

	case "create":
		if len(args) < 2 {
			return errors.New("prism: usage: prism playlist create <name>")
		}
		name := strings.Join(args[1:], " ")
		playlist, err := a.client.CreatePlaylist(ctx, name)
		if err != nil {
			return err
		}
		a.printf("created playlist %q (%s)\n", playlist.Name, playlist.ID)
		return nil

	case "show":
		if len(args) < 2 {
			return errors.New("prism: usage: prism playlist show <id|name>")
		}
		return a.playlistShow(ctx, args[1])

	case "add":
		if len(args) < 3 {
			return errors.New("prism: usage: prism playlist add <id|name> <track-id|index>...")
		}
		return a.playlistAdd(ctx, args[1], args[2:])

	case "rm", "remove":
		if len(args) < 3 {
			return errors.New("prism: usage: prism playlist rm <id|name> <position>")
		}
		return a.playlistRemove(ctx, args[1], args[2])

	case "reorder":
		if len(args) < 3 {
			return errors.New("prism: usage: prism playlist reorder <id|name> <position,position,...>")
		}
		return a.playlistReorder(ctx, args[1], args[2])

	case "rename":
		if len(args) < 3 {
			return errors.New("prism: usage: prism playlist rename <id|name> <new name>")
		}
		return a.playlistRename(ctx, args[1], strings.Join(args[2:], " "))

	case "delete", "rm-playlist":
		if len(args) < 2 {
			return errors.New("prism: usage: prism playlist delete <id|name>")
		}
		return a.playlistDelete(ctx, args[1])

	case "queue":
		if len(args) < 2 {
			return errors.New("prism: usage: prism playlist queue <id|name>")
		}
		return a.playlistQueue(ctx, args[1])

	case "play":
		if len(args) < 2 {
			return errors.New("prism: usage: prism playlist play <id|name>")
		}
		return a.playlistPlay(ctx, args[1])

	default:
		return fmt.Errorf("prism: unknown playlist command %q", args[0])
	}
}

func (a *App) playlistList(ctx context.Context) error {
	playlists, err := a.client.Playlists(ctx)
	if err != nil {
		return err
	}
	if len(playlists) == 0 {
		a.printf("no playlists; create one with \"prism playlist create <name>\"\n")
		return nil
	}
	for _, playlist := range playlists {
		a.printf("%s  %-28s %3d track(s)\n", playlist.ID, truncate(playlist.Name, 28), playlist.TrackCount)
	}
	return nil
}

func (a *App) playlistShow(ctx context.Context, argument string) error {
	playlist, err := a.resolvePlaylist(ctx, argument)
	if err != nil {
		return err
	}
	detail, err := a.client.Playlist(ctx, playlist.ID)
	if err != nil {
		return err
	}
	a.printPlaylist(detail)
	return nil
}

func (a *App) printPlaylist(detail *client.PlaylistDetail) {
	a.printf("%s (%s), %d track(s)\n", detail.Name, detail.ID, len(detail.Items))
	for _, item := range detail.Items {
		a.printf("%3d. %-44s %8s  %s\n", item.Position, truncate(item.Track.Title, 44),
			formatDuration(item.Track.DurationMs), joinArtists(item.Track.Artists))
	}
}

func (a *App) playlistAdd(ctx context.Context, argument string, tracks []string) error {
	playlist, err := a.resolvePlaylist(ctx, argument)
	if err != nil {
		return err
	}

	ids := make([]string, 0, len(tracks))
	for _, raw := range tracks {
		item, err := a.resolveTrack(ctx, raw)
		if err != nil {
			return err
		}
		ids = append(ids, item.TrackID)
	}

	detail, err := a.client.AddPlaylistTracks(ctx, playlist.ID, ids...)
	if err != nil {
		return err
	}
	a.printf("added %d track(s) to %q (%d total)\n", len(ids), detail.Name, len(detail.Items))
	return nil
}

func (a *App) playlistRemove(ctx context.Context, argument, rawPosition string) error {
	playlist, err := a.resolvePlaylist(ctx, argument)
	if err != nil {
		return err
	}
	position, err := strconv.Atoi(rawPosition)
	if err != nil {
		return fmt.Errorf("prism: %q is not a position", rawPosition)
	}
	detail, err := a.client.RemovePlaylistItem(ctx, playlist.ID, position)
	if err != nil {
		return err
	}
	a.printf("removed entry %d from %q (%d left)\n", position, detail.Name, len(detail.Items))
	return nil
}

func (a *App) playlistReorder(ctx context.Context, argument, rawOrder string) error {
	playlist, err := a.resolvePlaylist(ctx, argument)
	if err != nil {
		return err
	}
	order := make([]int, 0)
	for _, field := range strings.Split(rawOrder, ",") {
		position, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil {
			return fmt.Errorf("prism: %q is not a position list", rawOrder)
		}
		order = append(order, position)
	}
	detail, err := a.client.ReorderPlaylist(ctx, playlist.ID, order)
	if err != nil {
		return err
	}
	a.printPlaylist(detail)
	return nil
}

func (a *App) playlistRename(ctx context.Context, argument, name string) error {
	playlist, err := a.resolvePlaylist(ctx, argument)
	if err != nil {
		return err
	}
	renamed, err := a.client.RenamePlaylist(ctx, playlist.ID, name)
	if err != nil {
		return err
	}
	a.printf("renamed to %q\n", renamed.Name)
	return nil
}

func (a *App) playlistDelete(ctx context.Context, argument string) error {
	playlist, err := a.resolvePlaylist(ctx, argument)
	if err != nil {
		return err
	}
	if err := a.client.DeletePlaylist(ctx, playlist.ID); err != nil {
		return err
	}
	a.printf("deleted %q\n", playlist.Name)
	return nil
}

// playlistQueue appends a playlist to the local queue.
func (a *App) playlistQueue(ctx context.Context, argument string) error {
	playlist, err := a.resolvePlaylist(ctx, argument)
	if err != nil {
		return err
	}
	detail, err := a.client.Playlist(ctx, playlist.ID)
	if err != nil {
		return err
	}
	for _, item := range detail.Items {
		a.state.Queue = append(a.state.Queue, queueItem{TrackID: item.Track.ID, Title: item.Track.Title})
	}
	if err := a.state.save(); err != nil {
		return err
	}
	a.printf("queued %d track(s) from %q (%d in queue)\n", len(detail.Items), detail.Name, len(a.state.Queue))
	return nil
}

// playlistPlay replaces the local queue with a playlist and plays it.
func (a *App) playlistPlay(ctx context.Context, argument string) error {
	playlist, err := a.resolvePlaylist(ctx, argument)
	if err != nil {
		return err
	}
	detail, err := a.client.Playlist(ctx, playlist.ID)
	if err != nil {
		return err
	}
	if len(detail.Items) == 0 {
		return fmt.Errorf("prism: %q is empty", detail.Name)
	}

	a.state.Queue = a.state.Queue[:0]
	for _, item := range detail.Items {
		a.state.Queue = append(a.state.Queue, queueItem{TrackID: item.Track.ID, Title: item.Track.Title})
	}
	if err := a.state.save(); err != nil {
		return err
	}
	a.printf("playing %q (%d track(s))\n", detail.Name, len(detail.Items))
	return a.cmdPlay(ctx, nil)
}

// playlistTracks returns the track ids of a playlist, in order.
func (a *App) playlistTracks(ctx context.Context, argument string) ([]string, string, error) {
	playlist, err := a.resolvePlaylist(ctx, argument)
	if err != nil {
		return nil, "", err
	}
	detail, err := a.client.Playlist(ctx, playlist.ID)
	if err != nil {
		return nil, "", err
	}
	ids := make([]string, 0, len(detail.Items))
	for _, item := range detail.Items {
		ids = append(ids, item.Track.ID)
	}
	return ids, detail.Name, nil
}

// roomQueuePlaylist enqueues every track of a playlist into the current room.
func (a *App) roomQueuePlaylist(ctx context.Context, argument string) error {
	ids, name, err := a.playlistTracks(ctx, argument)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return fmt.Errorf("prism: %q is empty", name)
	}
	room, err := a.joinedRoom(ctx, true)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := room.Queue(ctx, id); err != nil {
			return err
		}
	}
	a.printf("queued %d track(s) from %q into %s\n", len(ids), name, room.RoomID)
	return nil
}
