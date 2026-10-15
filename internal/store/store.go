// Package store is the SQLite persistence layer.
//
// Everything relational lives here: the canonical track graph (tracks,
// artists, albums), the per-provider variants, the media files on disk,
// accounts, favorites and provider rankings. Repositories are exposed as
// narrow interfaces so consumers depend on one aggregate at a time and tests
// can run against an in-memory database.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

// Sentinel errors returned by every repository.
var (
	// ErrNotFound means the row does not exist.
	ErrNotFound = errors.New("not found")
	// ErrConflict means a uniqueness or foreign-key constraint was violated.
	ErrConflict = errors.New("conflict")
)

// Track is the canonical song. Provider renditions hang off it as Variants.
type Track struct {
	ID         uuid.UUID
	Title      string
	DurationMs int64
	CreatedAt  time.Time
	// ArtworkURL is where the track's image came from; the API turns it into a
	// path on this server.
	ArtworkURL string
}

// Artist is a canonical artist identity.
type Artist struct {
	ID   uuid.UUID
	Name string
	// ArtworkURL is where the artist's portrait came from.
	ArtworkURL string
}

// Album is a canonical album. Its identity is the title plus its primary
// artist, so two artists may each have a "Greatest Hits".
type Album struct {
	ID        uuid.UUID
	Title     string
	CreatedAt time.Time
	// ArtworkURL is where the cover came from.
	ArtworkURL string
}

// Variant is one provider's (or the local library's) rendition of a track.
type Variant struct {
	ID              uuid.UUID
	TrackID         uuid.UUID
	Provider        string
	ProviderTrackID string
	Title           string
	Artists         []string
	Album           string
	DurationMs      int64
	Downloadable    bool
	ISRC            string
	CreatedAt       time.Time
}

// MediaFile is a finished .opus file on disk for a variant.
type MediaFile struct {
	VariantID    uuid.UUID
	Path         string
	SHA256       string
	DurationMs   int64
	Bytes        int64
	DownloadedAt time.Time
	// AccessedAt is when the file was last served; eviction uses it as the
	// least-recently-used key.
	AccessedAt time.Time
}

// User is a local account.
type User struct {
	ID           uuid.UUID
	Username     string
	PasswordHash string
	// DisplayName and IconURL are what other people see; Username is how you
	// sign in. LastPlayedAt is what "online" is derived from.
	DisplayName string
	IconURL     string
	// IconVersion changes whenever the icon does. It is what makes a new
	// picture a new URL for everybody, rather than the same one a browser will
	// happily keep for a week.
	IconVersion  int
	LastPlayedAt time.Time
	CreatedAt    time.Time
	// SearchPlatforms is the platforms this account wants a search to ask by
	// default. It is empty when the account never chose, in which case the
	// client's own default applies.
	SearchPlatforms []string
}

// Session is a bearer token. Only the hash of the token is stored.
type Session struct {
	TokenHash string
	UserID    uuid.UUID
	CreatedAt time.Time
	ExpiresAt time.Time
}

