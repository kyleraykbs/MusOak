package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// schemaV13 is the social graph: who asked whom to be friends, who is
// ignored, what was shared and what the bell shows.
//
// A pair of accounts has at most one friendships row whichever way the
// request went: while it is pending, user_id asked friend_id; once accepted
// the row means the two are friends and every query looks at both columns.
// The users.last_played_at column this feature reads for "online" was added
// by schemaV12.
const schemaV13 = `
CREATE TABLE friendships (
  user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  friend_id  TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  state      TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (user_id, friend_id)
);
CREATE TABLE user_ignores (
  user_id   TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  ignored_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  PRIMARY KEY (user_id, ignored_id)
);
CREATE TABLE shares (
  id         TEXT PRIMARY KEY,
  from_user  TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  to_user    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  track_id   TEXT REFERENCES tracks(id) ON DELETE CASCADE,
  room_id    TEXT,
  created_at INTEGER NOT NULL
);
CREATE TABLE notifications (
  id         TEXT PRIMARY KEY,
  user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  kind       TEXT NOT NULL,
  from_user  TEXT,
  track_id   TEXT,
  room_id    TEXT,
  created_at INTEGER NOT NULL,
  read       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX friendships_friend ON friendships(friend_id);
CREATE INDEX user_ignores_ignored ON user_ignores(ignored_id);
CREATE INDEX shares_from ON shares(from_user);
CREATE INDEX shares_to ON shares(to_user);
CREATE INDEX shares_track ON shares(track_id);
CREATE INDEX notifications_user ON notifications(user_id);
`

// Friendship states as they live in the friendships row. 'pending' points
// from the requester to the target; 'accepted' is symmetric.
const (
	statePending  = "pending"
	stateAccepted = "accepted"
)

// Relationship is how one account relates to another, named the way the API
// names it.
type Relationship string

const (
	RelNone       Relationship = "none"
	RelFriend     Relationship = "friend"
	RelPendingIn  Relationship = "pending-in"
	RelPendingOut Relationship = "pending-out"
	RelIgnored    Relationship = "ignored"
)

// Share is one thing a user sent to another: a song or a room invite. TrackID
// and RoomID are alternatives — one of the two is set.
type Share struct {
	ID        uuid.UUID
	FromUser  uuid.UUID
	ToUser    uuid.UUID
	TrackID   *uuid.UUID
	RoomID    string
	CreatedAt time.Time
}

// Notification is one entry in a user's bell. FromUser is who caused it; it
// is only nil for entries no user caused.
type Notification struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	Kind      string // "share" or "room-invite"
	FromUser  *uuid.UUID
	TrackID   *uuid.UUID
	RoomID    string
	CreatedAt time.Time
	Read      bool
}

// maxNotifications is what a bell listing carries. There is no paging to
// scroll further: the interesting notifications are the recent ones.
const maxNotifications = 100

// publicUserColumns are the account columns anyone may see. The password hash
// is deliberately not among them: these rows end up in other people's API
// responses.
const publicUserColumns = `id, username, display_name, icon_url, last_played_at, created_at`

func scanPublicUser(row rowScanner) (*User, error) {
	var (
		u          User
		id         string
		created    int64
		lastPlayed int64
	)
	if err := row.Scan(&id, &u.Username, &u.DisplayName, &u.IconURL, &lastPlayed, &created); err != nil {
		return nil, mapErr(err)
	}
	var err error
	if u.ID, err = parseUUID(id); err != nil {
		return nil, err
	}
	u.CreatedAt = time.UnixMilli(created).UTC()
	// An account that never played anything has no time to show, rather than
	// the epoch one.
	if lastPlayed > 0 {
		u.LastPlayedAt = time.UnixMilli(lastPlayed).UTC()
	}
	return &u, nil
}

func scanPublicUsers(rows *sql.Rows) ([]User, error) {
	defer rows.Close()
	users := []User{}
	for rows.Next() {
		u, err := scanPublicUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, *u)
	}
	return users, mapErr(rows.Err())
}

