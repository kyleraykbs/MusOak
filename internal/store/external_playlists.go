package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

// schemaV7 adds external playlists: a playlist that lives on a provider, stored
// so the library can serve its cover and remember what it synced.
//
// Unlike an album, a playlist is never merged across providers: two services
// offering a playlist of the same name are different lists with different
// tracks, so the row is scoped to the provider that has it.
const schemaV7 = `
CREATE TABLE external_playlists (
    id                   TEXT PRIMARY KEY,
    provider             TEXT NOT NULL,
    provider_playlist_id TEXT NOT NULL,
    title                TEXT NOT NULL,
    owner                TEXT NOT NULL DEFAULT '',
    description          TEXT NOT NULL DEFAULT '',
    track_count          INTEGER NOT NULL DEFAULT 0,
    artwork_url          TEXT NOT NULL DEFAULT '',
    created_at           INTEGER NOT NULL,
    UNIQUE (provider, provider_playlist_id)
);

CREATE INDEX external_playlists_provider ON external_playlists(provider);

CREATE TABLE external_playlist_tracks (
    playlist_id TEXT NOT NULL REFERENCES external_playlists(id) ON DELETE CASCADE,
    position    INTEGER NOT NULL,
    track_id    TEXT NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
    PRIMARY KEY (playlist_id, position)
);

CREATE INDEX external_playlist_tracks_track_id ON external_playlist_tracks(track_id);
`

// ExternalPlaylist is a playlist that belongs to a provider.
type ExternalPlaylist struct {
	ID                 uuid.UUID
	Provider           string
	ProviderPlaylistID string
	Title              string
	Owner              string
	Description        string
	TrackCount         int
	ArtworkURL         string
	CreatedAt          time.Time
}

// ExternalPlaylistRepo stores provider playlists and what was synced from them.
type ExternalPlaylistRepo interface {
	// UpsertExternalPlaylist records a provider playlist, returning the stored
	// row and whether it was new. A later search refreshes what it knows
	// without losing the tracks already synced.
	UpsertExternalPlaylist(ctx context.Context, playlist *ExternalPlaylist) (*ExternalPlaylist, bool, error)
	// ExternalPlaylist reads one playlist by its id.
	ExternalPlaylist(ctx context.Context, id uuid.UUID) (*ExternalPlaylist, error)
	// SetExternalPlaylistTracks replaces the synced tracklist, in order, and
	// reports how many entries are new to the playlist.
	SetExternalPlaylistTracks(ctx context.Context, id uuid.UUID, trackIDs []uuid.UUID) (int, error)
	// ExternalPlaylistTracks lists the synced tracks in playlist order.
	ExternalPlaylistTracks(ctx context.Context, id uuid.UUID) ([]Track, error)
}

var _ ExternalPlaylistRepo = (*DB)(nil)

