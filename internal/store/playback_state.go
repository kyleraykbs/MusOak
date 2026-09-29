package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// schemaV9 keeps where a user was: the queue, the song, the timestamp — as a
// document the server stores but does not interpret, so the shape is the
// client's to decide.
const schemaV9 = `
CREATE TABLE playback_state (
    user_id    TEXT PRIMARY KEY,
    state      TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);
`

// PlaybackStateRepo stores one document per user: what they were last playing.
type PlaybackStateRepo interface {
	// SetPlaybackState records the caller's playback state, replacing the last.
	SetPlaybackState(ctx context.Context, userID uuid.UUID, state []byte) error
	// PlaybackState returns the stored state, or ErrNotFound when there is none.
	PlaybackState(ctx context.Context, userID uuid.UUID) ([]byte, error)
}

var _ PlaybackStateRepo = (*DB)(nil)

// SetPlaybackState records what a user was last playing.
func (d *DB) SetPlaybackState(ctx context.Context, userID uuid.UUID, state []byte) error {
	if userID == uuid.Nil {
		return errors.New("store: playback state needs a user")
	}
	if len(state) == 0 {
		return errors.New("store: empty playback state")
	}
	if !json.Valid(state) {
		return errors.New("store: playback state is not JSON")
	}
	_, err := d.execRetry(ctx, `
		INSERT INTO playback_state (user_id, state, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT (user_id) DO UPDATE SET
			state = excluded.state,
			updated_at = excluded.updated_at`,
		userID.String(), string(state), time.Now().UnixMilli())
	return mapErr(err)
}

// PlaybackState returns what a user was last playing.
func (d *DB) PlaybackState(ctx context.Context, userID uuid.UUID) ([]byte, error) {
	var state string
	err := d.db.QueryRowContext(ctx,
		`SELECT state FROM playback_state WHERE user_id = ?`, userID.String()).Scan(&state)
	if err != nil {
		return nil, mapErr(err)
	}
	return []byte(state), nil
}