// placeholders renders n SQL parameters for an IN list.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// SendFriendRequest asks friendID to be friends. A pair has at most one row:
// asking again, or asking somebody who already asked you, is a conflict
// rather than a second request chasing the first.
func (d *DB) SendFriendRequest(ctx context.Context, userID, friendID uuid.UUID) error {
	if userID == friendID {
		return errors.New("store: cannot friend yourself")
	}
	res, err := d.execRetry(ctx, `
		INSERT INTO friendships (user_id, friend_id, state, created_at)
		SELECT ?, ?, ?, ?
		WHERE NOT EXISTS (
			SELECT 1 FROM friendships
			WHERE (user_id = ? AND friend_id = ?) OR (user_id = ? AND friend_id = ?)
		)`,
		userID.String(), friendID.String(), statePending, time.Now().UnixMilli(),
		userID.String(), friendID.String(), friendID.String(), userID.String())
	if err != nil {
		return mapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: a request or friendship already exists", ErrConflict)
	}
	return nil
}

// AcceptFriendRequest accepts the request requesterID sent to userID.
func (d *DB) AcceptFriendRequest(ctx context.Context, userID, requesterID uuid.UUID) error {
	res, err := d.execRetry(ctx,
		`UPDATE friendships SET state = ? WHERE user_id = ? AND friend_id = ? AND state = ?`,
		stateAccepted, requesterID.String(), userID.String(), statePending)
	if err != nil {
		return mapErr(err)
	}
	return rowsAffectedOrNotFound(res)
}

// RemoveFriend drops whatever exists between the two accounts. Declining a
// request is the same action as unfriending: the row goes away either way.
func (d *DB) RemoveFriend(ctx context.Context, userID, friendID uuid.UUID) error {
	res, err := d.execRetry(ctx, `
		DELETE FROM friendships
		WHERE (user_id = ? AND friend_id = ?) OR (user_id = ? AND friend_id = ?)`,
		userID.String(), friendID.String(), friendID.String(), userID.String())
	if err != nil {
		return mapErr(err)
	}
	return rowsAffectedOrNotFound(res)
}

// Friends lists the accepted friends, most recently added first.
func (d *DB) Friends(ctx context.Context, userID uuid.UUID) ([]User, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT u.id, u.username, u.display_name, u.icon_url, u.last_played_at, u.created_at
		FROM friendships f
		JOIN users u ON u.id = CASE WHEN f.user_id = ? THEN f.friend_id ELSE f.user_id END
		WHERE (f.user_id = ? OR f.friend_id = ?) AND f.state = ?
		ORDER BY f.created_at DESC`,
		userID.String(), userID.String(), userID.String(), stateAccepted)
	if err != nil {
		return nil, mapErr(err)
	}
	return scanPublicUsers(rows)
}

// IncomingFriendRequests lists the people waiting for userID's answer.
func (d *DB) IncomingFriendRequests(ctx context.Context, userID uuid.UUID) ([]User, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT u.id, u.username, u.display_name, u.icon_url, u.last_played_at, u.created_at
		FROM friendships f
		JOIN users u ON u.id = f.user_id
		WHERE f.friend_id = ? AND f.state = ?
		ORDER BY f.created_at DESC`,
		userID.String(), statePending)
	if err != nil {
		return nil, mapErr(err)
	}
	return scanPublicUsers(rows)
}

// CountIncomingFriendRequests is how many people are waiting on an answer: the
// number the Friends tab carries before it is ever opened.
func (d *DB) CountIncomingFriendRequests(ctx context.Context, userID uuid.UUID) (int, error) {
	var count int
	err := d.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM friendships WHERE friend_id = ? AND state = ?`,
		userID.String(), statePending).Scan(&count)
	if err != nil {
		return 0, mapErr(err)
	}
	return count, nil
}

// OutgoingFriendRequests lists the requests userID is still waiting on.
func (d *DB) OutgoingFriendRequests(ctx context.Context, userID uuid.UUID) ([]User, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT u.id, u.username, u.display_name, u.icon_url, u.last_played_at, u.created_at
		FROM friendships f
		JOIN users u ON u.id = f.friend_id
		WHERE f.user_id = ? AND f.state = ?
		ORDER BY f.created_at DESC`,
		userID.String(), statePending)
	if err != nil {
		return nil, mapErr(err)
	}
	return scanPublicUsers(rows)
}

