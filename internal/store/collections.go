package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// schemaV5 gives albums a real identity and adds the collection tables.
//
// Until now `albums` existed only as a track credit: a title, unique on its
// own. A canonical album needs to know whose album it is, so the table is
// rebuilt with (title_key, artist_key) as its identity. SQLite cannot drop a
// UNIQUE constraint in place, and the child table has to go first so the
// foreign keys stay satisfied, hence the copy/rename dance.
const schemaV5 = `
CREATE TABLE albums_new (
    id         TEXT PRIMARY KEY,
    title      TEXT NOT NULL,
    title_key  TEXT NOT NULL DEFAULT '',
    artist_key TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL DEFAULT 0
);

INSERT INTO albums_new (id, title, title_key)
SELECT id, title, lower(trim(title)) FROM albums;

CREATE TABLE track_albums_new (
    track_id TEXT NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
    album_id TEXT NOT NULL REFERENCES albums_new(id) ON DELETE CASCADE,
    position INTEGER NOT NULL,
    PRIMARY KEY (track_id, album_id)
);

INSERT INTO track_albums_new (track_id, album_id, position)
SELECT track_id, album_id, position FROM track_albums;

DROP TABLE track_albums;
DROP TABLE albums;

ALTER TABLE albums_new RENAME TO albums;
ALTER TABLE track_albums_new RENAME TO track_albums;

CREATE UNIQUE INDEX albums_identity ON albums(title_key, artist_key);
CREATE INDEX track_albums_album_id ON track_albums(album_id);

CREATE TABLE album_artists (
    album_id  TEXT NOT NULL REFERENCES albums(id) ON DELETE CASCADE,
    artist_id TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    position  INTEGER NOT NULL,
    PRIMARY KEY (album_id, artist_id)
);

CREATE TABLE album_variants (
    id                TEXT PRIMARY KEY,
    album_id          TEXT NOT NULL REFERENCES albums(id) ON DELETE CASCADE,
    provider          TEXT NOT NULL,
    provider_album_id TEXT NOT NULL,
    title             TEXT NOT NULL,
    artists_json      TEXT NOT NULL DEFAULT '[]',
    year              TEXT NOT NULL DEFAULT '',
    track_count       INTEGER NOT NULL DEFAULT 0,
    created_at        INTEGER NOT NULL,
    UNIQUE (provider, provider_album_id)
);

CREATE INDEX album_variants_album_id ON album_variants(album_id);

CREATE TABLE album_tracks (
    album_id TEXT NOT NULL REFERENCES albums(id) ON DELETE CASCADE,
    track_id TEXT NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
    position INTEGER NOT NULL,
    PRIMARY KEY (album_id, track_id)
);

CREATE TABLE artist_variants (
    id                 TEXT PRIMARY KEY,
    artist_id          TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    provider           TEXT NOT NULL,
    provider_artist_id TEXT NOT NULL,
    name               TEXT NOT NULL,
    created_at         INTEGER NOT NULL,
    UNIQUE (provider, provider_artist_id)
);

CREATE INDEX artist_variants_artist_id ON artist_variants(artist_id);
`

// schemaV15 gives an album one tracklist and one cover.
//
// The relationship lived in two tables: the track path wrote track_albums while
// an album sync wrote album_tracks, and AlbumTracks read the second one - so an
// artist's page showed an album's tracks only for the albums a sync had added,
// and every album a track created read as empty. Fold them together, drop the
// second, and give the albums that never got a cover the cover of their tracks.
const schemaV15 = `
INSERT OR IGNORE INTO track_albums (track_id, album_id, position)
SELECT track_id, album_id, position FROM album_tracks;

DROP TABLE album_tracks;

UPDATE albums
SET artwork_url = COALESCE((
    SELECT t.artwork_url FROM track_albums ta
    JOIN tracks t ON t.id = ta.track_id
    WHERE ta.album_id = albums.id AND t.artwork_url <> ''
    LIMIT 1
), artwork_url)
WHERE artwork_url = '';
`

