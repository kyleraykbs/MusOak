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
}

// Artist is a canonical artist identity.
type Artist struct {
	ID   uuid.UUID
	Name string
}

// Album is a canonical album. Its identity is the title plus its primary
// artist, so two artists may each have a "Greatest Hits".
type Album struct {
	ID        uuid.UUID
	Title     string
	CreatedAt time.Time
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
	CreatedAt    time.Time
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
	// AlbumCandidates and ArtistCandidates are the prefilter for collection
	// matching; see AlbumRepo and ArtistRepo for the rest.
	AlbumCandidates(ctx context.Context, titleKey, artistKey string, limit int) ([]Album, error)
	ArtistCandidates(ctx context.Context, normalized string, limit int) ([]Artist, error)
	SetTrackDuration(ctx context.Context, id uuid.UUID, durationMs int64) error
	TrackArtists(ctx context.Context, id uuid.UUID) ([]Artist, error)
	SetTrackArtists(ctx context.Context, id uuid.UUID, names []string) error
	TrackAlbums(ctx context.Context, id uuid.UUID) ([]Album, error)
	SetTrackAlbums(ctx context.Context, id uuid.UUID, titles []string) error
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

// MediaRepo maps variants to finished files on disk.
type MediaRepo interface {
	UpsertMediaFile(ctx context.Context, m *MediaFile) error
	MediaFile(ctx context.Context, variantID uuid.UUID) (*MediaFile, error)
	MediaFileBySHA256(ctx context.Context, sha256 string) (*MediaFile, error)
	DeleteMediaFile(ctx context.Context, variantID uuid.UUID) error
	MediaFiles(ctx context.Context) ([]MediaFile, error)
	// TouchMediaFile records that a file was served, which is what the LRU
	// eviction order is based on.
	TouchMediaFile(ctx context.Context, variantID uuid.UUID, at time.Time) error
	// MediaFilesByLastUse lists files least recently used first.
	MediaFilesByLastUse(ctx context.Context) ([]MediaFile, error)
	// MediaBytes is the total size of the media cache.
	MediaBytes(ctx context.Context) (int64, error)
}

// UserRepo stores accounts.
type UserRepo interface {
	CreateUser(ctx context.Context, u *User) error
	User(ctx context.Context, id uuid.UUID) (*User, error)
	UserByUsername(ctx context.Context, username string) (*User, error)
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
}

// execer is satisfied by *sql.DB and *sql.Tx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

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
		dsn += "&_pragma=journal_mode(WAL)"
	}

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	if memory {
		// An in-memory database lives inside its connections; a single
		// pooled connection keeps it alive and consistent.
		sqlDB.SetMaxOpenConns(1)
	}

	ctx := context.Background()
	if err := sqlDB.PingContext(ctx); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("store: ping %s: %w", path, err)
	}

	d := &DB{db: sqlDB}
	if err := d.migrate(ctx); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return d, nil
}

// Close releases the database.
func (d *DB) Close() error { return d.db.Close() }

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
