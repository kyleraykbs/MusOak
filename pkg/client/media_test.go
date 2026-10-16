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
	if got, want := c.MediaURL("variant-1", 0), "http://localhost:4420/api/v1/media/variant-1"; got != want {
		t.Errorf("MediaURL = %q, want %q", got, want)
	}
}

// TestMediaURLCarriesTheStartOffset pins the URL a listener joining a song at
// 45 s gets: the server serves the stored file from there to its end, so the
// offset is a query on the same media URL, never a bare URL the player would
// have to seek with.
func TestMediaURLCarriesTheStartOffset(t *testing.T) {
	c := client.New("http://localhost:4420")
	if got, want := c.MediaURL("variant-1", 45_000), "http://localhost:4420/api/v1/media/variant-1?start=45000"; got != want {
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

// TestCacheVerifiesTheChecksum checks that a transfer whose bytes do not match
// the sha256 the server advertised is refused, not cached.
func TestCacheVerifiesTheChecksum(t *testing.T) {
	server := mediaServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"`+strings.Repeat("0", 64)+`"`)
		_, _ = w.Write([]byte("not the bytes that were promised"))
	})
	cache, err := client.OpenCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Fetch(context.Background(), client.New(server.URL), "v1"); err == nil {
		t.Fatal("a transfer that failed its checksum was cached")
	}
}

// TestCacheStoresAVerifiedRendition checks the happy path: a finished file whose
// bytes match the advertised sha256 lands in the cache and indexes under both
// the variant and the track.
func TestCacheStoresAVerifiedRendition(t *testing.T) {
	body := []byte("the whole, finished rendition")
	sum := sha256.Sum256(body)
	server := mediaServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
		_, _ = w.Write(body)
	})
	cache, err := client.OpenCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	entry, err := cache.Fetch(context.Background(), client.New(server.URL), "v1")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if entry.Path == "" || entry.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("entry = %+v, want the verified file", entry)
	}
	if got, ok := cache.Lookup("v1"); !ok || got.Path != entry.Path {
		t.Errorf("the rendition was not indexed under its variant")
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
