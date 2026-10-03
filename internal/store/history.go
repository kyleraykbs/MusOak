package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// schemaV18 is what a person listened to. A play is recorded when a track stops
// being played, carrying how much of it was heard: a skipped song and one that
// ran to its end are different listens, and only the length tells them apart.
const schemaV18 = `
CREATE TABLE IF NOT EXISTS play_history (
    id            TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL,
    track_id      TEXT NOT NULL,
    variant_id    TEXT NOT NULL DEFAULT '',
    started_at_ms INTEGER NOT NULL,
    played_ms     INTEGER NOT NULL DEFAULT 0,
    source        TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS play_history_user_started ON play_history (user_id, started_at_ms DESC);
CREATE INDEX IF NOT EXISTS play_history_user_track ON play_history (user_id, track_id);
`

// PlayRecord is one listen.
type PlayRecord struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	TrackID   uuid.UUID
	VariantID uuid.UUID
	StartedAt time.Time
	PlayedMs  int64
	// Source is where the listen came from: "web", "cli", "room".
	Source string
}

// PlayCount is how often somebody played a track, and when they last did.
type PlayCount struct {
	TrackID      uuid.UUID
	Plays        int
	LastPlayedAt time.Time
}

// HistoryRepo is what listening leaves behind.
type HistoryRepo interface {
	// RecordPlay stores one listen.
	RecordPlay(ctx context.Context, p *PlayRecord) error
	// History lists a user's listens, newest first.
	History(ctx context.Context, userID uuid.UUID, limit int) ([]PlayRecord, error)
	// TopTracks lists a user's most played, most played first.
	TopTracks(ctx context.Context, userID uuid.UUID, limit int) ([]PlayCount, error)
	// PlayCounts is how many times a user played each of these tracks, for the
	// lists that want to show it beside the title.
	PlayCounts(ctx context.Context, userID uuid.UUID, trackIDs []uuid.UUID) (map[uuid.UUID]PlayCount, error)
}

var _ HistoryRepo = (*DB)(nil)

// playHistoryLimit bounds a listing: a caller that asks for nothing gets a
// useful default, and one that asks for everything cannot.
func playHistoryLimit(limit int) int {
	switch {
	case limit <= 0:
		return 50
	case limit > 200:
		return 200
	}
	return limit
}

// RecordPlay stores one listen.
func (d *DB) RecordPlay(ctx context.Context, p *PlayRecord) error {
	if p == nil {
		return errors.New("store: nil play")
	}
	if p.UserID == uuid.Nil {
		return errors.New("store: play needs a user")
	}
	if p.TrackID == uuid.Nil {
		return errors.New("store: play needs a track")
	}
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	if p.StartedAt.IsZero() {
		p.StartedAt = time.Now()
	}
	// An empty variant is the default rendition, stored as the column default.
	variant := ""
	if p.VariantID != uuid.Nil {
		variant = p.VariantID.String()
	}
	_, err := d.execRetry(ctx, `
		INSERT INTO play_history (id, user_id, track_id, variant_id, started_at_ms, played_ms, source)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		p.ID.String(), p.UserID.String(), p.TrackID.String(), variant,
		p.StartedAt.UnixMilli(), p.PlayedMs, p.Source)
	return mapErr(err)
}

// History lists a user's listens, newest first.
func (d *DB) History(ctx context.Context, userID uuid.UUID, limit int) ([]PlayRecord, error) {
	if userID == uuid.Nil {
		return nil, errors.New("store: history needs a user")
	}
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, user_id, track_id, variant_id, started_at_ms, played_ms, source
		FROM play_history
		WHERE user_id = ?
		ORDER BY started_at_ms DESC, rowid DESC
		LIMIT ?`, userID.String(), playHistoryLimit(limit))
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	out := make([]PlayRecord, 0)
	for rows.Next() {
		record, err := scanPlay(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err)
	}
	return out, nil
}

