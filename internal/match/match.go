// Package match merges provider hits into canonical tracks.
//
// One recording exists once in the library; every provider's rendition of it
// (and every local file of it) hangs off the same canonical track as a
// variant. Matching is signal-based, in priority order:
//
//  1. identical ISRC — conclusive, score 1;
//  2. otherwise a weighted score over the normalized title, the artist sets,
//     the album and the duration, compared against a configured threshold.
//
// Durations more than about five seconds apart are never the same recording:
// that is a hard cut before scoring.
package match

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/prismusic/internal/provider"
	"codeberg.org/kyleraykbs/prismusic/internal/store"
)

// DurationToleranceMs is how far two renditions may differ and still be the
// same recording; provider metadata rounds to the second.
const DurationToleranceMs int64 = 5000

// Signal weights for the non-ISRC score. They sum to 1.
const (
	weightTitle    = 0.40
	weightArtists  = 0.25
	weightAlbum    = 0.15
	weightDuration = 0.20
)

// SearchTimeout bounds each provider search during resolution.
const SearchTimeout = 15 * time.Second

// resolveLimit is how many provider hits a resolution step considers.
const resolveLimit = 10

// ErrNoPlayableVariant means the track still has no downloadable rendition
// after resolution: nothing matched.
var ErrNoPlayableVariant = errors.New("no downloadable variant available")

// Candidate is one side of a comparison.
type Candidate struct {
	Title      string
	Artists    []string
	Album      string
	DurationMs int64
	ISRC       string
}

// Score returns the similarity of two candidates in [0,1]. Identical ISRCs
// short-circuit to 1; durations that cannot be the same recording return 0.
//
// Only the signals both sides actually carry are weighed: an untagged local
// file must still be able to match its track on title and duration alone,
// while two tracks that both name their artist and album are held to them.
func Score(a, b Candidate) float64 {
	if a.ISRC != "" && b.ISRC != "" && strings.EqualFold(strings.TrimSpace(a.ISRC), strings.TrimSpace(b.ISRC)) {
		return 1
	}
	if a.DurationMs > 0 && b.DurationMs > 0 && absDiff(a.DurationMs, b.DurationMs) > DurationToleranceMs {
		return 0
	}

	titleA, titleB := NormalizeTitle(a.Title), NormalizeTitle(b.Title)
	signals := []struct {
		weight float64
		value  float64
		known  bool
	}{
		{weightTitle, titleSimilarity(a.Title, b.Title), titleA != "" && titleB != ""},
		{weightArtists, artistSimilarity(a.Artists, b.Artists), len(artistSet(a.Artists)) > 0 && len(artistSet(b.Artists)) > 0},
		{weightAlbum, albumSimilarity(a.Album, b.Album), NormalizeTitle(a.Album) != "" && NormalizeTitle(b.Album) != ""},
		{weightDuration, durationSimilarity(a.DurationMs, b.DurationMs), a.DurationMs > 0 && b.DurationMs > 0},
	}

	var totalWeight, weighted float64
	for _, signal := range signals {
		if !signal.known {
			continue
		}
		totalWeight += signal.weight
		weighted += signal.weight * signal.value
	}
	if totalWeight == 0 {
		return 0
	}
	return weighted / totalWeight
}

func titleSimilarity(a, b string) float64 {
	na, nb := NormalizeTitle(a), NormalizeTitle(b)
	switch {
	case na == "" || nb == "":
		return 0
	case na == nb:
		return 1
	}
	return jaccard(tokens(na), tokens(nb))
}

func artistSimilarity(a, b []string) float64 {
	na, nb := artistSet(a), artistSet(b)
	if len(na) == 0 || len(nb) == 0 {
		// Unknown credits are neutral rather than disqualifying: an untagged
		// local file should still be able to join its track.
		return 0.5
	}
	return jaccard(na, nb)
}

func albumSimilarity(a, b string) float64 {
	na, nb := NormalizeTitle(a), NormalizeTitle(b)
	switch {
	case na == "" || nb == "":
		return 0.5
	case na == nb:
		return 1
	}
	return 0
}

func durationSimilarity(a, b int64) float64 {
	if a <= 0 || b <= 0 {
		return 0.5
	}
	diff := absDiff(a, b)
	if diff > DurationToleranceMs {
		return 0
	}
	return 1 - float64(diff)/float64(DurationToleranceMs)
}

