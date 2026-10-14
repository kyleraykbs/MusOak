package media

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kyleraykbs/MusOak/internal/provider"
	"github.com/kyleraykbs/MusOak/internal/store"
)

// streamProvider serves a source as a pipe the test drives, and can hold its
// download open to watch what happens to it when the listener goes away.
type streamProvider struct {
	*fakeProvider
	contentType string
	streamErr   error

	// pipe is what Stream hands out; writer is the test's end of it.
	pipe   *io.PipeReader
	writer *io.PipeWriter

	// hold keeps a download open until the test releases it.
	hold     bool
	started  chan struct{}
	release  chan struct{}
	recorded chan struct{}

	mu      sync.Mutex
	heldErr error
}

func (p *streamProvider) Capabilities() provider.Caps {
	return provider.Caps{Search: true, Download: true, Stream: true}
}

func (p *streamProvider) Stream(context.Context, string) (io.ReadCloser, string, error) {
	if p.streamErr != nil {
		return nil, "", p.streamErr
	}
	return p.pipe, p.contentType, nil
}

func (p *streamProvider) Download(ctx context.Context, id, dest string) error {
	if !p.hold {
		return p.fakeProvider.Download(ctx, id, dest)
	}
	if p.started != nil {
		select {
		case p.started <- struct{}{}:
		default:
		}
	}
	if p.release != nil {
		<-p.release
	}
	p.mu.Lock()
	p.heldErr = ctx.Err()
	p.mu.Unlock()
	if p.recorded != nil {
		close(p.recorded)
	}
	return errors.New("stream test: the download is not what is under test here")
}

