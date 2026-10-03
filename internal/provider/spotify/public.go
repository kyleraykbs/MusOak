package spotify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"codeberg.org/kyleraykbs/musoak/internal/provider"
)

// A playlist without credentials.
//
// The Web API wants client credentials, which a household may not have - and a
// playlist someone wants to bring over is usually a public one. The embed page
// does not need an account: Spotify renders the playlist into it as JSON, with
// each track's name, artists and length. That is everything the matcher needs
// to find the same song on a provider that can be played, which is the whole
// point of importing a playlist from somewhere it cannot be played.
//
// The page's shape is Spotify's to change, so the tracklist is found by looking
// for it rather than by a fixed path: any object that carries both a title and a
// trackList is the playlist, wherever it sits in the document.

const (
	embedBase = "https://open.spotify.com/embed/playlist/"
	// maxEmbedBytes caps what is read from the embed page.
	maxEmbedBytes = 8 << 20
	// embedUserAgent is a browser's, because the embed page is served to
	// browsers and nothing else is promised.
	embedUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"
)

// ErrNoPublicPlaylist means the page carried no tracklist: a private playlist, a
// wrong id, or a page whose shape has changed.
var ErrNoPublicPlaylist = errors.New("spotify: no playlist in the embed page")

// publicPlaylist reads a playlist from its public embed page.
func (p *Provider) publicPlaylist(ctx context.Context, providerPlaylistID string) (*provider.PlaylistDetail, error) {
	id := strings.TrimSpace(providerPlaylistID)
	if id == "" {
		return nil, fmt.Errorf("%w: no playlist id", ErrNoPublicPlaylist)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, embedBase+id, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", embedUserAgent)
	req.Header.Set("Accept", "text/html")
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("spotify: embed %s: %w", id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("spotify: embed %s answered %s", id, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxEmbedBytes))
	if err != nil {
		return nil, fmt.Errorf("spotify: embed %s: %w", id, err)
	}
	entity, ok := embedEntity(body)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoPublicPlaylist, id)
	}
	detail := detailFromEntity(id, entity)
	p.logger.Info("spotify playlist read without credentials", "playlist", id, "tracks", len(detail.Tracks))
	return detail, nil
}

// embedEntity pulls the playlist object out of an embed page. It is the JSON in
// the __NEXT_DATA__ script, found by shape rather than by path.
func embedEntity(page []byte) (map[string]any, bool) {
	start := indexOf(page, []byte(`id="__NEXT_DATA__"`))
	if start < 0 {
		return nil, false
	}
	open := indexOf(page[start:], []byte(">"))
	closeTag := indexOf(page[start:], []byte("</script>"))
	if open < 0 || closeTag < 0 || closeTag <= open {
		return nil, false
	}
	raw := page[start+open+1 : start+closeTag]
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, false
	}
	entity := findPlaylistObject(root)
	return entity, entity != nil
}

// findPlaylistObject walks the document for the object that looks like the
// playlist: a title and a trackList, at whatever depth they happen to be.
func findPlaylistObject(node any) map[string]any {
	switch value := node.(type) {
	case map[string]any:
		if _, hasTracks := value["trackList"].([]any); hasTracks {
			if _, hasTitle := value["title"].(string); hasTitle {
				return value
			}
		}
		for _, child := range value {
			if found := findPlaylistObject(child); found != nil {
				return found
			}
		}
	case []any:
		for _, child := range value {
			if found := findPlaylistObject(child); found != nil {
				return found
			}
		}
	}
	return nil
}

// detailFromEntity turns the embed's playlist into the shape every provider
// returns. A track's `subtitle` is its artists, joined; its `uri` is where the
// provider track id lives.
func detailFromEntity(id string, entity map[string]any) *provider.PlaylistDetail {
	detail := &provider.PlaylistDetail{
		Playlist: provider.Playlist{
			ProviderPlaylistID: id,
			Title:              stringOf(entity["title"]),
			Owner:              stringOf(entity["subtitle"]),
			Description:        stringOf(entity["description"]),
		},
	}
	if detail.Title == "" {
		detail.Title = stringOf(entity["name"])
	}
	tracks, _ := entity["trackList"].([]any)
	for _, entry := range tracks {
		item, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		providerID := strings.TrimSpace(strings.TrimPrefix(stringOf(item["uri"]), "spotify:track:"))
		title := stringOf(item["title"])
		if providerID == "" || title == "" {
			continue
		}
		detail.Tracks = append(detail.Tracks, provider.Track{
			ProviderTrackID: providerID,
			Title:           title,
			Artists:         splitArtists(stringOf(item["subtitle"])),
			Album:           stringOf(item["album"]),
			DurationMs:      int64Of(item["duration"]),
			ArtworkURL:      stringOf(item["coverArt"]),
		})
	}
	detail.TrackCount = len(detail.Tracks)
	return detail
}

// splitArtists turns the embed's artist line back into names. Spotify joins
// them with commas, and the last one may carry an "and".
func splitArtists(line string) []string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return nil
	}
	parts := strings.Split(trimmed, ",")
	artists := make([]string, 0, len(parts))
	for _, part := range parts {
		if name := strings.TrimSpace(part); name != "" {
			artists = append(artists, name)
		}
	}
	return artists
}

func stringOf(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func int64Of(value any) int64 {
	number, ok := value.(float64)
	if !ok || number < 0 {
		return 0
	}
	return int64(number)
}

// indexOf is bytes.Index without importing bytes for two call sites.
func indexOf(haystack, needle []byte) int {
	if len(needle) == 0 || len(haystack) < len(needle) {
		return -1
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
