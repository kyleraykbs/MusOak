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

	"github.com/kyleraykbs/MusOak/internal/provider"
	"github.com/kyleraykbs/MusOak/internal/store"
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

// backgroundSlots is how many songs are matched against the other platforms at
// once, and BackgroundMatchTimeout how long one song's match may take.
const (
	backgroundSlots        = 2
	BackgroundMatchTimeout = 2 * time.Minute
)

// ErrNoPlayableVariant means the track still has no downloadable rendition
// after resolution: nothing matched.
var ErrNoPlayableVariant = errors.New("no downloadable variant available")

// ErrUnknownProvider means the provider a caller named is not registered.
var ErrUnknownProvider = errors.New("unknown provider")

// Candidate is one side of a comparison.
type Candidate struct {
	Title      string
	Artists    []string
	Album      string
	DurationMs int64
	ISRC       string
}

// scored is a candidate with its comparison keys worked out once. Normalising
// is the expensive half of scoring - a title goes through six regular
// expressions - and a candidate compared against many others (a search hit
// against every track in the library) would otherwise be reduced again for
// each of them, on a side that does not change.
type scored struct {
	title    string
	album    string
	artists  map[string]bool
	names    []string
	duration int64
	isrc     string
}

func keysOf(c Candidate) scored {
	return scored{
		title:    NormalizeTitle(c.Title),
		album:    NormalizeTitle(c.Album),
		artists:  artistSet(c.Artists),
		names:    c.Artists,
		duration: c.DurationMs,
		isrc:     c.ISRC,
	}
}

// Score returns the similarity of two candidates in [0,1]. Identical ISRCs
// short-circuit to 1; durations that cannot be the same recording return 0.
//
// Only the signals both sides actually carry are weighed: an untagged local
// file must still be able to match its track on title and duration alone,
// while two tracks that both name their artist and album are held to them.
func Score(a, b Candidate) float64 {
	return keysOf(a).score(keysOf(b))
}

