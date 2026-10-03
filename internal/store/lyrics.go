package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// schemaV19 caches one song's words per rendition. Per rendition, not per
// track: two copies of a song can be different lengths, and synced timings
// belong to the copy they were written against.
const schemaV19 = `
CREATE TABLE IF NOT EXISTS lyrics (
    track_id      TEXT NOT NULL,
    variant_id    TEXT NOT NULL DEFAULT '',
    duration_ms   INTEGER NOT NULL DEFAULT 0,
    synced_json   TEXT NOT NULL DEFAULT '',
    plain         TEXT NOT NULL DEFAULT '',
    source        TEXT NOT NULL DEFAULT '',
    fetched_at_ms INTEGER NOT NULL,
    PRIMARY KEY (track_id, variant_id)
);
`

// LyricLine is one line and the moment it belongs to, in the rendition's own
// clock. AtMs is 0 for a source that has words but no timings.
type LyricLine struct {
	AtMs int64  `json:"atMs"`
	Text string `json:"text"`
}

// Lyrics are a song's words as one rendition has them.
type Lyrics struct {
	TrackID    uuid.UUID
	VariantID  uuid.UUID
	DurationMs int64
	Synced     []LyricLine
	Plain      string
	// Source names where the words came from, so a client can say so.
	Source    string
	FetchedAt time.Time
}

// LyricsRepo caches lyrics. Nothing here fetches: the service decides when a
// lookup is worth making, and the store only remembers what it found.
type LyricsRepo interface {
	Lyrics(ctx context.Context, trackID, variantID uuid.UUID) (*Lyrics, error)
	SaveLyrics(ctx context.Context, l *Lyrics) error
}

// Lyrics returns the words cached for one rendition, ErrNotFound when none are.
//
// A caller that named no rendition passes uuid.Nil, and the lookup files that
// under the zero id: the words are then the track's own, not any copy's.
func (d *DB) Lyrics(ctx context.Context, trackID, variantID uuid.UUID) (*Lyrics, error) {
	row := d.db.QueryRowContext(ctx, `
		SELECT duration_ms, synced_json, plain, source, fetched_at_ms
		FROM lyrics
		WHERE track_id = ? AND variant_id = ?`, trackID.String(), variantID.String())

	var (
		durationMs int64
		syncedJSON string
		plain      string
		source     string
		fetchedMs  int64
	)
	if err := row.Scan(&durationMs, &syncedJSON, &plain, &source, &fetchedMs); err != nil {
		return nil, mapErr(err)
	}

	lyrics := &Lyrics{
		TrackID:    trackID,
		VariantID:  variantID,
		DurationMs: durationMs,
		Plain:      plain,
		Source:     source,
		FetchedAt:  time.UnixMilli(fetchedMs),
	}
	if syncedJSON != "" {
		if err := json.Unmarshal([]byte(syncedJSON), &lyrics.Synced); err != nil {
			// A row we cannot read is a row we cannot trust; the service will
			// fetch a fresh answer rather than hand back half of one.
			return nil, err
		}
	}
	return lyrics, nil
}

// SaveLyrics remembers a rendition's words, replacing any earlier answer. An
// instrumental is a real answer with no words, so it overwrites a previous
// lookup that found something rather than being skipped.
func (d *DB) SaveLyrics(ctx context.Context, l *Lyrics) error {
	syncedJSON := ""
	if len(l.Synced) > 0 {
		raw, err := json.Marshal(l.Synced)
		if err != nil {
			return err
		}
		syncedJSON = string(raw)
	}
	fetchedMs := l.FetchedAt.UnixMilli()
	if l.FetchedAt.IsZero() {
		fetchedMs = time.Now().UnixMilli()
	}
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO lyrics (track_id, variant_id, duration_ms, synced_json, plain, source, fetched_at_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(track_id, variant_id) DO UPDATE SET
			duration_ms   = excluded.duration_ms,
			synced_json   = excluded.synced_json,
			plain         = excluded.plain,
			source        = excluded.source,
			fetched_at_ms = excluded.fetched_at_ms`,
		l.TrackID.String(), l.VariantID.String(), l.DurationMs, syncedJSON, l.Plain, l.Source, fetchedMs)
	return mapErr(err)
}
