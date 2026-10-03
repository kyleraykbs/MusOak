package match

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/musoak/internal/provider"
	"codeberg.org/kyleraykbs/musoak/internal/store"
)

// Album signal weights for the non-exact score; they sum to one before the
// unknown signals are dropped.
const (
	weightAlbumTitle    = 0.55
	weightAlbumArtists  = 0.30
	weightAlbumYear     = 0.10
	weightAlbumTracks   = 0.05
	albumYearTolerance  = 1
	albumTrackTolerance = 1
)

// AlbumCandidate is one side of an album comparison.
type AlbumCandidate struct {
	Title      string
	Artists    []string
	Year       string
	TrackCount int
}

// ScoreAlbum returns the similarity of two albums in [0,1].
//
// Titles carry most of the weight, the artist set decides whether it is even
// plausible, and the year and track count break ties. Signals neither side has
// are dropped and the rest renormalized, so an album known only by title still
// matches its release.
func ScoreAlbum(a, b AlbumCandidate) float64 {
	titleA, titleB := NormalizeTitle(a.Title), NormalizeTitle(b.Title)

	signals := []struct {
		weight float64
		value  float64
		known  bool
	}{
		{weightAlbumTitle, albumTitleSimilarity(titleA, titleB), titleA != "" && titleB != ""},
		{weightAlbumArtists, artistSimilarity(a.Artists, b.Artists), len(artistSet(a.Artists)) > 0 && len(artistSet(b.Artists)) > 0},
		{weightAlbumYear, albumYearSimilarity(a.Year, b.Year), strings.TrimSpace(a.Year) != "" && strings.TrimSpace(b.Year) != ""},
		{weightAlbumTracks, albumTrackSimilarity(a.TrackCount, b.TrackCount), a.TrackCount > 0 && b.TrackCount > 0},
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

func albumTitleSimilarity(a, b string) float64 {
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 1
	}
	return jaccard(tokens(a), tokens(b))
}

func albumYearSimilarity(a, b string) float64 {
	yearA, errA := strconv.Atoi(strings.TrimSpace(a))
	yearB, errB := strconv.Atoi(strings.TrimSpace(b))
	if errA != nil || errB != nil {
		return 0.5
	}
	diff := yearA - yearB
	if diff < 0 {
		diff = -diff
	}
	switch {
	case diff == 0:
		return 1
	case diff <= albumYearTolerance:
		return 0.7
	default:
		return 0
	}
}

func albumTrackSimilarity(a, b int) float64 {
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	if diff <= albumTrackTolerance {
		return 1
	}
	return 0.3
}

// Store is the repository slice matching needs. It is declared in match.go for
// tracks and extended here for collections; both are the same interface.
type collectionStore interface {
	store.AlbumRepo
	store.ArtistRepo
}

// MatchAlbum finds the canonical album a provider release belongs to, creating
// it when it is new, and records the provider release either way.
func (m *Matcher) MatchAlbum(ctx context.Context, providerName string, hit provider.Album) (*store.Album, *store.AlbumVariant, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.matchAlbumLocked(ctx, providerName, hit)
}

func (m *Matcher) matchAlbumLocked(ctx context.Context, providerName string, hit provider.Album) (*store.Album, *store.AlbumVariant, bool, error) {
	if hit.ProviderAlbumID == "" {
		return nil, nil, false, errors.New("match: album hit without a provider id")
	}

	// Already known: this provider release has an album.
	if existing, err := m.db.AlbumByProviderID(ctx, providerName, hit.ProviderAlbumID); err == nil {
		variant, _, err := m.db.AttachAlbumVariant(ctx, &store.AlbumVariant{
			AlbumID:         existing.ID,
			Provider:        providerName,
			ProviderAlbumID: hit.ProviderAlbumID,
			Title:           hit.Title,
			Artists:         hit.Artists,
			Year:            hit.Year,
			TrackCount:      hit.TrackCount,
		})
		if err != nil {
			return nil, nil, false, err
		}
		return existing, variant, false, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, nil, false, err
	}

	albumID, created, err := m.findOrCreateAlbumLocked(ctx, hit)
	if err != nil {
		return nil, nil, false, err
	}
	album, err := m.db.Album(ctx, albumID)
	if err != nil {
		return nil, nil, false, err
	}
	variant, _, err := m.db.AttachAlbumVariant(ctx, &store.AlbumVariant{
		AlbumID:         albumID,
		Provider:        providerName,
		ProviderAlbumID: hit.ProviderAlbumID,
		Title:           hit.Title,
		Artists:         hit.Artists,
		Year:            hit.Year,
		TrackCount:      hit.TrackCount,
	})
	if err != nil {
		return nil, nil, false, err
	}
	if hit.ArtworkURL != "" {
		if err := m.db.SetAlbumVariantArtwork(ctx, variant.ID, hit.ArtworkURL); err != nil {
			m.logger.Warn("match: set album variant artwork", "variant", variant.ID, "error", err)
		}
	}
	return album, variant, created, nil
}

// findOrCreateAlbumLocked scores the provider release against the albums that
// could be the same record, and creates one when none is close enough.
func (m *Matcher) findOrCreateAlbumLocked(ctx context.Context, hit provider.Album) (uuid.UUID, bool, error) {
	titleKey, artistKey := store.AlbumIdentity(hit.Title, hit.Artists)
	candidates, err := m.db.AlbumCandidates(ctx, titleKey, artistKey, 50)
	if err != nil {
		return uuid.Nil, false, err
	}

	want := AlbumCandidate{
		Title:      hit.Title,
		Artists:    hit.Artists,
		Year:       hit.Year,
		TrackCount: hit.TrackCount,
	}

	var (
		best      *store.Album
		bestScore float64
	)
	for i := range candidates {
		candidate := &candidates[i]
		artists, err := m.db.AlbumArtists(ctx, candidate.ID)
		if err != nil {
			return uuid.Nil, false, err
		}
		shape := AlbumCandidate{Title: candidate.Title, Artists: artistNames(artists)}
		if variant, err := m.albumYear(ctx, candidate.ID); err == nil {
			shape.Year = variant
		}
		if score := ScoreAlbum(shape, want); score > bestScore {
			best, bestScore = candidate, score
		}
	}

	if best != nil && bestScore >= m.threshold {
		m.logger.Debug("matched album", "title", hit.Title, "candidate", best.Title, "score", bestScore)
		if hit.ArtworkURL != "" {
			if err := m.db.SetAlbumArtwork(ctx, best.ID, hit.ArtworkURL); err != nil {
				m.logger.Warn("match: set album artwork", "album", best.ID, "error", err)
			}
		}
		return best.ID, false, nil
	}
	if best != nil {
		m.logger.Debug("closest album below threshold",
			"title", hit.Title, "candidate", best.Title, "score", bestScore, "threshold", m.threshold)
	}

	albumID, err := m.db.EnsureAlbum(ctx, hit.Title, hit.Artists)
	if err != nil {
		return uuid.Nil, false, err
	}
	if hit.ArtworkURL != "" {
		if err := m.db.SetAlbumArtwork(ctx, albumID, hit.ArtworkURL); err != nil {
			m.logger.Warn("match: set album artwork", "album", albumID, "error", err)
		}
	}
	m.logger.Debug("created canonical album", "album", albumID, "title", hit.Title, "provider", providerNameOf(hit))
	return albumID, true, nil
}

// albumYear reads the year any of an album's provider releases reports.
func (m *Matcher) albumYear(ctx context.Context, albumID uuid.UUID) (string, error) {
	variants, err := m.db.AlbumVariants(ctx, albumID)
	if err != nil {
		return "", err
	}
	for _, variant := range variants {
		if variant.Year != "" {
			return variant.Year, nil
		}
	}
	return "", nil
}

// providerNameOf exists so the log line above cannot accidentally format the
// whole hit struct.
func providerNameOf(provider.Album) string { return "album" }

// MatchArtist finds or creates the canonical artist for a provider page.
func (m *Matcher) MatchArtist(ctx context.Context, providerName string, hit provider.Artist) (*store.Artist, *store.ArtistVariant, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if hit.ProviderArtistID == "" || strings.TrimSpace(hit.Name) == "" {
		return nil, nil, false, errors.New("match: artist hit without an id or name")
	}

	if existing, err := m.db.ArtistByProviderID(ctx, providerName, hit.ProviderArtistID); err == nil {
		variant, _, err := m.db.AttachArtistVariant(ctx, &store.ArtistVariant{
			ArtistID: existing.ID, Provider: providerName,
			ProviderArtistID: hit.ProviderArtistID, Name: hit.Name,
		})
		if err != nil {
			return nil, nil, false, err
		}
		return existing, variant, false, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, nil, false, err
	}

	// An artist's identity is their name, so a normalised name match is the
	// whole story; there is no scoring to do.
	normalized := NormalizeArtist(hit.Name)
	candidates, err := m.db.ArtistCandidates(ctx, normalized, 25)
	if err != nil {
		return nil, nil, false, err
	}

	created := true
	artistID := uuid.Nil
	for _, candidate := range candidates {
		if NormalizeArtist(candidate.Name) == normalized {
			artistID = candidate.ID
			created = false
			break
		}
	}
	if artistID == uuid.Nil {
		artistID, err = m.db.EnsureArtist(ctx, hit.Name)
		if err != nil {
			return nil, nil, false, err
		}
	}

	artist, err := m.db.Artist(ctx, artistID)
	if err != nil {
		return nil, nil, false, err
	}
	variant, _, err := m.db.AttachArtistVariant(ctx, &store.ArtistVariant{
		ArtistID: artistID, Provider: providerName,
		ProviderArtistID: hit.ProviderArtistID, Name: hit.Name,
	})
	if err != nil {
		return nil, nil, false, err
	}
	if hit.ArtworkURL != "" {
		if err := m.db.SetArtistArtwork(ctx, artistID, hit.ArtworkURL); err != nil {
			m.logger.Warn("match: set artist artwork", "artist", artistID, "error", err)
		}
		if err := m.db.SetArtistVariantArtwork(ctx, variant.ID, hit.ArtworkURL); err != nil {
			m.logger.Warn("match: set artist variant artwork", "variant", variant.ID, "error", err)
		}
	}
	if created {
		m.logger.Debug("created canonical artist", "artist", artistID, "name", hit.Name)
	}
	return artist, variant, created, nil
}
