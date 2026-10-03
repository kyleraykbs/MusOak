package store

import (
	"context"
	"encoding/json"
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

// schemaV17 counts an account's icon changes, so a client can tell one picture
// from the next without asking the server what the file's bytes are.
const schemaV17 = `
ALTER TABLE users ADD COLUMN icon_version INTEGER NOT NULL DEFAULT 0;
`

// schemaV16 remembers which platforms an account wants a search to ask by
// default. The value is a JSON array of provider names; the empty string means
// the account never chose, so the client's own default applies.
const schemaV16 = `
ALTER TABLE users ADD COLUMN search_platforms TEXT NOT NULL DEFAULT '';
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

// SetIcon records a new picture and counts the change.
//
// The count is the only thing that can say the picture changed: the URL never
// does, because the file is filed under the path it is served from. Comparing
// the URL would therefore never see a difference, which is exactly the bug this
// method exists to avoid.
func (d *DB) SetIcon(ctx context.Context, id uuid.UUID, iconURL string) error {
	res, err := d.db.ExecContext(ctx,
		`UPDATE users SET icon_url = ?, icon_version = icon_version + 1 WHERE id = ?`,
		iconURL, id.String())
	if err != nil {
		return mapErr(err)
	}
	return rowsAffectedOrNotFound(res)
}

// SetSearchPlatforms replaces the platforms a search asks by default. An empty
// list clears the preference back to "not set".
func (d *DB) SetSearchPlatforms(ctx context.Context, id uuid.UUID, platforms []string) error {
	raw, err := marshalSearchPlatforms(platforms)
	if err != nil {
		return err
	}
	res, err := d.db.ExecContext(ctx,
		`UPDATE users SET search_platforms = ? WHERE id = ?`, raw, id.String())
	if err != nil {
		return mapErr(err)
	}
	return rowsAffectedOrNotFound(res)
}

// marshalSearchPlatforms encodes the stored preference. An absent preference
// and an explicitly empty one are both the empty string.
func marshalSearchPlatforms(platforms []string) (string, error) {
	if len(platforms) == 0 {
		return "", nil
	}
	raw, err := json.Marshal(platforms)
	if err != nil {
		return "", err
	}
	return string(raw), nil
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
