package store

import (
	"context"
	"database/sql"

	"github.com/google/uuid"
)

// Merging two canonical rows for the same recording.
//
// A library can end up holding one song twice: a provider's metadata arrives
// spelled differently enough that it does not match the row already there, so a
// second canonical track is made. What makes that hurt is the rendition - it is
// attached to one of the two, and a playlist entry or a favorite can point at
// the other, which then has nothing to play.
//
// Merging moves everything that pointed at the row being dropped onto the one
// being kept. The kept row keeps its own title and length; the other's
// renditions, credits, playlist entries and favorites move over, and anything
// that would then exist twice is dropped rather than duplicated.

// MergeTracks joins drop into keep and removes drop.
func (d *DB) MergeTracks(ctx context.Context, keep, drop uuid.UUID) error {
	if keep == drop {
		return nil
	}
	return d.withTx(ctx, func(tx *sql.Tx) error {
		for _, id := range []uuid.UUID{keep, drop} {
			if err := ensureTrackExists(ctx, tx, id); err != nil {
				return err
			}
		}

		// Entries that are numbered within a parent (a playlist's order, an
		// album's tracklist) cannot simply move: the parent may already hold
		// the kept track, and two entries cannot share a position.
		for _, table := range []string{"playlist_items", "external_playlist_tracks"} {
			if err := mergeOrdered(ctx, tx, table, keep, drop); err != nil {
				return err
			}
		}

		// Everything else points at the row rather than numbering it inside a
		// parent. Where the same thing would exist twice - a favorite of both,
		// the same credit on both - the move is skipped and the duplicate goes
		// with the row being dropped.
		for _, table := range []string{
			"variants", "track_artists", "track_albums",
			"favorites", "user_track_preference", "shares", "notifications", "votes",
		} {
			if _, err := tx.ExecContext(ctx,
				`UPDATE OR IGNORE `+table+` SET track_id = ? WHERE track_id = ?`,
				keep.String(), drop.String()); err != nil {
				return mapErr(err)
			}
		}

		if _, err := tx.ExecContext(ctx, `DELETE FROM tracks WHERE id = ?`, drop.String()); err != nil {
			return mapErr(err)
		}
		return nil
	})
}

// mergeOrdered moves a track's entries in a parent-ordered table. A parent that
// already holds the kept track is holding the same song twice, so the entry
// goes; the rest change hands, and the parent is renumbered because two sets of
// positions have just become one.
func mergeOrdered(ctx context.Context, tx *sql.Tx, table string, keep, drop uuid.UUID) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT DISTINCT playlist_id FROM `+table+` WHERE track_id = ?`, drop.String())
	if err != nil {
		return mapErr(err)
	}
	var parents []string
	for rows.Next() {
		var parent string
		if err := rows.Scan(&parent); err != nil {
			rows.Close()
			return mapErr(err)
		}
		parents = append(parents, parent)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return mapErr(err)
	}

	for _, parent := range parents {
		var held int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM `+table+` WHERE playlist_id = ? AND track_id = ?`,
			parent, keep.String()).Scan(&held); err != nil {
			return mapErr(err)
		}
		if held > 0 {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM `+table+` WHERE playlist_id = ? AND track_id = ?`,
				parent, drop.String()); err != nil {
				return mapErr(err)
			}
		} else if _, err := tx.ExecContext(ctx,
			`UPDATE `+table+` SET track_id = ? WHERE playlist_id = ? AND track_id = ?`,
			keep.String(), parent, drop.String()); err != nil {
			return mapErr(err)
		}
		if err := renumberTable(ctx, tx, table, parent); err != nil {
			return err
		}
	}
	return nil
}

// renumberTable rewrites a parent's entry positions to 0..n-1 in their current
// order. Positions are half of those tables' primary keys, so they are parked
// out of the way before being written back.
func renumberTable(ctx context.Context, tx *sql.Tx, table, parent string) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT position FROM `+table+` WHERE playlist_id = ? ORDER BY position`, parent)
	if err != nil {
		return mapErr(err)
	}
	var positions []int
	for rows.Next() {
		var position int
		if err := rows.Scan(&position); err != nil {
			rows.Close()
			return mapErr(err)
		}
		positions = append(positions, position)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return mapErr(err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE `+table+` SET position = -position - 1 WHERE playlist_id = ?`, parent); err != nil {
		return mapErr(err)
	}
	for index, position := range positions {
		if _, err := tx.ExecContext(ctx,
			`UPDATE `+table+` SET position = ? WHERE playlist_id = ? AND position = ?`,
			index, parent, -position-1); err != nil {
			return mapErr(err)
		}
	}
	return nil
}
