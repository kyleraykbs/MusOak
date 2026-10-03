package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// CreateUser inserts u, generating ID and CreatedAt when unset.
func (d *DB) CreateUser(ctx context.Context, u *User) error {
	if u.ID == uuid.Nil {
		u.ID = uuid.New()
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now()
	}
	platforms, err := marshalSearchPlatforms(u.SearchPlatforms)
	if err != nil {
		return err
	}
	_, err = d.db.ExecContext(ctx,
		`INSERT INTO users (id, username, password_hash, display_name, icon_url, search_platforms, last_played_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID.String(), u.Username, u.PasswordHash, u.DisplayName, u.IconURL, platforms, u.LastPlayedAt.UnixMilli(), u.CreatedAt.UnixMilli())
	return mapErr(err)
}

const userColumns = `id, username, password_hash, display_name, icon_url, icon_version, search_platforms, last_played_at, created_at`

// User returns the account by id.
func (d *DB) User(ctx context.Context, id uuid.UUID) (*User, error) {
	return scanUser(d.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE id = ?`, id.String()))
}

// UserByUsername returns the account by username.
func (d *DB) UserByUsername(ctx context.Context, username string) (*User, error) {
	return scanUser(d.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE username = ?`, username))
}

func scanUser(row rowScanner) (*User, error) {
	var (
		u          User
		id         string
		searchRaw  string
		created    int64
		lastPlayed int64
	)
	if err := row.Scan(&id, &u.Username, &u.PasswordHash, &u.DisplayName, &u.IconURL, &u.IconVersion, &searchRaw, &lastPlayed, &created); err != nil {
		return nil, mapErr(err)
	}
	var err error
	if u.ID, err = parseUUID(id); err != nil {
		return nil, err
	}
	if searchRaw != "" {
		if err := json.Unmarshal([]byte(searchRaw), &u.SearchPlatforms); err != nil {
			return nil, fmt.Errorf("store: bad search_platforms for user %s: %w", id, err)
		}
	}
	u.CreatedAt = time.UnixMilli(created).UTC()
	u.LastPlayedAt = time.UnixMilli(lastPlayed).UTC()
	return &u, nil
}

// CreateSession stores a hashed bearer token.
func (d *DB) CreateSession(ctx context.Context, s *Session) error {
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now()
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		s.TokenHash, s.UserID.String(), s.CreatedAt.UnixMilli(), s.ExpiresAt.UnixMilli())
	return mapErr(err)
}

// Session looks up a session by token hash. Expiry is the caller's business.
func (d *DB) Session(ctx context.Context, tokenHash string) (*Session, error) {
	var (
		s                  Session
		userID             string
		created, expiresAt int64
	)
	err := d.db.QueryRowContext(ctx,
		`SELECT token_hash, user_id, created_at, expires_at FROM sessions WHERE token_hash = ?`, tokenHash).
		Scan(&s.TokenHash, &userID, &created, &expiresAt)
	if err != nil {
		return nil, mapErr(err)
	}
	var err2 error
	if s.UserID, err2 = parseUUID(userID); err2 != nil {
		return nil, err2
	}
	s.CreatedAt = time.UnixMilli(created).UTC()
	s.ExpiresAt = time.UnixMilli(expiresAt).UTC()
	return &s, nil
}

// DeleteSession revokes a session (logout).
func (d *DB) DeleteSession(ctx context.Context, tokenHash string) error {
	res, err := d.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	if err != nil {
		return mapErr(err)
	}
	return rowsAffectedOrNotFound(res)
}

// DeleteExpiredSessions prunes sessions that expired at or before now.
func (d *DB) DeleteExpiredSessions(ctx context.Context, now time.Time) (int, error) {
	res, err := d.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, now.UnixMilli())
	if err != nil {
		return 0, mapErr(err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// AddFavorite is idempotent: favoriting twice is not an error.
func (d *DB) AddFavorite(ctx context.Context, userID, trackID uuid.UUID) error {
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO favorites (user_id, track_id, created_at) VALUES (?, ?, ?)
		ON CONFLICT (user_id, track_id) DO NOTHING`,
		userID.String(), trackID.String(), time.Now().UnixMilli())
	return mapErr(err)
}

// RemoveFavorite reports ErrNotFound when the favorite did not exist.
func (d *DB) RemoveFavorite(ctx context.Context, userID, trackID uuid.UUID) error {
	res, err := d.db.ExecContext(ctx,
		`DELETE FROM favorites WHERE user_id = ? AND track_id = ?`, userID.String(), trackID.String())
	if err != nil {
		return mapErr(err)
	}
	return rowsAffectedOrNotFound(res)
}

// Favorites lists a user's favorited track ids, newest first.
func (d *DB) Favorites(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT track_id FROM favorites WHERE user_id = ? ORDER BY created_at DESC`, userID.String())
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var ids []uuid.UUID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, mapErr(err)
		}
		parsed, err := parseUUID(id)
		if err != nil {
			return nil, err
		}
		ids = append(ids, parsed)
	}
	return ids, mapErr(rows.Err())
}

// SetRanking replaces a user's ordered provider preference.
func (d *DB) SetRanking(ctx context.Context, userID uuid.UUID, providers []string) error {
	return d.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM provider_rankings WHERE user_id = ?`, userID.String()); err != nil {
			return mapErr(err)
		}
		for i, provider := range providers {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO provider_rankings (user_id, provider, position) VALUES (?, ?, ?)`,
				userID.String(), provider, i); err != nil {
				return mapErr(err)
			}
		}
		return nil
	})
}

// Ranking returns a user's provider order, empty when unranked.
func (d *DB) Ranking(ctx context.Context, userID uuid.UUID) ([]string, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT provider FROM provider_rankings WHERE user_id = ? ORDER BY position`, userID.String())
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var providers []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, mapErr(err)
		}
		providers = append(providers, p)
	}
	return providers, mapErr(rows.Err())
}

// AllRankings returns every user's provider order. It feeds the aggregate.
func (d *DB) AllRankings(ctx context.Context) (map[uuid.UUID][]string, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT user_id, provider FROM provider_rankings ORDER BY user_id, position`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	out := make(map[uuid.UUID][]string)
	for rows.Next() {
		var userID, provider string
		if err := rows.Scan(&userID, &provider); err != nil {
			return nil, mapErr(err)
		}
		parsed, err := parseUUID(userID)
		if err != nil {
			return nil, err
		}
		out[parsed] = append(out[parsed], provider)
	}
	return out, mapErr(rows.Err())
}