// AlbumVariant is one provider's release of a canonical album.
type AlbumVariant struct {
	ID              uuid.UUID
	AlbumID         uuid.UUID
	Provider        string
	ProviderAlbumID string
	Title           string
	Artists         []string
	Year            string
	TrackCount      int
	CreatedAt       time.Time
}

// ArtistVariant is one provider's page for a canonical artist.
type ArtistVariant struct {
	ID               uuid.UUID
	ArtistID         uuid.UUID
	Provider         string
	ProviderArtistID string
	Name             string
	CreatedAt        time.Time
}

// ArtistWithAlbums is an artist together with the albums attached to it.
type ArtistWithAlbums struct {
	Artist Artist
	Albums []Album
}

// AlbumRepo stores canonical albums, their provider releases and their tracks.
type AlbumRepo interface {
	// EnsureAlbum returns the canonical album for a title and its artists,
	// creating it when needed. The first artist names the album's identity;
	// the rest are credits.
	EnsureAlbum(ctx context.Context, title string, artists []string) (uuid.UUID, error)
	Album(ctx context.Context, id uuid.UUID) (*Album, error)
	AlbumArtists(ctx context.Context, id uuid.UUID) ([]Artist, error)
	// SetAlbumArtists replaces an album's credits, adopting an identity when
	// the album had no artist yet.
	SetAlbumArtists(ctx context.Context, id uuid.UUID, artists []string) error
	AlbumVariants(ctx context.Context, id uuid.UUID) ([]AlbumVariant, error)
	// AttachAlbumVariant records a provider release; re-attaching the same
	// (provider, provider id) is a no-op.
	AttachAlbumVariant(ctx context.Context, v *AlbumVariant) (*AlbumVariant, bool, error)
	AlbumByProviderID(ctx context.Context, provider, providerAlbumID string) (*Album, error)
	// SetAlbumTracks replaces the album's tracklist.
	SetAlbumTracks(ctx context.Context, albumID uuid.UUID, trackIDs []uuid.UUID) error
	// AppendAlbumTracks adds the tracks that are not in the album yet, keeping
	// the existing order, and reports how many were added.
	AppendAlbumTracks(ctx context.Context, albumID uuid.UUID, trackIDs []uuid.UUID) (int, error)
	// SetAlbumVariantTrackCount records how many tracks a provider release
	// holds, so a client can tell a partial album from a complete one.
	SetAlbumVariantTrackCount(ctx context.Context, variantID uuid.UUID, count int) error
	AlbumTracks(ctx context.Context, albumID uuid.UUID) ([]Track, error)
	AlbumsForArtist(ctx context.Context, artistID uuid.UUID) ([]Album, error)
	AttachAlbumArtist(ctx context.Context, albumID, artistID uuid.UUID) error
}

// ArtistRepo stores artists and their provider pages.
type ArtistRepo interface {
	Artist(ctx context.Context, id uuid.UUID) (*Artist, error)
	ArtistByName(ctx context.Context, name string) (*Artist, error)
	ArtistVariants(ctx context.Context, id uuid.UUID) ([]ArtistVariant, error)
	AttachArtistVariant(ctx context.Context, v *ArtistVariant) (*ArtistVariant, bool, error)
	ArtistByProviderID(ctx context.Context, provider, providerArtistID string) (*Artist, error)
}

// albumIdentity is the comparison key of an album: its title and its primary
// artist, both reduced to a stable form.
func albumIdentity(title string, artists []string) (string, string) {
	titleKey := strings.ToLower(strings.Join(strings.Fields(title), " "))
	artistKey := ""
	if len(artists) > 0 {
		artistKey = strings.ToLower(strings.Join(strings.Fields(artists[0]), " "))
	}
	return titleKey, artistKey
}

