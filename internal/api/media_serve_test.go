package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kyleraykbs/MusOak/internal/provider"
	"github.com/kyleraykbs/MusOak/internal/store"
)

// serveStubProvider is a source that records how it was used. The media
// endpoint must never ask a provider for bytes, and this is how the tests check
// that it did not: the download count stays zero.
type serveStubProvider struct {
	name string

	mu        sync.Mutex
	downloads int
}

func (p *serveStubProvider) Name() string { return p.name }
func (p *serveStubProvider) Capabilities() provider.Caps {
	return provider.Caps{Search: true, Download: true}
}
func (p *serveStubProvider) Search(context.Context, string, provider.SearchOpts) ([]provider.Track, error) {
	return nil, nil
}
func (p *serveStubProvider) Download(context.Context, string, string, provider.ProgressFunc) error {
	p.mu.Lock()
	p.downloads++
	p.mu.Unlock()
	return nil
}

func (p *serveStubProvider) downloadCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.downloads
}

// soleVariant returns a track's single rendition, failing the test if the seed
// helper did not make exactly one.
func soleVariant(t *testing.T, c *testClient, trackID string) store.Variant {
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

// seedHeldFile writes a rendition's bytes to disk and records the media row, so
// the serve path has a finished file to answer from. The size and duration are
// the caller's, so `?start=` maps to a predictable byte.
func seedHeldFile(t *testing.T, c *testClient, variantID string, audio []byte, durationMs int64) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "held.opus")
	if err := os.WriteFile(path, audio, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(audio)
	if err := c.store.UpsertMediaFile(context.Background(), &store.MediaFile{
		VariantID:  uuid.MustParse(variantID),
		Path:       path,
		SHA256:     hex.EncodeToString(sum[:]),
		DurationMs: durationMs,
		Bytes:      int64(len(audio)),
	}); err != nil {
		t.Fatalf("UpsertMediaFile: %v", err)
	}
}

// TestMediaFileRefusesARenditionNotOnDisk is the new contract: a downloadable
// rendition whose bytes have not been fetched is refused with a 409 pointing at
// the download endpoint, rather than played from its source. No provider is
// asked for anything, and the answer is JSON, not audio.
func TestMediaFileRefusesARenditionNotOnDisk(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	stub := &serveStubProvider{name: "ytmusic"}
	c.providers.Register(stub)
	trackID := seedWithVariant(t, c, "Not Fetched", "ytmusic")
	variant := soleVariant(t, c, trackID)

	srv := httptest.NewServer(c.Server.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/media/" + variant.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want JSON, not audio", ct)
	}
	if resp.Header.Get("ETag") != "" {
		t.Error("a refusal must not carry an ETag")
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("refusal body %q is not the JSON envelope: %v", body, err)
	}
	if !strings.Contains(envelope.Error, "not downloaded yet") {
		t.Errorf("error = %q, want it to say the rendition is not downloaded yet", envelope.Error)
	}
	if !strings.Contains(envelope.Error, variant.ID.String()) {
		t.Errorf("error = %q, want it to name the variant %s", envelope.Error, variant.ID)
	}

	if n := stub.downloadCalls(); n != 0 {
		t.Errorf("provider Download called %d times, want 0: a media GET must not fetch", n)
	}
}

// TestMediaFileRefusesAMetadataOnlyRendition checks the other refusal: a
// rendition with no downloadable source is told to be resolved, mirroring the
// download endpoint's answer.
func TestMediaFileRefusesAMetadataOnlyRendition(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	ctx := context.Background()
	track := seedTracks(t, c, "Metadata Only")[0]
	variant := &store.Variant{
		TrackID:      track.ID,
		Provider:     "spotify",
		Title:        "Metadata Only",
		Artists:      []string{"Artist"},
		Downloadable: false,
	}
	if err := c.store.CreateVariant(ctx, variant); err != nil {
		t.Fatalf("CreateVariant: %v", err)
	}

	srv := httptest.NewServer(c.Server.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/media/" + variant.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("refusal body %q is not the JSON envelope: %v", body, err)
	}
	if !strings.Contains(envelope.Error, "not downloadable") {
		t.Errorf("error = %q, want it to say the variant is not downloadable", envelope.Error)
	}
}

