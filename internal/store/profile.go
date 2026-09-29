package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// schemaV12 gives an account a face and remembers when it last played.
//
// DisplayName and IconURL are what other people see beside your songs;
// LastPlayedAt is the only signal "online" is derived from, so it is a plain
// timestamp rather than a connection table.
const schemaV12 = `
ALTER TABLE users ADD COLUMN display_name   TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN icon_url       TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN last_played_at INTEGER NOT NULL DEFAULT 0;
`

// UpdateProfile sets what the account shows about itself.
func (d *DB) UpdateProfile(ctx context.Context, id uuid.UUID, displayName, iconURL string) error {
	res, err := d.db.ExecContext(ctx,
		`UPDATE users SET display_name = ?, icon_url = ? WHERE id = ?`,
		displayName, iconURL, id.String())
	if err != nil {
		return mapErr(err)
	}
	return rowsAffectedOrNotFound(res)
}

// TouchUser records that this account played something just now.
func (d *DB) TouchUser(ctx context.Context, id uuid.UUID, at time.Time) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE users SET last_played_at = ? WHERE id = ?`, at.UnixMilli(), id.String())
	return mapErr(err)
}

// UpdateUsername renames the account. The caller checks the password.
func (d *DB) UpdateUsername(ctx context.Context, id uuid.UUID, username string) error {
	res, err := d.db.ExecContext(ctx,
		`UPDATE users SET username = ? WHERE id = ?`, username, id.String())
	if err != nil {
		return mapErr(err)
	}
	return rowsAffectedOrNotFound(res)
}

// UpdatePasswordHash replaces how this account proves itself.
func (d *DB) UpdatePasswordHash(ctx context.Context, id uuid.UUID, hash string) error {
	res, err := d.db.ExecContext(ctx,
		`UPDATE users SET password_hash = ? WHERE id = ?`, hash, id.String())
	if err != nil {
		return mapErr(err)
	}
	return rowsAffectedOrNotFound(res)
}

// DeleteOtherSessions revokes every session but the one kept by hash. A
// password change must strand stolen sessions without signing the caller out,
// and zero others to revoke is the normal case, not a missing row.
func (d *DB) DeleteOtherSessions(ctx context.Context, id uuid.UUID, keepTokenHash string) error {
	_, err := d.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE user_id = ? AND token_hash <> ?`, id.String(), keepTokenHash)
	return mapErr(err)
}
