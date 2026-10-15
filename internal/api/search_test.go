package api

import (
	"context"
	"testing"

	"github.com/kyleraykbs/MusOak/internal/media"
	"github.com/kyleraykbs/MusOak/internal/store"
)

// A search answer is held for the whole window, and what it says about a
// rendition's file is live: a song that finished downloading while the question
// was being answered must be reported as ready on the next read, not as
// downloading for the rest of the window. The held answer itself is left as it
// was, because every reader of the window shares it.
func TestFreshSearchMediaRereadsTheDownloadState(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	ctx := context.Background()

	track := seedTracks(t, c, "Downloaded")[0]
	variant := &store.Variant{
		TrackID:         track.ID,
		Provider:        "local",
		ProviderTrackID: "local-1",
		Title:           "Downloaded",
		DurationMs:      180_000,
		Downloadable:    true,
	}
	if err := c.store.CreateVariant(ctx, variant); err != nil {
		t.Fatalf("CreateVariant: %v", err)
	}

	// What the search said while the file was still coming down.
	held := searchResponse{
		Query: "downloaded",
		Groups: []searchGroup{{
			Variants: []variantResponse{{
				ID:      variant.ID.String(),
				TrackID: track.ID.String(),
				Media: mediaStatusResponse{
					VariantID: variant.ID.String(),
					State:     string(media.StateDownloading),
				},
			}},
		}},
	}

	// With no file on disk the state is the manager's own, not the held one.
	before, ok := c.Server.freshSearchMedia(ctx, held).(searchResponse)
	if !ok {
		t.Fatal("freshSearchMedia did not answer with a search response")
	}
	if got := before.Groups[0].Variants[0].Media.State; got == string(media.StateDownloading) {
		t.Errorf("first read state = %q, want the state as it is now", got)
	}

	// The download landed.
	if err := c.store.UpsertMediaFile(ctx, &store.MediaFile{
		VariantID:  variant.ID,
		Path:       "/media/downloaded.opus",
		SHA256:     "sum of the file",
		DurationMs: 180_000,
		Bytes:      4_096,
	}); err != nil {
		t.Fatalf("UpsertMediaFile: %v", err)
	}

	after, ok := c.Server.freshSearchMedia(ctx, held).(searchResponse)
	if !ok {
		t.Fatal("freshSearchMedia did not answer with a search response")
	}
	got := after.Groups[0].Variants[0].Media
	if got.State != string(media.StateReady) || got.Bytes != 4_096 || got.DurationMs != 180_000 {
		t.Errorf("media = %+v, want the downloaded file reported ready", got)
	}
	if after.Groups[0].Variants[0].ID != variant.ID.String() {
		t.Errorf("restamping lost the rendition: %+v", after.Groups[0].Variants[0])
	}

	// The held answer is not the one that was written into.
	if got := held.Groups[0].Variants[0].Media.State; got != string(media.StateDownloading) {
		t.Errorf("the held answer was rewritten: state = %q", got)
	}
	if &held.Groups[0].Variants[0] == &after.Groups[0].Variants[0] {
		t.Error("the restamped answer shares the held answer's storage")
	}
}