// TestMediaFileServesTheHeldFileWithRangeAndStart pins the on-disk behaviour: a
// finished file is the whole file with an ETag, a byte range is a 206 with a
// Content-Range, and `?start=` begins it at the byte its millisecond position
// maps to. A file of a known size and a known duration makes that byte exact.
func TestMediaFileServesTheHeldFileWithRangeAndStart(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	trackID := seedWithVariant(t, c, "Stored Song", "ytmusic")
	variant := soleVariant(t, c, trackID)

	audio := bytes.Repeat([]byte("abcd"), 25) // 100 bytes
	const durationMs = 1000
	seedHeldFile(t, c, variant.ID.String(), audio, durationMs)
	sum := sha256.Sum256(audio)

	srv := httptest.NewServer(c.Server.Handler())
	t.Cleanup(srv.Close)
	url := srv.URL + "/api/v1/media/" + variant.ID.String()

	// The whole file, with the ETag of its bytes.
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("whole file = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("ETag"); got != `"`+hex.EncodeToString(sum[:])+`"` {
		t.Errorf("ETag = %q, want the file's own hash", got)
	}
	if got := resp.Header.Get("Content-Type"); got != "audio/ogg" {
		t.Errorf("Content-Type = %q, want audio/ogg", got)
	}
	if body, err := io.ReadAll(resp.Body); err != nil || !bytes.Equal(body, audio) {
		t.Errorf("body = %q (%v), want the whole file", body, err)
	}
	resp.Body.Close()

	// A byte range is a 206 with a Content-Range.
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=0-9")
	rangeResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if rangeResp.StatusCode != http.StatusPartialContent {
		t.Fatalf("range = %d, want 206", rangeResp.StatusCode)
	}
	if got := rangeResp.Header.Get("Content-Range"); got != "bytes 0-9/"+itoa(len(audio)) {
		t.Errorf("Content-Range = %q", got)
	}
	if chunk, err := io.ReadAll(rangeResp.Body); err != nil || !bytes.Equal(chunk, audio[:10]) {
		t.Errorf("range body = %q (%v), want the first ten bytes", chunk, err)
	}
	rangeResp.Body.Close()

	// ?start=250 into a 1000 ms track maps to byte 25 of 100; the answer is the
	// remainder, with a Content-Length that says so.
	const startMs = 250
	wantOffset := startMs * len(audio) / durationMs // 25
	startResp, err := http.Get(url + "?start=" + itoa(startMs))
	if err != nil {
		t.Fatal(err)
	}
	if startResp.StatusCode != http.StatusPartialContent {
		t.Fatalf("start = %d, want 206", startResp.StatusCode)
	}
	if got, want := startResp.Header.Get("Content-Range"), "bytes "+itoa(wantOffset)+"-"+itoa(len(audio)-1)+"/"+itoa(len(audio)); got != want {
		t.Errorf("start Content-Range = %q, want %q", got, want)
	}
	if got, want := startResp.Header.Get("Content-Length"), itoa(len(audio)-wantOffset); got != want {
		t.Errorf("start Content-Length = %q, want the remainder %q", got, want)
	}
	if body, err := io.ReadAll(startResp.Body); err != nil || !bytes.Equal(body, audio[wantOffset:]) {
		t.Errorf("start body = %q (%v), want the file from byte %d", body, err, wantOffset)
	}
	startResp.Body.Close()

	// A Range header is the client's own request and wins over ?start=.
	wonReq, err := http.NewRequest(http.MethodGet, url+"?start="+itoa(startMs), nil)
	if err != nil {
		t.Fatal(err)
	}
	wonReq.Header.Set("Range", "bytes=0-9")
	wonResp, err := http.DefaultClient.Do(wonReq)
	if err != nil {
		t.Fatal(err)
	}
	if wonResp.StatusCode != http.StatusPartialContent {
		t.Fatalf("range over start = %d, want 206", wonResp.StatusCode)
	}
	if got := wonResp.Header.Get("Content-Range"); got != "bytes 0-9/"+itoa(len(audio)) {
		t.Errorf("range over start Content-Range = %q, want the Range header honoured", got)
	}
	wonResp.Body.Close()

	// A zero or unparseable start is the whole file, exactly as before.
	for _, query := range []string{"?start=0", "?start=nonsense"} {
		whole, err := http.Get(url + query)
		if err != nil {
			t.Fatal(err)
		}
		if whole.StatusCode != http.StatusOK {
			t.Errorf("%s = %d, want the whole file as 200", query, whole.StatusCode)
		}
		if body, err := io.ReadAll(whole.Body); err != nil || !bytes.Equal(body, audio) {
			t.Errorf("%s body = %q (%v), want the whole file", query, body, err)
		}
		whole.Body.Close()
	}
}

// TestProvidersCapsHaveNoStreamField checks the capability JSON no longer
// offers a streaming flag: every rendition is a file this server holds.
func TestProvidersCapsHaveNoStreamField(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	c.providers.Register(&serveStubProvider{name: "ytmusic"})

	rec := c.do(http.MethodGet, "/api/v1/providers", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("providers = %d: %s", rec.Code, rec.Body.String())
	}
	var doc struct {
		Providers []struct {
			Name         string         `json:"name"`
			Capabilities map[string]any `json:"capabilities"`
		} `json:"providers"`
	}
	c.decode(rec, &doc)
	if len(doc.Providers) == 0 {
		t.Fatal("no providers in the answer")
	}
	for _, p := range doc.Providers {
		if _, ok := p.Capabilities["stream"]; ok {
			t.Errorf("%s capabilities still carry a stream flag", p.Name)
		}
		if _, ok := p.Capabilities["download"]; !ok {
			t.Errorf("%s capabilities lost the download flag", p.Name)
		}
	}
	if strings.Contains(rec.Body.String(), `"stream"`) {
		t.Errorf("providers body still mentions stream: %s", rec.Body.String())
	}
}

// Cancelling a download asks the manager to stop it, and says the same thing
// whether or not one was running: the client lets go of a song at any point in
// its fetch.
func TestMediaCancelIsIdempotent(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	id := uuid.NewString()
	for attempt := range 2 {
		rec := c.do(http.MethodDelete, "/api/v1/media/"+id+"/download", "", nil)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("cancel %d = %d: %s", attempt, rec.Code, rec.Body.String())
		}
	}
	if rec := c.do(http.MethodDelete, "/api/v1/media/not-a-uuid/download", "", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad variant id = %d, want 400", rec.Code)
	}
}
