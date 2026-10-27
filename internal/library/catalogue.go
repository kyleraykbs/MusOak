// An artist's catalogue: every release they have, read once in the background,
// because a page cannot ask for sixty provider reads and a listener should not
// have to press anything to get their songs.
//
// The read belongs to the artist, not to the page that started it: the page
// watches it (CatalogueStatus) and paints whatever has landed so far, and a
// second visit costs nothing while the last read is still fresh.

package library

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kyleraykbs/MusOak/internal/match"
	"github.com/kyleraykbs/MusOak/internal/provider"
	"github.com/kyleraykbs/MusOak/internal/store"
)

const (
	// catalogueFreshness is how long a finished catalogue read stands before
	// opening the artist again looks for what has changed. Releases already read
	// are not read again, so a refresh is the release list and nothing else.
	catalogueFreshness = 6 * time.Hour
	// catalogueBudget bounds one catalogue read. A catalogue is a few dozen
	// provider calls with a pause between them; a read still going after this is
	// stuck rather than slow.
	catalogueBudget = 15 * time.Minute
	// albumEnsureBudget bounds the wait an album page's read is allowed: the
	// page shows every song, and the read that gets it there is one provider
	// call, or two when the release has to be found first.
	albumEnsureBudget = 15 * time.Second
)

// CatalogueState is an artist's catalogue read as a page sees it.
type CatalogueState struct {
	ArtistID uuid.UUID `json:"artistId"`
	// Running is whether the read is still going.
	Running bool `json:"running"`
	// Done and Total are how many releases have been read and how many the
	// catalogue holds.
	Done  int `json:"done"`
	Total int `json:"total"`
	// Added is how many songs the read brought into the library.
	Added int `json:"added"`
	// Errors are the releases that could not be read.
	Errors []ProviderError `json:"errors,omitempty"`
	// StartedAt and FinishedAt are when the read ran; FinishedAt is zero while it
	// is still running.
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
}

// catalogue is one artist's catalogue read: the state a page sees, and the lock
// that keeps it honest while the read mutates it.
type catalogue struct {
	mu    sync.Mutex
	state CatalogueState
}

func (c *catalogue) snapshot() CatalogueState {
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.state
	state.Errors = append([]ProviderError(nil), c.state.Errors...)
	return state
}

func (c *catalogue) setTotal(total int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state.Total = total
}

// step records one release read, and how many songs it added.
func (c *catalogue) step(added int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state.Done++
	c.state.Added += added
}

func (c *catalogue) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state.Errors = append(c.state.Errors, ProviderError{Provider: "library", Error: err.Error()})
}

func (c *catalogue) addError(problem ProviderError) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state.Errors = append(c.state.Errors, problem)
}

func (c *catalogue) finish() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state.Running = false
	c.state.FinishedAt = time.Now()
}

// StartCatalogue reads an artist's whole catalogue in the background, and
// returns the read itself: the one already running, the last one while it is
// still fresh, or a new one.
//
// Opening an artist is what asks for it. A page that showed only what the
// library happened to hold would get fuller the more of it you clicked, and an
// artist's songs are not something to assemble by hand.
func (s *Service) StartCatalogue(artistID uuid.UUID) CatalogueState {
	s.catalogueMu.Lock()
	if previous := s.catalogues[artistID]; previous != nil {
		state := previous.snapshot()
		if state.Running || time.Since(state.FinishedAt) < catalogueFreshness {
			s.catalogueMu.Unlock()
			return state
		}
	}
	job := &catalogue{state: CatalogueState{ArtistID: artistID, Running: true, StartedAt: time.Now()}}
	s.catalogues[artistID] = job
	s.catalogueMu.Unlock()

	// The read outlives the request that asked for it: it is the artist's
	// catalogue, not that page's, and the page watches it through CatalogueStatus.
	go s.runCatalogue(artistID, job)
	return job.snapshot()
}

// CatalogueStatus is what this artist's catalogue read is doing, without
// starting one: what a page polling for new songs asks.
func (s *Service) CatalogueStatus(artistID uuid.UUID) (CatalogueState, bool) {
	s.catalogueMu.Lock()
	job := s.catalogues[artistID]
	s.catalogueMu.Unlock()
	if job == nil {
		return CatalogueState{ArtistID: artistID}, false
	}
	return job.snapshot(), true
}

