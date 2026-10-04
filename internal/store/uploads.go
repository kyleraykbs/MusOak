package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
)

// The provider ids whose bytes live on this machine and nowhere else: the songs
// people uploaded, and the files of the on-disk library. Nothing can fetch
// either again, so a missing file is missing for good and neither is cache.
const (
	UploadProvider = "user"
	LocalProvider  = "local"
)

// schemaV10 adds user uploads: a song somebody uploaded, stored as a
// user-sourced variant whose bytes are a media file like any downloaded
// rendition. The variant carries its own title, artists and album — an
// association to a canonical track never overwrites them — and
// uploader_user_id names who put it there.
const schemaV10 = `
CREATE TABLE uploads (
    id         TEXT PRIMARY KEY,
    variant_id TEXT NOT NULL REFERENCES variants(id) ON DELETE CASCADE,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    filename   TEXT NOT NULL,
    created_at INTEGER NOT NULL
);

ALTER TABLE variants ADD COLUMN uploader_user_id TEXT;

CREATE INDEX uploads_user ON uploads(user_id);
`

// Upload is a user-sourced variant: the uploaded song, the rendition it is
// stored as and who uploaded it.
type Upload struct {
	ID        uuid.UUID
	VariantID uuid.UUID
	UserID    uuid.UUID
	Filename  string
	CreatedAt time.Time
	Variant   Variant
	Uploader  VariantUploader
	// Solo reports the upload is the only rendition of its track, which is
	// what "No Association" looks like in a listing: the canonical track is
	// its own. An upload that joined a song shares its track with the other
	// renditions sooner or later.
	Solo bool
}

// uploadColumns is the column list every upload query shares: the upload row,
// its variant and who uploaded it. The subquery is the Solo flag; every table
// here has a created_at, hence the qualified names.
const uploadColumns = `u.id, u.variant_id, u.user_id, u.filename, u.created_at,
	v.id, v.track_id, v.provider, v.provider_track_id, v.title, v.artists_json, v.album, v.duration_ms, v.downloadable, v.isrc, v.created_at,
	us.username, us.display_name,
	NOT EXISTS (SELECT 1 FROM variants s WHERE s.track_id = v.track_id AND s.id <> v.id)`

// uploadFrom is the join every upload query shares.
const uploadFrom = `FROM uploads u
	JOIN variants v ON v.id = u.variant_id
	JOIN users us ON us.id = u.user_id`

func scanUpload(row rowScanner) (*Upload, error) {
	var (
		u                     Upload
		id, variantID, userID string
		created, vCreated     int64
		vID, vTrackID         string
		artistsJSON           string
		downloadable, solo    int
	)
	if err := row.Scan(&id, &variantID, &userID, &u.Filename, &created,
		&vID, &vTrackID, &u.Variant.Provider, &u.Variant.ProviderTrackID,
		&u.Variant.Title, &artistsJSON, &u.Variant.Album, &u.Variant.DurationMs,
		&downloadable, &u.Variant.ISRC, &vCreated,
		&u.Uploader.Username, &u.Uploader.DisplayName, &solo); err != nil {
		return nil, mapErr(err)
	}
	var err error
	if u.ID, err = parseUUID(id); err != nil {
		return nil, err
	}
	if u.VariantID, err = parseUUID(variantID); err != nil {
		return nil, err
	}
	if u.UserID, err = parseUUID(userID); err != nil {
		return nil, err
	}
	if u.Variant.ID, err = parseUUID(vID); err != nil {
		return nil, err
	}
	if u.Variant.TrackID, err = parseUUID(vTrackID); err != nil {
		return nil, err
	}
	if artistsJSON != "" {
		if err := json.Unmarshal([]byte(artistsJSON), &u.Variant.Artists); err != nil {
			return nil, fmt.Errorf("store: upload %s artists: %w", id, err)
		}
	}
	u.Uploader.UserID = u.UserID
	u.CreatedAt = time.UnixMilli(created).UTC()
	u.Variant.CreatedAt = time.UnixMilli(vCreated).UTC()
	u.Variant.Downloadable = downloadable != 0
	u.Solo = solo != 0
	return &u, nil
}

