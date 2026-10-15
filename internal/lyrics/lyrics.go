// Package lyrics resolves one rendition's words and remembers what it found.
//
// A song's words belong to a rendition, not to the canonical track: two copies
// of the same song can be different lengths, and the timings a source writes
// down belong to the copy they were written against. The service therefore asks
// about a particular variant when the caller knows one, and caches its answer
// per (track, variant).
package lyrics

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kyleraykbs/MusOak/internal/store"
)

// Query is what a lookup asks a source about. DurationMs is the rendition's
// own length, because a source matches on it.
type Query struct {
	Artist     string
	Track      string
	Album      string
	DurationMs int64
}

// Result is what a source answered. Instrumental is a real answer: the song
// has no words by design, not that the lookup failed.
type Result struct {
	Synced       []store.LyricLine
	Plain        string
	Instrumental bool
}

// Fetcher looks words up somewhere. It exists so the service can be tested
// without the network, and so a second source could be swapped in.
type Fetcher interface {
	Fetch(ctx context.Context, q Query) (Result, error)
}

// TrackReader is the slice of the library a query is built from.
type TrackReader interface {
	Track(ctx context.Context, id uuid.UUID) (*store.Track, error)
	TrackArtists(ctx context.Context, id uuid.UUID) ([]store.Artist, error)
	TrackAlbums(ctx context.Context, id uuid.UUID) ([]store.Album, error)
}

// VariantReader reads the rendition whose duration the query carries.
type VariantReader interface {
	Variant(ctx context.Context, id uuid.UUID) (*store.Variant, error)
}

// Store is everything the service needs to persist against: the cache plus the
// metadata that builds a lookup.
type Store interface {
	store.LyricsRepo
	TrackReader
	VariantReader
}

// Sources the service records on what it caches.
const (
	sourceLRCLIB             = "lrclib"
	sourceLRCLIBInstrumental = "lrclib-instrumental"
	// sourceLRCLIBNone is the source of the answer "asked and there are none":
	// a real answer, unlike a failed lookup, and cached so a song without words
	// is not fetched again on every open.
	sourceLRCLIBNone = "lrclib-none"
)

// Bound on one lookup. A caller that passes its own deadline keeps it; this is
// the service's own leash, so a source that has stopped answering cannot hold a
// lookup - and the goroutine that asked for it - open for the caller's whole
// patience.
const fetchTimeout = 10 * time.Second

// The failure breaker. A source that has just failed this many times in a row
// is left alone for the cooldown: a dead or hanging LRCLIB then costs a couple
// of lookups per cooldown instead of one per lyric-less song opened in a burst.
//
// Nothing is written to the cache while it is out, so a failure stays what it
// is - an unanswered question - rather than becoming the answer "this song has
// no words".
const (
	breakerFailures = 2
	breakerCooldown = 30 * time.Second
)

// maxConcurrentFetches bounds how many lookups are in flight at once. A burst
// of songs opened together then queues behind a handful of source calls rather
// than opening one connection per song, and a caller that cannot get a slot
// within its own deadline gives up cheaply instead of piling on.
const maxConcurrentFetches = 4

// Service resolves one rendition's lyrics and caches the answer.
type Service struct {
	store   Store
	fetcher Fetcher
	logger  *slog.Logger

	// now is what the breaker's cooldown is measured against.
	now func() time.Time
	// fetchTimeout bounds one lookup, so the service is bounded even when its
	// caller asks with no deadline at all.
	fetchTimeout time.Duration
	// slots is the concurrency bound above.
	slots chan struct{}

	breakerMu  sync.Mutex
	failures   int
	retryAfter time.Time
}

// New returns a service that reads and writes its cache through store and asks
// fetcher for anything the cache does not have.
func New(store Store, fetcher Fetcher, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		store:        store,
		fetcher:      fetcher,
		logger:       logger,
		now:          time.Now,
		fetchTimeout: fetchTimeout,
		slots:        make(chan struct{}, maxConcurrentFetches),
	}
}

