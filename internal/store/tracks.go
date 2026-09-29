package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/google/uuid"
)

// schemaV1 is the initial schema. Timestamps are unix milliseconds.
const schemaV1 = `
CREATE TABLE tracks (
    id          TEXT PRIMARY KEY,
    title       TEXT NOT NULL,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL
);

CREATE TABLE artists (
    id   TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE
);

CREATE TABLE albums (
    id    TEXT PRIMARY KEY,
    title TEXT NOT NULL UNIQUE
);

CREATE TABLE track_artists (
    track_id  TEXT NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
    artist_id TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    position  INTEGER NOT NULL,
    PRIMARY KEY (track_id, artist_id)
);

CREATE TABLE track_albums (
    track_id TEXT NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
    album_id TEXT NOT NULL REFERENCES albums(id) ON DELETE CASCADE,
    position INTEGER NOT NULL,
    PRIMARY KEY (track_id, album_id)
);

CREATE TABLE variants (
    id                TEXT PRIMARY KEY,
    track_id          TEXT NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
    provider          TEXT NOT NULL,
    provider_track_id TEXT NOT NULL,
    title             TEXT NOT NULL,
    artists_json      TEXT NOT NULL DEFAULT '[]',
    album             TEXT NOT NULL DEFAULT '',
    duration_ms       INTEGER NOT NULL DEFAULT 0,
    downloadable      INTEGER NOT NULL DEFAULT 0,
    isrc              TEXT NOT NULL DEFAULT '',
    created_at        INTEGER NOT NULL,
    UNIQUE (provider, provider_track_id)
);

CREATE INDEX variants_track_id ON variants(track_id);
CREATE INDEX variants_isrc ON variants(isrc) WHERE isrc <> '';

CREATE TABLE media_files (
    variant_id    TEXT PRIMARY KEY REFERENCES variants(id) ON DELETE CASCADE,
    path          TEXT NOT NULL,
    sha256        TEXT NOT NULL,
    duration_ms   INTEGER NOT NULL,
    bytes         INTEGER NOT NULL,
    downloaded_at INTEGER NOT NULL
);

CREATE TABLE users (
    id            TEXT PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    INTEGER NOT NULL
);

CREATE TABLE sessions (
    token_hash TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);

CREATE INDEX sessions_user_id ON sessions(user_id);

CREATE TABLE favorites (
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    track_id   TEXT NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (user_id, track_id)
);

CREATE TABLE provider_rankings (
    user_id  TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider TEXT NOT NULL,
    position INTEGER NOT NULL,
    PRIMARY KEY (user_id, provider)
);
`