// UpsertExternalPlaylist records a provider playlist and keeps its tracklist.
func (d *DB) UpsertExternalPlaylist(
	ctx context.Context, playlist *ExternalPlaylist,
) (*ExternalPlaylist, bool, error) {
	if playlist == nil {
		return nil, false, errors.New("store: nil playlist")
	}
	if playlist.Provider == "" || playlist.ProviderPlaylistID == "" {
		return nil, false, errors.New("store: playlist needs a provider and a provider id")
	}

	existing, err := d.externalPlaylistByProviderID(ctx, playlist.Provider, playlist.ProviderPlaylistID)
	switch {
	case err == nil:
		// A search that finds what it already knows must not write: writing on
		// every search is what made a busy database fail a search.
		if !externalPlaylistChanged(existing, playlist) {
			return existing, false, nil
		}
		// Refresh the metadata, but keep the id: the synced tracks hang off it.
		if _, err := d.execRetry(ctx, `
			UPDATE external_playlists
			   SET title = ?, owner = ?, description = ?, track_count = ?,
			       artwork_url = CASE WHEN ? <> '' THEN ? ELSE artwork_url END
			 WHERE id = ?`,
			playlist.Title, playlist.Owner, playlist.Description, playlist.TrackCount,
			playlist.ArtworkURL, playlist.ArtworkURL, existing.ID.String()); err != nil {
			return nil, false, mapErr(err)
		}
		updated, err := d.ExternalPlaylist(ctx, existing.ID)
		return updated, false, err
	case !errors.Is(err, ErrNotFound):
		return nil, false, err
	}

	if playlist.ID == uuid.Nil {
		playlist.ID = uuid.New()
	}
	if playlist.CreatedAt.IsZero() {
		playlist.CreatedAt = time.Now()
	}
	if _, err := d.execRetry(ctx, `
		INSERT INTO external_playlists
			(id, provider, provider_playlist_id, title, owner, description,
			 track_count, artwork_url, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		playlist.ID.String(), playlist.Provider, playlist.ProviderPlaylistID,
		playlist.Title, playlist.Owner, playlist.Description, playlist.TrackCount,
		playlist.ArtworkURL, playlist.CreatedAt.UnixMilli()); err != nil {
		return nil, false, mapErr(err)
	}
	return playlist, true, nil
}

// externalPlaylistChanged reports whether a provider playlist carries anything
// new, so a search that found nothing new does not write.
func externalPlaylistChanged(stored, incoming *ExternalPlaylist) bool {
	if stored.Title != incoming.Title ||
		stored.Owner != incoming.Owner ||
		stored.Description != incoming.Description ||
		stored.TrackCount != incoming.TrackCount {
		return true
	}
	// The cover is only worth writing when we have one and it differs; an empty
	// one from a search must never wipe a cover a sync recorded.
	return incoming.ArtworkURL != "" && incoming.ArtworkURL != stored.ArtworkURL
}

// ExternalPlaylist reads one playlist by id.
func (d *DB) ExternalPlaylist(ctx context.Context, id uuid.UUID) (*ExternalPlaylist, error) {
	row := d.db.QueryRowContext(ctx, `
		SELECT id, provider, provider_playlist_id, title, owner, description,
		       track_count, artwork_url, created_at
		  FROM external_playlists WHERE id = ?`, id.String())
	return scanExternalPlaylist(row)
}

func (d *DB) externalPlaylistByProviderID(
	ctx context.Context, provider, providerPlaylistID string,
) (*ExternalPlaylist, error) {
	row := d.db.QueryRowContext(ctx, `
		SELECT id, provider, provider_playlist_id, title, owner, description,
		       track_count, artwork_url, created_at
		  FROM external_playlists WHERE provider = ? AND provider_playlist_id = ?`,
		provider, providerPlaylistID)
	return scanExternalPlaylist(row)
}

func scanExternalPlaylist(row interface {
	Scan(dest ...any) error
}) (*ExternalPlaylist, error) {
	var (
		playlist  ExternalPlaylist
		id        string
		createdAt int64
	)
	if err := row.Scan(
		&id, &playlist.Provider, &playlist.ProviderPlaylistID, &playlist.Title,
		&playlist.Owner, &playlist.Description, &playlist.TrackCount,
		&playlist.ArtworkURL, &createdAt,
	); err != nil {
		return nil, mapErr(err)
	}
	parsed, err := uuid.Parse(id)
	if err != nil {
		return nil, err
	}
	playlist.ID = parsed
	playlist.CreatedAt = time.UnixMilli(createdAt)
	return &playlist, nil
}

// SetExternalPlaylistTracks replaces the synced tracklist with the given order.
func (d *DB) SetExternalPlaylistTracks(
	ctx context.Context, id uuid.UUID, trackIDs []uuid.UUID,
) (int, error) {
	var added int
	err := d.withTx(ctx, func(tx *sql.Tx) error {
		var before int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM external_playlist_tracks WHERE playlist_id = ?`,
			id.String()).Scan(&before); err != nil {
			return mapErr(err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM external_playlist_tracks WHERE playlist_id = ?`, id.String()); err != nil {
			return mapErr(err)
		}
		for position, trackID := range trackIDs {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO external_playlist_tracks (playlist_id, position, track_id)
				VALUES (?, ?, ?)`,
				id.String(), position, trackID.String()); err != nil {
				return mapErr(err)
			}
		}
		added = len(trackIDs) - before
		if added < 0 {
			added = 0
		}
		_, err := tx.ExecContext(ctx,
			`UPDATE external_playlists SET track_count = ? WHERE id = ?`,
			len(trackIDs), id.String())
		return err
	})
	if err != nil {
		return 0, err
	}
	return added, nil
}

// ExternalPlaylistTracks lists the synced tracks in playlist order.
func (d *DB) ExternalPlaylistTracks(ctx context.Context, id uuid.UUID) ([]Track, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT t.id, t.title, t.duration_ms, t.created_at
		  FROM external_playlist_tracks ept
		  JOIN tracks t ON t.id = ept.track_id
		 WHERE ept.playlist_id = ?
		 ORDER BY ept.position`, id.String())
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var tracks []Track
	for rows.Next() {
		var (
			track     Track
			trackID   string
			createdAt int64
		)
		if err := rows.Scan(&trackID, &track.Title, &track.DurationMs, &createdAt); err != nil {
			return nil, mapErr(err)
		}
		parsed, err := uuid.Parse(trackID)
		if err != nil {
			return nil, err
		}
		track.ID = parsed
		track.CreatedAt = time.UnixMilli(createdAt)
		tracks = append(tracks, track)
	}
	return tracks, rows.Err()
}