// EnsureAlbum returns the id of the album for title by artists, creating it
// when it is new. Artists become the album's credits.
func (d *DB) EnsureAlbum(ctx context.Context, title string, artists []string) (uuid.UUID, error) {
	titleKey, artistKey := albumIdentity(title, artists)

	var existing string
	err := d.db.QueryRowContext(ctx,
		`SELECT id FROM albums WHERE title_key = ? AND artist_key = ?`, titleKey, artistKey).Scan(&existing)
	switch {
	case err == nil:
		if id, parseErr := parseUUID(existing); parseErr != nil {
			return uuid.Nil, parseErr
		} else if err := d.attachAlbumArtists(ctx, id, artists); err != nil {
			return uuid.Nil, err
		} else {
			return id, nil
		}
	case !isNotFound(err):
		return uuid.Nil, mapErr(err)
	}

	album := &Album{ID: uuid.New(), Title: title}
	err = d.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO albums (id, title, title_key, artist_key, created_at) VALUES (?, ?, ?, ?, ?)`,
			album.ID.String(), title, titleKey, artistKey, time.Now().UnixMilli()); err != nil {
			return mapErr(err)
		}
		return attachAlbumArtists(ctx, tx, album.ID, artists)
	})
	if err != nil {
		return uuid.Nil, err
	}
	return album.ID, nil
}

func (d *DB) attachAlbumArtists(ctx context.Context, albumID uuid.UUID, artists []string) error {
	if len(artists) == 0 {
		return nil
	}
	return d.withTx(ctx, func(tx *sql.Tx) error {
		return attachAlbumArtists(ctx, tx, albumID, artists)
	})
}

func attachAlbumArtists(ctx context.Context, tx txLike, albumID uuid.UUID, artists []string) error {
	for i, name := range artists {
		if strings.TrimSpace(name) == "" {
			continue
		}
		artistID, err := ensureArtist(ctx, tx, name)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO album_artists (album_id, artist_id, position) VALUES (?, ?, ?)`,
			albumID.String(), artistID.String(), i); err != nil {
			return mapErr(err)
		}
	}
	return nil
}

// Album returns the canonical album by id.
func (d *DB) Album(ctx context.Context, id uuid.UUID) (*Album, error) {
	var (
		album   Album
		idStr   string
		created int64
	)
	err := d.db.QueryRowContext(ctx,
		`SELECT id, title, created_at, artwork_url FROM albums WHERE id = ?`, id.String()).
		Scan(&idStr, &album.Title, &created, &album.ArtworkURL)
	if err != nil {
		return nil, mapErr(err)
	}
	if album.ID, err = parseUUID(idStr); err != nil {
		return nil, err
	}
	if created > 0 {
		album.CreatedAt = time.UnixMilli(created).UTC()
	}
	return &album, nil
}

