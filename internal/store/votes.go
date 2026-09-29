package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// schemaV2 adds room votes: one score per member per track per room, kept for
// later statistics.
const schemaV2 = `
CREATE TABLE votes (
    room_id    TEXT NOT NULL,
    track_id   TEXT NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
    member_id  TEXT NOT NULL,
    user_id    TEXT REFERENCES users(id) ON DELETE SET NULL,
    score      INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (room_id, track_id, member_id)
);

CREATE INDEX votes_track_id ON votes(track_id);
`

// SaveVote records a member's score, replacing any earlier vote for the same
// track in the same room (votes can be changed while the track plays).
func (d *DB) SaveVote(ctx context.Context, v *Vote) error {
	if v.CreatedAt.IsZero() {
		v.CreatedAt = time.Now()
	}
	var userID any
	if v.UserID != nil {
		userID = v.UserID.String()
	}
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO votes (room_id, track_id, member_id, user_id, score, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (room_id, track_id, member_id) DO UPDATE SET
			user_id = excluded.user_id,
			score = excluded.score,
			created_at = excluded.created_at`,
		v.RoomID, v.TrackID.String(), v.MemberID, userID, v.Score, v.CreatedAt.UnixMilli())
	return mapErr(err)
}

// TrackVotes lists the votes a track received in one room.
func (d *DB) TrackVotes(ctx context.Context, roomID string, trackID uuid.UUID) ([]Vote, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT room_id, track_id, member_id, user_id, score, created_at
		FROM votes WHERE room_id = ? AND track_id = ?
		ORDER BY created_at`, roomID, trackID.String())
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var votes []Vote
	for rows.Next() {
		var (
			v         Vote
			trackID   string
			userID    *string
			createdAt int64
		)
		if err := rows.Scan(&v.RoomID, &trackID, &v.MemberID, &userID, &v.Score, &createdAt); err != nil {
			return nil, mapErr(err)
		}
		var err2 error
		if v.TrackID, err2 = parseUUID(trackID); err2 != nil {
			return nil, err2
		}
		if userID != nil && *userID != "" {
			parsed, err := parseUUID(*userID)
			if err != nil {
				return nil, err
			}
			v.UserID = &parsed
		}
		v.CreatedAt = time.UnixMilli(createdAt).UTC()
		votes = append(votes, v)
	}
	return votes, mapErr(rows.Err())
}

// TrackVoteStats returns how often a track was voted on and its mean score.
func (d *DB) TrackVoteStats(ctx context.Context, trackID uuid.UUID) (int, float64, error) {
	var (
		count int
		mean  *float64
	)
	err := d.db.QueryRowContext(ctx, `
		SELECT COUNT(*), AVG(score) FROM votes WHERE track_id = ?`, trackID.String()).
		Scan(&count, &mean)
	if err != nil {
		return 0, 0, mapErr(err)
	}
	if mean == nil {
		return 0, 0, nil
	}
	return count, *mean, nil
}
