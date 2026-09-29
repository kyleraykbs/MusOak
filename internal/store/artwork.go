package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

// schemaV6 records artwork per provider rendition and, canonically, on the
// entity itself. The canonical column is what listings read, so showing a cover
// never needs a join.
const schemaV6 = `
ALTER TABLE tracks ADD COLUMN artwork_url TEXT NOT NULL DEFAULT '';
ALTER TABLE albums ADD COLUMN artwork_url TEXT NOT NULL DEFAULT '';
ALTER TABLE artists ADD COLUMN artwork_url TEXT NOT NULL DEFAULT '';

ALTER TABLE variants ADD COLUMN artwork_url TEXT NOT NULL DEFAULT '';
ALTER TABLE album_variants ADD COLUMN artwork_url TEXT NOT NULL DEFAULT '';
ALTER TABLE artist_variants ADD COLUMN artwork_url TEXT NOT NULL DEFAULT '';
`

// ArtworkKind is which entity an image belongs to.
type ArtworkKind string

// The kinds of artwork the library holds.
const (
	ArtworkTrack    ArtworkKind = "track"
	ArtworkAlbum    ArtworkKind = "album"
	ArtworkArtist   ArtworkKind = "artist"
	ArtworkPlaylist ArtworkKind = "playlist"
)

// Valid reports whether kind is one of the known kinds.
func (k ArtworkKind) Valid() bool {
	switch k {
	case ArtworkTrack, ArtworkAlbum, ArtworkArtist, ArtworkPlaylist:
		return true
	}
	return false
}

// ArtworkRepo reads and writes artwork locations.
type ArtworkRepo interface {
	// SetTrackArtwork records an image for a track when it has none, or when
	// the caller says this one is better.
	SetTrackArtwork(ctx context.Context, id uuid.UUID, url string) error
	SetAlbumArtwork(ctx context.Context, id uuid.UUID, url string) error
	SetArtistArtwork(ctx context.Context, id uuid.UUID, url string) error
	// EntityArtwork returns the recorded image location for an entity.
	EntityArtwork(ctx context.Context, kind ArtworkKind, id uuid.UUID) (string, error)
	// The per-rendition setters keep what each provider reported, which is
	// what a refresh or an alternative cover reads.
	SetVariantArtwork(ctx context.Context, variantID uuid.UUID, url string) error
	SetAlbumVariantArtwork(ctx context.Context, variantID uuid.UUID, url string) error
	SetArtistVariantArtwork(ctx context.Context, variantID uuid.UUID, url string) error
	// ArtworkSources lists the images the providers gave, in provider order.
	ArtworkSources(ctx context.Context, kind ArtworkKind, id uuid.UUID, order []string) ([]ArtworkSource, error)
}

func (d *DB) setArtwork(ctx context.Context, table string, id uuid.UUID, url string) error {
	if url == "" {
		return nil
	}
	// The first provider to offer an image wins; only an empty slot is filled.
	res, err := d.db.ExecContext(ctx,
		`UPDATE `+table+` SET artwork_url = ? WHERE id = ? AND artwork_url = ''`, url, id.String())
	if err != nil {
		return mapErr(err)
	}
	if _, err := res.RowsAffected(); err != nil {
		return err
	}
	return nil
}

// SetTrackArtwork records a track's image if it has none.
func (d *DB) SetTrackArtwork(ctx context.Context, id uuid.UUID, url string) error {
	return d.setArtwork(ctx, "tracks", id, url)
}

// SetAlbumArtwork records an album's cover if it has none.
func (d *DB) SetAlbumArtwork(ctx context.Context, id uuid.UUID, url string) error {
	return d.setArtwork(ctx, "albums", id, url)
}

// SetArtistArtwork records an artist's portrait if they have none.
func (d *DB) SetArtistArtwork(ctx context.Context, id uuid.UUID, url string) error {
	return d.setArtwork(ctx, "artists", id, url)
}

// TrackArtwork returns the recorded image location for a track.
func (d *DB) TrackArtwork(ctx context.Context, id uuid.UUID) (string, error) {
	return d.artworkOf(ctx, "tracks", id)
}

// AlbumArtwork returns the recorded cover location for an album.
func (d *DB) AlbumArtwork(ctx context.Context, id uuid.UUID) (string, error) {
	return d.artworkOf(ctx, "albums", id)
}

// ArtistArtwork returns the recorded portrait location for an artist.
func (d *DB) ArtistArtwork(ctx context.Context, id uuid.UUID) (string, error) {
	return d.artworkOf(ctx, "artists", id)
}

// ExternalPlaylistArtwork returns the recorded image for a provider playlist.
func (d *DB) ExternalPlaylistArtwork(ctx context.Context, id uuid.UUID) (string, error) {
	return d.artworkOf(ctx, "external_playlists", id)
}

