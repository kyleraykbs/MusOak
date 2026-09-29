package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// txLike is the *sql.Tx surface playlists need; they read as well as write.
type txLike interface {
	execer
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// schemaV4 adds user playlists: an ordered list of canonical tracks.
const schemaV4 = `
CREATE TABLE playlists (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE INDEX playlists_user_id ON playlists(user_id);

CREATE TABLE playlist_items (
    playlist_id TEXT NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
    position    INTEGER NOT NULL,
    track_id    TEXT NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
    added_at    INTEGER NOT NULL,
    PRIMARY KEY (playlist_id, position)
);

CREATE INDEX playlist_items_track_id ON playlist_items(track_id);
`

// Playlist is one user's ordered list of tracks.
type Playlist struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	Name      string
	CreatedAt time.Time
	UpdatedAt time.Time
	// TrackCount is filled in by PlaylistsForUser, so a listing needs no
	// second query per playlist.
	TrackCount int
}

// PlaylistItem is one entry of a playlist.
type PlaylistItem struct {
	Position int
	TrackID  uuid.UUID
	AddedAt  time.Time
}

// PlaylistRepo stores user playlists.
type PlaylistRepo interface {
	CreatePlaylist(ctx context.Context, p *Playlist) error
	// Playlist returns a playlist; ErrNotFound covers both "missing" and
	// "belongs to somebody else".
	Playlist(ctx context.Context, id uuid.UUID) (*Playlist, error)
	PlaylistsForUser(ctx context.Context, userID uuid.UUID) ([]Playlist, error)
	RenamePlaylist(ctx context.Context, id uuid.UUID, name string) error
	DeletePlaylist(ctx context.Context, id uuid.UUID) error

	PlaylistItems(ctx context.Context, playlistID uuid.UUID) ([]PlaylistItem, error)
	AppendPlaylistItems(ctx context.Context, playlistID uuid.UUID, trackIDs []uuid.UUID) error
	RemovePlaylistItem(ctx context.Context, playlistID uuid.UUID, position int) error
	// ReorderPlaylist rearranges the existing entries; order must be a
	// permutation of the current positions.
	ReorderPlaylist(ctx context.Context, playlistID uuid.UUID, order []int) error
}

// CreatePlaylist inserts p, generating ID and timestamps when unset.
func (d *DB) CreatePlaylist(ctx context.Context, p *Playlist) error {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now()
	}
	p.UpdatedAt = p.CreatedAt

	_, err := d.db.ExecContext(ctx,
		`INSERT INTO playlists (id, user_id, name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		p.ID.String(), p.UserID.String(), p.Name, p.CreatedAt.UnixMilli(), p.UpdatedAt.UnixMilli())
	return mapErr(err)
}

// Playlist returns the playlist by id.
func (d *DB) Playlist(ctx context.Context, id uuid.UUID) (*Playlist, error) {
	var (
		playlist         Playlist
		idStr, userID    string
		created, updated int64
	)
	err := d.db.QueryRowContext(ctx, `
		SELECT id, user_id, name, created_at, updated_at FROM playlists WHERE id = ?`, id.String()).
		Scan(&idStr, &userID, &playlist.Name, &created, &updated)
	if err != nil {
		return nil, mapErr(err)
	}
	if playlist.ID, err = parseUUID(idStr); err != nil {
		return nil, err
	}
	if playlist.UserID, err = parseUUID(userID); err != nil {
		return nil, err
	}
	playlist.CreatedAt = time.UnixMilli(created).UTC()
	playlist.UpdatedAt = time.UnixMilli(updated).UTC()
	return &playlist, nil
}

// PlaylistsForUser lists a user's playlists, newest first, with track counts.
func (d *DB) PlaylistsForUser(ctx context.Context, userID uuid.UUID) ([]Playlist, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT p.id, p.user_id, p.name, p.created_at, p.updated_at,
		       (SELECT COUNT(*) FROM playlist_items i WHERE i.playlist_id = p.id)
		FROM playlists p
		WHERE p.user_id = ?
		ORDER BY p.created_at DESC`, userID.String())
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var playlists []Playlist
	for rows.Next() {
		var (
			playlist         Playlist
			idStr, userIDStr string
			created, updated int64
		)
		if err := rows.Scan(&idStr, &userIDStr, &playlist.Name, &created, &updated, &playlist.TrackCount); err != nil {
			return nil, mapErr(err)
		}
		var err2 error
		if playlist.ID, err2 = parseUUID(idStr); err2 != nil {
			return nil, err2
		}
		if playlist.UserID, err2 = parseUUID(userIDStr); err2 != nil {
			return nil, err2
		}
		playlist.CreatedAt = time.UnixMilli(created).UTC()
		playlist.UpdatedAt = time.UnixMilli(updated).UTC()
		playlists = append(playlists, playlist)
	}
	return playlists, mapErr(rows.Err())
}

// RenamePlaylist changes a playlist's name.
func (d *DB) RenamePlaylist(ctx context.Context, id uuid.UUID, name string) error {
	res, err := d.db.ExecContext(ctx,
		`UPDATE playlists SET name = ?, updated_at = ? WHERE id = ?`,
		name, time.Now().UnixMilli(), id.String())
	if err != nil {
		return mapErr(err)
	}
	return rowsAffectedOrNotFound(res)
}

// DeletePlaylist removes a playlist and its entries.
func (d *DB) DeletePlaylist(ctx context.Context, id uuid.UUID) error {
	res, err := d.db.ExecContext(ctx, `DELETE FROM playlists WHERE id = ?`, id.String())
	if err != nil {
		return mapErr(err)
	}
	return rowsAffectedOrNotFound(res)
}

