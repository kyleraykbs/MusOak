// Package spotify is the metadata-only Spotify provider.
//
// Spotify streams are DRM-protected and can never be downloaded, so this
// provider searches and reports metadata — including the ISRC, which is the
// strongest cross-provider matching signal — and refuses to download anything.
// Its tracks are played through a rendition matched from a downloadable
// provider (see the resolution step in internal/match).
package spotify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"codeberg.org/kyleraykbs/prismusic/internal/provider"
)

// Name is the provider id.
const Name = "spotify"

const (
	defaultAPIBase     = "https://api.spotify.com/v1"
	defaultTokenURL    = "https://accounts.spotify.com/api/token"
	defaultSearchLimit = 20
	// tokenSkew renews a little before the token actually expires.
	tokenSkew = 60 * time.Second
)

// Provider implements provider.Provider against the Spotify Web API using the
// client-credentials flow.
type Provider struct {
	clientID     string
	clientSecret string
	apiBase      string
	tokenURL     string
	http         *http.Client
	logger       *slog.Logger

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// New returns the provider. Empty credentials are allowed at construction so
// that a misconfigured server fails on use with a clear message rather than at
// startup.
func New(clientID, clientSecret string, logger *slog.Logger) *Provider {
	if logger == nil {
		logger = slog.Default()
	}
	return &Provider{
		clientID:     clientID,
		clientSecret: clientSecret,
		apiBase:      defaultAPIBase,
		tokenURL:     defaultTokenURL,
		http:         &http.Client{Timeout: 20 * time.Second},
		logger:       logger,
	}
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return Name }

// Capabilities implements provider.Provider: search only, never download.
func (p *Provider) Capabilities() provider.Caps {
	return provider.Caps{
		Search:        true,
		Download:      false,
		SearchAlbums:  true,
		SearchArtists: true,
		Radio:         true,
	}
}

// MissingDeps implements provider.DependencyChecker: Spotify needs nothing
// beyond the credentials in the configuration.
func (p *Provider) MissingDeps() []string { return nil }

// Download always fails: Spotify audio cannot be fetched, so the server must
// never pretend it can.
func (p *Provider) Download(ctx context.Context, providerTrackID, destPath string) error {
	return fmt.Errorf("%w: spotify streams are DRM-protected; resolve the track to a downloadable variant instead",
		provider.ErrDownloadUnsupported)
}

// Search implements provider.Provider.
func (p *Provider) Search(ctx context.Context, q string, opts provider.SearchOpts) ([]provider.Track, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if limit > 50 {
		limit = 50 // Spotify's own cap for this endpoint
	}

	token, err := p.accessToken(ctx)
	if err != nil {
		return nil, err
	}

	endpoint := p.apiBase + "/search?" + url.Values{
		"q":     {q},
		"type":  {"track"},
		"limit": {strconv.Itoa(limit)},
	}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("spotify search: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("spotify search: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var page struct {
		Tracks struct {
			Items []struct {
				ID         string `json:"id"`
				Name       string `json:"name"`
				DurationMs int64  `json:"duration_ms"`
				Artists    []struct {
					Name string `json:"name"`
				} `json:"artists"`
				Album struct {
					Name   string         `json:"name"`
					Images []spotifyImage `json:"images"`
				} `json:"album"`
				ExternalIDs struct {
					ISRC string `json:"isrc"`
				} `json:"external_ids"`
			} `json:"items"`
		} `json:"tracks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return nil, fmt.Errorf("spotify search: parse response: %w", err)
	}

	tracks := make([]provider.Track, 0, len(page.Tracks.Items))
	for _, item := range page.Tracks.Items {
		if item.ID == "" {
			continue
		}
		artists := make([]string, 0, len(item.Artists))
		for _, artist := range item.Artists {
			if artist.Name != "" {
				artists = append(artists, artist.Name)
			}
		}
		tracks = append(tracks, provider.Track{
			ProviderTrackID: item.ID,
			Title:           item.Name,
			Artists:         artists,
			Album:           item.Album.Name,
			DurationMs:      item.DurationMs,
			ISRC:            strings.ToUpper(strings.TrimSpace(item.ExternalIDs.ISRC)),
			ArtworkURL:      bestImage(item.Album.Images),
		})
	}
	return tracks, nil
}

// SearchAlbums implements provider.AlbumSearcher.
func (p *Provider) SearchAlbums(ctx context.Context, q string, opts provider.SearchOpts) ([]provider.Album, error) {
	var page struct {
		Albums struct {
			Items []spotifyAlbum `json:"items"`
		} `json:"albums"`
	}
	if err := p.searchInto(ctx, q, "album", opts.Limit, &page); err != nil {
		return nil, err
	}
	albums := make([]provider.Album, 0, len(page.Albums.Items))
	for _, item := range page.Albums.Items {
		if item.ID == "" {
			continue
		}
		albums = append(albums, provider.Album{
			ProviderAlbumID: item.ID,
			Title:           item.Name,
			Artists:         item.artistNames(),
			Year:            yearOf(item.ReleaseDate),
			TrackCount:      item.TotalTracks,
			ArtworkURL:      bestImage(item.Images),
		})
	}
	return albums, nil
}

// SearchArtists implements provider.ArtistSearcher.
func (p *Provider) SearchArtists(ctx context.Context, q string, opts provider.SearchOpts) ([]provider.Artist, error) {
	var page struct {
		Artists struct {
			Items []struct {
				ID     string         `json:"id"`
				Name   string         `json:"name"`
				Images []spotifyImage `json:"images"`
			} `json:"items"`
		} `json:"artists"`
	}
	if err := p.searchInto(ctx, q, "artist", opts.Limit, &page); err != nil {
		return nil, err
	}
	artists := make([]provider.Artist, 0, len(page.Artists.Items))
	for _, item := range page.Artists.Items {
		if item.ID == "" || item.Name == "" {
			continue
		}
		artists = append(artists, provider.Artist{
			ProviderArtistID: item.ID,
			Name:             item.Name,
			ArtworkURL:       bestImage(item.Images),
		})
	}
	return artists, nil
}

// Album implements provider.AlbumSearcher.
func (p *Provider) Album(ctx context.Context, providerAlbumID string) (*provider.AlbumDetail, error) {
	var album spotifyAlbum
	if err := p.get(ctx, "/albums/"+providerAlbumID, &album); err != nil {
		return nil, err
	}

	var tracks struct {
		Items []struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			DurationMs int64  `json:"duration_ms"`
			Artists    []struct {
				Name string `json:"name"`
			} `json:"artists"`
		} `json:"items"`
	}
	if err := p.get(ctx, "/albums/"+providerAlbumID+"/tracks?limit=50", &tracks); err != nil {
		return nil, err
	}

	detail := &provider.AlbumDetail{
		Album: provider.Album{
			ProviderAlbumID: providerAlbumID,
			Title:           album.Name,
			Artists:         album.artistNames(),
			Year:            yearOf(album.ReleaseDate),
			TrackCount:      album.TotalTracks,
			ArtworkURL:      bestImage(album.Images),
		},
		Tracks: make([]provider.Track, 0, len(tracks.Items)),
	}
	for _, item := range tracks.Items {
		if item.ID == "" {
			continue
		}
		artists := make([]string, 0, len(item.Artists))
		for _, artist := range item.Artists {
			if artist.Name != "" {
				artists = append(artists, artist.Name)
			}
		}
		if len(artists) == 0 {
			artists = detail.Artists
		}
		detail.Tracks = append(detail.Tracks, provider.Track{
			ProviderTrackID: item.ID,
			Title:           item.Name,
			Artists:         artists,
			Album:           album.Name,
			DurationMs:      item.DurationMs,
			ArtworkURL:      bestImage(album.Images),
		})
	}
	return detail, nil
}

// ArtistAlbums implements provider.ArtistSearcher.
func (p *Provider) ArtistAlbums(ctx context.Context, providerArtistID string) ([]provider.Album, error) {
	var page struct {
		Items []spotifyAlbum `json:"items"`
	}
	path := "/artists/" + providerArtistID + "/albums?include_groups=album,single&limit=50"
	if err := p.get(ctx, path, &page); err != nil {
		return nil, err
	}
	albums := make([]provider.Album, 0, len(page.Items))
	for _, item := range page.Items {
		if item.ID == "" {
			continue
		}
		artists := item.artistNames()
		if len(artists) == 0 {
			artists = []string{}
		}
		albums = append(albums, provider.Album{
			ProviderAlbumID: item.ID,
			Title:           item.Name,
			Artists:         artists,
			Year:            yearOf(item.ReleaseDate),
			TrackCount:      item.TotalTracks,
			ArtworkURL:      bestImage(item.Images),
		})
	}
	return albums, nil
}

// Radio implements provider.RadioProvider through Spotify's recommendations.
//
// Spotify deprecated /recommendations for applications created after November
// 2024; when the endpoint is gone the error says so, and the caller's other
// providers still fill the station.
func (p *Provider) Radio(ctx context.Context, seed provider.Track, limit int) ([]provider.Track, error) {
	if seed.ProviderTrackID == "" {
		return nil, fmt.Errorf("spotify radio: the seed has no spotify rendition")
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	var page struct {
		Tracks []struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			DurationMs int64  `json:"duration_ms"`
			Artists    []struct {
				Name string `json:"name"`
			} `json:"artists"`
			Album struct {
				Name   string         `json:"name"`
				Images []spotifyImage `json:"images"`
			} `json:"album"`
			ExternalIDs struct {
				ISRC string `json:"isrc"`
			} `json:"external_ids"`
		} `json:"tracks"`
	}
	path := "/recommendations?limit=" + strconv.Itoa(limit) + "&seed_tracks=" + url.QueryEscape(seed.ProviderTrackID)
	if err := p.get(ctx, path, &page); err != nil {
		return nil, fmt.Errorf("%w: %w", errRadioGone, err)
	}

	tracks := make([]provider.Track, 0, len(page.Tracks))
	for _, item := range page.Tracks {
		if item.ID == "" {
			continue
		}
		artists := make([]string, 0, len(item.Artists))
		for _, artist := range item.Artists {
			if artist.Name != "" {
				artists = append(artists, artist.Name)
			}
		}
		tracks = append(tracks, provider.Track{
			ProviderTrackID: item.ID,
			Title:           item.Name,
			Artists:         artists,
			Album:           item.Album.Name,
			DurationMs:      item.DurationMs,
			ISRC:            strings.ToUpper(strings.TrimSpace(item.ExternalIDs.ISRC)),
			ArtworkURL:      bestImage(item.Album.Images),
		})
	}
	return tracks, nil
}

// spotifyImage is one cover size.
type spotifyImage struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// bestImage picks the largest cover the provider offers.
func bestImage(images []spotifyImage) string {
	best, area := "", -1
	for _, image := range images {
		if image.URL == "" {
			continue
		}
		if size := image.Width * image.Height; size >= area {
			best, area = image.URL, size
		}
	}
	return best
}

// spotifyAlbum is the album shape both search and lookup return.
type spotifyAlbum struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	ReleaseDate string         `json:"release_date"`
	TotalTracks int            `json:"total_tracks"`
	Images      []spotifyImage `json:"images"`
	Artists     []struct {
		Name string `json:"name"`
	} `json:"artists"`
}

func (a spotifyAlbum) artistNames() []string {
	names := make([]string, 0, len(a.Artists))
	for _, artist := range a.Artists {
		if artist.Name != "" {
			names = append(names, artist.Name)
		}
	}
	return names
}

func yearOf(releaseDate string) string {
	if len(releaseDate) >= 4 {
		return releaseDate[:4]
	}
	return ""
}

// searchInto runs a Spotify search of one type and decodes it.
func (p *Provider) searchInto(ctx context.Context, q, kind string, limit int, out any) error {
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if limit > 50 {
		limit = 50
	}
	endpoint := "/search?" + url.Values{
		"q":     {q},
		"type":  {kind},
		"limit": {strconv.Itoa(limit)},
	}.Encode()
	return p.get(ctx, endpoint, out)
}

// get performs an authenticated GET against the Web API.
func (p *Provider) get(ctx context.Context, path string, out any) error {
	token, err := p.accessToken(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.apiBase+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := p.http.Do(req)
	if err != nil {
		return fmt.Errorf("spotify %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("spotify %s: %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("spotify %s: parse response: %w", path, err)
	}
	return nil
}

// errRadioGone marks Spotify's removed recommendation endpoint, so the message
// explains itself instead of looking like a generic failure.
var errRadioGone = errors.New("spotify radio is unavailable (Spotify removed the recommendations endpoint)")

// accessToken returns a cached client-credentials token, fetching a new one
// when it is missing or about to expire.
func (p *Provider) accessToken(ctx context.Context) (string, error) {
	if p.clientID == "" || p.clientSecret == "" {
		return "", fmt.Errorf("spotify: clientId and clientSecret must be configured")
	}

	p.mu.Lock()
	if p.token != "" && time.Now().Before(p.expiresAt) {
		token := p.token
		p.mu.Unlock()
		return token, nil
	}
	p.mu.Unlock()

	form := url.Values{"grant_type": {"client_credentials"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(p.clientID+":"+p.clientSecret)))

	resp, err := p.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("spotify: token request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("spotify: token request failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var payload struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("spotify: parse token response: %w", err)
	}
	if payload.AccessToken == "" {
		return "", fmt.Errorf("spotify: token response carried no access token")
	}

	expiresIn := time.Duration(payload.ExpiresIn) * time.Second
	if expiresIn <= tokenSkew {
		expiresIn = tokenSkew + time.Minute
	}

	p.mu.Lock()
	p.token = payload.AccessToken
	p.expiresAt = time.Now().Add(expiresIn - tokenSkew)
	p.mu.Unlock()

	p.logger.Debug("spotify: obtained a client-credentials token", "expires_in", payload.ExpiresIn)
	return payload.AccessToken, nil
}

var (
	_ provider.Provider          = (*Provider)(nil)
	_ provider.DependencyChecker = (*Provider)(nil)
)