// TopTracks lists a user's most played, most played first. Ties break towards
// the one played most recently, which is the more useful second look.
func (d *DB) TopTracks(ctx context.Context, userID uuid.UUID, limit int) ([]PlayCount, error) {
	if userID == uuid.Nil {
		return nil, errors.New("store: top tracks need a user")
	}
	rows, err := d.db.QueryContext(ctx, `
		SELECT track_id, COUNT(*) AS plays, MAX(started_at_ms) AS last_played_ms
		FROM play_history
		WHERE user_id = ?
		GROUP BY track_id
		ORDER BY plays DESC, last_played_ms DESC, track_id ASC
		LIMIT ?`, userID.String(), playHistoryLimit(limit))
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	out := make([]PlayCount, 0)
	for rows.Next() {
		var (
			trackID  string
			plays    int
			lastPlay int64
		)
		if err := rows.Scan(&trackID, &plays, &lastPlay); err != nil {
			return nil, mapErr(err)
		}
		id, err := parseUUID(trackID)
		if err != nil {
			return nil, err
		}
		out = append(out, PlayCount{
			TrackID:      id,
			Plays:        plays,
			LastPlayedAt: time.UnixMilli(lastPlay),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err)
	}
	return out, nil
}

// PlayCounts is how many times a user played each of these tracks. Tracks with
// no plays are absent from the result rather than present as zero.
func (d *DB) PlayCounts(ctx context.Context, userID uuid.UUID, trackIDs []uuid.UUID) (map[uuid.UUID]PlayCount, error) {
	out := make(map[uuid.UUID]PlayCount, len(trackIDs))
	if userID == uuid.Nil || len(trackIDs) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(trackIDs)+1)
	args = append(args, userID.String())
	seen := make(map[uuid.UUID]bool, len(trackIDs))
	for _, id := range trackIDs {
		if id == uuid.Nil || seen[id] {
			continue
		}
		seen[id] = true
		args = append(args, id.String())
	}
	if len(args) == 1 {
		return out, nil
	}

	rows, err := d.db.QueryContext(ctx, `
		SELECT track_id, COUNT(*) AS plays, MAX(started_at_ms) AS last_played_ms
		FROM play_history
		WHERE user_id = ? AND track_id IN (`+placeholders(len(args)-1)+`)
		GROUP BY track_id`, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			trackID  string
			plays    int
			lastPlay int64
		)
		if err := rows.Scan(&trackID, &plays, &lastPlay); err != nil {
			return nil, mapErr(err)
		}
		id, err := parseUUID(trackID)
		if err != nil {
			return nil, err
		}
		out[id] = PlayCount{
			TrackID:      id,
			Plays:        plays,
			LastPlayedAt: time.UnixMilli(lastPlay),
		}
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err)
	}
	return out, nil
}

// scanPlay reads one play_history row. The caller supplies the column list;
// every row scans the same seven columns.
func scanPlay(scan func(dest ...any) error) (PlayRecord, error) {
	var (
		p                                      PlayRecord
		id, userID, trackID, variantID, source string
		startedMs, playedMs                    int64
	)
	if err := scan(&id, &userID, &trackID, &variantID, &startedMs, &playedMs, &source); err != nil {
		return PlayRecord{}, mapErr(err)
	}
	var err error
	if p.ID, err = parseUUID(id); err != nil {
		return PlayRecord{}, err
	}
	if p.UserID, err = parseUUID(userID); err != nil {
		return PlayRecord{}, err
	}
	if p.TrackID, err = parseUUID(trackID); err != nil {
		return PlayRecord{}, err
	}
	if variantID != "" {
		if p.VariantID, err = parseUUID(variantID); err != nil {
			return PlayRecord{}, err
		}
	}
	p.StartedAt = time.UnixMilli(startedMs)
	p.PlayedMs = playedMs
	p.Source = source
	return p, nil
}
