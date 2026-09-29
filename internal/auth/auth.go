// Package auth owns accounts: argon2id password hashing, bearer-token
// sessions, the registration policy and identity resolution.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/argon2"

	"codeberg.org/kyleraykbs/prismusic/internal/config"
	"codeberg.org/kyleraykbs/prismusic/internal/store"
)

// SessionTTL is how long a bearer token stays valid.
const SessionTTL = 30 * 24 * time.Hour

// argon2id parameters: 64 MiB, three passes, two lanes.
const (
	argonMemoryKiB = 64 * 1024
	argonTime      = 3
	argonThreads   = 2
	argonKeyLen    = 32
	argonSaltLen   = 16
)

// minPasswordLen is the shortest accepted password. A password only has to be
// non-empty: length policy belongs to whoever runs the server, and the cost of
// guessing is argon2id plus the login rate limit, not a minimum.
const minPasswordLen = 1

var (
	// ErrRegistrationClosed means registrationOpen is false.
	ErrRegistrationClosed = errors.New("registration is closed")
	// ErrUsernameTaken means the username already exists.
	ErrUsernameTaken = errors.New("username is taken")
	// ErrInvalidCredentials covers a wrong username and a wrong password.
	ErrInvalidCredentials = errors.New("invalid username or password")
	// ErrInvalidToken means the bearer token is unknown or expired.
	ErrInvalidToken = errors.New("invalid or expired token")
	// ErrInvalidInput means the username or password fails the policy.
	ErrInvalidInput = errors.New("invalid username or password format")
)

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{3,32}$`)

// Store is the repository slice accounts need.
type Store interface {
	store.UserRepo
	store.SessionRepo
}

// Service authenticates users and issues sessions.
type Service struct {
	db     Store
	cfg    *config.Config
	logger *slog.Logger
	now    func() time.Time

	dummyOnce sync.Once
	dummyHash string
}

// New returns the auth service.
func New(cfg *config.Config, db Store, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{db: db, cfg: cfg, logger: logger, now: time.Now}
}

// RegistrationOpen reports whether new accounts are accepted.
func (s *Service) RegistrationOpen() bool { return s.cfg.RegistrationOpen }

// RequireLogin reports whether unauthenticated requests are rejected outright.
func (s *Service) RequireLogin() bool { return s.cfg.RequireLogin }

// Register creates an account and returns it with a fresh session token.
func (s *Service) Register(ctx context.Context, username, password string) (*store.User, string, error) {
	if !s.cfg.RegistrationOpen {
		return nil, "", ErrRegistrationClosed
	}
	if err := validateCredentials(username, password); err != nil {
		return nil, "", err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return nil, "", err
	}
	user := &store.User{Username: username, PasswordHash: hash}
	if err := s.db.CreateUser(ctx, user); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, "", ErrUsernameTaken
		}
		return nil, "", err
	}
	token, err := s.issue(ctx, user.ID)
	if err != nil {
		return nil, "", err
	}
	s.logger.Info("user registered", "user", user.Username)
	return user, token, nil
}

// Login verifies credentials and returns a fresh session token.
func (s *Service) Login(ctx context.Context, username, password string) (*store.User, string, error) {
	user, err := s.db.UserByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Spend comparable time on unknown users so the endpoint does not
			// leak which usernames exist.
			_, _ = VerifyPassword(password, s.dummy())
			return nil, "", ErrInvalidCredentials
		}
		return nil, "", err
	}
	ok, err := VerifyPassword(password, user.PasswordHash)
	if err != nil {
		s.logger.Error("stored password hash is malformed", "user", user.Username, "error", err)
		return nil, "", ErrInvalidCredentials
	}
	if !ok {
		return nil, "", ErrInvalidCredentials
	}
	token, err := s.issue(ctx, user.ID)
	if err != nil {
		return nil, "", err
	}
	return user, token, nil
}

// Logout revokes one session.
func (s *Service) Logout(ctx context.Context, token string) error {
	if strings.TrimSpace(token) == "" {
		return ErrInvalidToken
	}
	return s.db.DeleteSession(ctx, HashToken(token))
}

// Authenticate resolves a bearer token to a user.
func (s *Service) Authenticate(ctx context.Context, token string) (*store.User, error) {
	if strings.TrimSpace(token) == "" {
		return nil, ErrInvalidToken
	}
	session, err := s.db.Session(ctx, HashToken(token))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrInvalidToken
		}
		return nil, err
	}
	if !session.ExpiresAt.After(s.now()) {
		_ = s.db.DeleteSession(ctx, session.TokenHash)
		return nil, ErrInvalidToken
	}
	user, err := s.db.User(ctx, session.UserID)
	if err != nil {
		return nil, ErrInvalidToken
	}
	return user, nil
}

// issue creates a session for userID and returns the raw token.
func (s *Service) issue(ctx context.Context, userID uuid.UUID) (string, error) {
	token, err := newToken()
	if err != nil {
		return "", err
	}
	now := s.now()
	session := &store.Session{
		TokenHash: HashToken(token),
		UserID:    userID,
		CreatedAt: now,
		ExpiresAt: now.Add(SessionTTL),
	}
	if err := s.db.CreateSession(ctx, session); err != nil {
		return "", err
	}
	if pruned, err := s.db.DeleteExpiredSessions(ctx, now); err != nil {
		s.logger.Warn("auth: prune sessions", "error", err)
	} else if pruned > 0 {
		s.logger.Debug("auth: pruned expired sessions", "count", pruned)
	}
	return token, nil
}

// dummy returns a hash of a random password, used to equalise login timing.
func (s *Service) dummy() string {
	s.dummyOnce.Do(func() {
		password, err := newToken()
		if err != nil {
			s.dummyHash = ""
			return
		}
		hash, err := HashPassword(password)
		if err != nil {
			s.dummyHash = ""
			return
		}
		s.dummyHash = hash
	})
	return s.dummyHash
}

// HashToken is the stored form of a bearer token: nothing secret is persisted.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func newToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("auth: generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func validateCredentials(username, password string) error {
	if !usernamePattern.MatchString(username) {
		return fmt.Errorf("%w: username must be 3-32 characters of letters, digits, dot, dash or underscore", ErrInvalidInput)
	}
	if len(password) < minPasswordLen {
		return fmt.Errorf("%w: password is required", ErrInvalidInput)
	}
	return nil
}

// HashPassword returns a PHC-format argon2id hash.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generate salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemoryKiB, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword checks password against a PHC-format argon2id hash.
func VerifyPassword(password, encoded string) (bool, error) {
	params, salt, want, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, params.time, params.memory, params.threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

type hashParams struct {
	memory  uint32
	time    uint32
	threads uint8
}

func decodeHash(encoded string) (hashParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return hashParams{}, nil, nil, fmt.Errorf("auth: unsupported password hash format")
	}
	if _, err := strconv.Atoi(strings.TrimPrefix(parts[2], "v=")); err != nil {
		return hashParams{}, nil, nil, fmt.Errorf("auth: bad hash version %q", parts[2])
	}

	var params hashParams
	for _, field := range strings.Split(parts[3], ",") {
		name, value, ok := strings.Cut(field, "=")
		if !ok {
			return hashParams{}, nil, nil, fmt.Errorf("auth: bad hash parameter %q", field)
		}
		n, err := strconv.Atoi(value)
		if err != nil {
			return hashParams{}, nil, nil, fmt.Errorf("auth: bad hash parameter %q", field)
		}
		switch name {
		case "m":
			params.memory = uint32(n)
		case "t":
			params.time = uint32(n)
		case "p":
			params.threads = uint8(n)
		default:
			return hashParams{}, nil, nil, fmt.Errorf("auth: unknown hash parameter %q", name)
		}
	}
	if params.memory == 0 || params.time == 0 || params.threads == 0 {
		return hashParams{}, nil, nil, fmt.Errorf("auth: incomplete hash parameters")
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return hashParams{}, nil, nil, fmt.Errorf("auth: bad salt: %w", err)
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return hashParams{}, nil, nil, fmt.Errorf("auth: bad hash: %w", err)
	}
	return params, salt, key, nil
}
