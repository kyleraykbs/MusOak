package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A wait on the server's work runs for as long as its context allows, while an
// ordinary request still gives up at the client's own timeout.
func TestALongCallOutlivesTheClientTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":"ready"}`))
	}))
	defer server.Close()

	c := New(server.URL, WithHTTPClient(&http.Client{Timeout: 50 * time.Millisecond}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := c.StartDownloadAndWait(ctx, "variant"); err != nil {
		t.Fatalf("a download wait was cut off by the client timeout: %v", err)
	}
	if _, err := c.MediaStatus(ctx, "variant"); err == nil {
		t.Error("an ordinary request outlived the client timeout")
	}
}