// AlbumArtists lists an album's credited artists.
func (d *DB) AlbumArtists(ctx context.Context, id uuid.UUID) ([]Artist, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT a.id, a.name, a.artwork_url FROM artists a
		JOIN album_artists aa ON aa.artist_id = a.id
		WHERE aa.album_id = ? ORDER BY aa.position`, id.String())
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var artists []Artist
	for rows.Next() {
		var (
			artist Artist
			idStr  string
		)
		if err := rows.Scan(&idStr, &artist.Name, &artist.ArtworkURL); err != nil {
			return nil, mapErr(err)
		}
		if artist.ID, err = parseUUID(idStr); err != nil {
			return nil, err
		}
		artists = append(artists, artist)
	}
	return artists, mapErr(rows.Err())
}

const albumVariantColumns = `id, album_id, provider, provider_album_id, title, artists_json, year, track_count, created_at`

func scanAlbumVariant(row rowScanner) (*AlbumVariant, error) {
	var (
		variant     AlbumVariant
		id, albumID string
		artistsJSON string
		created     int64
	)
	if err := row.Scan(&id, &albumID, &variant.Provider, &variant.ProviderAlbumID, &variant.Title,
		&artistsJSON, &variant.Year, &variant.TrackCount, &created); err != nil {
		return nil, mapErr(err)
	}
	var err error
	if variant.ID, err = parseUUID(id); err != nil {
		return nil, err
	}
	if variant.AlbumID, err = parseUUID(albumID); err != nil {
		return nil, err
	}
	if artistsJSON != "" {
		if err := json.Unmarshal([]byte(artistsJSON), &variant.Artists); err != nil {
			return nil, fmt.Errorf("store: album variant %s artists: %w", id, err)
		}
	}
	variant.CreatedAt = time.UnixMilli(created).UTC()
	return &variant, nil
}

// AttachAlbumVariant records a provider release of an album. It reports whether
// the variant was created, so a sync can say what it learned.
func (d *DB) AttachAlbumVariant(ctx context.Context, v *AlbumVariant) (*AlbumVariant, bool, error) {
	if existing, err := d.albumVariantByProviderID(ctx, v.Provider, v.ProviderAlbumID); err == nil {
		return existing, false, nil
	} else if !isNotFound(err) {
		return nil, false, err
	}

	if v.ID == uuid.Nil {
		v.ID = uuid.New()
	}
	if v.CreatedAt.IsZero() {
		v.CreatedAt = time.Now()
	}
	v.Artists = normalizeStrings(v.Artists)
	artistsJSON, err := json.Marshal(v.Artists)
	if err != nil {
		return nil, false, fmt.Errorf("store: marshal album variant artists: %w", err)
	}

	_, err = d.db.ExecContext(ctx,
		`INSERT INTO album_variants (`+albumVariantColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		v.ID.String(), v.AlbumID.String(), v.Provider, v.ProviderAlbumID, v.Title,
		string(artistsJSON), v.Year, v.TrackCount, v.CreatedAt.UnixMilli())
	if err != nil {
		if isConflict(err) {
			// Another writer attached it first.
			if existing, lookupErr := d.albumVariantByProviderID(ctx, v.Provider, v.ProviderAlbumID); lookupErr == nil {
				return existing, false, nil
			}
		}
		return nil, false, mapErr(err)
	}
	return v, true, nil
}

func (d *DB) albumVariantByProviderID(ctx context.Context, provider, providerAlbumID string) (*AlbumVariant, error) {
	row := d.db.QueryRowContext(ctx,
		`SELECT `+albumVariantColumns+` FROM album_variants WHERE provider = ? AND provider_album_id = ?`,
		provider, providerAlbumID)
	return scanAlbumVariant(row)
}

// AlbumVariants lists a canonical album's provider releases.
func (d *DB) AlbumVariants(ctx context.Context, id uuid.UUID) ([]AlbumVariant, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+albumVariantColumns+` FROM album_variants WHERE album_id = ? ORDER BY created_at`, id.String())
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var variants []AlbumVariant
	for rows.Next() {
		variant, err := scanAlbumVariant(rows)
		if err != nil {
			return nil, err
		}
		variants = append(variants, *variant)
	}
	return variants, mapErr(rows.Err())
}

// AlbumByProviderID resolves the canonical album a provider release belongs to.
func (d *DB) AlbumByProviderID(ctx context.Context, provider, providerAlbumID string) (*Album, error) {
	variant, err := d.albumVariantByProviderID(ctx, provider, providerAlbumID)
	if err != nil {
		return nil, err
	}
	return d.Album(ctx, variant.AlbumID)
}

// SetAlbumVariantTrackCount records a release's tracklist size. A count already
// at least that large is left alone: the first answer stands.
func (d *DB) SetAlbumVariantTrackCount(ctx context.Context, variantID uuid.UUID, count int) error {
	if count <= 0 {
		return nil
	}
	_, err := d.db.ExecContext(ctx,
		`UPDATE album_variants SET track_count = ? WHERE id = ? AND track_count < ?`,
		count, variantID.String(), count)
	return mapErr(err)
}

// SetAlbumTracks replaces an album's tracklist.
func (d *DB) SetAlbumTracks(ctx context.Context, albumID uuid.UUID, trackIDs []uuid.UUID) error {
	return d.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM track_albums WHERE album_id = ?`, albumID.String()); err != nil {
			return mapErr(err)
		}
		if err := insertAlbumTracks(ctx, tx, albumID, trackIDs, 0); err != nil {
			return err
		}
		return adoptAlbumArtwork(ctx, tx, albumID)
	})
}

