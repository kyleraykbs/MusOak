// Package client is the public Go SDK for Prismusic.
//
// Everything the server does is an API call, so the CLI, a Discord bot or any
// other tool is a plain client: authenticate, search, make media ready, queue
// it in a room and follow that room's timeline. Nothing here reaches into
// server internals.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultTimeout bounds a single API request, not a media transfer.
const DefaultTimeout = 30 * time.Second

// Client talks to one Prismusic server.
type Client struct {
	baseURL string
	token   string
	// memberID identifies a guest in rooms; authenticated clients use their
	// account instead.
	memberID string
	http     *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithToken authenticates every request with a bearer token.
func WithToken(token string) Option {
	return func(c *Client) { c.token = token }
}

// WithMemberID sets the room member id used as a guest.
func WithMemberID(memberID string) Option {
	return func(c *Client) { c.memberID = memberID }
}

// WithHTTPClient replaces the underlying HTTP client; media transfers need an
// http.Client without a short timeout.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.http = hc }
}

// New returns a client for a server, e.g. "http://localhost:8080".
func New(baseURL string, opts ...Option) *Client {
	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: DefaultTimeout},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// BaseURL is the server this client talks to.
func (c *Client) BaseURL() string { return c.baseURL }

// Token returns the current bearer token, empty for a guest.
func (c *Client) Token() string { return c.token }

// MemberID returns the guest member id, empty when none is set.
func (c *Client) MemberID() string { return c.memberID }

// SetToken replaces the bearer token (after login or logout).
func (c *Client) SetToken(token string) { c.token = token }

// SetMemberID replaces the guest room member id.
func (c *Client) SetMemberID(memberID string) { c.memberID = memberID }

// APIError is a non-2xx response.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("prismusic: server returned %s", http.StatusText(e.Status))
	}
	return fmt.Sprintf("prismusic: %s (%d)", e.Message, e.Status)
}

// IsNotFound reports whether err is a 404 from the server.
func IsNotFound(err error) bool { return statusOf(err) == http.StatusNotFound }

// IsUnauthorized reports whether err is a 401 from the server.
func IsUnauthorized(err error) bool { return statusOf(err) == http.StatusUnauthorized }

// IsConflict reports whether err is a 409 from the server.
func IsConflict(err error) bool { return statusOf(err) == http.StatusConflict }

func statusOf(err error) int {
	var apiErr *APIError
	if ok := asAPIError(err, &apiErr); ok {
		return apiErr.Status
	}
	return 0
}

func asAPIError(err error, target **APIError) bool {
	for err != nil {
		if apiErr, ok := err.(*APIError); ok {
			*target = apiErr
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

// do performs a request and decodes the JSON response into out (when non-nil).
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.decorate(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return decodeError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		if err == io.EOF {
			return nil
		}
		return fmt.Errorf("prismusic: decode %s %s: %w", method, path, err)
	}
	return nil
}

func (c *Client) decorate(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.memberID != "" {
		req.Header.Set("X-Member-Id", c.memberID)
	}
}

func decodeError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var body struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &body)
	if body.Error == "" {
		body.Error = strings.TrimSpace(string(raw))
	}
	return &APIError{Status: resp.StatusCode, Message: body.Error}
}

// --- types -----------------------------------------------------------------

// Capabilities describes what a provider can do.
type Capabilities struct {
	Search   bool `json:"search"`
	Download bool `json:"download"`
}

// Provider is one configured music source.
type Provider struct {
	Name         string       `json:"name"`
	Capabilities Capabilities `json:"capabilities"`
	MissingDeps  []string     `json:"missingDeps,omitempty"`
}

// Track is a canonical song.
type Track struct {
	ID         string    `json:"id"`
	Title      string    `json:"title"`
	DurationMs int64     `json:"durationMs"`
	Artists    []string  `json:"artists"`
	Albums     []string  `json:"albums"`
	CreatedAt  time.Time `json:"createdAt"`
}

// Variant is one provider's rendition of a track.
type Variant struct {
	ID              string      `json:"id"`
	TrackID         string      `json:"trackId"`
	Provider        string      `json:"provider"`
	ProviderTrackID string      `json:"providerTrackId"`
	Title           string      `json:"title"`
	Artists         []string    `json:"artists"`
	Album           string      `json:"album"`
	DurationMs      int64       `json:"durationMs"`
	Downloadable    bool        `json:"downloadable"`
	ISRC            string      `json:"isrc,omitempty"`
	Media           MediaStatus `json:"media"`
}