// IgnoreUser silences ignoredID: their shares stop reaching the bell.
// Ignoring twice is not an error.
func (d *DB) IgnoreUser(ctx context.Context, userID, ignoredID uuid.UUID) error {
	_, err := d.execRetry(ctx,
		`INSERT INTO user_ignores (user_id, ignored_id) VALUES (?, ?)
		 ON CONFLICT (user_id, ignored_id) DO NOTHING`,
		userID.String(), ignoredID.String())
	return mapErr(err)
}

// UnignoreUser reports ErrNotFound when they were not ignored in the first
// place.
func (d *DB) UnignoreUser(ctx context.Context, userID, ignoredID uuid.UUID) error {
	res, err := d.execRetry(ctx,
		`DELETE FROM user_ignores WHERE user_id = ? AND ignored_id = ?`,
		userID.String(), ignoredID.String())
	if err != nil {
		return mapErr(err)
	}
	return rowsAffectedOrNotFound(res)
}

// SearchUsers lists accounts whose username or display name contains query,
// case-insensitively. The rows carry no password hashes: they are what other
// people get to see.
func (d *DB) SearchUsers(ctx context.Context, query string, limit int) ([]User, error) {
	if limit <= 0 {
		limit = 20
	}
	// LIKE is case-insensitive for the names people actually type; the
	// wildcard characters in the query are escaped so they match themselves.
	pattern := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(query) + "%"
	rows, err := d.db.QueryContext(ctx, `
		SELECT `+publicUserColumns+` FROM users
		WHERE username LIKE ? ESCAPE '\' OR display_name LIKE ? ESCAPE '\'
		ORDER BY username
		LIMIT ?`, pattern, pattern, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	return scanPublicUsers(rows)
}

// CountUsers is how many accounts the server holds.
func (d *DB) CountUsers(ctx context.Context) (int, error) {
	var total int
	if err := d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&total); err != nil {
		return 0, mapErr(err)
	}
	return total, nil
}

// UserForViewer returns one account and how it relates to the viewer.
func (d *DB) UserForViewer(ctx context.Context, viewerID, userID uuid.UUID) (*User, Relationship, error) {
	user, err := scanPublicUser(d.db.QueryRowContext(ctx,
		`SELECT `+publicUserColumns+` FROM users WHERE id = ?`, userID.String()))
	if err != nil {
		return nil, RelNone, err
	}
	relationships, err := d.Relationships(ctx, viewerID, []uuid.UUID{userID})
	if err != nil {
		return nil, RelNone, err
	}
	return user, relationships[userID], nil
}

