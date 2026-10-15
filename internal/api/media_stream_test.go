package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kyleraykbs/MusOak/internal/provider"
	"github.com/kyleraykbs/MusOak/internal/store"
)

// streamStubProvider is a source that can hand out a track's audio as it
// arrives: the body is whatever the test is writing into its pipe. Its download
// is unsupported, because what a track stream does to the download behind it is
// not what these tests are about, and an unsupported download is the one error
// the manager never retries.
type streamStubProvider struct {
	name string
	body io.ReadCloser
	mime string
}

func (p *streamStubProvider) Name() string { return p.name }
func (p *streamStubProvider) Capabilities() provider.Caps {
	return provider.Caps{Search: true, Download: true, Stream: true}
}
func (p *streamStubProvider) Search(context.Context, string, provider.SearchOpts) ([]provider.Track, error) {
	return nil, nil
}
func (p *streamStubProvider) Download(context.Context, string, string, provider.ProgressFunc) error {
	return provider.ErrDownloadUnsupported
}
func (p *streamStubProvider) Stream(context.Context, string, int64) (io.ReadCloser, string, error) {
	return p.body, p.mime, nil
}

// TestTrackStreamPicksAndPlaysWithoutNamingAVariant is the point of the
// endpoint: a caller that knows only the track id gets audio, and the answer
// names the rendition the server chose for them, so nothing had to be listed or
// picked first.
func TestTrackStreamPicksAndPlaysWithoutNamingAVariant(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	pr, pw := io.Pipe()
	c.providers.Register(&streamStubProvider{name: "ytmusic", body: pr, mime: "audio/webm"})
	trackID := seedWithVariant(t, c, "Stream Me", "ytmusic")
	variantID := onlyVariant(t, c, trackID).ID

	srv := httptest.NewServer(c.Server.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/tracks/" + trackID + "/stream")
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Musoak-Variant"); got != variantID.String() {
		t.Errorf("X-Musoak-Variant = %q, want the chosen rendition %q", got, variantID)
	}
	if resp.Header.Get("X-Musoak-Stream") != "1" {
		t.Error("X-Musoak-Stream missing: a not-on-disk rendition must stream")
	}
	if got := resp.Header.Get("Content-Type"); got != "audio/webm" {
		t.Errorf("Content-Type = %q, want the source's own container", got)
	}
	// The split between this server's work and the provider's startup is
	// carried on the answer, free, so the floor streaming leaves is a number.
	if _, err := strconv.ParseInt(resp.Header.Get("X-Musoak-Provider-Ms"), 10, 64); err != nil {
		t.Errorf("X-Musoak-Provider-Ms = %q, want the provider start measured",
			resp.Header.Get("X-Musoak-Provider-Ms"))
	}

	// The provider's source is still open: the bytes go out as they arrive.
	audio := []byte("the song, arriving")
	if _, err := pw.Write(audio); err != nil {
		t.Fatalf("write source: %v", err)
	}
	_ = pw.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if !bytes.Equal(body, audio) {
		t.Errorf("streamed %q, want %q", body, audio)
	}
}