func artistSet(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, name := range names {
		if key := NormalizeArtist(name); key != "" {
			out[key] = true
		}
	}
	return out
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if b[k] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func absDiff(a, b int64) int64 {
	if a > b {
		return a - b
	}
	return b - a
}

// Store is the repository slice matching needs: tracks and their renditions,
// plus the collection tables (albums, artists and their provider releases).
type Store interface {
	store.TrackRepo
	store.VariantRepo
	store.AlbumRepo
	store.ArtistRepo
}

// Group is a canonical track with the variants that matched onto it.
type Group struct {
	Track    store.Track
	Variants []store.Variant
}

// Matcher merges provider hits into canonical tracks.
type Matcher struct {
	db        Store
	providers *provider.Registry
	threshold float64
	logger    *slog.Logger

	// mu serialises attach operations so two simultaneous hits for one
	// recording cannot create two canonical tracks.
	mu sync.Mutex
}

// New returns a matcher using the given score threshold.
func New(db Store, providers *provider.Registry, threshold float64, logger *slog.Logger) *Matcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Matcher{db: db, providers: providers, threshold: threshold, logger: logger}
}

// Attach finds or creates the canonical track for a provider hit and stores
// the hit as a variant of it.
func (m *Matcher) Attach(ctx context.Context, providerName string, hit provider.Track) (*store.Track, *store.Variant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.attachLocked(ctx, providerName, hit)
}

// attachLocked is Attach without the lock, so Resolve can call it while it
// already holds one.
func (m *Matcher) attachLocked(ctx context.Context, providerName string, hit provider.Track) (*store.Track, *store.Variant, error) {
	// Already known: this exact provider track has a variant.
	if existing, err := m.db.VariantByProviderTrack(ctx, providerName, hit.ProviderTrackID); err == nil {
		track, err := m.db.Track(ctx, existing.TrackID)
		if err != nil {
			return nil, nil, err
		}
		return track, existing, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, nil, err
	}

	track, err := m.findTrack(ctx, hit)
	if err != nil {
		return nil, nil, err
	}
	if track == nil {
		track = &store.Track{Title: hit.Title, DurationMs: hit.DurationMs}
		if err := m.db.CreateTrack(ctx, track); err != nil {
			return nil, nil, err
		}
		if len(hit.Artists) > 0 {
			if err := m.db.SetTrackArtists(ctx, track.ID, hit.Artists); err != nil {
				return nil, nil, err
			}
		}
		if hit.Album != "" {
			if err := m.db.SetTrackAlbums(ctx, track.ID, []string{hit.Album}); err != nil {
				return nil, nil, err
			}
		}
		m.logger.Debug("created canonical track", "track", track.ID, "title", hit.Title, "provider", providerName)
	}

	variant := &store.Variant{
		TrackID:         track.ID,
		Provider:        providerName,
		ProviderTrackID: hit.ProviderTrackID,
		Title:           hit.Title,
		Artists:         hit.Artists,
		Album:           hit.Album,
		DurationMs:      hit.DurationMs,
		Downloadable:    m.canDownload(providerName),
		ISRC:            hit.ISRC,
	}
	if err := m.db.CreateVariant(ctx, variant); err != nil {
		if errors.Is(err, store.ErrConflict) {
			// Another attach won the race; use its variant.
			if existing, lookupErr := m.db.VariantByProviderTrack(ctx, providerName, hit.ProviderTrackID); lookupErr == nil {
				return track, existing, nil
			}
		}
		return nil, nil, err
	}

	// A canonical track learns its duration the first time anyone tells us.
	if track.DurationMs == 0 && hit.DurationMs > 0 {
		if err := m.db.SetTrackDuration(ctx, track.ID, hit.DurationMs); err != nil {
			m.logger.Warn("match: set track duration", "track", track.ID, "error", err)
		}
	}
	return track, variant, nil
}