// Relationships computes the viewer's relationship to many accounts at once,
// so a listing can label every row without a query each. The viewer relates
// to itself as nobody in particular.
func (d *DB) Relationships(ctx context.Context, viewerID uuid.UUID, userIDs []uuid.UUID) (map[uuid.UUID]Relationship, error) {
	out := make(map[uuid.UUID]Relationship, len(userIDs)+1)
	out[viewerID] = RelNone
	ids := make([]string, 0, len(userIDs))
	for _, id := range userIDs {
		out[id] = RelNone
		if id != viewerID {
			ids = append(ids, id.String())
		}
	}
	if len(ids) == 0 {
		return out, nil
	}

	args := func() []any {
		a := make([]any, 0, len(ids)+1)
		a = append(a, viewerID.String())
		for _, id := range ids {
			a = append(a, id)
		}
		return a
	}

	// Ignoring is the viewer's own decision and outranks everything else:
	// it is what the ignore button reflects.
	irows, err := d.db.QueryContext(ctx,
		`SELECT ignored_id FROM user_ignores
		 WHERE user_id = ? AND ignored_id IN (`+placeholders(len(ids))+`)`, args()...)
	if err != nil {
		return nil, mapErr(err)
	}
	for irows.Next() {
		var id string
		if err := irows.Scan(&id); err != nil {
			irows.Close()
			return nil, mapErr(err)
		}
		parsed, err := parseUUID(id)
		if err != nil {
			irows.Close()
			return nil, err
		}
		out[parsed] = RelIgnored
	}
	if err := irows.Err(); err != nil {
		irows.Close()
		return nil, mapErr(err)
	}
	irows.Close()

	rows, err := d.db.QueryContext(ctx, `
		SELECT user_id, friend_id, state FROM friendships
		WHERE (user_id = ? AND friend_id IN (`+placeholders(len(ids))+`))
		   OR (friend_id = ? AND user_id IN (`+placeholders(len(ids))+`))`,
		append(args(), args()...)...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var userID, friendID, state string
		if err := rows.Scan(&userID, &friendID, &state); err != nil {
			return nil, mapErr(err)
		}
		other := friendID
		if other == viewerID.String() {
			other = userID
		}
		parsed, err := parseUUID(other)
		if err != nil {
			return nil, err
		}
		if out[parsed] != RelNone {
			continue // ignored outranks the friendship
		}
		switch {
		case state == stateAccepted:
			out[parsed] = RelFriend
		case userID == viewerID.String():
			out[parsed] = RelPendingOut
		default:
			out[parsed] = RelPendingIn
		}
	}
	return out, mapErr(rows.Err())
}

// RecordShare inserts share, generating its ID and timestamp when unset.
func (d *DB) RecordShare(ctx context.Context, share *Share) error {
	if share.ID == uuid.Nil {
		share.ID = uuid.New()
	}
	if share.CreatedAt.IsZero() {
		share.CreatedAt = time.Now()
	}
	_, err := d.execRetry(ctx, `
		INSERT INTO shares (id, from_user, to_user, track_id, room_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		share.ID.String(), share.FromUser.String(), share.ToUser.String(),
		optionalUUID(share.TrackID), share.RoomID, share.CreatedAt.UnixMilli())
	return mapErr(err)
}

// SharesBetween lists what from sent to to, newest first.
func (d *DB) SharesBetween(ctx context.Context, from, to uuid.UUID) ([]Share, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, from_user, to_user, track_id, room_id, created_at FROM shares
		WHERE from_user = ? AND to_user = ?
		ORDER BY created_at DESC, rowid DESC`,
		from.String(), to.String())
	if err != nil {
		return nil, mapErr(err)
	}
	return scanShares(rows)
}

// UserShares lists every share that touches the user, sent or received,
// newest first.
func (d *DB) UserShares(ctx context.Context, userID uuid.UUID) ([]Share, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, from_user, to_user, track_id, room_id, created_at FROM shares
		WHERE from_user = ? OR to_user = ?
		ORDER BY created_at DESC, rowid DESC`,
		userID.String(), userID.String())
	if err != nil {
		return nil, mapErr(err)
	}
	return scanShares(rows)
}

func scanShares(rows *sql.Rows) ([]Share, error) {
	defer rows.Close()
	shares := []Share{}
	for rows.Next() {
		var (
			share           Share
			id, from, to    string
			trackID, roomID sql.NullString
			created         int64
		)
		if err := rows.Scan(&id, &from, &to, &trackID, &roomID, &created); err != nil {
			return nil, mapErr(err)
		}
		var err error
		if share.ID, err = parseUUID(id); err != nil {
			return nil, err
		}
		if share.FromUser, err = parseUUID(from); err != nil {
			return nil, err
		}
		if share.ToUser, err = parseUUID(to); err != nil {
			return nil, err
		}
		if trackID.Valid {
			parsed, err := parseUUID(trackID.String)
			if err != nil {
				return nil, err
			}
			share.TrackID = &parsed
		}
		share.RoomID = roomID.String
		share.CreatedAt = time.UnixMilli(created).UTC()
		shares = append(shares, share)
	}
	return shares, mapErr(rows.Err())
}

// CreateNotification records n for its recipient, unless the recipient
// ignores the sender — an ignored person's shares never reach the bell. It
// reports whether the entry was created.
func (d *DB) CreateNotification(ctx context.Context, n *Notification) (bool, error) {
	if n.FromUser != nil {
		var ignored int
		err := d.db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM user_ignores WHERE user_id = ? AND ignored_id = ?)`,
			n.UserID.String(), n.FromUser.String()).Scan(&ignored)
		if err != nil {
			return false, mapErr(err)
		}
		if ignored != 0 {
			return false, nil
		}
	}
	if n.ID == uuid.Nil {
		n.ID = uuid.New()
	}
	if n.CreatedAt.IsZero() {
		n.CreatedAt = time.Now()
	}
	_, err := d.execRetry(ctx, `
		INSERT INTO notifications (id, user_id, kind, from_user, track_id, room_id, created_at, read)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0)`,
		n.ID.String(), n.UserID.String(), n.Kind, optionalUUID(n.FromUser),
		optionalUUID(n.TrackID), n.RoomID, n.CreatedAt.UnixMilli())
	if err != nil {
		return false, mapErr(err)
	}
	return true, nil
}

