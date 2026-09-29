package media

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/prismusic/internal/store"
)

// runFFmpeg and strconvFormat keep the test helpers free of a direct ffmpeg
// dependency in the test file body.
func runFFmpeg(ctx context.Context, args []string) error {
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg: %w: %s", err, stderr.String())
	}
	return nil
}

func strconvFormat(f float64) string { return strconv.FormatFloat(f, 'f', 3, 64) }

func TestServeFileRangeAndETag(t *testing.T) {
	dir := t.TempDir()
	fixture := tone(t, dir, "fixture.opus", 1.0)
	fake := &fakeProvider{name: "ytmusic", fixture: fixture}
	m, db := newTestManager(t, fake)
	variant := seedVariant(t, db, true, "ytmusic")

	if _, err := m.Ensure(context.Background(), variant.ID); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	file, err := db.MediaFile(context.Background(), variant.ID)
	if err != nil {
		t.Fatal(err)
	}

	serve := func(id uuid.UUID, mutate func(*http.Request)) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/media/"+id.String(), nil)
		if mutate != nil {
			mutate(req)
		}
		rec := httptest.NewRecorder()
		m.ServeFile(rec, req, id)
		return rec
	}

	full := serve(variant.ID, nil)
	if full.Code != http.StatusOK {
		t.Fatalf("full GET status = %d", full.Code)
	}
	if got := full.Header().Get("ETag"); got != `"`+file.SHA256+`"` {
		t.Errorf("ETag = %s, want the sha256", got)
	}
	if int64(full.Body.Len()) != file.Bytes {
		t.Errorf("body = %d bytes, want %d", full.Body.Len(), file.Bytes)
	}

	partial := serve(variant.ID, func(r *http.Request) { r.Header.Set("Range", "bytes=0-9") })
	if partial.Code != http.StatusPartialContent {
		t.Fatalf("range status = %d, want 206", partial.Code)
	}
	if partial.Body.Len() != 10 {
		t.Errorf("range body = %d bytes, want 10", partial.Body.Len())
	}

	conditional := serve(variant.ID, func(r *http.Request) {
		r.Header.Set("If-None-Match", `"`+file.SHA256+`"`)
	})
	if conditional.Code != http.StatusNotModified {
		t.Errorf("conditional status = %d, want 304", conditional.Code)
	}

	missing := serve(uuid.New(), nil)
	if missing.Code != http.StatusNotFound {
		t.Errorf("unknown variant status = %d, want 404", missing.Code)
	}
}

func TestServeFileRejectsIncompleteMedia(t *testing.T) {
	m, db := newTestManager(t, nil)
	variant := seedVariant(t, db, true, "ytmusic")

	// A row pointing at a file that is not there (deleted cache) must not be
	// served as if it were complete.
	err := db.UpsertMediaFile(context.Background(), &store.MediaFile{
		VariantID:  variant.ID,
		Path:       filepath.Join(t.TempDir(), "gone.opus"),
		SHA256:     "deadbeef",
		DurationMs: 1000,
		Bytes:      123,
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	m.ServeFile(rec, httptest.NewRequest(http.MethodGet, "/media", nil), variant.ID)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