// findTrack looks for the canonical track a hit belongs to, or nil.
func (m *Matcher) findTrack(ctx context.Context, hit provider.Track) (*store.Track, error) {
	if strings.TrimSpace(hit.ISRC) != "" {
		track, err := m.db.TrackByISRC(ctx, hit.ISRC)
		if err == nil {
			return track, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
	}

	candidates, err := m.db.CandidateTracks(ctx, hit.DurationMs, DurationToleranceMs, 0)
	if err != nil {
		return nil, err
	}

	var (
		best      *store.Track
		bestScore float64
	)
	for i := range candidates {
		candidate := &candidates[i]
		shape, err := m.shape(ctx, *candidate)
		if err != nil {
			return nil, err
		}
		score := Score(shape, Candidate{
			Title:      hit.Title,
			Artists:    hit.Artists,
			Album:      hit.Album,
			DurationMs: hit.DurationMs,
			ISRC:       hit.ISRC,
		})
		if score > bestScore {
			best, bestScore = candidate, score
		}
	}
	if best == nil || bestScore < m.threshold {
		if best != nil {
			m.logger.Debug("closest candidate below threshold",
				"title", hit.Title, "candidate", best.Title, "score", bestScore, "threshold", m.threshold)
		}
		return nil, nil
	}
	return best, nil
}

// shape loads a canonical track's comparison signals.
func (m *Matcher) shape(ctx context.Context, track store.Track) (Candidate, error) {
	artists, err := m.db.TrackArtists(ctx, track.ID)
	if err != nil {
		return Candidate{}, err
	}
	albums, err := m.db.TrackAlbums(ctx, track.ID)
	if err != nil {
		return Candidate{}, err
	}
	shape := Candidate{
		Title:      track.Title,
		Artists:    artistNames(artists),
		DurationMs: track.DurationMs,
	}
	if len(albums) > 0 {
		shape.Album = albums[0].Title
	}
	return shape, nil
}

// Adopt matches a freshly imported rendition against the existing library and
// re-points it at the canonical track it belongs to. The throwaway track the
// import created is removed once nothing points at it, so importing the same
// song twice does not leave two canonical tracks behind.
func (m *Matcher) Adopt(ctx context.Context, variantID uuid.UUID) (*store.Track, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	variant, err := m.db.Variant(ctx, variantID)
	if err != nil {
		return nil, err
	}
	track, err := m.db.Track(ctx, variant.TrackID)
	if err != nil {
		return nil, err
	}

	candidates, err := m.db.CandidateTracks(ctx, variant.DurationMs, DurationToleranceMs, 0)
	if err != nil {
		return nil, err
	}

	want := Candidate{
		Title:      variant.Title,
		Artists:    variant.Artists,
		Album:      variant.Album,
		DurationMs: variant.DurationMs,
		ISRC:       variant.ISRC,
	}

	var (
		best      *store.Track
		bestScore float64
	)
	for i := range candidates {
		candidate := &candidates[i]
		if candidate.ID == track.ID {
			continue
		}
		shape, err := m.shape(ctx, *candidate)
		if err != nil {
			return nil, err
		}
		if score := Score(shape, want); score > bestScore {
			best, bestScore = candidate, score
		}
	}
	if best == nil || bestScore < m.threshold {
		return track, nil
	}

	if err := m.db.SetVariantTrack(ctx, variant.ID, best.ID); err != nil {
		return nil, err
	}
	// The canonical track learns whatever its first rendition could not tell it.
	if artists, err := m.db.TrackArtists(ctx, best.ID); err == nil && len(artists) == 0 && len(variant.Artists) > 0 {
		if err := m.db.SetTrackArtists(ctx, best.ID, variant.Artists); err != nil {
			return nil, err
		}
	}
	if albums, err := m.db.TrackAlbums(ctx, best.ID); err == nil && len(albums) == 0 && variant.Album != "" {
		if err := m.db.SetTrackAlbums(ctx, best.ID, []string{variant.Album}); err != nil {
			return nil, err
		}
	}
	if best.DurationMs == 0 && variant.DurationMs > 0 {
		if err := m.db.SetTrackDuration(ctx, best.ID, variant.DurationMs); err != nil {
			m.logger.Warn("match: set track duration", "track", best.ID, "error", err)
		}
	}
	if err := m.db.DeleteTrack(ctx, track.ID); err != nil && !errors.Is(err, store.ErrConflict) {
		return nil, err
	}

	m.logger.Info("adopted imported rendition",
		"track", best.ID, "variant", variant.ID, "score", bestScore)
	return m.db.Track(ctx, best.ID)
}

// Group merges a fan-out search into canonical tracks, persisting every hit as
// a variant. Provider order is preserved: the first group encountered wins.
func (m *Matcher) Group(ctx context.Context, results []provider.Result) ([]Group, error) {
	var (
		groups []Group
		index  = make(map[uuid.UUID]int)
	)
	for _, result := range results {
		if result.Err != nil || len(result.Tracks) == 0 {
			continue
		}
		for _, hit := range result.Tracks {
			track, variant, err := m.Attach(ctx, result.Provider, hit)
			if err != nil {
				return nil, fmt.Errorf("match %s/%s: %w", result.Provider, hit.ProviderTrackID, err)
			}
			i, seen := index[track.ID]
			if !seen {
				groups = append(groups, Group{Track: *track})
				i = len(groups) - 1
				index[track.ID] = i
			}
			groups[i].Variants = append(groups[i].Variants, *variant)
		}
	}
	return groups, nil
}

// Resolve makes sure a track has at least one downloadable variant. When all
// it has is metadata-only renditions (Spotify), a matching rendition is
// searched for on the downloadable providers and attached.
func (m *Matcher) Resolve(ctx context.Context, trackID uuid.UUID) ([]store.Variant, error) {
	variants, err := m.db.VariantsForTrack(ctx, trackID)
	if err != nil {
		return nil, err
	}
	if hasDownloadable(variants) {
		return variants, nil
	}

	track, err := m.db.Track(ctx, trackID)
	if err != nil {
		return nil, err
	}
	artists, err := m.db.TrackArtists(ctx, trackID)
	if err != nil {
		return nil, err
	}
	albums, err := m.db.TrackAlbums(ctx, trackID)
	if err != nil {
		return nil, err
	}

	want := Candidate{Title: track.Title, Artists: artistNames(artists), DurationMs: track.DurationMs}
	if len(albums) > 0 {
		want.Album = albums[0].Title
	}
	// An ISRC from any existing rendition makes the search conclusive.
	for _, variant := range variants {
		if variant.ISRC != "" {
			want.ISRC = variant.ISRC
			break
		}
	}

	query := strings.TrimSpace(track.Title + " " + strings.Join(want.Artists, " "))

	for _, p := range m.providers.All() {
		if !p.Capabilities().Download {
			continue
		}
		// Searching is slow: it happens without the lock so other matches are
		// not blocked behind a network round trip.
		searchCtx, cancel := context.WithTimeout(ctx, SearchTimeout)
		hits, err := p.Search(searchCtx, query, provider.SearchOpts{Limit: resolveLimit})
		cancel()
		if err != nil {
			m.logger.Warn("match: resolution search failed", "provider", p.Name(), "track", trackID, "error", err)
			continue
		}
		best, bestScore := bestHit(want, hits)
		if best == nil || bestScore < m.threshold {
			m.logger.Debug("match: no resolution candidate above threshold",
				"provider", p.Name(), "track", trackID, "score", bestScore)
			continue
		}

		m.mu.Lock()
		current, err := m.db.VariantsForTrack(ctx, trackID)
		if err != nil {
			m.mu.Unlock()
			return nil, err
		}
		if hasDownloadable(current) {
			// Somebody else resolved it while we were searching.
			m.mu.Unlock()
			return current, nil
		}
		_, _, attachErr := m.attachLocked(ctx, p.Name(), *best)
		m.mu.Unlock()
		if attachErr != nil {
			return nil, attachErr
		}
		m.logger.Info("resolved track onto a downloadable variant",
			"track", trackID, "provider", p.Name(), "match_score", bestScore)
		break
	}

	variants, err = m.db.VariantsForTrack(ctx, trackID)
	if err != nil {
		return nil, err
	}
	if !hasDownloadable(variants) {
		return variants, ErrNoPlayableVariant
	}
	return variants, nil
}

func bestHit(want Candidate, hits []provider.Track) (*provider.Track, float64) {
	var (
		best      *provider.Track
		bestScore float64
	)
	for i := range hits {
		hit := &hits[i]
		score := Score(want, Candidate{
			Title:      hit.Title,
			Artists:    hit.Artists,
			Album:      hit.Album,
			DurationMs: hit.DurationMs,
			ISRC:       hit.ISRC,
		})
		if score > bestScore {
			best, bestScore = hit, score
		}
	}
	return best, bestScore
}

func (m *Matcher) canDownload(providerName string) bool {
	p, ok := m.providers.Get(providerName)
	if !ok {
		return false
	}
	return p.Capabilities().Download
}

// HasDownloadable reports whether any variant can be fetched.
func HasDownloadable(variants []store.Variant) bool { return hasDownloadable(variants) }

func hasDownloadable(variants []store.Variant) bool {
	for _, variant := range variants {
		if variant.Downloadable {
			return true
		}
	}
	return false
}

func artistNames(artists []store.Artist) []string {
	names := make([]string, 0, len(artists))
	for _, artist := range artists {
		names = append(names, artist.Name)
	}
	return names
}