// TrackRepo is the canonical-track side of the library.
type TrackRepo interface {
	CreateTrack(ctx context.Context, t *Track) error
	Track(ctx context.Context, id uuid.UUID) (*Track, error)
	// TrackByISRC finds the canonical track that already has a variant with
	// this ISRC (the strongest match signal).
	TrackByISRC(ctx context.Context, isrc string) (*Track, error)
	// CandidateTracks lists tracks whose duration is close enough to be the
	// same recording, or that have no known duration. A non-positive
	// durationMs means "no duration filter".
	CandidateTracks(ctx context.Context, durationMs, toleranceMs int64, limit int) ([]Track, error)
	// DeleteTrack removes a canonical track nothing points at any more.
	DeleteTrack(ctx context.Context, id uuid.UUID) error
	// SetIcon records a new picture for an account and counts the change, which
	// is what tells every client to fetch it again.
	SetIcon(ctx context.Context, id uuid.UUID, iconURL string) error
	// MergeTracks joins two canonical rows for the same recording: everything
	// pointing at drop moves to keep, and drop goes away. It is what makes a
	// library that matched one song twice playable again.
	MergeTracks(ctx context.Context, keep, drop uuid.UUID) error
	// AlbumCandidates and ArtistCandidates are the prefilter for collection
	// matching; see AlbumRepo and ArtistRepo for the rest.
	AlbumCandidates(ctx context.Context, titleKey, artistKey string, limit int) ([]Album, error)
	ArtistCandidates(ctx context.Context, normalized string, limit int) ([]Artist, error)
	SetTrackDuration(ctx context.Context, id uuid.UUID, durationMs int64) error
	TrackArtists(ctx context.Context, id uuid.UUID) ([]Artist, error)
	// TrackArtistsFor and TrackAlbumsFor are the batch reads matching uses:
	// one query for a whole candidate set, rather than one per candidate.
	TrackArtistsFor(ctx context.Context, trackIDs []uuid.UUID) (map[uuid.UUID][]Artist, error)
	// TracksFor is the batch read a listing uses: a page of songs is one query
	// rather than one per row.
	TracksFor(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Track, error)
	SetTrackArtists(ctx context.Context, id uuid.UUID, names []string) error
	TrackAlbums(ctx context.Context, id uuid.UUID) ([]Album, error)
	TrackAlbumsFor(ctx context.Context, trackIDs []uuid.UUID) (map[uuid.UUID][]Album, error)
	SetTrackAlbums(ctx context.Context, id uuid.UUID, titles []string) error
	// TrackArtwork is the cover's path on this server, empty when none is known.
	// The room manager needs it for the queue rows, which show the artwork.
	TrackArtwork(ctx context.Context, id uuid.UUID) (string, error)
	EnsureArtist(ctx context.Context, name string) (uuid.UUID, error)
	EnsureAlbum(ctx context.Context, title string, artists []string) (uuid.UUID, error)
}

// VariantRepo stores provider renditions, unique per (provider, provider id).
type VariantRepo interface {
	CreateVariant(ctx context.Context, v *Variant) error
	Variant(ctx context.Context, id uuid.UUID) (*Variant, error)
	VariantByProviderTrack(ctx context.Context, provider, providerTrackID string) (*Variant, error)
	VariantsForTrack(ctx context.Context, trackID uuid.UUID) ([]Variant, error)
	SetVariantTrack(ctx context.Context, variantID, trackID uuid.UUID) error
}

// SourceRepo reads what the reserved ranking slots resolve against: who
// uploaded a track's variants, and how they have been voted on.
type SourceRepo interface {
	VariantUploaders(ctx context.Context, trackID uuid.UUID) (map[uuid.UUID]VariantUploader, error)
	VariantVoteCounts(ctx context.Context, trackID uuid.UUID) (map[uuid.UUID]VariantVoteCount, error)
}

// MediaRepo maps variants to finished files on disk.
type MediaRepo interface {
	UpsertMediaFile(ctx context.Context, m *MediaFile) error
	MediaFile(ctx context.Context, variantID uuid.UUID) (*MediaFile, error)
	MediaFileBySHA256(ctx context.Context, sha256 string) (*MediaFile, error)
	DeleteMediaFile(ctx context.Context, variantID uuid.UUID) error
	MediaFiles(ctx context.Context) ([]MediaFile, error)
	// MediaFilesFor is the batch read a listing uses: which of a set of
	// renditions already have a file on disk, in one query rather than one
	// per row.
	MediaFilesFor(ctx context.Context, variantIDs []uuid.UUID) (map[uuid.UUID]MediaFile, error)
	// TouchMediaFile records that a file was served, which is what the LRU
	// eviction order is based on.
	TouchMediaFile(ctx context.Context, variantID uuid.UUID, at time.Time) error
	// MediaBytes is the total size of the media cache.
	MediaBytes(ctx context.Context) (int64, error)
	// MediaEvictionCandidates lists only the files an eviction has to remove to
	// free excessBytes, least recently served first: an eviction reads the rows
	// it will delete rather than the whole cache. A rendition fetched from a
	// provider can be fetched again; one that came off this disk - an upload, a
	// file from the local library - is somebody's only copy and is never
	// listed.
	MediaEvictionCandidates(ctx context.Context, excessBytes int64) ([]MediaFile, error)
}