// Notifications lists the recipient's bell entries, newest first, with how
// many are unread.
func (d *DB) Notifications(ctx context.Context, userID uuid.UUID) ([]Notification, int, error) {
	var unread int
	err := d.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM notifications WHERE user_id = ? AND read = 0`,
		userID.String()).Scan(&unread)
	if err != nil {
		return nil, 0, mapErr(err)
	}

	rows, err := d.db.QueryContext(ctx, `
		SELECT id, user_id, kind, from_user, track_id, room_id, created_at, read
		FROM notifications
		WHERE user_id = ?
		ORDER BY created_at DESC, rowid DESC
		LIMIT ?`, userID.String(), maxNotifications)
	if err != nil {
		return nil, 0, mapErr(err)
	}
	defer rows.Close()

	entries := []Notification{}
	for rows.Next() {
		var (
			n                     Notification
			id, userIDStr         string
			from, trackID, roomID sql.NullString
			created               int64
			read                  int
		)
		if err := rows.Scan(&id, &userIDStr, &n.Kind, &from, &trackID, &roomID, &created, &read); err != nil {
			return nil, 0, mapErr(err)
		}
		var err error
		if n.ID, err = parseUUID(id); err != nil {
			return nil, 0, err
		}
		if n.UserID, err = parseUUID(userIDStr); err != nil {
			return nil, 0, err
		}
		if from.Valid {
			parsed, err := parseUUID(from.String)
			if err != nil {
				return nil, 0, err
			}
			n.FromUser = &parsed
		}
		if trackID.Valid {
			parsed, err := parseUUID(trackID.String)
			if err != nil {
				return nil, 0, err
			}
			n.TrackID = &parsed
		}
		n.RoomID = roomID.String
		n.CreatedAt = time.UnixMilli(created).UTC()
		n.Read = read != 0
		entries = append(entries, n)
	}
	return entries, unread, mapErr(rows.Err())
}

// MarkNotificationsRead marks every entry read, emptying the bell's badge.
func (d *DB) MarkNotificationsRead(ctx context.Context, userID uuid.UUID) error {
	_, err := d.execRetry(ctx,
		`UPDATE notifications SET read = 1 WHERE user_id = ?`, userID.String())
	return mapErr(err)
}

// RecentPlayback returns the stored playback document and when it was saved.
// The friends endpoints need the timestamp to tell "listening now" from "was
// listening once".
func (d *DB) RecentPlayback(ctx context.Context, userID uuid.UUID) ([]byte, time.Time, error) {
	var (
		state   string
		updated int64
	)
	err := d.db.QueryRowContext(ctx,
		`SELECT state, updated_at FROM playback_state WHERE user_id = ?`,
		userID.String()).Scan(&state, &updated)
	if err != nil {
		return nil, time.Time{}, mapErr(err)
	}
	return []byte(state), time.UnixMilli(updated).UTC(), nil
}

// optionalUUID renders an optional id as a SQL value.
func optionalUUID(id *uuid.UUID) any {
	if id == nil {
		return nil
	}
	return id.String()
}