// AppendAlbumTracks adds the tracks the album does not have yet, after the ones
// it has. It returns how many were added.
func (d *DB) AppendAlbumTracks(ctx context.Context, albumID uuid.UUID, trackIDs []uuid.UUID) (int, error) {
	added := 0
	err := d.withTx(ctx, func(tx *sql.Tx) error {
		var next int
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(position) + 1, 0) FROM track_albums WHERE album_id = ?`,
			albumID.String()).Scan(&next); err != nil {
			return mapErr(err)
		}
		fresh := make([]uuid.UUID, 0, len(trackIDs))
		for _, trackID := range trackIDs {
			var exists int
			if err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM track_albums WHERE album_id = ? AND track_id = ?`,
				albumID.String(), trackID.String()).Scan(&exists); err != nil {
				return mapErr(err)
			}
			if exists == 0 {
				fresh = append(fresh, trackID)
			}
		}
		if err := insertAlbumTracks(ctx, tx, albumID, fresh, next); err != nil {
			return err
		}
		added = len(fresh)
		return adoptAlbumArtwork(ctx, tx, albumID)
	})
	return added, err
}

func insertAlbumTracks(ctx context.Context, tx txLike, albumID uuid.UUID, trackIDs []uuid.UUID, start int) error {
	for i, trackID := range trackIDs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO track_albums (track_id, album_id, position) VALUES (?, ?, ?)`,
			trackID.String(), albumID.String(), start+i); err != nil {
			return mapErr(err)
		}
	}
	return nil
}

// adoptAlbumArtwork gives an album the cover of one of its tracks when it has
// none of its own. A track carries the release's image, so an album that a
// track created would otherwise be a blank square on every page that lists it.
func adoptAlbumArtwork(ctx context.Context, tx txLike, albumID uuid.UUID) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE albums
		SET artwork_url = COALESCE((
			SELECT t.artwork_url FROM track_albums ta
			JOIN tracks t ON t.id = ta.track_id
			WHERE ta.album_id = albums.id AND t.artwork_url <> ''
			LIMIT 1
		), artwork_url)
		WHERE id = ? AND artwork_url = ''`, albumID.String())
	return mapErr(err)
}

// AlbumTracks lists an album's tracks in album order.
func (d *DB) AlbumTracks(ctx context.Context, albumID uuid.UUID) ([]Track, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT t.id, t.title, t.duration_ms, t.created_at, t.artwork_url
		FROM tracks t
		JOIN track_albums ta ON ta.track_id = t.id
		WHERE ta.album_id = ? ORDER BY ta.position`, albumID.String())
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var tracks []Track
	for rows.Next() {
		track, err := scanTrack(rows)
		if err != nil {
			return nil, err
		}
		tracks = append(tracks, *track)
	}
	return tracks, mapErr(rows.Err())
}

// AlbumsForArtist lists the albums an artist is credited on.
func (d *DB) AlbumsForArtist(ctx context.Context, artistID uuid.UUID) ([]Album, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT al.id, al.title, al.created_at, al.artwork_url
		FROM albums al
		JOIN album_artists aa ON aa.album_id = al.id
		WHERE aa.artist_id = ?
		ORDER BY al.title`, artistID.String())
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var albums []Album
	for rows.Next() {
		var (
			album   Album
			idStr   string
			created int64
		)
		if err := rows.Scan(&idStr, &album.Title, &created, &album.ArtworkURL); err != nil {
			return nil, mapErr(err)
		}
		var err error
		if album.ID, err = parseUUID(idStr); err != nil {
			return nil, err
		}
		if created > 0 {
			album.CreatedAt = time.UnixMilli(created).UTC()
		}
		albums = append(albums, album)
	}
	return albums, mapErr(rows.Err())
}