// runCatalogue is the read itself: the release list, then every release's
// tracklist, then the singles collected into their one container.
func (s *Service) runCatalogue(artistID uuid.UUID, job *catalogue) {
	ctx, cancel := context.WithTimeout(context.Background(), catalogueBudget)
	defer cancel()
	defer job.finish()

	artist, err := s.db.Artist(ctx, artistID)
	if err != nil {
		job.fail(err)
		return
	}
	variants, err := s.db.ArtistVariants(ctx, artistID)
	if err != nil {
		job.fail(err)
		return
	}
	if len(variants) == 0 {
		// An artist that came in through a song has no provider page, and
		// without one there is no catalogue to read.
		if err := s.attachProviderArtist(ctx, artist); err != nil {
			job.fail(err)
			return
		}
		if variants, err = s.db.ArtistVariants(ctx, artistID); err != nil {
			job.fail(err)
			return
		}
	}

	albums, singles, problems := s.artistReleases(ctx, variants)
	for _, problem := range problems {
		job.addError(problem)
	}
	if len(albums) == 0 && len(singles) == 0 {
		return
	}

	var containerID uuid.UUID
	if len(singles) > 0 {
		container, err := s.db.EnsureSinglesAlbum(ctx, artistID)
		if err != nil {
			job.fail(err)
			return
		}
		containerID = container.ID
	}

	// Records first, then the singles: the songs panel fills with what a
	// listener thinks of first.
	releases := append(append([]matchedRelease{}, albums...), singles...)
	job.setTotal(len(releases))
	for index, release := range releases {
		if ctx.Err() != nil {
			return
		}
		if index > 0 {
			sleepContext(ctx, releasePause)
		}
		album, _, _, err := s.matcher.MatchAlbum(ctx, release.provider, release.hit)
		if err != nil {
			job.addError(ProviderError{Provider: release.provider, Error: err.Error()})
			job.step(0)
			continue
		}
		if err := s.db.AttachAlbumArtist(ctx, album.ID, artistID); err != nil {
			job.fail(err)
			return
		}
		if s.releaseComplete(ctx, album.ID) {
			job.step(0)
			continue
		}
		synced, err := s.SyncAlbum(ctx, album.ID, []string{release.provider}, false)
		if err != nil {
			job.addError(ProviderError{Provider: release.hit.Title, Error: err.Error()})
			job.step(0)
			continue
		}
		for _, problem := range synced.Errors {
			job.addError(problem)
		}
		if isSingleRelease(release.hit.Kind) && containerID != uuid.Nil {
			if _, err := s.db.AppendAlbumTracks(ctx, containerID, trackIDs(synced.Tracks)); err != nil {
				job.addError(ProviderError{Provider: release.hit.Title, Error: err.Error()})
			}
		}
		job.step(synced.Added)
	}
}

// artistReleases reads one artist page per provider page the artist has, and
// returns the records and the singles, newest first.
func (s *Service) artistReleases(ctx context.Context, variants []store.ArtistVariant) ([]matchedRelease, []matchedRelease, []ProviderError) {
	var (
		albums   []matchedRelease
		singles  []matchedRelease
		problems []ProviderError
	)
	for _, variant := range variants {
		releases, err := s.providers.ArtistAlbums(ctx, variant.Provider, variant.ProviderArtistID)
		if err != nil {
			problems = append(problems, ProviderError{Provider: variant.Provider, Error: err.Error()})
			continue
		}
		for _, hit := range releases {
			release := matchedRelease{provider: variant.Provider, hit: hit}
			if isSingleRelease(hit.Kind) {
				singles = append(singles, release)
			} else {
				albums = append(albums, release)
			}
		}
	}
	return capReleases(newestFirst(albums)), capReleases(newestFirst(singles)), problems
}

// releaseComplete reports whether every release of an album has been read and
// the album holds all of it. A release whose tracklist was never read is not
// complete however much the album holds: nobody knows what is missing.
func (s *Service) releaseComplete(ctx context.Context, albumID uuid.UUID) bool {
	variants, err := s.db.AlbumVariants(ctx, albumID)
	if err != nil || len(variants) == 0 {
		return false
	}
	want := 0
	for _, variant := range variants {
		if variant.TrackCount <= 0 {
			return false
		}
		if variant.TrackCount > want {
			want = variant.TrackCount
		}
	}
	tracks, err := s.db.AlbumTracks(ctx, albumID)
	if err != nil {
		return false
	}
	return len(tracks) >= want
}

