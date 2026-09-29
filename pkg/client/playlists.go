package client

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// Playlist is one of the caller's playlists.
type Playlist struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	TrackCount int       `json:"trackCount"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// PlaylistItem is one entry of a playlist.
type PlaylistItem struct {
	Position int   `json:"position"`
	Track    Track `json:"track"`
}

// PlaylistDetail is a playlist with its tracks.
type PlaylistDetail struct {
	Playlist
	Items []PlaylistItem `json:"items"`
}

// Playlists lists the caller's playlists.
func (c *Client) Playlists(ctx context.Context) ([]Playlist, error) {
	var out struct {
		Playlists []Playlist `json:"playlists"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/me/playlists", nil, &out); err != nil {
		return nil, err
	}
	return out.Playlists, nil
}

// CreatePlaylist makes an empty playlist.
func (c *Client) CreatePlaylist(ctx context.Context, name string) (*Playlist, error) {
	var out Playlist
	if err := c.do(ctx, http.MethodPost, "/api/v1/me/playlists", map[string]string{"name": name}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Playlist returns one playlist with its tracks.
func (c *Client) Playlist(ctx context.Context, playlistID string) (*PlaylistDetail, error) {
	var out PlaylistDetail
	if err := c.do(ctx, http.MethodGet, "/api/v1/me/playlists/"+playlistID, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RenamePlaylist changes a playlist's name.
func (c *Client) RenamePlaylist(ctx context.Context, playlistID, name string) (*Playlist, error) {
	var out Playlist
	if err := c.do(ctx, http.MethodPatch, "/api/v1/me/playlists/"+playlistID, map[string]string{"name": name}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeletePlaylist removes a playlist.
func (c *Client) DeletePlaylist(ctx context.Context, playlistID string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/me/playlists/"+playlistID, nil, nil)
}

// AddPlaylistTracks appends tracks to a playlist.
func (c *Client) AddPlaylistTracks(ctx context.Context, playlistID string, trackIDs ...string) (*PlaylistDetail, error) {
	var out PlaylistDetail
	body := map[string]any{"trackIds": trackIDs}
	if err := c.do(ctx, http.MethodPost, "/api/v1/me/playlists/"+playlistID+"/items", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RemovePlaylistItem drops the entry at a position.
func (c *Client) RemovePlaylistItem(ctx context.Context, playlistID string, position int) (*PlaylistDetail, error) {
	var out PlaylistDetail
	path := fmt.Sprintf("/api/v1/me/playlists/%s/items/%d", playlistID, position)
	if err := c.do(ctx, http.MethodDelete, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ReorderPlaylist applies a new order, given as the current positions.
func (c *Client) ReorderPlaylist(ctx context.Context, playlistID string, order []int) (*PlaylistDetail, error) {
	var out PlaylistDetail
	body := map[string][]int{"order": order}
	if err := c.do(ctx, http.MethodPost, "/api/v1/me/playlists/"+playlistID+"/reorder", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PlaylistByName resolves a playlist by name; it reports the candidates when
// the name is ambiguous.
func (c *Client) PlaylistByName(ctx context.Context, name string) (*Playlist, error) {
	playlists, err := c.Playlists(ctx)
	if err != nil {
		return nil, err
	}
	var matches []Playlist
	for _, playlist := range playlists {
		if playlist.Name == name {
			matches = append(matches, playlist)
		}
	}
	switch len(matches) {
	case 0:
		return nil, &APIError{Status: http.StatusNotFound, Message: fmt.Sprintf("no playlist named %q", name)}
	case 1:
		return &matches[0], nil
	default:
		return nil, &APIError{
			Status:  http.StatusConflict,
			Message: fmt.Sprintf("%d playlists are named %q; use an id instead", len(matches), name),
		}
	}
}