// Resolve returns one rendition's lyrics, or nil when the lookup failed. A
// source that answers nothing is an answer - this song has no words - and it is
// cached like an instrumental, so a song without lyrics is not asked about
// again on every open. A source that fails is not an answer at all: nothing is
// cached, and the next open tries again.
//
// A source that has just failed repeatedly is not asked again for a short
// while, which is also not an answer: the lookup fails cheaply (see the breaker
// above) and the next open after the cooldown asks again.
//
// The only errors that leave this method are the store's: an unknown track, or
// a cache that could not be read.
func (s *Service) Resolve(ctx context.Context, trackID, variantID uuid.UUID) (*store.Lyrics, error) {
	if cached, err := s.store.Lyrics(ctx, trackID, variantID); err == nil {
		return cached, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if !s.asking() {
		// The source is out. Nothing is cached for a question that was never
		// put to it, and the caller is told what a failed lookup tells it.
		return nil, nil
	}

	query, err := s.query(ctx, trackID, variantID)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, s.fetchTimeout)
	defer cancel()
	if !s.enter(ctx) {
		// No slot within the deadline: the answer is not worth waiting for.
		return nil, nil
	}
	defer s.leave()

	result, err := s.fetcher.Fetch(ctx, query)
	if err != nil {
		s.failed()
		s.warn("lyrics fetch failed", "track", trackID, "variant", variantID, "error", err)
		return nil, nil
	}
	s.answered()
	if result.Instrumental {
		found := &store.Lyrics{
			TrackID:    trackID,
			VariantID:  variantID,
			DurationMs: query.DurationMs,
			Source:     sourceLRCLIBInstrumental,
			FetchedAt:  time.Now(),
		}
		s.save(ctx, found)
		return found, nil
	}
	plain := strings.TrimSpace(result.Plain)
	if len(result.Synced) == 0 && plain == "" {
		// The source was asked and had nothing. That is an answer, and it is
		// cached under its own source so the cache hit above short-circuits the
		// next open instead of paying the fetch again.
		found := &store.Lyrics{
			TrackID:    trackID,
			VariantID:  variantID,
			DurationMs: query.DurationMs,
			Source:     sourceLRCLIBNone,
			FetchedAt:  time.Now(),
		}
		s.save(ctx, found)
		return found, nil
	}
	found := &store.Lyrics{
		TrackID:    trackID,
		VariantID:  variantID,
		DurationMs: query.DurationMs,
		Synced:     result.Synced,
		Plain:      plain,
		Source:     sourceLRCLIB,
		FetchedAt:  time.Now(),
	}
	s.save(ctx, found)
	return found, nil
}

// query builds what to ask about. It is best effort about the credits: an
// artist or album that cannot be read is one the source will not be told, but a
// track that cannot be read is a track that does not exist.
func (s *Service) query(ctx context.Context, trackID, variantID uuid.UUID) (Query, error) {
	track, err := s.store.Track(ctx, trackID)
	if err != nil {
		return Query{}, err
	}
	q := Query{Track: track.Title, DurationMs: track.DurationMs}
	// The source matches one artist string, and the first credit is the name
	// the song is filed under.
	if artists, err := s.store.TrackArtists(ctx, trackID); err == nil && len(artists) > 0 {
		q.Artist = artists[0].Name
	}
	if albums, err := s.store.TrackAlbums(ctx, trackID); err == nil && len(albums) > 0 {
		q.Album = albums[0].Title
	}
	// Timings belong to the copy they were written for, so a named rendition's
	// duration beats the track's; a copy whose length is unknown falls back.
	if variantID != uuid.Nil {
		if variant, err := s.store.Variant(ctx, variantID); err == nil && variant.DurationMs > 0 {
			q.DurationMs = variant.DurationMs
		}
	}
	return q, nil
}

