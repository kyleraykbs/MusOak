package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// schemaV11 is the Sources feature: one vote per user per variant, and the
// variant a user prefers for a track.
//
// Votes are shared: they reshape the default source ordering for everyone, so
// they hang off the variant itself (1 up, -1 down, and a withdrawn vote is
// simply deleted). The preference is the opposite: purely personal, one row
// per user and track, and it wins over the vote ordering when that user plays
// the song. Both are filtered by variant_id or track_id, hence the indexes.
const schemaV11 = `
CREATE TABLE variant_votes (
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    variant_id TEXT NOT NULL REFERENCES variants(id) ON DELETE CASCADE,
    value      INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (user_id, variant_id)
);

CREATE TABLE user_track_preference (
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    track_id   TEXT NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
    variant_id TEXT NOT NULL REFERENCES variants(id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, track_id)
);

CREATE INDEX variant_votes_variant_id ON variant_votes(variant_id);
CREATE INDEX user_track_preference_track_id ON user_track_preference(track_id);
CREATE INDEX user_track_preference_variant_id ON user_track_preference(variant_id);
`

// VariantVoteCount is the tally of one variant's up and down votes.
type VariantVoteCount struct {
	Up   int
	Down int
}

// VariantUploader is who uploaded a user-sourced variant.
type VariantUploader struct {
	UserID      uuid.UUID
	Username    string
	DisplayName string
}

// SetVariantVote records a user's vote for a variant: 1 up, -1 down, and 0
// withdraws it by deleting the row. Voting again replaces the earlier value,
// so there is only ever one vote per user per variant.
func (d *DB) SetVariantVote(ctx context.Context, userID, variantID uuid.UUID, value int) error {
	switch value {
	case 1, -1:
		_, err := d.db.ExecContext(ctx, `
			INSERT INTO variant_votes (user_id, variant_id, value, updated_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (user_id, variant_id) DO UPDATE SET
				value = excluded.value,
				updated_at = excluded.updated_at`,
			userID.String(), variantID.String(), value, time.Now().UnixMilli())
		return mapErr(err)
	case 0:
		// Withdrawing is idempotent: a vote that is already gone is fine.
		_, err := d.db.ExecContext(ctx,
			`DELETE FROM variant_votes WHERE user_id = ? AND variant_id = ?`,
			userID.String(), variantID.String())
		return mapErr(err)
	default:
		return fmt.Errorf("store: vote value %d must be -1, 0 or 1", value)
	}
}

// VariantVoteCounts tallies up and down votes per variant for a track's
// variants; variants nobody voted on are absent from the map.
func (d *DB) VariantVoteCounts(ctx context.Context, trackID uuid.UUID) (map[uuid.UUID]VariantVoteCount, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT vv.variant_id,
			SUM(CASE WHEN vv.value > 0 THEN 1 ELSE 0 END),
			SUM(CASE WHEN vv.value < 0 THEN 1 ELSE 0 END)
		FROM variant_votes vv
		JOIN variants v ON v.id = vv.variant_id
		WHERE v.track_id = ?
		GROUP BY vv.variant_id`, trackID.String())
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	counts := make(map[uuid.UUID]VariantVoteCount)
	for rows.Next() {
		var (
			id    string
			count VariantVoteCount
		)
		if err := rows.Scan(&id, &count.Up, &count.Down); err != nil {
			return nil, mapErr(err)
		}
		variantID, err := parseUUID(id)
		if err != nil {
			return nil, err
		}
		counts[variantID] = count
	}
	return counts, mapErr(rows.Err())
}

// UserVariantVotes returns one user's vote per variant of a track (1 or -1);
// variants they have not voted on are absent from the map.
func (d *DB) UserVariantVotes(ctx context.Context, userID, trackID uuid.UUID) (map[uuid.UUID]int, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT vv.variant_id, vv.value
		FROM variant_votes vv
		JOIN variants v ON v.id = vv.variant_id
		WHERE vv.user_id = ? AND v.track_id = ?`,
		userID.String(), trackID.String())
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	votes := make(map[uuid.UUID]int)
	for rows.Next() {
		var (
			id    string
			value int
		)
		if err := rows.Scan(&id, &value); err != nil {
			return nil, mapErr(err)
		}
		variantID, err := parseUUID(id)
		if err != nil {
			return nil, err
		}
		votes[variantID] = value
	}
	return votes, mapErr(rows.Err())
}

// SetTrackPreference saves the variant a user prefers to play for a track,
// replacing any earlier choice.
func (d *DB) SetTrackPreference(ctx context.Context, userID, trackID, variantID uuid.UUID) error {
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO user_track_preference (user_id, track_id, variant_id)
		VALUES (?, ?, ?)
		ON CONFLICT (user_id, track_id) DO UPDATE SET variant_id = excluded.variant_id`,
		userID.String(), trackID.String(), variantID.String())
	return mapErr(err)
}

// TrackPreference returns the variant a user prefers for a track, or
// uuid.Nil when they have not chosen one.
func (d *DB) TrackPreference(ctx context.Context, userID, trackID uuid.UUID) (uuid.UUID, error) {
	var variantID string
	err := d.db.QueryRowContext(ctx,
		`SELECT variant_id FROM user_track_preference WHERE user_id = ? AND track_id = ?`,
		userID.String(), trackID.String()).Scan(&variantID)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, nil
	}
	if err != nil {
		return uuid.Nil, mapErr(err)
	}
	return parseUUID(variantID)
}

// VariantUploaders reports who uploaded each user-sourced variant of a track.
// Provider variants, and uploads whose uploader account is gone, are absent
// from the map.
func (d *DB) VariantUploaders(ctx context.Context, trackID uuid.UUID) (map[uuid.UUID]VariantUploader, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT v.id, u.id, u.username, u.display_name
		FROM variants v
		JOIN users u ON u.id = v.uploader_user_id
		WHERE v.track_id = ?`, trackID.String())
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	uploaders := make(map[uuid.UUID]VariantUploader)
	for rows.Next() {
		var (
			id, userID string
			uploader   VariantUploader
		)
		if err := rows.Scan(&id, &userID, &uploader.Username, &uploader.DisplayName); err != nil {
			return nil, mapErr(err)
		}
		variantID, err := parseUUID(id)
		if err != nil {
			return nil, err
		}
		if uploader.UserID, err = parseUUID(userID); err != nil {
			return nil, err
		}
		uploaders[variantID] = uploader
	}
	return uploaders, mapErr(rows.Err())
}
