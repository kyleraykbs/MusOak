package store

import (
	"context"
	"database/sql"
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
	// Kind is what the provider calls this release: "album", "single" or "ep",
	// or "" when nobody has said yet. An EP and a single are both a song's
	// release rather than a record, which is the distinction the artist page
	// draws.
	Kind      string
	CreatedAt time.Time
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
	// AlbumArtistsFor and AlbumYearsFor are the batch reads album matching
	// uses: one query for a whole candidate set, rather than one per candidate.
	AlbumArtistsFor(ctx context.Context, albumIDs []uuid.UUID) (map[uuid.UUID][]Artist, error)
	AlbumYearsFor(ctx context.Context, albumIDs []uuid.UUID) (map[uuid.UUID]string, error)
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
	// EnsureSinglesAlbum returns the container an artist's single releases are
	// collected into, creating it when the artist does not have one yet.
	EnsureSinglesAlbum(ctx context.Context, artistID uuid.UUID) (*Album, error)
	AttachAlbumArtist(ctx context.Context, albumID, artistID uuid.UUID) error
}

// ArtistRepo stores artists and their provider pages.
type ArtistRepo interface {
	Artist(ctx context.Context, id uuid.UUID) (*Artist, error)
	ArtistByName(ctx context.Context, name string) (*Artist, error)
	ArtistVariants(ctx context.Context, id uuid.UUID) ([]ArtistVariant, error)
	AttachArtistVariant(ctx context.Context, v *ArtistVariant) (*ArtistVariant, bool, error)
	ArtistByProviderID(ctx context.Context, provider, providerArtistID string) (*Artist, error)
	// ArtistTracks lists the songs credited to an artist, by title, with query
	// matched anywhere in the title, and CountArtistTracks is how many there are.
	ArtistTracks(ctx context.Context, artistID uuid.UUID, query string, limit, offset int) ([]Track, error)
	CountArtistTracks(ctx context.Context, artistID uuid.UUID, query string) (int, error)
}

// What a release is, in the provider's words and in this library's: an album is
// a record, a single is one song's release, and an EP is a single with a few
// more songs on it. A provider that says nothing leaves it empty, which means
// "not known yet" rather than "album" - the next read of that release is what
// fills it in.
const (
	ReleaseKindAlbum  = "album"
	ReleaseKindSingle = "single"
	ReleaseKindEP     = "ep"
)

// SinglesAlbumTitle is the name of the one album this library makes up.
const SinglesAlbumTitle = "Singles"

// AlbumKindSingles is the container an artist's single releases are collected
// into, so an artist's page lists their records and one row for everything they
// put out as a single, rather than twenty rows that are not records at all. It
// carries no provider variant of its own and is never matched against one.
const AlbumKindSingles = "singles"

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
		`SELECT id, title, kind, created_at, artwork_url FROM albums WHERE id = ?`, id.String()).
		Scan(&idStr, &album.Title, &album.Kind, &created, &album.ArtworkURL)
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
	return scanArtists(rows)
}