// AttachAlbumArtist credits an album to an artist.
func (d *DB) AttachAlbumArtist(ctx context.Context, albumID, artistID uuid.UUID) error {
	_, err := d.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO album_artists (album_id, artist_id, position) VALUES (?, ?, 0)`,
		albumID.String(), artistID.String())
	return mapErr(err)
}

// Artist returns the canonical artist by id.
func (d *DB) Artist(ctx context.Context, id uuid.UUID) (*Artist, error) {
	var (
		artist Artist
		idStr  string
	)
	if err := d.db.QueryRowContext(ctx, `SELECT id, name, artwork_url FROM artists WHERE id = ?`, id.String()).
		Scan(&idStr, &artist.Name, &artist.ArtworkURL); err != nil {
		return nil, mapErr(err)
	}
	var err error
	if artist.ID, err = parseUUID(idStr); err != nil {
		return nil, err
	}
	return &artist, nil
}

// ArtistByName returns the canonical artist by name.
func (d *DB) ArtistByName(ctx context.Context, name string) (*Artist, error) {
	var (
		artist Artist
		idStr  string
	)
	if err := d.db.QueryRowContext(ctx, `SELECT id, name, artwork_url FROM artists WHERE name = ?`, name).
		Scan(&idStr, &artist.Name, &artist.ArtworkURL); err != nil {
		return nil, mapErr(err)
	}
	var err error
	if artist.ID, err = parseUUID(idStr); err != nil {
		return nil, err
	}
	return &artist, nil
}

const artistVariantColumns = `id, artist_id, provider, provider_artist_id, name, created_at`

func scanArtistVariant(row rowScanner) (*ArtistVariant, error) {
	var (
		variant      ArtistVariant
		id, artistID string
		created      int64
	)
	if err := row.Scan(&id, &artistID, &variant.Provider, &variant.ProviderArtistID, &variant.Name, &created); err != nil {
		return nil, mapErr(err)
	}
	var err error
	if variant.ID, err = parseUUID(id); err != nil {
		return nil, err
	}
	if variant.ArtistID, err = parseUUID(artistID); err != nil {
		return nil, err
	}
	variant.CreatedAt = time.UnixMilli(created).UTC()
	return &variant, nil
}

// AttachArtistVariant records a provider's artist page.
func (d *DB) AttachArtistVariant(ctx context.Context, v *ArtistVariant) (*ArtistVariant, bool, error) {
	if existing, err := d.artistVariantByProviderID(ctx, v.Provider, v.ProviderArtistID); err == nil {
		return existing, false, nil
	} else if !isNotFound(err) {
		return nil, false, err
	}

	if v.ID == uuid.Nil {
		v.ID = uuid.New()
	}
	if v.CreatedAt.IsZero() {
		v.CreatedAt = time.Now()
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO artist_variants (`+artistVariantColumns+`) VALUES (?, ?, ?, ?, ?, ?)`,
		v.ID.String(), v.ArtistID.String(), v.Provider, v.ProviderArtistID, v.Name, v.CreatedAt.UnixMilli())
	if err != nil {
		if isConflict(err) {
			if existing, lookupErr := d.artistVariantByProviderID(ctx, v.Provider, v.ProviderArtistID); lookupErr == nil {
				return existing, false, nil
			}
		}
		return nil, false, mapErr(err)
	}
	return v, true, nil
}

func (d *DB) artistVariantByProviderID(ctx context.Context, provider, providerArtistID string) (*ArtistVariant, error) {
	row := d.db.QueryRowContext(ctx,
		`SELECT `+artistVariantColumns+` FROM artist_variants WHERE provider = ? AND provider_artist_id = ?`,
		provider, providerArtistID)
	return scanArtistVariant(row)
}

// ArtistVariants lists a canonical artist's provider pages.
func (d *DB) ArtistVariants(ctx context.Context, id uuid.UUID) ([]ArtistVariant, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+artistVariantColumns+` FROM artist_variants WHERE artist_id = ? ORDER BY created_at`, id.String())
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var variants []ArtistVariant
	for rows.Next() {
		variant, err := scanArtistVariant(rows)
		if err != nil {
			return nil, err
		}
		variants = append(variants, *variant)
	}
	return variants, mapErr(rows.Err())
}