// CreateUpload records a new upload: its rows and the media file holding the
// uploaded bytes, so the file is served exactly like a downloaded rendition.
// The variant's identity follows from the upload — provider "user", the upload
// id as its provider track id, the uploader's id on the row — and is wired up
// here rather than trusted from the caller. IDs and timestamps are generated
// when unset, as everywhere else.
func (d *DB) CreateUpload(ctx context.Context, u *Upload, v *Variant, m *MediaFile) error {
	if u.ID == uuid.Nil {
		u.ID = uuid.New()
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now()
	}
	if v.ID == uuid.Nil {
		v.ID = uuid.New()
	}
	if v.CreatedAt.IsZero() {
		v.CreatedAt = u.CreatedAt
	}
	u.VariantID = v.ID
	v.Provider = UploadProvider
	v.ProviderTrackID = u.ID.String()
	v.Downloadable = true
	v.ISRC = strings.ToUpper(strings.TrimSpace(v.ISRC))

	m.VariantID = v.ID
	if m.DownloadedAt.IsZero() {
		m.DownloadedAt = u.CreatedAt
	}
	if m.AccessedAt.IsZero() {
		m.AccessedAt = m.DownloadedAt
	}

	artists := v.Artists
	if artists == nil {
		artists = []string{}
	}
	artistsJSON, err := json.Marshal(artists)
	if err != nil {
		return fmt.Errorf("store: marshal variant artists: %w", err)
	}

	return d.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO variants (id, track_id, provider, provider_track_id, title, artists_json, album, duration_ms, downloadable, isrc, created_at, uploader_user_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			v.ID.String(), v.TrackID.String(), v.Provider, v.ProviderTrackID, v.Title,
			string(artistsJSON), v.Album, v.DurationMs, boolInt(v.Downloadable), v.ISRC,
			v.CreatedAt.UnixMilli(), u.UserID.String()); err != nil {
			return mapErr(err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO uploads (id, variant_id, user_id, filename, created_at) VALUES (?, ?, ?, ?, ?)`,
			u.ID.String(), v.ID.String(), u.UserID.String(), u.Filename, u.CreatedAt.UnixMilli()); err != nil {
			return mapErr(err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO media_files (variant_id, path, sha256, duration_ms, bytes, downloaded_at, accessed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			v.ID.String(), m.Path, m.SHA256, m.DurationMs, m.Bytes,
			m.DownloadedAt.UnixMilli(), m.AccessedAt.UnixMilli()); err != nil {
			return mapErr(err)
		}
		return nil
	})
}

// Upload returns the upload by id.
func (d *DB) Upload(ctx context.Context, id uuid.UUID) (*Upload, error) {
	row := d.db.QueryRowContext(ctx,
		`SELECT `+uploadColumns+` `+uploadFrom+` WHERE u.id = ?`, id.String())
	return scanUpload(row)
}

// UploadByVariant returns the upload a user-sourced variant belongs to.
func (d *DB) UploadByVariant(ctx context.Context, variantID uuid.UUID) (*Upload, error) {
	row := d.db.QueryRowContext(ctx,
		`SELECT `+uploadColumns+` `+uploadFrom+` WHERE u.variant_id = ?`, variantID.String())
	return scanUpload(row)
}

// UploadsForUser lists a user's own uploads, newest first.
func (d *DB) UploadsForUser(ctx context.Context, userID uuid.UUID) ([]Upload, error) {
	return d.queryUploads(ctx,
		`SELECT `+uploadColumns+` `+uploadFrom+` WHERE u.user_id = ? ORDER BY u.created_at DESC`,
		userID.String())
}

// AllUploads lists every user's uploads, newest first: uploaded songs are
// visible to every user.
func (d *DB) AllUploads(ctx context.Context) ([]Upload, error) {
	return d.queryUploads(ctx,
		`SELECT `+uploadColumns+` `+uploadFrom+` ORDER BY u.created_at DESC`)
}

// UploadGroup is a set of one user's uploads whose stored bytes are identical,
// keyed by the content hash they share.
type UploadGroup struct {
	SHA256  string
	Uploads []Upload
}

// DuplicateUploads groups a user's uploads by the content hash of the file each
// one holds, keeping only the hashes more than one upload shares. The create
// endpoint refuses bytes it already has, so this finds the copies stored before
// that check existed.
func (d *DB) DuplicateUploads(ctx context.Context, userID uuid.UUID) ([]UploadGroup, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT m.sha256
		FROM uploads u
		JOIN media_files m ON m.variant_id = u.variant_id
		WHERE u.user_id = ?
		GROUP BY m.sha256
		HAVING COUNT(*) > 1
		ORDER BY m.sha256`, userID.String())
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var hashes []string
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, mapErr(err)
		}
		hashes = append(hashes, hash)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err)
	}

	var groups []UploadGroup
	for _, hash := range hashes {
		uploads, err := d.queryUploads(ctx, `
			SELECT `+uploadColumns+` `+uploadFrom+`
			JOIN media_files m ON m.variant_id = u.variant_id
			WHERE u.user_id = ? AND m.sha256 = ?
			ORDER BY u.created_at DESC`, userID.String(), hash)
		if err != nil {
			return nil, err
		}
		groups = append(groups, UploadGroup{SHA256: hash, Uploads: uploads})
	}
	return groups, nil
}

