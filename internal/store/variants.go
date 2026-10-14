package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const variantColumns = `id, track_id, provider, provider_track_id, title, artists_json, album, duration_ms, downloadable, isrc, created_at`

type rowScanner interface {
	Scan(dest ...any) error
}

// marshalArtists renders a credit list for storage. label names the row it
// belongs to in the error, so the one helper serves variants, album variants
// and uploads without losing which of them failed.
func marshalArtists(artists []string, label string) (string, error) {
	if artists == nil {
		artists = []string{}
	}
	raw, err := json.Marshal(artists)
	if err != nil {
		return "", fmt.Errorf("store: marshal %s artists: %w", label, err)
	}
	return string(raw), nil
}

// unmarshalArtists reads a stored credit list; an empty column is no credits.
func unmarshalArtists(raw, label, id string, dest *[]string) error {
	if raw == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(raw), dest); err != nil {
		return fmt.Errorf("store: %s %s artists: %w", label, id, err)
	}
	return nil
}

func scanVariant(row rowScanner) (*Variant, error) {
	var (
		v            Variant
		id, trackID  string
		artistsJSON  string
		downloadable int
		created      int64
	)
	if err := row.Scan(&id, &trackID, &v.Provider, &v.ProviderTrackID, &v.Title,
		&artistsJSON, &v.Album, &v.DurationMs, &downloadable, &v.ISRC, &created); err != nil {
		return nil, mapErr(err)
	}
	var err error
	if v.ID, err = parseUUID(id); err != nil {
		return nil, err
	}
	if v.TrackID, err = parseUUID(trackID); err != nil {
		return nil, err
	}
	if err := unmarshalArtists(artistsJSON, "variant", id, &v.Artists); err != nil {
		return nil, err
	}
	v.Downloadable = downloadable != 0
	v.CreatedAt = time.UnixMilli(created).UTC()
	return &v, nil
}

// CreateVariant inserts v, generating ID and CreatedAt when unset.
func (d *DB) CreateVariant(ctx context.Context, v *Variant) error {
	if v.ID == uuid.Nil {
		v.ID = uuid.New()
	}
	if v.CreatedAt.IsZero() {
		v.CreatedAt = time.Now()
	}
	v.ISRC = strings.ToUpper(strings.TrimSpace(v.ISRC))
	artistsJSON, err := marshalArtists(v.Artists, "variant")
	if err != nil {
		return err
	}
	_, err = d.db.ExecContext(ctx,
		`INSERT INTO variants (`+variantColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		v.ID.String(), v.TrackID.String(), v.Provider, v.ProviderTrackID, v.Title,
		artistsJSON, v.Album, v.DurationMs, boolInt(v.Downloadable), v.ISRC,
		v.CreatedAt.UnixMilli())
	return mapErr(err)
}

// Variant returns the variant by id.
func (d *DB) Variant(ctx context.Context, id uuid.UUID) (*Variant, error) {
	row := d.db.QueryRowContext(ctx,
		`SELECT `+variantColumns+` FROM variants WHERE id = ?`, id.String())
	return scanVariant(row)
}

// VariantByProviderTrack returns the variant for a provider's own track id.
func (d *DB) VariantByProviderTrack(ctx context.Context, provider, providerTrackID string) (*Variant, error) {
	row := d.db.QueryRowContext(ctx,
		`SELECT `+variantColumns+` FROM variants WHERE provider = ? AND provider_track_id = ?`,
		provider, providerTrackID)
	return scanVariant(row)
}

// VariantsForTrack lists every rendition of a track, oldest provider first.
func (d *DB) VariantsForTrack(ctx context.Context, trackID uuid.UUID) ([]Variant, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+variantColumns+` FROM variants WHERE track_id = ? ORDER BY created_at, provider`,
		trackID.String())
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var variants []Variant
	for rows.Next() {
		v, err := scanVariant(rows)
		if err != nil {
			return nil, err
		}
		variants = append(variants, *v)
	}
	return variants, mapErr(rows.Err())
}

// SetVariantTrack re-points a variant at its canonical track.
func (d *DB) SetVariantTrack(ctx context.Context, variantID, trackID uuid.UUID) error {
	res, err := d.db.ExecContext(ctx,
		`UPDATE variants SET track_id = ? WHERE id = ?`, trackID.String(), variantID.String())
	if err != nil {
		return mapErr(err)
	}
	return rowsAffectedOrNotFound(res)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