// EntityArtwork returns the recorded image location for any entity.
func (d *DB) EntityArtwork(ctx context.Context, kind ArtworkKind, id uuid.UUID) (string, error) {
	switch kind {
	case ArtworkTrack:
		return d.TrackArtwork(ctx, id)
	case ArtworkAlbum:
		return d.AlbumArtwork(ctx, id)
	case ArtworkArtist:
		return d.ArtistArtwork(ctx, id)
	case ArtworkPlaylist:
		// A provider playlist first, then one of our own: the ids are UUIDs
		// from different tables, so only one of them can match.
		url, err := d.ExternalPlaylistArtwork(ctx, id)
		if err == nil {
			return url, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return "", err
		}
		return d.artworkOf(ctx, "playlists", id)
	}
	return "", ErrNotFound
}

func (d *DB) artworkOf(ctx context.Context, table string, id uuid.UUID) (string, error) {
	var url string
	if err := d.db.QueryRowContext(ctx,
		`SELECT artwork_url FROM `+table+` WHERE id = ?`, id.String()).Scan(&url); err != nil {
		return "", mapErr(err)
	}
	if url == "" {
		return "", ErrNotFound
	}
	return url, nil
}

// SetVariantArtwork stores the image a provider reported for its rendition,
// which is what a refresh would read.
func (d *DB) SetVariantArtwork(ctx context.Context, variantID uuid.UUID, url string) error {
	if url == "" {
		return nil
	}
	_, err := d.db.ExecContext(ctx,
		`UPDATE variants SET artwork_url = ? WHERE id = ?`, url, variantID.String())
	return mapErr(err)
}

// SetAlbumVariantArtwork stores a provider release's cover.
func (d *DB) SetAlbumVariantArtwork(ctx context.Context, variantID uuid.UUID, url string) error {
	if url == "" {
		return nil
	}
	_, err := d.db.ExecContext(ctx,
		`UPDATE album_variants SET artwork_url = ? WHERE id = ?`, url, variantID.String())
	return mapErr(err)
}

// SetArtistVariantArtwork stores a provider page's portrait.
func (d *DB) SetArtistVariantArtwork(ctx context.Context, variantID uuid.UUID, url string) error {
	if url == "" {
		return nil
	}
	_, err := d.db.ExecContext(ctx,
		`UPDATE artist_variants SET artwork_url = ? WHERE id = ?`, url, variantID.String())
	return mapErr(err)
}

// ArtworkSource is a provider image location with the provider it came from.
type ArtworkSource struct {
	Provider string
	URL      string
}

// ArtworkSources lists every image location the library knows for an entity, in
// the given provider order. It is how a caller picks the cover of its choice
// when the canonical one is missing or unwanted.
func (d *DB) ArtworkSources(ctx context.Context, kind ArtworkKind, id uuid.UUID, order []string) ([]ArtworkSource, error) {
	var query string
	switch kind {
	case ArtworkTrack:
		query = `SELECT provider, artwork_url FROM variants WHERE track_id = ? AND artwork_url <> ''`
	case ArtworkAlbum:
		query = `SELECT provider, artwork_url FROM album_variants WHERE album_id = ? AND artwork_url <> ''`
	case ArtworkArtist:
		query = `SELECT provider, artwork_url FROM artist_variants WHERE artist_id = ? AND artwork_url <> ''`
	case ArtworkPlaylist:
		query = `SELECT provider, artwork_url FROM external_playlists WHERE id = ? AND artwork_url <> ''`
	default:
		return nil, ErrNotFound
	}

	rows, err := d.db.QueryContext(ctx, query, id.String())
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var sources []ArtworkSource
	for rows.Next() {
		var source ArtworkSource
		if err := rows.Scan(&source.Provider, &source.URL); err != nil {
			return nil, mapErr(err)
		}
		sources = append(sources, source)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err)
	}
	return sortByProviderOrder(sources, order), nil
}

// sortByProviderOrder orders images by the caller's provider ranking, leaving
// unknown providers in their original order at the end.
func sortByProviderOrder(sources []ArtworkSource, order []string) []ArtworkSource {
	if len(order) == 0 {
		return sources
	}
	rank := make(map[string]int, len(order))
	for i, name := range order {
		rank[name] = i
	}
	out := append([]ArtworkSource(nil), sources...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			left, leftKnown := rank[out[j-1].Provider]
			right, rightKnown := rank[out[j].Provider]
			if !leftKnown {
				left = len(order)
			}
			if !rightKnown {
				right = len(order)
			}
			if left <= right {
				break
			}
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

var _ ArtworkRepo = (*DB)(nil)

var _ = sql.ErrNoRows
var _ = time.Now