// serveStream puts a manager's stream behind a real HTTP server, so the test
// sees what a listener sees.
func serveStream(t *testing.T, m *Manager, variantID uuid.UUID) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.Stream(w, r, variantID) {
			http.Error(w, "media not ready", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A track that is still downloading is played from its source as it arrives, not
// buffered into one piece, and the answer says what it is so no client mistakes
// it for the finished rendition.
func TestStreamSendsTheSourceAsItArrives(t *testing.T) {
	pr, pw := io.Pipe()
	p := &streamProvider{
		fakeProvider: &fakeProvider{name: "ytmusic"},
		contentType:  "audio/webm",
		pipe:         pr,
		writer:       pw,
	}
	m, db := newTestManager(t, p)
	m.SetDownloadRetry(1, time.Millisecond)
	variant := seedVariant(t, db, true, "ytmusic")
	srv := serveStream(t, m, variant.ID)

	first := bytes.Repeat([]byte("a"), streamChunk)
	rest := []byte("the tail of the song")

	type read struct {
		chunk []byte
		err   error
	}
	chunks := make(chan read, 2)
	go func() {
		resp, err := http.Get(srv.URL)
		if err != nil {
			chunks <- read{err: err}
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			chunks <- read{err: errors.New("status " + resp.Status)}
			return
		}
		for _, header := range []struct{ name, want string }{
			{"X-Musoak-Stream", "1"},
			{"Cache-Control", "no-store"},
			{"ETag", ""},
			{"Content-Type", "audio/webm"},
		} {
			if got := resp.Header.Get(header.name); got != header.want {
				t.Errorf("%s = %q, want %q", header.name, got, header.want)
			}
		}
		buf := make([]byte, streamChunk)
		if _, err := io.ReadFull(resp.Body, buf); err != nil {
			chunks <- read{err: err}
			return
		}
		chunks <- read{chunk: buf}
		rest, err := io.ReadAll(resp.Body)
		chunks <- read{chunk: rest, err: err}
	}()

	// The listener has the headers and no audio yet: the fetch is still running.
	time.Sleep(50 * time.Millisecond)
	if _, err := pw.Write(first); err != nil {
		t.Fatalf("write first chunk: %v", err)
	}
	select {
	case got := <-chunks:
		if got.err != nil {
			t.Fatalf("read first chunk: %v", got.err)
		}
		if !bytes.Equal(got.chunk, first) {
			t.Errorf("first chunk = %d bytes, want the %d that were written", len(got.chunk), len(first))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first chunk did not reach the listener while the source was still open")
	}

	if _, err := pw.Write(rest); err != nil {
		t.Fatalf("write tail: %v", err)
	}
	_ = pw.Close()
	select {
	case got := <-chunks:
		if got.err != nil {
			t.Fatalf("read tail: %v", got.err)
		}
		if !bytes.Equal(got.chunk, rest) {
			t.Errorf("tail = %q, want %q", got.chunk, rest)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the tail never arrived")
	}
}

// A finished file is served from disk; the stream is for the gap before it.
func TestStreamLeavesAFinishedFileToDisk(t *testing.T) {
	fixture := tone(t, t.TempDir(), "fixture.opus", 0.5)
	pr, pw := io.Pipe()
	defer pw.Close()
	p := &streamProvider{
		fakeProvider: &fakeProvider{name: "ytmusic", fixture: fixture},
		contentType:  "audio/webm",
		pipe:         pr,
		writer:       pw,
	}
	m, db := newTestManager(t, p)
	variant := seedVariant(t, db, true, "ytmusic")
	if _, err := m.Ensure(context.Background(), variant.ID); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/media/"+variant.ID.String(), nil)
	if m.Stream(httptest.NewRecorder(), req, variant.ID) {
		t.Fatal("a finished file must be served from disk, not streamed")
	}
}

// Nothing to stream from: the request falls through to the file and its status,
// exactly as it did before there was a stream at all.
func TestStreamRefusesWhatItCannotStream(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()

	cases := []struct {
		name         string
		variantOwner string
		downloadable bool
		provider     provider.Provider
	}{
		{
			name:         "a provider that cannot stream",
			variantOwner: "plain",
			downloadable: true,
			provider:     &fakeProvider{name: "plain"},
		},
		{
			name:         "a source with no audio (metadata only)",
			variantOwner: "spotify",
			downloadable: false,
			provider:     &fakeProvider{name: "spotify"},
		},
		{
			name:         "bytes that exist only on this disk",
			variantOwner: store.LocalProvider,
			downloadable: true,
			provider:     &fakeProvider{name: store.LocalProvider},
		},
		{
			name:         "a provider whose fetch refuses",
			variantOwner: "ytmusic",
			downloadable: true,
			provider: &streamProvider{
				fakeProvider: &fakeProvider{name: "ytmusic"},
				streamErr:    errors.New("no audio for that id"),
				pipe:         pr,
				writer:       pw,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, db := newTestManager(t, tc.provider)
			m.SetDownloadRetry(1, time.Millisecond)
			variant := seedVariant(t, db, tc.downloadable, tc.variantOwner)

			req := httptest.NewRequest(http.MethodGet, "/api/v1/media/"+variant.ID.String(), nil)
			if m.Stream(httptest.NewRecorder(), req, variant.ID) {
				t.Fatal("Stream claimed to stream something it cannot")
			}
		})
	}
}

// The listener hanging up mid-song must not call off the download behind it: the
// next listener is waiting for that file.
func TestStreamKeepsTheDownloadAfterTheListenerLeaves(t *testing.T) {
	pr, pw := io.Pipe()
	p := &streamProvider{
		fakeProvider: &fakeProvider{name: "ytmusic"},
		contentType:  "audio/webm",
		pipe:         pr,
		writer:       pw,
		hold:         true,
		started:      make(chan struct{}, 4),
		release:      make(chan struct{}),
		recorded:     make(chan struct{}),
	}
	m, db := newTestManager(t, p)
	m.SetDownloadRetry(1, time.Millisecond)
	variant := seedVariant(t, db, true, "ytmusic")
	srv := serveStream(t, m, variant.ID)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("the stream never started: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", resp.StatusCode)
	}

	cancel()      // the listener skipped mid-song
	_ = resp.Body.Close()
	_ = pw.Close() // the source ends, so the handler can return

	select {
	case <-p.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the download behind the stream never started")
	}
	close(p.release)
	select {
	case <-p.recorded:
	case <-time.After(5 * time.Second):
		t.Fatal("the download never finished looking at its context")
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.heldErr != nil {
		t.Fatalf("the download was cancelled with the listener: %v", p.heldErr)
	}
}

// A player that starts part-way in - a session picked up where it was left, or a
// room whose song is already a minute gone - asks for a byte range. What a
// source hands out starts at zero, so answering the range from the source means
// the element buffers from the beginning to reach that position, which on a
// three minute song is most of it. The file the download is being written to
// holds the bytes from the start, so a range it covers is answered from there.
func TestStreamAnswersARangeFromTheDownloadedPart(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte("from the source"))
		_ = pw.Close()
	}()
	// The download is held: this test is about what is already written, and a
	// download that fails would clean its part file up underneath it.
	p := &streamProvider{
		fakeProvider: &fakeProvider{name: "ytmusic"},
		contentType:  "audio/webm",
		pipe:         pr,
		hold:         true,
		started:      make(chan struct{}, 1),
		release:      make(chan struct{}),
	}
	m, db := newTestManager(t, p)
	m.SetDownloadRetry(1, time.Millisecond)
	variant := seedVariant(t, db, true, "ytmusic")

	// What the download has written so far: sixty-four bytes from the start. The
	// manager makes its directory when it downloads something, so the test makes
	// it for what it is pretending was downloaded.
	part := bytes.Repeat([]byte("abcdefgh"), 8)
	partPath := m.pathFor(variant.ID) + ".part"
	if err := os.MkdirAll(filepath.Dir(partPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partPath, part, 0o644); err != nil {
		t.Fatal(err)
	}
	srv := serveStream(t, m, variant.ID)

	ask := func(spec string) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Range", spec)
		response, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		return response, body
	}

	response, body := ask("bytes=16-31")
	if response.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", response.StatusCode)
	}
	if !bytes.Equal(body, part[16:32]) {
		t.Errorf("body = %q, want the bytes the part file holds", body)
	}
	if got := response.Header.Get("X-Musoak-Stream"); got != "1" {
		t.Errorf("X-Musoak-Stream = %q: a part file is not the rendition this URL names", got)
	}

	// A range the download has not reached is not answered from it: the source
	// is streamed instead, which is what this path did for every range before.
	response, body = ask("bytes=400-500")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the streamed answer", response.StatusCode)
	}
	if string(body) != "from the source" {
		t.Errorf("body = %q, want the source", body)
	}
}