// MediaStatus is a variant's download state.
type MediaStatus struct {
	VariantID  string  `json:"variantId"`
	State      string  `json:"state"`
	Progress   float64 `json:"progress"`
	Error      string  `json:"error,omitempty"`
	DurationMs int64   `json:"durationMs,omitempty"`
	Bytes      int64   `json:"bytes,omitempty"`
}

// Media states.
const (
	MediaNone        = "none"
	MediaDownloading = "downloading"
	MediaReady       = "ready"
	MediaFailed      = "failed"
)

// SearchGroup is a canonical track with the variants that matched onto it.
type SearchGroup struct {
	Track    Track     `json:"track"`
	Variants []Variant `json:"variants"`
}

// ProviderError reports one provider's failure during a search.
type ProviderError struct {
	Provider string `json:"provider"`
	Error    string `json:"error"`
}

// SearchResult is a merged fan-out search.
type SearchResult struct {
	Query          string          `json:"query"`
	Groups         []SearchGroup   `json:"groups"`
	ProviderErrors []ProviderError `json:"providerErrors,omitempty"`
}

// Imported is one file added to the library.
type Imported struct {
	Track   Track   `json:"track"`
	Variant Variant `json:"variant"`
}

// ImportResult reports what a library import did.
type ImportResult struct {
	Imported []Imported `json:"imported"`
	Failures []string   `json:"failures,omitempty"`
}

// User is an account.
type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

// Auth is the response to register or login.
type Auth struct {
	Token string `json:"token"`
	User  User   `json:"user"`
}

// Me describes the caller.
type Me struct {
	Authenticated    bool  `json:"authenticated"`
	Guest            bool  `json:"guest"`
	User             *User `json:"user,omitempty"`
	RequireLogin     bool  `json:"requireLogin"`
	RegistrationOpen bool  `json:"registrationOpen"`
}

// Ranking is a user's provider order with the aggregate and default around it.
type Ranking struct {
	Ranking   []string `json:"ranking"`
	Effective []string `json:"effective"`
	Aggregate []string `json:"aggregate"`
	Default   []string `json:"default"`
}

// --- calls -----------------------------------------------------------------

// Providers lists the enabled providers.
func (c *Client) Providers(ctx context.Context) ([]Provider, error) {
	var out struct {
		Providers []Provider `json:"providers"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/providers", nil, &out); err != nil {
		return nil, err
	}
	return out.Providers, nil
}

// Search runs a merged, cross-provider search.
func (c *Client) Search(ctx context.Context, query string, limit int) (*SearchResult, error) {
	params := url.Values{"q": {query}}
	if limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}
	var out SearchResult
	if err := c.do(ctx, http.MethodGet, "/api/v1/search?"+params.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Track fetches one canonical track.
func (c *Client) Track(ctx context.Context, trackID string) (*Track, error) {
	var out Track
	if err := c.do(ctx, http.MethodGet, "/api/v1/tracks/"+trackID, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TrackVariants lists every rendition of a track.
func (c *Client) TrackVariants(ctx context.Context, trackID string) ([]Variant, error) {
	var out struct {
		Variants []Variant `json:"variants"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/tracks/"+trackID+"/variants", nil, &out); err != nil {
		return nil, err
	}
	return out.Variants, nil
}