// ArtistByProviderID resolves the canonical artist a provider page belongs to.
func (d *DB) ArtistByProviderID(ctx context.Context, provider, providerArtistID string) (*Artist, error) {
	variant, err := d.artistVariantByProviderID(ctx, provider, providerArtistID)
	if err != nil {
		return nil, err
	}
	return d.Artist(ctx, variant.ArtistID)
}

func normalizeStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

var (
	_ AlbumRepo  = (*DB)(nil)
	_ ArtistRepo = (*DB)(nil)
)

// AlbumCandidates lists albums that could be the same record as a provider
// release: the ones with the same title, plus everything by the same primary
// artist. Scoring decides between them.
func (d *DB) AlbumCandidates(ctx context.Context, titleKey, artistKey string, limit int) ([]Album, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, title, created_at, artwork_url FROM albums
		WHERE title_key = ? OR artist_key = ?
		ORDER BY CASE WHEN title_key = ? THEN 0 ELSE 1 END, title
		LIMIT ?`, titleKey, artistKey, titleKey, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	return scanAlbums(rows)
}

// ArtistCandidates lists artists whose name matches once normalised. The
// comparison happens in SQL so the caller does not have to load every artist.
func (d *DB) ArtistCandidates(ctx context.Context, normalized string, limit int) ([]Artist, error) {
	if limit <= 0 {
		limit = 25
	}
	rows, err := d.db.QueryContext(ctx,
		`SELECT id, name, artwork_url FROM artists WHERE lower(trim(name)) = ? LIMIT ?`, normalized, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var artists []Artist
	for rows.Next() {
		var (
			artist Artist
			idStr  string
		)
		if err := rows.Scan(&idStr, &artist.Name, &artist.ArtworkURL); err != nil {
			return nil, mapErr(err)
		}
		if artist.ID, err = parseUUID(idStr); err != nil {
			return nil, err
		}
		artists = append(artists, artist)
	}
	return artists, mapErr(rows.Err())
}

func scanAlbums(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}) ([]Album, error) {
	var albums []Album
	for rows.Next() {
		var (
			album   Album
			idStr   string
			created int64
		)
		if err := rows.Scan(&idStr, &album.Title, &created, &album.ArtworkURL); err != nil {
			return nil, mapErr(err)
		}
		var err error
		if album.ID, err = parseUUID(idStr); err != nil {
			return nil, err
		}
		if created > 0 {
			album.CreatedAt = time.UnixMilli(created).UTC()
		}
		albums = append(albums, album)
	}
	return albums, mapErr(rows.Err())
}

// AlbumIdentity exposes the album comparison key, so matching and the store
// cannot disagree about what makes an album the same album.
func AlbumIdentity(title string, artists []string) (string, string) {
	return albumIdentity(title, artists)
}

// SetAlbumArtists replaces an album's credits, and its identity when it had
// none. It is how an album first seen without an artist (a provider result that
// simply omitted them) becomes properly attributed once its tracklist arrives.
func (d *DB) SetAlbumArtists(ctx context.Context, albumID uuid.UUID, artists []string) error {
	if len(artists) == 0 {
		return nil
	}
	titleKey, artistKey := albumIdentity("", artists)

	return d.withTx(ctx, func(tx *sql.Tx) error {
		var (
			title      string
			currentKey string
		)
		if err := tx.QueryRowContext(ctx,
			`SELECT title, artist_key FROM albums WHERE id = ?`, albumID.String()).
			Scan(&title, &currentKey); err != nil {
			return mapErr(err)
		}
		_ = titleKey

		if currentKey == "" {
			// Only adopt an identity nobody else holds.
			var taken int
			if err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM albums WHERE artist_key = ? AND title_key = (SELECT title_key FROM albums WHERE id = ?)`,
				artistKey, albumID.String()).Scan(&taken); err != nil {
				return mapErr(err)
			}
			if taken == 0 {
				if _, err := tx.ExecContext(ctx,
					`UPDATE albums SET artist_key = ? WHERE id = ?`, artistKey, albumID.String()); err != nil {
					return mapErr(err)
				}
			}
		}
		return attachAlbumArtists(ctx, tx, albumID, artists)
	})
}