// UserRepo stores accounts.
type UserRepo interface {
	CreateUser(ctx context.Context, u *User) error
	User(ctx context.Context, id uuid.UUID) (*User, error)
	UserByUsername(ctx context.Context, username string) (*User, error)
	// CountUsers is how many accounts the server holds.
	CountUsers(ctx context.Context) (int, error)
	// SetSearchPlatforms replaces the platforms a search asks by default; an
	// empty list clears the preference.
	SetSearchPlatforms(ctx context.Context, userID uuid.UUID, platforms []string) error
}

// SessionRepo stores bearer-token sessions.
type SessionRepo interface {
	CreateSession(ctx context.Context, s *Session) error
	Session(ctx context.Context, tokenHash string) (*Session, error)
	DeleteSession(ctx context.Context, tokenHash string) error
	DeleteExpiredSessions(ctx context.Context, now time.Time) (int, error)
}

// FavoriteRepo stores per-user track favorites.
type FavoriteRepo interface {
	AddFavorite(ctx context.Context, userID, trackID uuid.UUID) error
	RemoveFavorite(ctx context.Context, userID, trackID uuid.UUID) error
	Favorites(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error)
}

// RankingRepo stores each user's ordered provider preference.
type RankingRepo interface {
	SetRanking(ctx context.Context, userID uuid.UUID, providers []string) error
	Ranking(ctx context.Context, userID uuid.UUID) ([]string, error)
	AllRankings(ctx context.Context) (map[uuid.UUID][]string, error)
}

// Vote is one room member's score for the track playing in that room.
type Vote struct {
	RoomID    string
	TrackID   uuid.UUID
	MemberID  string
	UserID    *uuid.UUID
	Score     int
	CreatedAt time.Time
}

// VoteRepo stores room votes so they outlive the room.
type VoteRepo interface {
	// SaveVote records (or replaces) a member's vote for a track in a room.
	SaveVote(ctx context.Context, v *Vote) error
	// TrackVotes lists every vote a track received in a room.
	TrackVotes(ctx context.Context, roomID string, trackID uuid.UUID) ([]Vote, error)
	// TrackVoteStats returns the vote count and mean score for a track
	// across all rooms.
	TrackVoteStats(ctx context.Context, trackID uuid.UUID) (int, float64, error)
}

var (
	_ TrackRepo    = (*DB)(nil)
	_ VariantRepo  = (*DB)(nil)
	_ MediaRepo    = (*DB)(nil)
	_ UserRepo     = (*DB)(nil)
	_ SessionRepo  = (*DB)(nil)
	_ FavoriteRepo = (*DB)(nil)
	_ RankingRepo  = (*DB)(nil)
	_ VoteRepo     = (*DB)(nil)
	_ PlaylistRepo = (*DB)(nil)
)

// DB is the SQLite-backed store. It implements every repository interface.
type DB struct {
	db *sql.DB
	// root is the storage directory this database lives in. Media paths are
	// stored relative to it, so that moving the directory - to another disk,
	// to another machine - moves the library with it instead of leaving every
	// row pointing at where it used to be.
	root string
}

// execer is satisfied by *sql.DB and *sql.Tx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// dbMaxConns is the size of the connection pool a file database gets. SQLite
// admits one writer at a time, so this is really a reader budget: enough to
// keep requests moving alongside the writer, few enough that writes do not
// spend their time waiting on each other.
const dbMaxConns = 8

