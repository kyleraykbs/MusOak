package client

import (
	"context"
	"fmt"
	"net/http"
)

// AlbumVariant is one provider's release of a canonical album.
type AlbumVariant struct {
	ID              string   `json:"id"`
	Provider        string   `json:"provider"`
	ProviderAlbumID string   `json:"providerAlbumId"`
	Title           string   `json:"title"`
	Artists         []string `json:"artists"`
	Year            string   `json:"year,omitempty"`
	TrackCount      int      `json:"trackCount,omitempty"`
}

// Album is a canonical album; providers lists the sources that know it.
type Album struct {
	ID         string         `json:"id"`
	Title      string         `json:"title"`
	Artists    []string       `json:"artists"`
	Year       string         `json:"year,omitempty"`
	TrackCount int            `json:"trackCount"`
	Providers  []string       `json:"providers"`
	Variants   []AlbumVariant `json:"variants,omitempty"`
	Tracks     []Track        `json:"tracks,omitempty"`
}

// ArtistVariant is one provider's page for a canonical artist.
type ArtistVariant struct {
	ID               string `json:"id"`
	Provider         string `json:"provider"`
	ProviderArtistID string `json:"providerArtistId"`
	Name             string `json:"name"`
}

// Artist is a canonical artist.
type Artist struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Providers []string        `json:"providers"`
	Variants  []ArtistVariant `json:"variants,omitempty"`
	Albums    []Album         `json:"albums,omitempty"`
}

// SyncOptions tunes a collection sync.
type SyncOptions struct {
	// Providers selects what to sync from; empty means every known release.
	Providers []string
	// Resolve also makes sure each track has something playable.
	Resolve bool
	// SyncAlbums (artist syncs) fetches each album's tracklist too.
	SyncAlbums bool
}

// SyncResult reports what a sync added.
type SyncResult struct {
	AlbumID        string          `json:"albumId,omitempty"`
	ArtistID       string          `json:"artistId,omitempty"`
	Providers      []string        `json:"providers"`
	Added          int             `json:"added"`
	Tracks         []Track         `json:"tracks,omitempty"`
	Albums         []Album         `json:"albums,omitempty"`
	ProviderErrors []ProviderError `json:"providerErrors,omitempty"`
}

// SearchAlbums searches every provider that can browse albums.
func (c *Client) SearchAlbums(ctx context.Context, query string, limit int) ([]Album, []ProviderError, error) {
	var out struct {
		Albums         []Album         `json:"albums"`
		ProviderErrors []ProviderError `json:"providerErrors"`
	}
	path := "/api/v1/albums/search?q=" + urlQuery(query) + limitQuery(limit)
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, nil, err
	}
	return out.Albums, out.ProviderErrors, nil
}

// Album fetches one album with its tracklist.
func (c *Client) Album(ctx context.Context, albumID string) (*Album, error) {
	var out Album
	if err := c.do(ctx, http.MethodGet, "/api/v1/albums/"+albumID, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SyncAlbum pulls an album's provider tracklists into the library.
func (c *Client) SyncAlbum(ctx context.Context, albumID string, opts SyncOptions) (*SyncResult, error) {
	var out SyncResult
	body := syncBody(opts)
	if err := c.do(ctx, http.MethodPost, "/api/v1/albums/"+albumID+"/sync", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SearchArtists searches every provider that can browse artists.
func (c *Client) SearchArtists(ctx context.Context, query string, limit int) ([]Artist, []ProviderError, error) {
	var out struct {
		Artists        []Artist        `json:"artists"`
		ProviderErrors []ProviderError `json:"providerErrors"`
	}
	path := "/api/v1/artists/search?q=" + urlQuery(query) + limitQuery(limit)
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, nil, err
	}
	return out.Artists, out.ProviderErrors, nil
}

// Artist fetches one artist with their albums.
func (c *Client) Artist(ctx context.Context, artistID string) (*Artist, error) {
	var out Artist
	if err := c.do(ctx, http.MethodGet, "/api/v1/artists/"+artistID, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SyncArtist records an artist's provider pages and pulls their albums.
func (c *Client) SyncArtist(ctx context.Context, artistID string, opts SyncOptions) (*SyncResult, error) {
	var out SyncResult
	body := syncBody(opts)
	if err := c.do(ctx, http.MethodPost, "/api/v1/artists/"+artistID+"/sync", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PlaylistFromAlbum stores an album's tracklist as a new playlist.
func (c *Client) PlaylistFromAlbum(ctx context.Context, albumID, name string) (*PlaylistDetail, error) {
	album, err := c.Album(ctx, albumID)
	if err != nil {
		return nil, err
	}
	if len(album.Tracks) == 0 {
		return nil, fmt.Errorf("prismusic: album %q has no tracklist yet; sync it first", album.Title)
	}
	if name == "" {
		name = album.Title
	}
	playlist, err := c.CreatePlaylist(ctx, name)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(album.Tracks))
	for _, track := range album.Tracks {
		ids = append(ids, track.ID)
	}
	return c.AddPlaylistTracks(ctx, playlist.ID, ids...)
}

func syncBody(opts SyncOptions) map[string]any {
	body := map[string]any{}
	if len(opts.Providers) > 0 {
		body["providers"] = opts.Providers
	}
	if opts.Resolve {
		body["resolve"] = true
	}
	if opts.SyncAlbums {
		body["syncAlbums"] = true
	}
	return body
}

func limitQuery(limit int) string {
	if limit <= 0 {
		return ""
	}
	return fmt.Sprintf("&limit=%d", limit)
}