// SearchUploads finds user uploads whose own metadata mentions the query,
// newest first. Search surfaces them above provider results.
func (d *DB) SearchUploads(ctx context.Context, query string, limit int) ([]Upload, error) {
	if limit <= 0 {
		limit = defaultUploadSearchLimit
	}
	pattern := "%" + escapeLike(strings.TrimSpace(query)) + "%"
	return d.queryUploads(ctx, `
		SELECT `+uploadColumns+` `+uploadFrom+`
		WHERE v.title LIKE ? ESCAPE '\' OR v.artists_json LIKE ? ESCAPE '\'
			OR v.album LIKE ? ESCAPE '\' OR u.filename LIKE ? ESCAPE '\'
		ORDER BY u.created_at DESC
		LIMIT ?`, pattern, pattern, pattern, pattern, limit)
}

// defaultUploadSearchLimit bounds a search whose caller gives no limit.
const defaultUploadSearchLimit = 20

// escapeLike neutralises the wildcard characters so a query matches literally.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func (d *DB) queryUploads(ctx context.Context, query string, args ...any) ([]Upload, error) {
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var uploads []Upload
	for rows.Next() {
		u, err := scanUpload(rows)
		if err != nil {
			return nil, err
		}
		uploads = append(uploads, *u)
	}
	return uploads, mapErr(rows.Err())
}

// SetUploadTrack re-points an upload's variant at a canonical track. The
// variant keeps its own title, artists and album: association only decides
// which song the upload is a source of.
func (d *DB) SetUploadTrack(ctx context.Context, id, trackID uuid.UUID) error {
	res, err := d.db.ExecContext(ctx, `
		UPDATE variants SET track_id = ?
		WHERE id = (SELECT variant_id FROM uploads WHERE id = ?)`,
		trackID.String(), id.String())
	if err != nil {
		return mapErr(err)
	}
	return rowsAffectedOrNotFound(res)
}

// DeleteUpload removes an upload: its rows and the file holding its bytes. The
// file goes first, so a failure can be retried into a clean state, where a
// deleted row would orphan the file forever.
func (d *DB) DeleteUpload(ctx context.Context, id uuid.UUID) error {
	var path string
	err := d.db.QueryRowContext(ctx, `
		SELECT COALESCE(m.path, '') FROM uploads u
		LEFT JOIN media_files m ON m.variant_id = u.variant_id
		WHERE u.id = ?`, id.String()).Scan(&path)
	if err != nil {
		return mapErr(err)
	}
	if path != "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("store: remove upload file: %w", err)
		}
	}
	return d.withTx(ctx, func(tx *sql.Tx) error {
		var variantID string
		if err := tx.QueryRowContext(ctx,
			`SELECT variant_id FROM uploads WHERE id = ?`, id.String()).Scan(&variantID); err != nil {
			return mapErr(err)
		}
		// The upload row goes first: the other two cascade off the variant,
		// and this is the one whose absence means "already deleted".
		res, err := tx.ExecContext(ctx, `DELETE FROM uploads WHERE id = ?`, id.String())
		if err != nil {
			return mapErr(err)
		}
		if err := rowsAffectedOrNotFound(res); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM media_files WHERE variant_id = ?`, variantID); err != nil {
			return mapErr(err)
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM variants WHERE id = ?`, variantID)
		return mapErr(err)
	})
}