// TestTrackStreamServesTheFinishedFileWithRange checks that once the file is on
// disk this endpoint is the media URL: the whole file with an ETag, and a byte
// range answered as a 206, both still naming the rendition that was chosen.
func TestTrackStreamServesTheFinishedFileWithRange(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	trackID := seedWithVariant(t, c, "Stored Song", "ytmusic")
	variant := onlyVariant(t, c, trackID)

	audio := []byte("ogg opus bytes, whole and on disk")
	path := filepath.Join(t.TempDir(), "stored.opus")
	if err := os.WriteFile(path, audio, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(audio)
	if err := c.store.UpsertMediaFile(context.Background(), &store.MediaFile{
		VariantID:  variant.ID,
		Path:       path,
		SHA256:     hex.EncodeToString(sum[:]),
		DurationMs: 180_000,
		Bytes:      int64(len(audio)),
	}); err != nil {
		t.Fatalf("UpsertMediaFile: %v", err)
	}

	srv := httptest.NewServer(c.Server.Handler())
	t.Cleanup(srv.Close)
	url := srv.URL + "/api/v1/tracks/" + trackID + "/stream"

	// Whole file.
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("whole file = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Musoak-Variant"); got != variant.ID.String() {
		t.Errorf("X-Musoak-Variant = %q, want %q", got, variant.ID)
	}
	if resp.Header.Get("X-Musoak-Stream") != "" {
		t.Error("a finished file must not be marked as a stream")
	}
	if got := resp.Header.Get("ETag"); got != `"`+hex.EncodeToString(sum[:])+`"` {
		t.Errorf("ETag = %q, want the file's own hash", got)
	}
	if body, err := io.ReadAll(resp.Body); err != nil || !bytes.Equal(body, audio) {
		t.Errorf("body = %q (%v), want the whole file", body, err)
	}

	// A byte range is a 206 with a Content-Range, not a stream from the source.
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=0-9")
	rangeResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer rangeResp.Body.Close()
	if rangeResp.StatusCode != http.StatusPartialContent {
		t.Fatalf("range = %d, want 206", rangeResp.StatusCode)
	}
	if got := rangeResp.Header.Get("Content-Range"); got != "bytes 0-9/"+itoa(len(audio)) {
		t.Errorf("Content-Range = %q", got)
	}
	chunk, err := io.ReadAll(rangeResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(chunk, audio[:10]) {
		t.Errorf("range body = %q, want the first ten bytes", chunk)
	}
}

// TestTrackStreamWithoutDownloadableRenditionIs404 checks the clean refusal: a
// track whose only renditions are metadata-only, and one with none at all, are
// each a 404 that says what is missing rather than an empty or half answer.
func TestTrackStreamWithoutDownloadableRenditionIs404(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	ctx := context.Background()

	metadataOnly := seedTracks(t, c, "Metadata Only")[0]
	if err := c.store.CreateVariant(ctx, &store.Variant{
		TrackID:      metadataOnly.ID,
		Provider:     "spotify",
		Title:        "Metadata Only",
		Artists:      []string{"Artist"},
		Downloadable: false,
	}); err != nil {
		t.Fatalf("CreateVariant: %v", err)
	}
	noRendition := seedTracks(t, c, "Nothing At All")[0]

	srv := httptest.NewServer(c.Server.Handler())
	t.Cleanup(srv.Close)

	for _, track := range []*store.Track{metadataOnly, noRendition} {
		resp, err := http.Get(srv.URL + "/api/v1/tracks/" + track.ID.String() + "/stream")
		if err != nil {
			t.Fatalf("%s: %v", track.Title, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", track.Title, resp.StatusCode)
		}
		if !strings.Contains(string(body), "no downloadable rendition") {
			t.Errorf("%s body = %q, want it to name the missing rendition", track.Title, body)
		}
	}

	// A track that does not exist is a 404 too, and a malformed id a 400.
	if resp, err := http.Get(srv.URL + "/api/v1/tracks/" + uuid.Nil.String() + "/stream"); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("unknown track = %d, want 404", resp.StatusCode)
		}
	}
	if resp, err := http.Get(srv.URL + "/api/v1/tracks/not-a-uuid/stream"); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("bad id = %d, want 400", resp.StatusCode)
		}
	}
}

// TestTrackStreamPrefersTheCallersRanking checks that the pick is the caller's
// own order, not merely the first row in the store: with two downloadable
// renditions, the one whose provider the caller ranked first is streamed.
func TestTrackStreamPrefersTheCallersRanking(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	pr, pw := io.Pipe()
	defer pw.Close()
	for _, name := range []string{"ytmusic", "youtube"} {
		c.providers.Register(&streamStubProvider{name: name, body: pr, mime: "audio/webm"})
	}
	trackID := seedWithVariants(t, c, "Two Ways", "ytmusic", "youtube")
	token := c.register("kyle", "hunter2hunter2")
	if rec := c.do(http.MethodPut, "/api/v1/me/providers/ranking", token,
		map[string]any{"ranking": []string{"youtube", "ytmusic"}}); rec.Code != http.StatusOK {
		t.Fatalf("set ranking = %d: %s", rec.Code, rec.Body.String())
	}

	srv := httptest.NewServer(c.Server.Handler())
	t.Cleanup(srv.Close)
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/tracks/"+trackID+"/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	want := variantFor(t, c, trackID, "youtube")
	if got := resp.Header.Get("X-Musoak-Variant"); got != want.ID.String() {
		t.Errorf("X-Musoak-Variant = %q, want the ranked-first rendition %q", got, want.ID)
	}
}

// onlyVariant returns a track's single rendition, failing the test if the seed
// helper did not make exactly one.
func onlyVariant(t *testing.T, c *testClient, trackID string) store.Variant {
	t.Helper()
	variants, err := c.store.VariantsForTrack(context.Background(), uuid.MustParse(trackID))
	if err != nil {
		t.Fatalf("VariantsForTrack: %v", err)
	}
	if len(variants) != 1 {
		t.Fatalf("variants = %d, want 1", len(variants))
	}
	return variants[0]
}

// variantFor returns the rendition of a track in the named provider.
func variantFor(t *testing.T, c *testClient, trackID, providerName string) store.Variant {
	t.Helper()
	variants, err := c.store.VariantsForTrack(context.Background(), uuid.MustParse(trackID))
	if err != nil {
		t.Fatalf("VariantsForTrack: %v", err)
	}
	for _, variant := range variants {
		if variant.Provider == providerName {
			return variant
		}
	}
	t.Fatalf("no %s rendition of %s", providerName, trackID)
	return store.Variant{}
}
