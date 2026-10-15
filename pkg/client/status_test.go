package client_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kyleraykbs/MusOak/pkg/client"
)

// statusFixture is what the status endpoint writes while a download runs: the
// stage, the fraction of that stage, and the stage's own numbers, here the
// bytes fetched of the bytes to fetch.
const statusFixture = `{
	"variantId": "v1",
	"state": "downloading",
	"stage": "download",
	"progress": 0.42,
	"bytes": 3984588,
	"totalBytes": 8808038,
	"durationMs": 214000
}`

// unknownTotalFixture is the same endpoint with nothing countable: the
// provider never said how large the source is, so progress is -1 and there is
// no totalBytes at all. A client must read the stage's count instead.
const unknownTotalFixture = `{
	"variantId": "v1",
	"state": "downloading",
	"stage": "convert",
	"progress": -1,
	"bytes": 45000
}`

// statusServer answers the media endpoints with one of the fixtures above.
func statusServer(t *testing.T, body string) *client.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return client.New(server.URL)
}

// TestMediaStatusDecodesTheStageAndItsNumbers pins the status wire shape: the
// stage and the stage's own numbers come through beside the state and the
// fraction the client already knew about.
func TestMediaStatusDecodesTheStageAndItsNumbers(t *testing.T) {
	ctx := context.Background()

	status, err := statusServer(t, statusFixture).MediaStatus(ctx, "v1")
	if err != nil {
		t.Fatalf("MediaStatus: %v", err)
	}

	if status.VariantID != "v1" || status.State != client.MediaDownloading {
		t.Errorf("status = %+v, want v1 downloading", status)
	}
	if status.Stage != client.MediaStageDownload {
		t.Errorf("Stage = %q, want %q", status.Stage, client.MediaStageDownload)
	}
	if status.Progress != 0.42 {
		t.Errorf("Progress = %v, want 0.42", status.Progress)
	}
	if status.Bytes != 3_984_588 || status.TotalBytes != 8_808_038 {
		t.Errorf("bytes = %d/%d, want 3984588/8808038", status.Bytes, status.TotalBytes)
	}
	if status.DurationMs != 214_000 {
		t.Errorf("DurationMs = %d, want 214000", status.DurationMs)
	}
}

// TestMediaStatusDecodesAnUnknownTotal checks the "nothing countable" answer:
// -1 is not a fraction, and a missing total is 0 rather than a made-up size.
func TestMediaStatusDecodesAnUnknownTotal(t *testing.T) {
	ctx := context.Background()

	status, err := statusServer(t, unknownTotalFixture).MediaStatus(ctx, "v1")
	if err != nil {
		t.Fatalf("MediaStatus: %v", err)
	}

	if status.Stage != client.MediaStageConvert {
		t.Errorf("Stage = %q, want %q", status.Stage, client.MediaStageConvert)
	}
	if status.Progress != -1 {
		t.Errorf("Progress = %v, want -1", status.Progress)
	}
	if status.Bytes != 45_000 || status.TotalBytes != 0 {
		t.Errorf("bytes = %d/%d, want 45000/0", status.Bytes, status.TotalBytes)
	}
}

// TestDownloadPathsCarryTheStage checks that starting a download and waiting
// for it answer with the same status shape as the poll, so a caller that skips
// the poll sees the stage too.
func TestDownloadPathsCarryTheStage(t *testing.T) {
	ctx := context.Background()
	api := statusServer(t, unknownTotalFixture)

	started, err := api.StartDownload(ctx, "v1")
	if err != nil {
		t.Fatalf("StartDownload: %v", err)
	}
	if started.Stage != client.MediaStageConvert || started.Bytes != 45_000 || started.Progress != -1 {
		t.Errorf("StartDownload status = %+v, want the convert stage with 45000 bytes", started)
	}

	waited, err := api.StartDownloadAndWait(ctx, "v1")
	if err != nil {
		t.Fatalf("StartDownloadAndWait: %v", err)
	}
	if waited.Stage != client.MediaStageConvert || waited.Bytes != 45_000 || waited.Progress != -1 {
		t.Errorf("StartDownloadAndWait status = %+v, want the convert stage with 45000 bytes", waited)
	}
}
