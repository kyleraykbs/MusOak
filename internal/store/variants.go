package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

const variantColumns = `id, track_id, provider, provider_track_id, title, artists_json, album, duration_ms, downloadable, isrc, created_at`

type rowScanner interface {
	Scan(dest ...any) error
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
	if artistsJSON != "" {
		if err := json.Unmarshal([]byte(artistsJSON), &v.Artists); err != nil {
			return nil, fmt.Errorf("store: variant %s artists: %w", id, err)
		}
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
	artists := v.Artists
	if artists == nil {
		artists = []string{}
	}
	artistsJSON, err := json.Marshal(artists)
	if err != nil {
		return fmt.Errorf("store: marshal variant artists: %w", err)
	}
	_, err = d.db.ExecContext(ctx,
		`INSERT INTO variants (`+variantColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		v.ID.String(), v.TrackID.String(), v.Provider, v.ProviderTrackID, v.Title,
		string(artistsJSON), v.Album, v.DurationMs, boolInt(v.Downloadable), v.ISRC,
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

// UpsertMediaFile records (or refreshes) the finished file for a variant.
func (d *DB) UpsertMediaFile(ctx context.Context, m *MediaFile) error {
	if m.DownloadedAt.IsZero() {
		m.DownloadedAt = time.Now()
	}
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO media_files (variant_id, path, sha256, duration_ms, bytes, downloaded_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (variant_id) DO UPDATE SET
			path = excluded.path,
			sha256 = excluded.sha256,
			duration_ms = excluded.duration_ms,
			bytes = excluded.bytes,
			downloaded_at = excluded.downloaded_at`,
		m.VariantID.String(), m.Path, m.SHA256, m.DurationMs, m.Bytes, m.DownloadedAt.UnixMilli())
	return mapErr(err)
}

// MediaFile returns the file record for a variant.
func (d *DB) MediaFile(ctx context.Context, variantID uuid.UUID) (*MediaFile, error) {
	var (
		m        MediaFile
		variant  string
		downedAt int64
	)
	err := d.db.QueryRowContext(ctx, `
		SELECT variant_id, path, sha256, duration_ms, bytes, downloaded_at
		FROM media_files WHERE variant_id = ?`, variantID.String()).
		Scan(&variant, &m.Path, &m.SHA256, &m.DurationMs, &m.Bytes, &downedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	var err2 error
	if m.VariantID, err2 = parseUUID(variant); err2 != nil {
		return nil, err2
	}
	m.DownloadedAt = time.UnixMilli(downedAt).UTC()
	return &m, nil
}

// DeleteMediaFile forgets the file record for a variant.
func (d *DB) DeleteMediaFile(ctx context.Context, variantID uuid.UUID) error {
	res, err := d.db.ExecContext(ctx,
		`DELETE FROM media_files WHERE variant_id = ?`, variantID.String())
	if err != nil {
		return mapErr(err)
	}
	return rowsAffectedOrNotFound(res)
}

// MediaFiles lists every finished file, oldest download first (eviction order).
func (d *DB) MediaFiles(ctx context.Context) ([]MediaFile, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT variant_id, path, sha256, duration_ms, bytes, downloaded_at
		FROM media_files ORDER BY downloaded_at`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var files []MediaFile
	for rows.Next() {
		var (
			m        MediaFile
			variant  string
			downedAt int64
		)
		if err := rows.Scan(&variant, &m.Path, &m.SHA256, &m.DurationMs, &m.Bytes, &downedAt); err != nil {
			return nil, mapErr(err)
		}
		var err2 error
		if m.VariantID, err2 = parseUUID(variant); err2 != nil {
			return nil, err2
		}
		m.DownloadedAt = time.UnixMilli(downedAt).UTC()
		files = append(files, m)
	}
	return files, mapErr(rows.Err())
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