// Open opens the SQLite database at path and applies pending migrations.
// path is a filesystem path, ":memory:", or a full DSN such as
// "file:test?mode=memory&cache=shared".
func Open(path string) (*DB, error) {
	if path == "" {
		return nil, errors.New("store: empty database path")
	}

	dsn := path
	memory := path == ":memory:" || strings.Contains(path, "mode=memory")
	switch {
	case path == ":memory:":
		dsn = "file::memory:?cache=shared"
	case memory:
		// keep the caller's DSN
	case !strings.HasPrefix(path, "file:"):
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("store: create database directory: %w", err)
			}
		}
		dsn = "file:" + path
	}

	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	dsn += sep + "_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	if !memory {
		// WAL commits still fsync unless synchronous is relaxed, and the library
		// is rewritten constantly by matching, downloads and uploads; NORMAL
		// spares each commit a full flush. The page cache and mmap sizes are
		// what those repeated reads are served from instead of the file.
		dsn += "&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)" +
			"&_pragma=cache_size(-16000)&_pragma=mmap_size(268435456)"
	}

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	if memory {
		// An in-memory database lives inside its connections; a single
		// pooled connection keeps it alive and consistent.
		sqlDB.SetMaxOpenConns(1)
	} else {
		// database/sql otherwise opens as many connections as callers ask for,
		// and SQLite admits one writer at a time: a crowd of them only becomes
		// lock contention. A small pool lets readers run while the writer does,
		// which is what WAL makes cheap.
		sqlDB.SetMaxOpenConns(dbMaxConns)
		sqlDB.SetMaxIdleConns(dbMaxConns)
	}

	ctx := context.Background()
	if err := sqlDB.PingContext(ctx); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("store: ping %s: %w", path, err)
	}

	d := &DB{db: sqlDB, root: storageRoot(path, memory)}
	if err := d.migrate(ctx); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return d, nil
}

// Close releases the database.
func (d *DB) Close() error { return d.db.Close() }

// storageRoot is the directory a database path lives in, which is what media
// paths are stored relative to: the database sits at <storageDir>/musoak.db, so
// its own directory is the storage directory. An in-memory database has no
// directory, and neither has a path that names none - those keep their media
// paths exactly as they are given.
func storageRoot(path string, memory bool) string {
	if memory || path == "" {
		return ""
	}
	name := strings.TrimPrefix(path, "file:")
	if at := strings.IndexByte(name, '?'); at >= 0 {
		name = name[:at]
	}
	dir := filepath.Dir(name)
	if dir == "." {
		return ""
	}
	return dir
}

// schemaV21 adds the indexes the hot reads were missing. Each one backs a
// query that already exists: the candidate search orders by created_at, so its
// ORDER BY is answered from the index instead of sorting the whole table;
// uploads and imports look a media file up by content hash; an artist's albums
// join album_artists by artist; and the eviction subqueries filter variants by
// provider.
const schemaV21 = `
CREATE INDEX IF NOT EXISTS tracks_created ON tracks(created_at);
CREATE INDEX IF NOT EXISTS media_files_sha256 ON media_files(sha256);
CREATE INDEX IF NOT EXISTS album_artists_artist_id ON album_artists(artist_id);
CREATE INDEX IF NOT EXISTS variants_provider ON variants(provider);
`

type migration struct {
	version int
	name    string
	sql     string
}

