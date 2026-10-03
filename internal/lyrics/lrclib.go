package lyrics

import (
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

// LRCLIBBaseURL is the public lyrics database the service asks by default. It
// needs no key and no account.
const LRCLIBBaseURL = "https://lrclib.net"

// maxLRCLIBBody caps how much of a response is read. Lyrics are kilobytes; a
// response larger than this is not one.
const maxLRCLIBBody = 1 << 20

// LRCLIB is a Fetcher backed by lrclib.net.
type LRCLIB struct {
	base   string
	client *http.Client
}

// NewLRCLIB returns a fetcher talking to the public lrclib.net.
func NewLRCLIB() *LRCLIB {
	return &LRCLIB{base: LRCLIBBaseURL, client: &http.Client{Timeout: 15 * time.Second}}
}

// lrclibResponse is the part of /api/get the service uses. Absent fields read
// as their zero value, which is exactly how the API omits them.
type lrclibResponse struct {
	SyncedLyrics string `json:"syncedLyrics"`
	PlainLyrics  string `json:"plainLyrics"`
	Instrumental bool   `json:"instrumental"`
	TrackName    string `json:"trackName"`
	ArtistName   string `json:"artistName"`
}

// Fetch asks LRCLIB for one recording. A 404 means the database has nothing,
// which is a nil Result rather than an error.
func (l *LRCLIB) Fetch(ctx context.Context, q Query) (Result, error) {
	params := url.Values{}
	params.Set("artist_name", q.Artist)
	params.Set("track_name", q.Track)
	if q.Album != "" {
		params.Set("album_name", q.Album)
	}
	if q.DurationMs > 0 {
		// The API matches durations in whole seconds; round to the nearest.
		params.Set("duration", strconv.FormatInt((q.DurationMs+500)/1000, 10))
	}

	endpoint := l.base + "/api/get?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Result{}, err
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return Result{}, nil
	default:
		return Result{}, fmt.Errorf("lrclib: %s", resp.Status)
	}

	var body lrclibResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxLRCLIBBody)).Decode(&body); err != nil {
		return Result{}, fmt.Errorf("lrclib: %w", err)
	}
	if body.Instrumental {
		return Result{Instrumental: true}, nil
	}
	return Result{
		Synced: ParseLRC(body.SyncedLyrics),
		Plain:  strings.TrimSpace(body.PlainLyrics),
	}, nil
}