// ResolveTrack makes sure a track has a downloadable variant, matching one
// from a downloadable provider when it only has metadata-only renditions.
// The bool reports whether the track is now playable.
func (c *Client) ResolveTrack(ctx context.Context, trackID string) ([]Variant, bool, error) {
	var out struct {
		Variants []Variant `json:"variants"`
		Playable bool      `json:"playable"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/v1/tracks/"+trackID+"/resolve", nil, &out); err != nil {
		return nil, false, err
	}
	return out.Variants, out.Playable, nil
}

// ImportPath adds one local file to the library.
func (c *Client) ImportPath(ctx context.Context, path string) (*ImportResult, error) {
	var out ImportResult
	if err := c.do(ctx, http.MethodPost, "/api/v1/library/import", map[string]string{"path": path}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ImportDir adds every audio file under a directory to the library.
func (c *Client) ImportDir(ctx context.Context, dir string) (*ImportResult, error) {
	var out ImportResult
	if err := c.do(ctx, http.MethodPost, "/api/v1/library/import", map[string]string{"dir": dir}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// MediaStatus reports a variant's download progress.
func (c *Client) MediaStatus(ctx context.Context, variantID string) (*MediaStatus, error) {
	var out MediaStatus
	if err := c.do(ctx, http.MethodGet, "/api/v1/media/"+variantID+"/status", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// StartDownload asks the server to prepare a variant. It returns immediately;
// call MediaStatus or WaitForMedia to follow progress.
func (c *Client) StartDownload(ctx context.Context, variantID string) (*MediaStatus, error) {
	var out MediaStatus
	if err := c.do(ctx, http.MethodPost, "/api/v1/media/"+variantID+"/download", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// StartDownloadAndWait asks the server to prepare a variant and blocks until
// it is ready, failed or ctx ends.
func (c *Client) StartDownloadAndWait(ctx context.Context, variantID string) (*MediaStatus, error) {
	var out MediaStatus
	path := "/api/v1/media/" + variantID + "/download?wait=1"
	if err := c.do(ctx, http.MethodPost, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// WaitForMedia polls until the variant is ready or failed.
func (c *Client) WaitForMedia(ctx context.Context, variantID string) (*MediaStatus, error) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := c.MediaStatus(ctx, variantID)
		if err != nil {
			return nil, err
		}
		switch status.State {
		case MediaReady:
			return status, nil
		case MediaFailed:
			return status, fmt.Errorf("prismusic: download failed: %s", status.Error)
		}
		select {
		case <-ctx.Done():
			return status, ctx.Err()
		case <-ticker.C:
		}
	}
}

// OpenMedia streams a ready variant's audio. The caller closes the reader.
func (c *Client) OpenMedia(ctx context.Context, variantID string) (io.ReadCloser, error) {
	resp, err := c.mediaResponse(ctx, variantID)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// DownloadMedia writes a ready variant's audio to w.
func (c *Client) DownloadMedia(ctx context.Context, variantID string, w io.Writer) (int64, error) {
	body, err := c.OpenMedia(ctx, variantID)
	if err != nil {
		return 0, err
	}
	defer body.Close()
	return io.Copy(w, body)
}

// Clock reports the server clock in milliseconds.
func (c *Client) Clock(ctx context.Context) (int64, error) {
	var out struct {
		NowMs int64 `json:"nowMs"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/clock", nil, &out); err != nil {
		return 0, err
	}
	return out.NowMs, nil
}

// Register creates an account and stores the session token on the client.
func (c *Client) Register(ctx context.Context, username, password string) (*Auth, error) {
	var out Auth
	body := map[string]string{"username": username, "password": password}
	if err := c.do(ctx, http.MethodPost, "/api/v1/auth/register", body, &out); err != nil {
		return nil, err
	}
	c.token = out.Token
	return &out, nil
}

// Login authenticates and stores the session token on the client.
func (c *Client) Login(ctx context.Context, username, password string) (*Auth, error) {
	var out Auth
	body := map[string]string{"username": username, "password": password}
	if err := c.do(ctx, http.MethodPost, "/api/v1/auth/login", body, &out); err != nil {
		return nil, err
	}
	c.token = out.Token
	return &out, nil
}

// Logout revokes the current session.
func (c *Client) Logout(ctx context.Context) error {
	if err := c.do(ctx, http.MethodPost, "/api/v1/auth/logout", nil, nil); err != nil {
		return err
	}
	c.token = ""
	return nil
}

// Me describes the caller: account, guest or login-required.
func (c *Client) Me(ctx context.Context) (*Me, error) {
	var out Me
	if err := c.do(ctx, http.MethodGet, "/api/v1/me", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Favorites lists the caller's favorited tracks.
func (c *Client) Favorites(ctx context.Context) ([]Track, error) {
	var out struct {
		Tracks []Track `json:"tracks"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/me/favorites", nil, &out); err != nil {
		return nil, err
	}
	return out.Tracks, nil
}

// AddFavorite favorites a track; it is idempotent.
func (c *Client) AddFavorite(ctx context.Context, trackID string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/me/favorites", map[string]string{"trackId": trackID}, nil)
}

// RemoveFavorite removes a track from the caller's favorites.
func (c *Client) RemoveFavorite(ctx context.Context, trackID string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/me/favorites/"+trackID, nil, nil)
}

// Ranking reports the caller's ranking, the aggregate and the default.
func (c *Client) Ranking(ctx context.Context) (*Ranking, error) {
	var out Ranking
	if err := c.do(ctx, http.MethodGet, "/api/v1/me/providers/ranking", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetRanking replaces the caller's provider order.
func (c *Client) SetRanking(ctx context.Context, providers []string) (*Ranking, error) {
	var out Ranking
	body := map[string][]string{"ranking": providers}
	if err := c.do(ctx, http.MethodPut, "/api/v1/me/providers/ranking", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// urlQuery escapes a query parameter value.
func urlQuery(value string) string {
	return url.QueryEscape(value)
}
