package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kyleraykbs/MusOak/internal/config"
	"github.com/kyleraykbs/MusOak/pkg/client"
)

// localItem is the delivery for a rendition already on disk: the file to play,
// from wherever it is on this machine.
func localItem(path string) playbackItem {
	return playbackItem{source: path}
}

// newSlotApp is a minimal App enough for slot resolution and delivery: a client
// pointed at a server and an empty cache.
func newSlotApp(t *testing.T, baseURL string) *App {
	t.Helper()
	cache, err := client.OpenCache(t.TempDir())
	if err != nil {
		t.Fatalf("OpenCache: %v", err)
	}
	return &App{
		cfg:    config.Default(),
		logger: slog.New(slog.DiscardHandler),
		client: client.New(baseURL),
		cache:  cache,
	}
}

// TestSlotDeliversTheFetchedFile checks the whole point of a slot: a track that
// is not on disk is fetched, and what it hands back is the file that landed -
// never a URL, because the server refuses a rendition it has not stored.
func TestSlotDeliversTheFetchedFile(t *testing.T) {
	body := []byte("opus-bytes")
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/tracks/t1/variants", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"variants": []map[string]any{
			{"id": "v1", "provider": "p", "downloadable": true},
		}})
	})
	mux.HandleFunc("/api/v1/media/v1/download", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, client.MediaStatus{VariantID: "v1", State: client.MediaReady, DurationMs: 300_000})
	})
	mux.HandleFunc("/api/v1/media/v1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", etag)
		_, _ = w.Write(body)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	app := newSlotApp(t, server.URL)
	order := func(context.Context) []string { return []string{"p"} }
	s := app.startSlot(context.Background(), queueItem{TrackID: "t1", Title: "Song"}, order)
	<-s.resolved
	if s.err != nil {
		t.Fatalf("resolve: %v", s.err)
	}
	if s.variantID != "v1" {
		t.Fatalf("variantID = %q, want the resolved rendition", s.variantID)
	}

	entry, ok := <-s.local
	if !ok {
		t.Fatal("the slot delivered no file")
	}
	if entry.Path == "" || !strings.Contains(entry.Path, app.cache.Dir()) {
		t.Errorf("path = %q, want a file in the cache %q", entry.Path, app.cache.Dir())
	}
	if _, err := os.Stat(entry.Path); err != nil {
		t.Errorf("the delivered file is not on disk: %v", err)
	}
}

// TestSlotUsesTheCachedCopy checks that a rendition this client already holds
// needs no resolution and no download.
func TestSlotUsesTheCachedCopy(t *testing.T) {
	app := newSlotApp(t, "http://127.0.0.1:1")
	path := app.cache.Path("v1")
	if err := os.WriteFile(path, []byte("tone"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := app.cache.Put(client.CacheEntry{VariantID: "v1", TrackID: "t1", Path: path}); err != nil {
		t.Fatal(err)
	}

	order := func(context.Context) []string { return nil }
	s := app.startSlot(context.Background(), queueItem{TrackID: "t1", Title: "Song"}, order)
	<-s.resolved
	if s.err != nil {
		t.Fatalf("resolve: %v", s.err)
	}
	entry, ok := <-s.local
	if !ok || entry.Path != path {
		t.Fatalf("entry = %+v (ok %v), want the cached file %q", entry, ok, path)
	}
}

// writeJSON writes v as JSON, for a stub server.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// TestSlotDownloadSurvivesContextCancel checks that cancelling the command's
// context — what a skip or a track change amounts to — does not abandon an
// in-flight download. The slot detaches the transfer, so the file still lands.
func TestSlotDownloadSurvivesContextCancel(t *testing.T) {
	const variantID = "v1"
	release := make(chan struct{})
	// started fires when the server has the request in hand and is writing the
	// body, which is what "the transfer is in flight" means here.
	started := make(chan struct{}, 1)
	body := []byte("first-half-second-half")
	hashed := sha256.Sum256(body)
	etag := hex.EncodeToString(hashed[:])

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/ranking", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"effective": []string{"p"}})
	})
	mux.HandleFunc("/api/v1/tracks/t1/variants", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"variants": []map[string]any{
			{"id": variantID, "provider": "p", "downloadable": true},
		}})
	})
	mux.HandleFunc("/api/v1/media/"+variantID+"/download", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, client.MediaStatus{VariantID: variantID, State: client.MediaReady})
	})
	mux.HandleFunc("/api/v1/media/"+variantID, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"`+etag+`"`)
		_, _ = w.Write(body[:len("first-half")])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		_, _ = w.Write(body[len("first-half"):])
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	app := newSlotApp(t, server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The play session's provider order, as cmdPlay would resolve it once.
	order := func(context.Context) []string { return []string{"p"} }
	s := app.startSlot(ctx, queueItem{TrackID: "t1", Title: "Song"}, order)
	<-s.resolved
	if s.err != nil {
		t.Fatalf("resolve: %v", s.err)
	}

	// Wait until the transfer is actually in flight, then cancel the context
	// the way a skip would and let the server finish the response.
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the download did not start")
	}
	cancel()
	close(release)

	select {
	case entry, ok := <-s.local:
		if !ok {
			t.Fatal("the download was abandoned when the context was cancelled")
		}
		if entry.VariantID != variantID {
			t.Fatalf("entry = %+v, want variant %s", entry, variantID)
		}
		if _, err := os.Stat(filepath.Join(app.cache.Dir(), variantID+".opus")); err != nil {
			t.Errorf("the cached file is missing: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the download did not finish after the context was cancelled")
	}
}