// score is Score with both sides already reduced to their keys.
func (a scored) score(b scored) float64 {
	if a.isrc != "" && b.isrc != "" && strings.EqualFold(strings.TrimSpace(a.isrc), strings.TrimSpace(b.isrc)) {
		return 1
	}
	if a.duration > 0 && b.duration > 0 && absDiff(a.duration, b.duration) > DurationToleranceMs {
		return 0
	}

	signals := []struct {
		weight float64
		value  float64
		known  bool
	}{
		{weightTitle, titleSimilarityKeys(a.title, b.title), a.title != "" && b.title != ""},
		{weightArtists, artistSimilaritySets(a.names, a.artists, b.names, b.artists), len(a.artists) > 0 && len(b.artists) > 0},
		{weightAlbum, albumSimilarityKeys(a.album, b.album), a.album != "" && b.album != ""},
		{weightDuration, durationSimilarity(a.duration, b.duration), a.duration > 0 && b.duration > 0},
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

// titleSimilarityKeys compares two titles that are already normalised.
func titleSimilarityKeys(na, nb string) float64 {
	switch {
	case na == "" || nb == "":
		return 0
	case na == nb:
		return 1
	}
	return jaccard(tokens(na), tokens(nb))
}

// artistSimilaritySets compares two credits lists from the normalised sets the
// caller already holds; the raw names are kept for the word fallback below.
func artistSimilaritySets(a []string, na map[string]bool, b []string, nb map[string]bool) float64 {
	if len(na) == 0 || len(nb) == 0 {
		// Unknown credits are neutral rather than disqualifying: an untagged
		// local file should still be able to join its track.
		return 0.5
	}
	if score := jaccard(na, nb); score > 0 {
		return score
	}
	// The same credits arrive spelled differently. Spotify's public playlist
	// pages give one joined line per track, so a name with a comma in it looks
	// like two people ("Tyler, The Creator" becomes "Tyler" and "The Creator"),
	// and a featured artist can be credited on one side and not the other.
	// Comparing the words of the credits, rather than the names they were split
	// into, is what makes those the same artists.
	if score := jaccard(artistWords(a), artistWords(b)); score > 0 {
		return score
	}
	return 0
}

// artistWords is every word of every credit: what a credit line still shares
// when it was split, joined or trimmed differently.
func artistWords(names []string) map[string]bool {
	out := make(map[string]bool)
	for _, name := range names {
		for word := range tokens(NormalizeArtist(name)) {
			out[word] = true
		}
	}
	return out
}

// albumSimilarityKeys compares two albums that are already normalised.
func albumSimilarityKeys(na, nb string) float64 {
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
	store.ArtworkRepo
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

	// mu serialises the write half of attaching: creating a canonical track and
	// the rendition rows under it. The library search that decides what to
	// write runs outside it, so one slow match no longer queues every other
	// search behind it; the variant's (provider, provider id) uniqueness is
	// what actually settles two attaches racing for the same rendition.
	mu sync.Mutex

	// background bounds how many songs are being matched against the other
	// platforms at once, so attaching a playlist cannot become a hundred
	// simultaneous provider searches.
	background chan struct{}
}

// New returns a matcher using the given score threshold.
func New(db Store, providers *provider.Registry, threshold float64, logger *slog.Logger) *Matcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Matcher{
		db:         db,
		providers:  providers,
		threshold:  threshold,
		logger:     logger,
		background: make(chan struct{}, backgroundSlots),
	}
}

// Attach finds or creates the canonical track for a provider hit and stores
// the hit as a variant of it.
//
// The caller is answered with that one rendition and the other platforms are
// looked for behind the answer: attaching is a person waiting on the song they
// picked, and a search result that costs a dozen provider round trips is a
// search result that feels broken.
func (m *Matcher) Attach(ctx context.Context, providerName string, hit provider.Track) (*store.Track, *store.Variant, error) {
	track, variant, err := m.attach(ctx, providerName, hit)
	if err != nil {
		return nil, nil, err
	}
	if track != nil {
		m.matchTheRest(ctx, track.ID)
	}
	return track, variant, nil
}

// attach finds or creates the canonical track for a hit and stores it as a
// variant. The library is read outside the attach mutex: the candidate search
// is a scan of every track, and holding the lock across it queued every other
// search, import and room resolve behind one slow match. Only the writes take
// the lock, and the rendition's own (provider, provider id) uniqueness is what
// settles two attaches arriving for the same one.
func (m *Matcher) attach(ctx context.Context, providerName string, hit provider.Track) (*store.Track, *store.Variant, error) {
	if track, variant, ok, err := m.knownRendition(ctx, providerName, hit.ProviderTrackID); err != nil {
		return nil, nil, err
	} else if ok {
		m.learn(ctx, track, variant, hit)
		return track, variant, nil
	}

	match, err := m.findTrack(ctx, hit)
	if err != nil {
		return nil, nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	return m.attachFound(ctx, providerName, hit, match)
}

// attachFound stores a hit as a variant of match, or of a new canonical track
// when match is nil. The caller holds the attach mutex; the re-check inside it
// is what catches an attach that stored this rendition - or the track it
// belongs to - while the caller was reading the library.
func (m *Matcher) attachFound(ctx context.Context, providerName string, hit provider.Track, match *store.Track) (*store.Track, *store.Variant, error) {
	if track, variant, ok, err := m.knownRendition(ctx, providerName, hit.ProviderTrackID); err != nil {
		return nil, nil, err
	} else if ok {
		m.learn(ctx, track, variant, hit)
		return track, variant, nil
	}

	if match == nil {
		// A scan that found nothing is what a brand new recording looks like,
		// and also what two attaches of one recording racing each other look
		// like. Creating the canonical row must not be a guess, so the scan is
		// repeated under the lock; that is the only step this lock still
		// guards, and it is paid only where a row is about to be created.
		again, err := m.findTrack(ctx, hit)
		if err != nil {
			return nil, nil, err
		}
		match = again
	}

	track := match
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

	m.learn(ctx, track, variant, hit)
	return track, variant, nil
}

// knownRendition returns the track and variant of a (provider, provider track)
// pair already stored, or ok=false when there is none. It only reads, so a
// caller can ask before deciding whether the lock is worth taking.
func (m *Matcher) knownRendition(ctx context.Context, providerName, providerTrackID string) (*store.Track, *store.Variant, bool, error) {
	existing, err := m.db.VariantByProviderTrack(ctx, providerName, providerTrackID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, false, nil
		}
		return nil, nil, false, err
	}
	track, err := m.db.Track(ctx, existing.TrackID)
	if err != nil {
		return nil, nil, false, err
	}
	return track, existing, true, nil
}

// learn records what a hit teaches the canonical track and its rendition, and
// refreshes the structs the caller receives so a response never reads a stale
// row.
func (m *Matcher) learn(ctx context.Context, track *store.Track, variant *store.Variant, hit provider.Track) {
	if hit.DurationMs > 0 && track.DurationMs == 0 {
		if err := m.db.SetTrackDuration(ctx, track.ID, hit.DurationMs); err != nil {
			m.logger.Warn("match: set track duration", "track", track.ID, "error", err)
		}
		track.DurationMs = hit.DurationMs
	}
	if hit.ArtworkURL == "" {
		return
	}
	if track.ArtworkURL == "" {
		if err := m.db.SetTrackArtwork(ctx, track.ID, hit.ArtworkURL); err != nil {
			m.logger.Warn("match: set track artwork", "track", track.ID, "error", err)
		} else {
			track.ArtworkURL = hit.ArtworkURL
		}
	}
	if err := m.db.SetVariantArtwork(ctx, variant.ID, hit.ArtworkURL); err != nil {
		m.logger.Warn("match: set variant artwork", "variant", variant.ID, "error", err)
	}
}

// AttachToTrack stores a hit as a variant of exactly the given track. Unlike
// Attach it never matches the hit against the library: the caller picked this
// track deliberately. The write is idempotent for the same (provider,
// providerTrackId), so posting the same pick twice returns the variant already
// stored. An unregistered provider is ErrUnknownProvider.
// AttachToTrack stores a provider hit as a variant of an existing track, and
// looks for the other platforms behind the answer, as Attach does.
func (m *Matcher) AttachToTrack(ctx context.Context, trackID uuid.UUID, providerName string, hit provider.Track) (*store.Track, *store.Variant, error) {
	track, variant, err := m.attachToTrack(ctx, trackID, providerName, hit)
	if err != nil {
		return nil, nil, err
	}
	m.matchTheRest(ctx, trackID)
	return track, variant, nil
}

func (m *Matcher) attachToTrack(ctx context.Context, trackID uuid.UUID, providerName string, hit provider.Track) (*store.Track, *store.Variant, error) {
	p, ok := m.providers.Get(providerName)
	if !ok {
		return nil, nil, ErrUnknownProvider
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	track, err := m.db.Track(ctx, trackID)
	if err != nil {
		return nil, nil, err
	}
	if existing, err := m.db.VariantByProviderTrack(ctx, providerName, hit.ProviderTrackID); err == nil {
		if existing.TrackID != track.ID {
			// The rendition somebody picked is already a variant of another row
			// for the same recording: this library holds that song twice. The
			// pick was made for this song, so the two are joined - the same
			// thing resolution does when it finds the same situation.
			if err := m.db.MergeTracks(ctx, track.ID, existing.TrackID); err != nil {
				return nil, nil, err
			}
			kept, err := m.db.Track(ctx, track.ID)
			if err != nil {
				return nil, nil, err
			}
			// The merge re-pointed the variant, so the copy in hand is stale.
			moved, err := m.db.VariantByProviderTrack(ctx, providerName, hit.ProviderTrackID)
			if err != nil {
				return nil, nil, err
			}
			return kept, moved, nil
		}
		m.learn(ctx, track, existing, hit)
		return track, existing, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, nil, err
	}

	// Fill the credits the track never had, so the pick leaves no empty row
	// behind; what the track already knows is never clobbered.
	if len(hit.Artists) > 0 {
		artists, err := m.db.TrackArtists(ctx, track.ID)
		if err != nil {
			return nil, nil, err
		}
		if len(artists) == 0 {
			if err := m.db.SetTrackArtists(ctx, track.ID, hit.Artists); err != nil {
				return nil, nil, err
			}
		}
	}
	if hit.Album != "" {
		albums, err := m.db.TrackAlbums(ctx, track.ID)
		if err != nil {
			return nil, nil, err
		}
		if len(albums) == 0 {
			if err := m.db.SetTrackAlbums(ctx, track.ID, []string{hit.Album}); err != nil {
				return nil, nil, err
			}
		}
	}

	variant := &store.Variant{
		TrackID:         track.ID,
		Provider:        providerName,
		ProviderTrackID: hit.ProviderTrackID,
		Title:           hit.Title,
		Artists:         hit.Artists,
		Album:           hit.Album,
		DurationMs:      hit.DurationMs,
		Downloadable:    p.Capabilities().Download,
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

	m.learn(ctx, track, variant, hit)
	return track, variant, nil
}

// matchTheRest looks for a track on the providers that have no rendition of it
// yet, and attaches what it finds, without the caller waiting for it.
//
// What this is for: a song attached from a search has one rendition - the one
// the provider it was found on gave - so when that file will not fetch there is
// nothing else to play and the song is skipped. The other platforms' renditions
// are what make the second attempt possible, and finding them is not worth
// making somebody wait for.
func (m *Matcher) matchTheRest(ctx context.Context, trackID uuid.UUID) {
	if m.background == nil {
		return
	}
	go func() {
		// The request that attached this song is answered the moment it
		// returns, so its cancellation must not reach this work.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), BackgroundMatchTimeout)
		defer cancel()
		select {
		case m.background <- struct{}{}:
		case <-ctx.Done():
			return
		}
		defer func() { <-m.background }()
		m.matchRest(ctx, trackID)
	}()
}

// matchRest is the body of matchTheRest, with the slot already held.
func (m *Matcher) matchRest(ctx context.Context, trackID uuid.UUID) {
	track, err := m.db.Track(ctx, trackID)
	if err != nil {
		m.logger.Warn("match: background match found no track", "track", trackID, "error", err)
		return
	}
	variants, err := m.db.VariantsForTrack(ctx, trackID)
	if err != nil {
		return
	}
	artists, err := m.db.TrackArtists(ctx, trackID)
	if err != nil {
		return
	}
	albums, err := m.db.TrackAlbums(ctx, trackID)
	if err != nil {
		return
	}

	known := make(map[string]bool, len(variants))
	want := Candidate{Title: track.Title, Artists: artistNames(artists), DurationMs: track.DurationMs}
	if len(albums) > 0 {
		want.Album = albums[0].Title
	}
	for _, variant := range variants {
		known[variant.Provider] = true
		if want.ISRC == "" && variant.ISRC != "" {
			want.ISRC = variant.ISRC
		}
	}
	query := strings.TrimSpace(track.Title + " " + strings.Join(want.Artists, " "))
	if query == "" {
		return
	}

	for _, p := range m.providers.All() {
		if ctx.Err() != nil {
			return
		}
		// A platform that already has a rendition of this song has nothing to
		// add, and one that cannot hand a file over has nothing to play from.
		if known[p.Name()] || !p.Capabilities().Download {
			continue
		}
		best, score := m.searchFor(ctx, p, want, query, trackID)
		if best == nil || score < m.threshold {
			m.logger.Debug("match: no background candidate above threshold",
				"provider", p.Name(), "track", trackID, "score", score)
			continue
		}
		if _, _, err := m.attachToTrack(ctx, trackID, p.Name(), *best); err != nil {
			m.logger.Warn("match: could not attach a background rendition",
				"provider", p.Name(), "track", trackID, "error", err)
			continue
		}
		m.logger.Info("matched another rendition in the background",
			"track", trackID, "provider", p.Name(), "match_score", score)
	}
}

// searchFor asks one provider for a recording and returns its best candidate,
// with the title alone as the second half of the search.
func (m *Matcher) searchFor(ctx context.Context, p provider.Provider, want Candidate, query string, trackID uuid.UUID) (*provider.Track, float64) {
	searchCtx, cancel := context.WithTimeout(ctx, SearchTimeout)
	hits, err := p.Search(searchCtx, query, provider.SearchOpts{Limit: resolveLimit})
	cancel()
	if err != nil {
		m.logger.Warn("match: search failed", "provider", p.Name(), "track", trackID, "error", err)
		return nil, 0
	}
	best, bestScore := bestHit(want, hits)
	if best == nil || bestScore < m.threshold {
		// One query can miss what another finds. The title alone is the other
		// half of this search: credits are what providers spell differently,
		// and a query carrying them can push the right answer out of the ten
		// hits entirely. Asked only when the first found nothing, so a search
		// that works costs exactly what it did.
		if plain := strings.TrimSpace(want.Title); plain != "" && plain != query {
			moreCtx, moreCancel := context.WithTimeout(ctx, SearchTimeout)
			more, moreErr := p.Search(moreCtx, plain, provider.SearchOpts{Limit: resolveLimit})
			moreCancel()
			if moreErr != nil {
				m.logger.Warn("match: search failed", "provider", p.Name(), "track", trackID, "error", moreErr)
			} else if better, betterScore := bestHit(want, more); better != nil && betterScore > bestScore {
				best, bestScore = better, betterScore
			}
		}
	}
	return best, bestScore
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

	shapes, err := m.shapes(ctx, candidates)
	if err != nil {
		return nil, err
	}

	var (
		best      *store.Track
		bestScore float64
	)
	want := Candidate{
		Title:      hit.Title,
		Artists:    hit.Artists,
		Album:      hit.Album,
		DurationMs: hit.DurationMs,
		ISRC:       hit.ISRC,
	}
	// The hit is the same for every candidate, so its keys are worked out once.
	wantKeys := keysOf(want)
	for i := range candidates {
		candidate := &candidates[i]
		// Score is symmetric; candidate first keeps the comparison the way it
		// read before.
		score := keysOf(shapes[candidate.ID]).score(wantKeys)
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

// shapes loads the comparison signals of many canonical tracks at once. It is
// shape for a whole candidate set: the artists and albums of every candidate in
// two queries rather than two per candidate, which is what a scorer that walks
// hundreds of candidates needs.
func (m *Matcher) shapes(ctx context.Context, tracks []store.Track) (map[uuid.UUID]Candidate, error) {
	ids := make([]uuid.UUID, 0, len(tracks))
	for i := range tracks {
		ids = append(ids, tracks[i].ID)
	}
	artists, err := m.db.TrackArtistsFor(ctx, ids)
	if err != nil {
		return nil, err
	}
	albums, err := m.db.TrackAlbumsFor(ctx, ids)
	if err != nil {
		return nil, err
	}

	out := make(map[uuid.UUID]Candidate, len(tracks))
	for i := range tracks {
		track := &tracks[i]
		shape := Candidate{
			Title:      track.Title,
			Artists:    artistNames(artists[track.ID]),
			DurationMs: track.DurationMs,
		}
		if list := albums[track.ID]; len(list) > 0 {
			shape.Album = list[0].Title
		}
		out[track.ID] = shape
	}
	return out, nil
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

	shapes, err := m.shapes(ctx, candidates)
	if err != nil {
		return nil, err
	}

	wantKeys := keysOf(want)
	var (
		best      *store.Track
		bestScore float64
	)
	for i := range candidates {
		candidate := &candidates[i]
		if candidate.ID == track.ID {
			continue
		}
		score := keysOf(shapes[candidate.ID]).score(wantKeys)
		if score > bestScore {
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
		best, bestScore := m.searchFor(ctx, p, want, query, trackID)
		if best == nil || bestScore < m.threshold {
			m.logger.Debug("match: no resolution candidate above threshold",
				"provider", p.Name(), "track", trackID, "score", bestScore)
			continue
		}

		// The library scan is a read; it happens before the lock so another
		// match is not queued behind it.
		match, err := m.findTrack(ctx, *best)
		if err != nil {
			return nil, err
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
		other, _, attachErr := m.attachFound(ctx, p.Name(), *best, match)
		m.mu.Unlock()
		if attachErr != nil {
			return nil, attachErr
		}
		if other != nil && other.ID != trackID {
			// The rendition is already a variant of another canonical track:
			// this library holds the same recording twice, and the copy being
			// resolved is the one without a source. Joining them is what makes
			// it playable - the matcher has just said they are the same
			// recording, by the same score that picked the hit.
			if err := m.db.MergeTracks(ctx, trackID, other.ID); err != nil {
				m.logger.Warn("match: could not join duplicate tracks",
					"track", trackID, "other", other.ID, "error", err)
			} else {
				m.logger.Info("joined duplicate tracks",
					"kept", trackID, "dropped", other.ID, "provider", p.Name(), "match_score", bestScore)
			}
		}
		m.logger.Info("matched a rendition for a track with no source",
			"track", trackID, "provider", p.Name(), "match_score", bestScore)
		break
	}

	variants, err = m.db.VariantsForTrack(ctx, trackID)
	if err != nil {
		return nil, err
	}
	if !hasDownloadable(variants) {
		// The rendition that matched is already a variant of another canonical
		// track: this library holds the same recording twice, and this copy is
		// the one still without a source. Joining the two is a merge, which is
		// not this function's to do.
		m.logger.Info("track still has no source after resolving",
			"track", trackID, "hint", "the rendition is attached to another track with the same recording")
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
		}) + rankBonus(i)
		if score > bestScore {
			best, bestScore = hit, score
		}
	}
	return best, bestScore
}

// rankBonus is what a provider's own order is worth.
//
// A search answers in the order the platform thinks is relevant, and this
// scoring throws that away: two candidates a hair apart are usually the same
// answer, and the one the provider put first is the better guess. The bonus is
// sized against the noise it absorbs - providers round durations to the second,
// and the duration signal is worth 0.20 across five of them - so it settles
// near-ties without ever promoting a wrong answer over a right one.
func rankBonus(index int) float64 {
	const first = 0.06
	const step = 0.03
	bonus := first - float64(index)*step
	if bonus < 0 {
		return 0
	}
	return bonus
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