// PlaylistItems lists the entries in play order.
func (d *DB) PlaylistItems(ctx context.Context, playlistID uuid.UUID) ([]PlaylistItem, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT position, track_id, added_at FROM playlist_items
		WHERE playlist_id = ? ORDER BY position`, playlistID.String())
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var items []PlaylistItem
	for rows.Next() {
		var (
			item    PlaylistItem
			trackID string
			addedAt int64
		)
		if err := rows.Scan(&item.Position, &trackID, &addedAt); err != nil {
			return nil, mapErr(err)
		}
		var err2 error
		if item.TrackID, err2 = parseUUID(trackID); err2 != nil {
			return nil, err2
		}
		item.AddedAt = time.UnixMilli(addedAt).UTC()
		items = append(items, item)
	}
	return items, mapErr(rows.Err())
}

// AppendPlaylistItems adds tracks to the end of a playlist.
func (d *DB) AppendPlaylistItems(ctx context.Context, playlistID uuid.UUID, trackIDs []uuid.UUID) error {
	if len(trackIDs) == 0 {
		return nil
	}
	return d.withTx(ctx, func(tx *sql.Tx) error {
		if err := ensurePlaylistExists(ctx, tx, playlistID); err != nil {
			return err
		}
		var next int
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(position) + 1, 0) FROM playlist_items WHERE playlist_id = ?`,
			playlistID.String()).Scan(&next); err != nil {
			return mapErr(err)
		}
		now := time.Now().UnixMilli()
		for i, trackID := range trackIDs {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO playlist_items (playlist_id, position, track_id, added_at) VALUES (?, ?, ?, ?)`,
				playlistID.String(), next+i, trackID.String(), now); err != nil {
				return mapErr(err)
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE playlists SET updated_at = ? WHERE id = ?`, now, playlistID.String())
		return mapErr(err)
	})
}

// RemovePlaylistItem drops one entry and closes the gap.
func (d *DB) RemovePlaylistItem(ctx context.Context, playlistID uuid.UUID, position int) error {
	return d.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM playlist_items WHERE playlist_id = ? AND position = ?`, playlistID.String(), position)
		if err != nil {
			return mapErr(err)
		}
		if err := rowsAffectedOrNotFound(res); err != nil {
			return err
		}
		// Renumber the tail so positions stay dense and ordered.
		rows, err := tx.QueryContext(ctx,
			`SELECT position FROM playlist_items WHERE playlist_id = ? ORDER BY position`, playlistID.String())
		if err != nil {
			return mapErr(err)
		}
		var positions []int
		for rows.Next() {
			var p int
			if err := rows.Scan(&p); err != nil {
				rows.Close()
				return mapErr(err)
			}
			positions = append(positions, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return mapErr(err)
		}
		return renumber(ctx, tx, playlistID, positions)
	})
}

// ReorderPlaylist applies a new order to the existing entries.
func (d *DB) ReorderPlaylist(ctx context.Context, playlistID uuid.UUID, order []int) error {
	return d.withTx(ctx, func(tx *sql.Tx) error {
		if err := ensurePlaylistExists(ctx, tx, playlistID); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT position FROM playlist_items WHERE playlist_id = ? ORDER BY position`, playlistID.String())
		if err != nil {
			return mapErr(err)
		}
		var current []int
		for rows.Next() {
			var p int
			if err := rows.Scan(&p); err != nil {
				rows.Close()
				return mapErr(err)
			}
			current = append(current, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return mapErr(err)
		}

		if len(order) != len(current) {
			return fmt.Errorf("%w: order lists %d entries, the playlist has %d",
				ErrConflict, len(order), len(current))
		}
		seen := make(map[int]bool, len(order))
		for _, position := range order {
			if seen[position] {
				return fmt.Errorf("%w: position %d appears twice", ErrConflict, position)
			}
			seen[position] = true
			found := false
			for _, existing := range current {
				if existing == position {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("%w: no entry at position %d", ErrNotFound, position)
			}
		}

		// Park the entries out of the way first: positions are the primary key.
		if _, err := tx.ExecContext(ctx,
			`UPDATE playlist_items SET position = -position - 1 WHERE playlist_id = ?`, playlistID.String()); err != nil {
			return mapErr(err)
		}
		for newPosition, position := range order {
			if _, err := tx.ExecContext(ctx,
				`UPDATE playlist_items SET position = ? WHERE playlist_id = ? AND position = ?`,
				newPosition, playlistID.String(), -position-1); err != nil {
				return mapErr(err)
			}
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE playlists SET updated_at = ? WHERE id = ?`, time.Now().UnixMilli(), playlistID.String())
		return mapErr(err)
	})
}

// renumber rewrites positions to 0..n-1 in their current order.
func renumber(ctx context.Context, tx txLike, playlistID uuid.UUID, positions []int) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE playlist_items SET position = -position - 1 WHERE playlist_id = ?`, playlistID.String()); err != nil {
		return mapErr(err)
	}
	for newPosition, position := range positions {
		if _, err := tx.ExecContext(ctx,
			`UPDATE playlist_items SET position = ? WHERE playlist_id = ? AND position = ?`,
			newPosition, playlistID.String(), -position-1); err != nil {
			return mapErr(err)
		}
	}
	_, err := tx.ExecContext(ctx,
		`UPDATE playlists SET updated_at = ? WHERE id = ?`, time.Now().UnixMilli(), playlistID.String())
	return mapErr(err)
}

func ensurePlaylistExists(ctx context.Context, q execer, id uuid.UUID) error {
	var count int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM playlists WHERE id = ?`, id.String()).Scan(&count); err != nil {
		return mapErr(err)
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}