// attachProviderArtist finds an artist's page on a provider for an artist that
// came into the library through a song rather than a search: the artist exists,
// nothing says where their page is, and without it there is no catalogue.
//
// The name has to match once normalised, because the search answers with
// everyone who sounds like it - "Pink Floyd Mayweather" is not this artist, and
// their records are not theirs to show.
func (s *Service) attachProviderArtist(ctx context.Context, artist *store.Artist) error {
	want := normalizedName(artist.Name)
	results := s.providers.SearchSomeArtists(ctx, nil, artist.Name, provider.SearchOpts{Limit: 10})
	for _, result := range results {
		if result.Err != nil {
			continue
		}
		for _, hit := range result.Artists {
			if normalizedName(hit.Name) != want {
				continue
			}
			if _, _, _, err := s.matcher.MatchArtist(ctx, result.Provider, hit); err != nil {
				return err
			}
			return nil
		}
	}
	return fmt.Errorf("%w: no provider page is named %q", ErrNoSources, artist.Name)
}

// EnsureAlbumTracks makes an album's tracklist complete before it is read.
//
// An album can be short for two reasons, and neither is something a listener
// should have to press a button about. Either it has releases nobody has read
// yet - a read that failed, a release matched but never synced - or it is an
// album a *track* created: one song out of Dark Side of the Moon leaves an album
// of that name holding that one song, because a track's album is a name, not a
// release. The second case searches for the release by the album's own name and
// artist, records it as this album's release, and then reads it.
//
// It waits, within a budget: a page that shows a tracklist is a page that shows
// all of it, and that is one provider call (two when the release has to be found
// first).
func (s *Service) EnsureAlbumTracks(ctx context.Context, albumID uuid.UUID) {
	album, err := s.db.Album(ctx, albumID)
	if err != nil {
		return
	}
	// The singles container is filled by the artist's catalogue read, not by a
	// release of its own: it has none, and a search for "Singles" would find
	// somebody else's.
	if album.Kind == store.AlbumKindSingles {
		return
	}

	budget, cancel := context.WithTimeout(ctx, albumEnsureBudget)
	defer cancel()

	variants, err := s.db.AlbumVariants(ctx, albumID)
	if err != nil {
		return
	}
	if len(variants) == 0 {
		if _, err := s.attachAlbumRelease(budget, album); err != nil {
			s.logger.Info("album has no release to read", "album", album.ID, "title", album.Title,
				"error", err)
			return
		}
	}
	if s.releaseComplete(budget, albumID) {
		return
	}
	if _, err := s.SyncAlbum(budget, albumID, nil, false); err != nil {
		s.logger.Warn("album could not be completed", "album", album.ID, "title", album.Title,
			"error", err)
	}
}

// attachAlbumRelease finds the provider release an album was created without,
// and records it as this album's own. The album exists because a song named it;
// this is the record that song came off.
//
// The best search hit that clears the matcher's threshold wins, and nothing is
// recorded when none does: a wrong release on an album is worse than an album
// that stays short.
func (s *Service) attachAlbumRelease(ctx context.Context, album *store.Album) (*store.AlbumVariant, error) {
	artists, err := s.db.AlbumArtists(ctx, album.ID)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(artists))
	for _, artist := range artists {
		if name := strings.TrimSpace(artist.Name); name != "" {
			names = append(names, name)
		}
	}
	query := strings.TrimSpace(strings.Join(append([]string{album.Title}, names...), " "))
	if query == "" {
		return nil, errors.New("no name to look for")
	}

	// What is known about this album is its title and who made it: no year and no
	// length, because the song that created it said neither. ScoreAlbum drops the
	// signals one side does not have, so those two carry the match.
	want := match.AlbumCandidate{Title: album.Title, Artists: names}
	var (
		best         *provider.Album
		bestProvider string
		bestScore    float64
	)
	for _, result := range s.providers.SearchSomeAlbums(ctx, nil, query, provider.SearchOpts{Limit: 10}) {
		if result.Err != nil {
			continue
		}
		for i := range result.Albums {
			hit := result.Albums[i]
			score := match.ScoreAlbum(match.AlbumCandidate{
				Title: hit.Title, Artists: hit.Artists, Year: hit.Year, TrackCount: hit.TrackCount,
			}, want)
			if score > bestScore {
				candidate := hit
				best, bestProvider, bestScore = &candidate, result.Provider, score
			}
		}
	}
	if best == nil || bestScore < s.matcher.Threshold() {
		return nil, fmt.Errorf("no release named %q by %s was found", album.Title, strings.Join(names, ", "))
	}
	variant, err := s.matcher.AttachAlbumRelease(ctx, album.ID, bestProvider, *best)
	if err != nil {
		return nil, err
	}
	s.logger.Info("album adopted its release", "album", album.ID, "title", album.Title,
		"provider", bestProvider, "release", best.ProviderAlbumID, "score", bestScore)
	return variant, nil
}

// normalizedName is how two spellings of one artist's name are compared: the
// words, lowercased, in order.
func normalizedName(name string) string {
	return strings.ToLower(strings.Join(strings.Fields(name), " "))
}