// CreateTrack inserts t, generating ID and CreatedAt when unset.
func (d *DB) CreateTrack(ctx context.Context, t *Track) error {
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO tracks (id, title, duration_ms, created_at) VALUES (?, ?, ?, ?)`,
		t.ID.String(), t.Title, t.DurationMs, t.CreatedAt.UnixMilli())
	return mapErr(err)
}

// Track returns the track by id.
func (d *DB) Track(ctx context.Context, id uuid.UUID) (*Track, error) {
	var (
		t       Track
		idStr   string
		created int64
	)
	err := d.db.QueryRowContext(ctx,
		`SELECT id, title, duration_ms, created_at FROM tracks WHERE id = ?`, id.String()).
		Scan(&idStr, &t.Title, &t.DurationMs, &created)
	if err != nil {
		return nil, mapErr(err)
	}
	if t.ID, err = parseUUID(idStr); err != nil {
		return nil, err
	}
	t.CreatedAt = time.UnixMilli(created).UTC()
	return &t, nil
}

// SetTrackDuration stores the canonical duration, e.g. after media is probed.
func (d *DB) SetTrackDuration(ctx context.Context, id uuid.UUID, durationMs int64) error {
	res, err := d.db.ExecContext(ctx,
		`UPDATE tracks SET duration_ms = ? WHERE id = ?`, durationMs, id.String())
	if err != nil {
		return mapErr(err)
	}
	return rowsAffectedOrNotFound(res)
}

// EnsureArtist returns the id of name, creating it if needed.
func (d *DB) EnsureArtist(ctx context.Context, name string) (uuid.UUID, error) {
	return ensureArtist(ctx, d.db, name)
}

func ensureArtist(ctx context.Context, q execer, name string) (uuid.UUID, error) {
	if _, err := q.ExecContext(ctx,
		`INSERT OR IGNORE INTO artists (id, name) VALUES (?, ?)`, uuid.NewString(), name); err != nil {
		return uuid.Nil, mapErr(err)
	}
	var id string
	if err := q.QueryRowContext(ctx, `SELECT id FROM artists WHERE name = ?`, name).Scan(&id); err != nil {
		return uuid.Nil, mapErr(err)
	}
	return parseUUID(id)
}

// EnsureAlbum returns the id of title, creating it if needed.
func (d *DB) EnsureAlbum(ctx context.Context, title string) (uuid.UUID, error) {
	return ensureAlbum(ctx, d.db, title)
}

func ensureAlbum(ctx context.Context, q execer, title string) (uuid.UUID, error) {
	if _, err := q.ExecContext(ctx,
		`INSERT OR IGNORE INTO albums (id, title) VALUES (?, ?)`, uuid.NewString(), title); err != nil {
		return uuid.Nil, mapErr(err)
	}
	var id string
	if err := q.QueryRowContext(ctx, `SELECT id FROM albums WHERE title = ?`, title).Scan(&id); err != nil {
		return uuid.Nil, mapErr(err)
	}
	return parseUUID(id)
}

// TrackArtists lists a track's artists in credit order.
func (d *DB) TrackArtists(ctx context.Context, id uuid.UUID) ([]Artist, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT a.id, a.name
		FROM artists a
		JOIN track_artists ta ON ta.artist_id = a.id
		WHERE ta.track_id = ?
		ORDER BY ta.position`, id.String())
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var artists []Artist
	for rows.Next() {
		var (
			a     Artist
			idStr string
		)
		if err := rows.Scan(&idStr, &a.Name); err != nil {
			return nil, mapErr(err)
		}
		if a.ID, err = parseUUID(idStr); err != nil {
			return nil, err
		}
		artists = append(artists, a)
	}
	return artists, mapErr(rows.Err())
}

// TrackAlbums lists a track's albums in order.
func (d *DB) TrackAlbums(ctx context.Context, id uuid.UUID) ([]Album, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT al.id, al.title
		FROM albums al
		JOIN track_albums ta ON ta.album_id = al.id
		WHERE ta.track_id = ?
		ORDER BY ta.position`, id.String())
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var albums []Album
	for rows.Next() {
		var (
			a     Album
			idStr string
		)
		if err := rows.Scan(&idStr, &a.Title); err != nil {
			return nil, mapErr(err)
		}
		if a.ID, err = parseUUID(idStr); err != nil {
			return nil, err
		}
		albums = append(albums, a)
	}
	return albums, mapErr(rows.Err())
}

// SetTrackArtists replaces a track's artist credits.
func (d *DB) SetTrackArtists(ctx context.Context, id uuid.UUID, names []string) error {
	return d.withTx(ctx, func(tx *sql.Tx) error {
		if err := ensureTrackExists(ctx, tx, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM track_artists WHERE track_id = ?`, id.String()); err != nil {
			return mapErr(err)
		}
		for i, name := range names {
			artistID, err := ensureArtist(ctx, tx, name)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO track_artists (track_id, artist_id, position) VALUES (?, ?, ?)`,
				id.String(), artistID.String(), i); err != nil {
				return mapErr(err)
			}
		}
		return nil
	})
}

// SetTrackAlbums replaces a track's album credits.
func (d *DB) SetTrackAlbums(ctx context.Context, id uuid.UUID, titles []string) error {
	return d.withTx(ctx, func(tx *sql.Tx) error {
		if err := ensureTrackExists(ctx, tx, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM track_albums WHERE track_id = ?`, id.String()); err != nil {
			return mapErr(err)
		}
		for i, title := range titles {
			albumID, err := ensureAlbum(ctx, tx, title)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO track_albums (track_id, album_id, position) VALUES (?, ?, ?)`,
				id.String(), albumID.String(), i); err != nil {
				return mapErr(err)
			}
		}
		return nil
	})
}

func ensureTrackExists(ctx context.Context, q execer, id uuid.UUID) error {
	var count int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM tracks WHERE id = ?`, id.String()).Scan(&count); err != nil {
		return mapErr(err)
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func rowsAffectedOrNotFound(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
