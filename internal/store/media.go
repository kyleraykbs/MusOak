package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"path/filepath"
	"strings"
)

// schemaV3 records when a rendition was last served, which is what the LRU
// eviction of the media cache is ordered by.
const schemaV3 = `
ALTER TABLE media_files ADD COLUMN accessed_at INTEGER NOT NULL DEFAULT 0;
UPDATE media_files SET accessed_at = downloaded_at;
`

// mediaColumns is the column list every media_files query shares.
// schemaV20 stores media paths relative to the storage directory. A file that
// lives in the media directory is recorded as "media/<name>": the database then
// describes the library rather than the disk it happened to sit on, and moving
// the directory moves the files with it. Paths that are already relative - and
// ones outside the media directory - are left alone.
const schemaV20 = `
UPDATE media_files
   SET path = 'media/' || substr(path, instr(path, '/media/') + 7)
 WHERE path LIKE '/%' AND instr(path, '/media/') > 0;
`

const mediaColumns = `variant_id, path, sha256, duration_ms, bytes, downloaded_at, accessed_at`

// pathIn is a media path as it is stored: relative to the storage directory,
// so the directory can move without the database pointing at where it was. A
// path outside the directory is not ours to rewrite, and is kept as it is.
func (d *DB) pathIn(path string) string {
	if d.root == "" || path == "" || !filepath.IsAbs(path) {
		return path
	}
	rel, err := filepath.Rel(d.root, path)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return path
	}
	return rel
}

// pathOut is a stored path as the filesystem needs it: relative paths are
// joined to the storage directory. An absolute path - one written before paths
// were relative, or one that lives outside the directory - is returned as it is.
func (d *DB) pathOut(path string) string {
	if d.root == "" || path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(d.root, path)
}

func (d *DB) scanMediaFile(row rowScanner) (*MediaFile, error) {
	var (
		m          MediaFile
		variantID  string
		downloaded int64
		accessed   int64
	)
	if err := row.Scan(&variantID, &m.Path, &m.SHA256, &m.DurationMs, &m.Bytes, &downloaded, &accessed); err != nil {
		return nil, mapErr(err)
	}
	var err error
	if m.VariantID, err = parseUUID(variantID); err != nil {
		return nil, err
	}
	m.DownloadedAt = time.UnixMilli(downloaded).UTC()
	m.AccessedAt = time.UnixMilli(accessed).UTC()
	m.Path = d.pathOut(m.Path)
	return &m, nil
}

// UpsertMediaFile records (or refreshes) the finished file for a variant.
func (d *DB) UpsertMediaFile(ctx context.Context, m *MediaFile) error {
	if m.DownloadedAt.IsZero() {
		m.DownloadedAt = time.Now()
	}
	if m.AccessedAt.IsZero() {
		m.AccessedAt = m.DownloadedAt
	}
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO media_files (variant_id, path, sha256, duration_ms, bytes, downloaded_at, accessed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (variant_id) DO UPDATE SET
			path = excluded.path,
			sha256 = excluded.sha256,
			duration_ms = excluded.duration_ms,
			bytes = excluded.bytes,
			downloaded_at = excluded.downloaded_at,
			accessed_at = excluded.accessed_at`,
		m.VariantID.String(), d.pathIn(m.Path), m.SHA256, m.DurationMs, m.Bytes,
		m.DownloadedAt.UnixMilli(), m.AccessedAt.UnixMilli())
	return mapErr(err)
}

// MediaFile returns the file record for a variant.
func (d *DB) MediaFile(ctx context.Context, variantID uuid.UUID) (*MediaFile, error) {
	row := d.db.QueryRowContext(ctx,
		`SELECT `+mediaColumns+` FROM media_files WHERE variant_id = ?`, variantID.String())
	return d.scanMediaFile(row)
}

// MediaFileBySHA256 finds a finished file by content hash; it is how imports
// recognise content they already have.
func (d *DB) MediaFileBySHA256(ctx context.Context, sha256 string) (*MediaFile, error) {
	row := d.db.QueryRowContext(ctx,
		`SELECT `+mediaColumns+` FROM media_files WHERE sha256 = ? LIMIT 1`, sha256)
	return d.scanMediaFile(row)
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

// MediaFiles lists every finished file, oldest download first.
func (d *DB) MediaFiles(ctx context.Context) ([]MediaFile, error) {
	return d.queryMediaFiles(ctx, `SELECT `+mediaColumns+` FROM media_files ORDER BY downloaded_at`)
}

// MediaFilesByLastUse lists the files an eviction may remove, least recently
// served first: that is the order it removes them in. A rendition fetched from
// a provider can be fetched again; one that came off this disk - an upload, a
// file from the local library - is somebody's only copy and is never listed.
func (d *DB) MediaFilesByLastUse(ctx context.Context) ([]MediaFile, error) {
	return d.queryMediaFiles(ctx,
		`SELECT `+mediaColumns+` FROM media_files
		 WHERE variant_id IN (SELECT id FROM variants WHERE provider NOT IN (?, ?))
		 ORDER BY accessed_at, downloaded_at`, LocalProvider, UploadProvider)
}

func (d *DB) queryMediaFiles(ctx context.Context, query string, args ...any) ([]MediaFile, error) {
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var files []MediaFile
	for rows.Next() {
		file, err := d.scanMediaFile(rows)
		if err != nil {
			return nil, err
		}
		files = append(files, *file)
	}
	return files, mapErr(rows.Err())
}

// TouchMediaFile records a serve, so eviction keeps what is actually played.
func (d *DB) TouchMediaFile(ctx context.Context, variantID uuid.UUID, at time.Time) error {
	if at.IsZero() {
		at = time.Now()
	}
	res, err := d.db.ExecContext(ctx,
		`UPDATE media_files SET accessed_at = ? WHERE variant_id = ?`, at.UnixMilli(), variantID.String())
	if err != nil {
		return mapErr(err)
	}
	return rowsAffectedOrNotFound(res)
}

// MediaBytes is the size of the media cache: the renditions eviction may
// remove, which is what the quota is about. A file that came off this disk is
// not part of the cache and is not counted against it.
func (d *DB) MediaBytes(ctx context.Context) (int64, error) {
	var total *int64
	if err := d.db.QueryRowContext(ctx, `SELECT SUM(bytes) FROM media_files
		WHERE variant_id IN (SELECT id FROM variants WHERE provider NOT IN (?, ?))`,
		LocalProvider, UploadProvider).Scan(&total); err != nil {
		return 0, mapErr(err)
	}
	if total == nil {
		return 0, nil
	}
	return *total, nil
}
