package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
