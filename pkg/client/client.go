// Package client is the public Go SDK for MusOak.
//
// Everything the server does is an API call, so the CLI, a Discord bot or any
// other tool is a plain client: authenticate, search, prepare media, queue it
// in a room and follow the host's playback state. Nothing reaches into server
// internals.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultTimeout bounds a single API request, not a media transfer.
const DefaultTimeout = 30 * time.Second

// Client talks to one MusOak server.
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

// New returns a client for a server, e.g. "http://localhost:4420".
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

// MediaURL is the URL a variant's stored file is served from, the same URL the
// media GET uses. A player that speaks HTTP (mpv does) can be handed it
// directly. The server answers only for a rendition it has finished storing:
// for one it has not, the request is refused and the caller must download it
// first (Cache.Fetch does that).
//
// startMs, when positive, asks the file to be served from that far into the
// track: a listener joining a song already under way hears the rest of it
// rather than its head. Zero is the whole file from the beginning.
func (c *Client) MediaURL(variantID string, startMs int64) string {
	media := c.baseURL + "/api/v1/media/" + variantID
	if startMs <= 0 {
		return media
	}
	query := url.Values{}
	query.Set("start", strconv.FormatInt(startMs, 10))
	return media + "?" + query.Encode()
}

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
		return fmt.Sprintf("musoak: server returned %s", http.StatusText(e.Status))
	}
	return fmt.Sprintf("musoak: %s (%d)", e.Message, e.Status)
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

// Retry bounds. An idempotent GET is tried this many times before its error is
// returned, and the wait between attempts doubles from retryBaseDelay with
// jitter, so a fleet that failed together does not come back together.
const (
	maxAttempts    = 3
	retryBaseDelay = 100 * time.Millisecond
)

// do performs a request and decodes the JSON response into out (when non-nil).
// An idempotent GET is retried on a transport failure or a 429/5xx, so a
// momentary blip does not fail the caller outright.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var raw []byte
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			return err
		}
	}
	hasBody := body != nil

	attempts := 1
	if method == http.MethodGet {
		attempts = maxAttempts
	}
	var lastErr error
	for attempt := range attempts {
		if attempt > 0 {
			if err := sleepBackoff(ctx, attempt); err != nil {
				return err
			}
		}
		lastErr = c.attempt(ctx, method, path, raw, hasBody, out)
		if lastErr == nil {
			return nil
		}
		if attempt+1 < attempts && !isRetryable(lastErr) {
			return lastErr
		}
	}
	return lastErr
}

// attempt is one try of do's request.
func (c *Client) attempt(ctx context.Context, method, path string, raw []byte, hasBody bool, out any) error {
	var reader io.Reader
	if raw != nil {
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	if hasBody {
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
		return fmt.Errorf("musoak: decode %s %s: %w", method, path, err)
	}
	return nil
}

// isRetryable reports whether a failed GET is worth another try: a transport
// failure (the request may never have landed) or a server that says it is
// busy (429) or broken (5xx). A caller's own cancellation is not.
func isRetryable(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status == http.StatusTooManyRequests || apiErr.Status >= 500
	}
	return true
}

// sleepBackoff waits before retry attempt n, jittered around a doubling delay.
func sleepBackoff(ctx context.Context, attempt int) error {
	delay := retryBaseDelay << (attempt - 1)
	wait := delay/2 + time.Duration(rand.Int64N(int64(delay)+1))
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
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
	Rating     float64   `json:"rating"`
}

// Variant is one provider's rendition of a track.
type Variant struct {
	ID              string   `json:"id"`
	TrackID         string   `json:"trackId"`
	Provider        string   `json:"provider"`
	ProviderTrackID string   `json:"providerTrackId"`
	Title           string   `json:"title"`
	Artists         []string `json:"artists"`
	Album           string   `json:"album"`
	DurationMs      int64    `json:"durationMs"`
	Downloadable    bool     `json:"downloadable"`
	ISRC            string   `json:"isrc,omitempty"`
	// Slot is the account-order position this rendition fills when it is not a
	// provider's own: "self" for the caller's upload, "uploaded" for somebody
	// else's. Empty for a provider rendition, whose position is its provider.
	Slot  string      `json:"slot,omitempty"`
	Media MediaStatus `json:"media"`
}

// MediaStatus is a variant's download state.
type MediaStatus struct {
	VariantID string `json:"variantId"`
	State     string `json:"state"`
	// Stage is what the download is doing right now, one of the Media stages,
	// and empty when nothing is running.
	Stage string `json:"stage,omitempty"`
	// Progress is the fraction of the current stage that is done, and -1 when
	// there is nothing to count: a length the provider never reported, or a
	// stage with no bytes. A -1 is a caller's cue to show the stage's own
	// numbers instead of a percentage.
	Progress float64 `json:"progress"`
	Error    string  `json:"error,omitempty"`
	// DurationMs is the finished rendition's length, once it is known.
	DurationMs int64 `json:"durationMs,omitempty"`
	// Bytes and TotalBytes are the current stage's own numbers: bytes for the
	// download stage, milliseconds for the conversion. TotalBytes <= 0 means
	// unknown.
	Bytes      int64 `json:"bytes,omitempty"`
	TotalBytes int64 `json:"totalBytes,omitempty"`
}

// Media states.
const (
	MediaNone        = "none"
	MediaDownloading = "downloading"
	MediaReady       = "ready"
	MediaFailed      = "failed"
)

// Media stages: what a download is doing between "downloading" and "ready".
const (
	// MediaStageDownload means bytes are arriving from the provider.
	MediaStageDownload = "download"
	// MediaStageConvert means the fetched source is being written as the stored
	// rendition, so Bytes/TotalBytes are milliseconds there.
	MediaStageConvert = "convert"
	// MediaStageStore means the finished file is being probed and put in place.
	MediaStageStore = "store"
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
			return status, fmt.Errorf("musoak: download failed: %s", status.Error)
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

// Favorites lists the caller's favorited tracks, including saved ratings.
func (c *Client) Favorites(ctx context.Context) ([]Track, error) {
	var out struct {
		Tracks []Track `json:"tracks"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/me/favorites", nil, &out); err != nil {
		return nil, err
	}
	return out.Tracks, nil
}

// AddFavorite favorites a track with a decimal rating (0.1 to 5.0).
func (c *Client) AddFavorite(ctx context.Context, trackID string, rating ...float64) error {
	body := struct {
		TrackID string   `json:"trackId"`
		Rating  *float64 `json:"rating,omitempty"`
	}{TrackID: trackID}
	if len(rating) > 0 {
		body.Rating = &rating[0]
	}
	return c.do(ctx, http.MethodPost, "/api/v1/me/favorites", body, nil)

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