// save writes the cache, but never at the cost of the answer: if it cannot be
// written the listener still gets the words.
func (s *Service) save(ctx context.Context, l *store.Lyrics) {
	if err := s.store.SaveLyrics(ctx, l); err != nil {
		s.warn("could not cache lyrics", "track", l.TrackID, "variant", l.VariantID, "error", err)
	}
}

// asking reports whether the source is worth asking now. After enough failures
// in a row it is left alone until the cooldown has passed; the first failure
// after that closes the window again, so a source that is still down is asked
// about once per cooldown rather than once per song.
func (s *Service) asking() bool {
	s.breakerMu.Lock()
	defer s.breakerMu.Unlock()
	return s.failures < breakerFailures || !s.now().Before(s.retryAfter)
}

// failed records a lookup the source did not answer, and arms the cooldown.
func (s *Service) failed() {
	s.breakerMu.Lock()
	defer s.breakerMu.Unlock()
	s.failures++
	s.retryAfter = s.now().Add(breakerCooldown)
}

// answered records that the source is alive, which is enough to close the
// breaker: one answer is what it takes to know the source is not out.
func (s *Service) answered() {
	s.breakerMu.Lock()
	defer s.breakerMu.Unlock()
	s.failures, s.retryAfter = 0, time.Time{}
}

// enter takes a slot for one lookup, or reports false when the deadline passes
// first, so a burst cannot open a source call per song.
func (s *Service) enter(ctx context.Context) bool {
	select {
	case s.slots <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// leave gives the slot back.
func (s *Service) leave() { <-s.slots }

func (s *Service) warn(message string, args ...any) {
	if s.logger != nil {
		s.logger.Warn(message, args...)
	}
}

// ParseLRC turns LRC text into timed lines.
//
// A line may carry several time tags with one piece of text ("[00:01.00][01:30.00]
// chorus"), which is more than one entry here; metadata tags such as [ar:] and
// [ti:] are not words and are skipped. The result is sorted by time, because a
// file is not obliged to be.
func ParseLRC(text string) []store.LyricLine {
	var out []store.LyricLine
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		var times []int64
		rest := line
		for strings.HasPrefix(rest, "[") {
			end := strings.IndexByte(rest, ']')
			if end < 0 {
				break
			}
			tag := rest[1:end]
			atMs, ok := parseTimestamp(tag)
			if !ok {
				// [ar:], [ti:], [offset:] and friends are not times. A line
				// that begins with one carries no words; one after a time tag
				// is left in the text and the time already read stands.
				break
			}
			times = append(times, atMs)
			rest = strings.TrimSpace(rest[end+1:])
		}
		for _, atMs := range times {
			out = append(out, store.LyricLine{AtMs: atMs, Text: rest})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].AtMs < out[j].AtMs })
	return out
}

// parseTimestamp reads "mm:ss", "mm:ss.xx" or "mm:ss.xxx". Anything else,
// including a metadata tag, is not a timestamp.
func parseTimestamp(tag string) (int64, bool) {
	colon := strings.IndexByte(tag, ':')
	if colon <= 0 {
		return 0, false
	}
	minutes, ok := parseDigits(tag[:colon])
	if !ok {
		return 0, false
	}
	rest := tag[colon+1:]
	fraction := ""
	if dot := strings.IndexAny(rest, ".:"); dot >= 0 {
		fraction, rest = rest[dot+1:], rest[:dot]
	}
	seconds, ok := parseDigits(rest)
	if !ok || seconds > 59 {
		return 0, false
	}
	var millis int64
	if fraction != "" {
		digits, ok := parseDigits(fraction)
		if !ok {
			return 0, false
		}
		switch len(fraction) {
		case 1:
			millis = digits * 100
		case 2:
			millis = digits * 10
		default:
			// Anything longer than milliseconds keeps its first three digits.
			millis = digits
			for i := 3; i < len(fraction); i++ {
				millis /= 10
			}
		}
	}
	return minutes*60_000 + seconds*1000 + millis, true
}

func parseDigits(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return int64(n), true
}