// AlbumArtistsFor lists many albums' credited artists at once, in credit order,
// keyed by album id; an album with no credits has no entry. The batch form of
// AlbumArtists, for scoring a search hit against many candidate albums.
func (d *DB) AlbumArtistsFor(ctx context.Context, albumIDs []uuid.UUID) (map[uuid.UUID][]Artist, error) {
	if len(albumIDs) == 0 {
		return map[uuid.UUID][]Artist{}, nil
	}
	rows, err := d.db.QueryContext(ctx, `
		SELECT aa.album_id, a.id, a.name, a.artwork_url
		FROM album_artists aa
		JOIN artists a ON a.id = aa.artist_id
		WHERE aa.album_id IN (`+placeholders(len(albumIDs))+`)
		ORDER BY aa.album_id, aa.position`, uuidStrings(albumIDs)...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	out := make(map[uuid.UUID][]Artist, len(albumIDs))
	for rows.Next() {
		var (
			ownerStr, idStr string
			artist          Artist
		)
		if err := rows.Scan(&ownerStr, &idStr, &artist.Name, &artist.ArtworkURL); err != nil {
			return nil, mapErr(err)
		}
		owner, err := parseUUID(ownerStr)
		if err != nil {
			return nil, err
		}
		if artist.ID, err = parseUUID(idStr); err != nil {
			return nil, err
		}
		out[owner] = append(out[owner], artist)
	}
	return out, mapErr(rows.Err())
}

// AlbumYearsFor reads the year any of an album's provider releases reports,
// keyed by album id; an album whose releases report none has no entry. The year
// is the first a release gives, in release order, matching AlbumVariants.
func (d *DB) AlbumYearsFor(ctx context.Context, albumIDs []uuid.UUID) (map[uuid.UUID]string, error) {
	if len(albumIDs) == 0 {
		return map[uuid.UUID]string{}, nil
	}
	rows, err := d.db.QueryContext(ctx, `
		SELECT album_id, year FROM album_variants
		WHERE album_id IN (`+placeholders(len(albumIDs))+`) AND year <> ''
		ORDER BY album_id, created_at`, uuidStrings(albumIDs)...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	out := make(map[uuid.UUID]string, len(albumIDs))
	for rows.Next() {
		var albumID, year string
		if err := rows.Scan(&albumID, &year); err != nil {
			return nil, mapErr(err)
		}
		parsed, err := parseUUID(albumID)
		if err != nil {
			return nil, err
		}
		if _, seen := out[parsed]; !seen {
			out[parsed] = year
		}
	}
	return out, mapErr(rows.Err())
}

const albumVariantColumns = `id, album_id, provider, provider_album_id, title, artists_json, year, track_count, kind, created_at`

func scanAlbumVariant(row rowScanner) (*AlbumVariant, error) {
	var (
		variant     AlbumVariant
		id, albumID string
		artistsJSON string
		created     int64
	)
	if err := row.Scan(&id, &albumID, &variant.Provider, &variant.ProviderAlbumID, &variant.Title,
		&artistsJSON, &variant.Year, &variant.TrackCount, &variant.Kind, &created); err != nil {
		return nil, mapErr(err)
	}
	var err error
	if variant.ID, err = parseUUID(id); err != nil {
		return nil, err
	}
	if variant.AlbumID, err = parseUUID(albumID); err != nil {
		return nil, err
	}
	if err := unmarshalArtists(artistsJSON, "album variant", id, &variant.Artists); err != nil {
		return nil, err
	}
	variant.CreatedAt = time.UnixMilli(created).UTC()
	return &variant, nil
}

// AttachAlbumVariant records a provider release of an album. It reports whether
// the variant was created, so a sync can say what it learned.
//
// Re-attaching a release that is already stored fills in what nobody knew the
// first time round - its kind, its length, its year, a title the provider has
// since corrected - and changes nothing that is already known. That is what
// tells the singles already in a library what they are: they were stored before
// anything said, and the next read of that release is what says.
func (d *DB) AttachAlbumVariant(ctx context.Context, v *AlbumVariant) (*AlbumVariant, bool, error) {
	if existing, err := d.albumVariantByProviderID(ctx, v.Provider, v.ProviderAlbumID); err == nil {
		filled, err := d.fillAlbumVariant(ctx, existing, v)
		if err != nil {
			return nil, false, err
		}
		return filled, false, nil
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
	artistsJSON, err := marshalArtists(v.Artists, "album variant")
	if err != nil {
		return nil, false, err
	}

	_, err = d.db.ExecContext(ctx,
		`INSERT INTO album_variants (`+albumVariantColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		v.ID.String(), v.AlbumID.String(), v.Provider, v.ProviderAlbumID, v.Title,
		artistsJSON, v.Year, v.TrackCount, v.Kind, v.CreatedAt.UnixMilli())
	if err != nil {
		if isConflict(err) {
			// Another writer attached it first.
			if existing, lookupErr := d.albumVariantByProviderID(ctx, v.Provider, v.ProviderAlbumID); lookupErr == nil {
				filled, fillErr := d.fillAlbumVariant(ctx, existing, v)
				if fillErr != nil {
					return nil, false, fillErr
				}
				return filled, false, nil
			}
		}
		return nil, false, mapErr(err)
	}
	return v, true, nil
}

// fillAlbumVariant writes what the provider knows and this library does not onto
// a release that is already stored. Nothing known is overwritten: the kind of a
// release nobody classified, the length of one nobody measured, the year and
// the title of one the provider has since filled in.
func (d *DB) fillAlbumVariant(ctx context.Context, stored, read *AlbumVariant) (*AlbumVariant, error) {
	kind, year, title, count := stored.Kind, stored.Year, stored.Title, stored.TrackCount
	if kind == "" {
		kind = read.Kind
	}
	if year == "" {
		year = read.Year
	}
	if title == "" {
		title = read.Title
	}
	if count <= 0 && read.TrackCount > 0 {
		count = read.TrackCount
	}
	if kind == stored.Kind && year == stored.Year && title == stored.Title && count == stored.TrackCount {
		return stored, nil
	}
	if _, err := d.db.ExecContext(ctx,
		`UPDATE album_variants SET kind = ?, year = ?, title = ?, track_count = ? WHERE id = ?`,
		kind, year, title, count, stored.ID.String()); err != nil {
		return nil, mapErr(err)
	}
	stored.Kind, stored.Year, stored.Title, stored.TrackCount = kind, year, title, count
	return stored, nil
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
		existing, err := existingTrackIDs(ctx, tx, albumID, trackIDs)
		if err != nil {
			return err
		}
		fresh := make([]uuid.UUID, 0, len(trackIDs))
		for _, trackID := range trackIDs {
			if !existing[trackID] {
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

// existingTrackIDs reports which of trackIDs the album already holds, in one
// query rather than one count per track.
func existingTrackIDs(ctx context.Context, q txLike, albumID uuid.UUID, trackIDs []uuid.UUID) (map[uuid.UUID]bool, error) {
	out := make(map[uuid.UUID]bool, len(trackIDs))
	if len(trackIDs) == 0 {
		return out, nil
	}
	args := append([]any{albumID.String()}, uuidStrings(trackIDs)...)
	rows, err := q.QueryContext(ctx, `
		SELECT track_id FROM track_albums
		WHERE album_id = ? AND track_id IN (`+placeholders(len(trackIDs))+`)`, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	for rows.Next() {
		var idStr string
		if err := rows.Scan(&idStr); err != nil {
			return nil, mapErr(err)
		}
		id, err := parseUUID(idStr)
		if err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, mapErr(rows.Err())
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
//
// The order is the one an artist's page is read in: the singles container
// first, because it is not a release and has no date to place it by, then
// newest release first, and anything whose year nobody knows last rather than
// pretending it is from year zero. A record's year is its earliest release's -
// a remaster does not move the record to the year it was remastered - and
// within one year the title decides, so the order is stable between reads.
func (d *DB) AlbumsForArtist(ctx context.Context, artistID uuid.UUID) ([]Album, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT al.id, al.title, al.kind, al.created_at, al.artwork_url
		FROM albums al
		JOIN album_artists aa ON aa.album_id = al.id
		WHERE aa.artist_id = ?
		ORDER BY
			CASE WHEN al.kind = '`+AlbumKindSingles+`' THEN 0 ELSE 1 END,
			(SELECT MIN(CAST(av.year AS INTEGER)) FROM album_variants av
				WHERE av.album_id = al.id AND av.year <> '') IS NULL,
			(SELECT MIN(CAST(av.year AS INTEGER)) FROM album_variants av
				WHERE av.album_id = al.id AND av.year <> '') DESC,
			al.title`, artistID.String())
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	return scanAlbums(rows)
}

// AttachAlbumArtist credits an album to an artist.
func (d *DB) AttachAlbumArtist(ctx context.Context, albumID, artistID uuid.UUID) error {
	_, err := d.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO album_artists (album_id, artist_id, position) VALUES (?, ?, 0)`,
		albumID.String(), artistID.String())
	return mapErr(err)
}

// EnsureSinglesAlbum returns the container holding an artist's single releases,
// creating it the first time that artist is asked about.
//
// The container is this library's invention, not a release: it has no provider
// variant and so can never be matched to one, it is one row per artist, and its
// tracklist is every song the artist put out as a single. It exists so a page
// can list an artist's records and one row for the singles, rather than twenty
// rows that are not records at all.
func (d *DB) EnsureSinglesAlbum(ctx context.Context, artistID uuid.UUID) (*Album, error) {
	var idStr string
	err := d.db.QueryRowContext(ctx, `
		SELECT al.id FROM albums al
		JOIN album_artists aa ON aa.album_id = al.id
		WHERE aa.artist_id = ? AND al.kind = ?
		LIMIT 1`, artistID.String(), AlbumKindSingles).Scan(&idStr)
	switch {
	case err == nil:
		id, parseErr := parseUUID(idStr)
		if parseErr != nil {
			return nil, parseErr
		}
		return d.Album(ctx, id)
	case !isNotFound(err):
		return nil, mapErr(err)
	}

	artist, err := d.Artist(ctx, artistID)
	if err != nil {
		return nil, err
	}
	titleKey, artistKey := albumIdentity(SinglesAlbumTitle, []string{artist.Name})
	album := &Album{ID: uuid.New(), Title: SinglesAlbumTitle, Kind: AlbumKindSingles}
	err = d.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO albums (id, title, kind, title_key, artist_key, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			album.ID.String(), album.Title, album.Kind, titleKey, artistKey,
			time.Now().UnixMilli()); err != nil {
			return mapErr(err)
		}
		return attachAlbumArtists(ctx, tx, album.ID, []string{artist.Name})
	})
	if err != nil {
		return nil, err
	}
	return album, nil
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

// ArtistTracks lists the songs an artist is credited on, in title order, with
// the query matched anywhere in the title the way a search box expects.
//
// Every credit counts, features included: a song the artist is on is a song
// their page can offer, and separating "by" from "featuring" is the provider's
// distinction, not one this library keeps.
func (d *DB) ArtistTracks(ctx context.Context, artistID uuid.UUID, query string, limit, offset int) ([]Track, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := d.db.QueryContext(ctx, `
		SELECT t.id, t.title, t.duration_ms, t.created_at, t.artwork_url
		FROM tracks t
		JOIN track_artists ta ON ta.track_id = t.id
		JOIN artists a ON a.id = ta.artist_id
		WHERE lower(trim(a.name)) = (SELECT lower(trim(name)) FROM artists WHERE id = ?)
			AND t.title LIKE ? ESCAPE '\'
		ORDER BY t.title COLLATE NOCASE
		LIMIT ? OFFSET ?`, artistID.String(), trackPattern(query), limit, offset)
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

// CountArtistTracks is how many songs that list holds, so a page can say how
// many there are without reading them all.
func (d *DB) CountArtistTracks(ctx context.Context, artistID uuid.UUID, query string) (int, error) {
	var total int
	if err := d.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM tracks t
		JOIN track_artists ta ON ta.track_id = t.id
		JOIN artists a ON a.id = ta.artist_id
		WHERE lower(trim(a.name)) = (SELECT lower(trim(name)) FROM artists WHERE id = ?)
			AND t.title LIKE ? ESCAPE '\'`,
		artistID.String(), trackPattern(query)).Scan(&total); err != nil {
		return 0, mapErr(err)
	}
	return total, nil
}

// trackPattern is a search box's query as a LIKE pattern: anywhere in the
// title, and the wildcards somebody typed match themselves.
func trackPattern(query string) string {
	return "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(query) + "%"
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
		SELECT id, title, kind, created_at, artwork_url FROM albums
		WHERE kind <> ? AND (title_key = ? OR artist_key = ?)
		ORDER BY CASE WHEN title_key = ? THEN 0 ELSE 1 END, title
		LIMIT ?`, AlbumKindSingles, titleKey, artistKey, titleKey, limit)
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
	return scanArtists(rows)
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
		if err := rows.Scan(&idStr, &album.Title, &album.Kind, &created, &album.ArtworkURL); err != nil {
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