// migrations are append-only; never edit an applied migration.
var migrations = []migration{
	{version: 1, name: "initial schema", sql: schemaV1},
	{version: 2, name: "votes", sql: schemaV2},
	{version: 3, name: "media last use", sql: schemaV3},
	{version: 4, name: "playlists", sql: schemaV4},
	{version: 5, name: "albums and artists", sql: schemaV5},
	{version: 6, name: "artwork", sql: schemaV6},
	{version: 7, name: "external playlists", sql: schemaV7},
	{version: 8, name: "playlist artwork", sql: schemaV8},
	{version: 9, name: "playback state", sql: schemaV9},
	{version: 10, name: "uploads", sql: schemaV10},
	{version: 11, name: "source votes", sql: schemaV11},
	{version: 12, name: "profiles", sql: schemaV12},
	{version: 13, name: "friends and notifications", sql: schemaV13},
	{version: 14, name: "playlist visibility", sql: schemaV14},
	{version: 15, name: "one album tracklist", sql: schemaV15},
	{version: 16, name: "search platforms", sql: schemaV16},
	{version: 17, name: "icon version", sql: schemaV17},
	{version: 18, name: "play history", sql: schemaV18},
	{version: 19, name: "lyrics", sql: schemaV19},
	{version: 20, name: "media paths relative to the storage directory", sql: schemaV20},
	{version: 21, name: "read indexes", sql: schemaV21},
}

func (d *DB) migrate(ctx context.Context) error {
	if _, err := d.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT NOT NULL,
			applied_at INTEGER NOT NULL
		)`); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}

	var current int
	if err := d.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		err := d.withTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, m.sql); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx,
				`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
				m.version, m.name, time.Now().UnixMilli())
			return err
		})
		if err != nil {
			return fmt.Errorf("store: migration %d (%s): %w", m.version, m.name, err)
		}
	}
	return nil
}

func (d *DB) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	// A busy database is a timing problem, not a failure: another writer had the
	// lock. Trying again shortly is what SQLite's own busy handling does for
	// single statements, and it applies to transactions just as well.
	var err error
	for attempt := 0; attempt < busyAttempts; attempt++ {
		err = d.runTx(ctx, fn)
		if !isBusy(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(busyBackoff(attempt)):
		}
	}
	return err
}

func (d *DB) runTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// execRetry runs one write statement, waiting out a busy database.
func (d *DB) execRetry(ctx context.Context, query string, args ...any) (sql.Result, error) {
	var (
		result sql.Result
		err    error
	)
	for attempt := 0; attempt < busyAttempts; attempt++ {
		result, err = d.db.ExecContext(ctx, query, args...)
		if !isBusy(err) {
			return result, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(busyBackoff(attempt)):
		}
	}
	return result, err
}

// busyAttempts is how many times a write is retried while the database is busy,
// and busyBackoff how long to wait between tries. The driver already sits out
// busy_timeout inside each attempt, so a retry can cost seconds; two attempts
// bound a contending write at about ten seconds rather than the twenty the old
// budget allowed.
const busyAttempts = 2

func busyBackoff(attempt int) time.Duration {
	return time.Duration(20*(1<<attempt)) * time.Millisecond
}

// isBusy reports whether the database was locked by another writer. The driver
// is the one that decides, and it says so in the message.
func isBusy(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "database is locked") ||
		strings.Contains(message, "SQLITE_BUSY") ||
		strings.Contains(message, "database table is locked")
}

// mapErr translates driver errors into the package's sentinels.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	var coded interface{ Code() int }
	if errors.As(err, &coded) {
		// 19 = SQLITE_CONSTRAINT, 1555 = PK, 2067 = UNIQUE, 787 = FK.
		switch coded.Code() {
		case 19, 1555, 2067, 787:
			return fmt.Errorf("%w: %v", ErrConflict, err)
		}
	}
	if strings.Contains(err.Error(), "constraint failed") {
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return err
}

// isNotFound and isConflict let the collection code branch on the sentinels
// while the driver errors are still a single value away.
func isNotFound(err error) bool { return errors.Is(mapErr(err), ErrNotFound) }

func isConflict(err error) bool { return errors.Is(mapErr(err), ErrConflict) }

func parseUUID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, fmt.Errorf("store: bad uuid %q: %w", s, err)
	}
	return id, nil
}
