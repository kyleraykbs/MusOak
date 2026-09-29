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
	return provider.Caps{Search: true, Download: false}
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
					Name string `json:"name"`
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
		})
	}
	return tracks, nil
}

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
