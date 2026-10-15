package client_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kyleraykbs/MusOak/pkg/client"
)

func TestMediaURLMatchesMediaGet(t *testing.T) {
	c := client.New("http://localhost:4420/")
	if got, want := c.MediaURL("variant-1"), "http://localhost:4420/api/v1/media/variant-1"; got != want {
		t.Errorf("MediaURL = %q, want %q", got, want)
	}
}

// mediaServer answers the two calls Cache.Fetch makes: an instantly-ready
// download, and a media GET whose headers the test chooses.
func mediaServer(t *testing.T, media http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/media/v1/download", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(client.MediaStatus{VariantID: "v1", State: client.MediaReady})
	})
	mux.HandleFunc("/api/v1/media/v1", media)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// TestCacheRefusesStreamedResponse checks that a progressive stream — the
// bytes of a rendition the server is still preparing — is never cached as if
// it were the finished file.
func TestCacheRefusesStreamedResponse(t *testing.T) {
	server := mediaServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Musoak-Stream", "1")
		_, _ = w.Write([]byte("half a song"))
	})
	cache, err := client.OpenCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Fetch(context.Background(), client.New(server.URL), "v1"); err == nil {
		t.Fatal("a streamed response was cached as a finished rendition")
	} else if !strings.Contains(err.Error(), "stream") {
		t.Errorf("error = %v, want it to name the stream", err)
	}
}

// TestCacheRequiresChecksum checks that a finished file must name its own
// sha256, so a response without an ETag cannot slip past verification.
func TestCacheRequiresChecksum(t *testing.T) {
	server := mediaServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("finished but unverifiable"))
	})
	cache, err := client.OpenCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Fetch(context.Background(), client.New(server.URL), "v1"); err == nil {
		t.Fatal("a response without an ETag was accepted")
	}
}

// Two callers racing for the same variant share one download. Without the
// in-flight group they both write the same fixed temp path and the slower one
// can rename a half-written file into place, so the cache ends up holding a
// truncated rendition.
func TestConcurrentFetchesShareOneDownload(t *testing.T) {
	body := []byte("one rendition")
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`

	var (
		mu        sync.Mutex
		mediaGets int
	)
	release := make(chan struct{})
	var once sync.Once
	releaseNow := func() { once.Do(func() { close(release) }) }

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/media/v1/download":
			_ = json.NewEncoder(w).Encode(client.MediaStatus{VariantID: "v1", State: client.MediaReady})
		case "/api/v1/media/v1":
			mu.Lock()
			mediaGets++
			mu.Unlock()
			<-release
			w.Header().Set("ETag", etag)
			_, _ = w.Write(body)
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(releaseNow)

	cache, err := client.OpenCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	api := client.New(server.URL)

	start := make(chan struct{})
	var (
		wg      sync.WaitGroup
		entries [2]client.CacheEntry
		errs    [2]error
	)
	for i := range entries {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			entries[i], errs[i] = cache.Fetch(context.Background(), api, "v1")
		}(i)
	}
	close(start)
	// Let both calls land in the in-flight group before the transfer finishes.
	time.Sleep(100 * time.Millisecond)
	releaseNow()
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Fetch %d: %v", i, err)
		}
	}
	if entries[0] != entries[1] {
		t.Errorf("callers got different entries: %+v and %+v", entries[0], entries[1])
	}
	mu.Lock()
	gets := mediaGets
	mu.Unlock()
	if gets != 1 {
		t.Errorf("the variant was downloaded %d times, want one shared transfer", gets)
	}
}
