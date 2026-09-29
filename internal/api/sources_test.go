package api

import (
	"context"
	"encoding/base64"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/prismusic/internal/store"
)

// seedSourceVariant creates one variant of a track straight in the store.
func seedSourceVariant(t *testing.T, c *testClient, trackID uuid.UUID, provider, providerTrackID string) *store.Variant {
	t.Helper()
	variant := &store.Variant{
		TrackID:         trackID,
		Provider:        provider,
		ProviderTrackID: providerTrackID,
		Title:           provider + " " + providerTrackID,
		Artists:         []string{"Artist"},
		Album:           "Album",
		DurationMs:      180_000,
		Downloadable:    true,
	}
	if err := c.store.CreateVariant(context.Background(), variant); err != nil {
		t.Fatalf("CreateVariant: %v", err)
	}
	return variant
}

// fetchSources reads the playbar picker document and checks its invariants:
// a non-empty document marks exactly one source default, and that source is
// the saved preference whenever the preference is among the sources.
func fetchSources(t *testing.T, c *testClient, token string, trackID uuid.UUID) trackSourcesResponse {
	t.Helper()
	rec := c.do(http.MethodGet, "/api/v1/tracks/"+trackID.String()+"/sources", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("sources = %d: %s", rec.Code, rec.Body.String())
	}
	var doc trackSourcesResponse
	c.decode(rec, &doc)

	defaults := 0
	defaultID := ""
	preferredIsDefault := true
	for _, source := range doc.Sources {
		if source.Default {
			defaults++
			defaultID = source.VariantID
		}
		if source.VariantID == doc.PreferredVariantID && !source.Default {
			preferredIsDefault = false
		}
	}
	if len(doc.Sources) > 0 && defaults != 1 {
		t.Fatalf("sources mark %d defaults, want exactly 1: %+v", defaults, doc.Sources)
	}
	if doc.PreferredVariantID != "" && !preferredIsDefault {
		t.Fatalf("default = %q, want the preferred variant %q", defaultID, doc.PreferredVariantID)
	}
	return doc
}

// sourceFor returns one source row by variant id.
func sourceFor(t *testing.T, doc trackSourcesResponse, variantID string) sourceResponse {
	t.Helper()
	for _, source := range doc.Sources {
		if source.VariantID == variantID {
			return source
		}
	}
	t.Fatalf("no source %q among %v", variantID, sourceVariantIDs(doc))
	return sourceResponse{}
}

func sourceVariantIDs(doc trackSourcesResponse) []string {
	ids := make([]string, 0, len(doc.Sources))
	for _, source := range doc.Sources {
		ids = append(ids, source.VariantID)
	}
	return ids
}

// sourceVote casts one vote as the token's user.
func sourceVote(t *testing.T, c *testClient, token string, variantID uuid.UUID, value int) {
	t.Helper()
	rec := c.do(http.MethodPost, "/api/v1/variants/"+variantID.String()+"/vote", token, map[string]int{"value": value})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("vote %d = %d: %s", value, rec.Code, rec.Body.String())
	}
}

func TestTrackSourcesOrderingByVotes(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	token := c.register("kyle", "hunter2hunter2")
	track := seedTracks(t, c, "Song")[0]

	ytmusic := seedSourceVariant(t, c, track.ID, "ytmusic", "o1")
	spotify := seedSourceVariant(t, c, track.ID, "spotify", "o2")
	first := seedSourceVariant(t, c, track.ID, "user", "u1")
	second := seedSourceVariant(t, c, track.ID, "user", "u2")

	// Tied in votes, the caller's provider ranking decides between the
	// official sources.
	rec := c.do(http.MethodPut, "/api/v1/me/providers/ranking", token, map[string][]string{
		"ranking": {"spotify", "ytmusic"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("ranking = %d: %s", rec.Code, rec.Body.String())
	}

	doc := fetchSources(t, c, token, track.ID)
	if doc.PreferredVariantID != "" {
		t.Fatalf("preferredVariantId = %q, want none", doc.PreferredVariantID)
	}
	ids := sourceVariantIDs(doc)
	if ids[0] != spotify.ID.String() || ids[1] != ytmusic.ID.String() {
		t.Fatalf("official order = %v, want spotify then ytmusic", ids[:2])
	}
	userSources := map[string]bool{ids[2]: true, ids[3]: true}
	if !userSources[first.ID.String()] || !userSources[second.ID.String()] {
		t.Fatalf("user sources = %v, want %v and %v last", ids[2:], first.ID, second.ID)
	}
	if !sourceFor(t, doc, ids[0]).Default {
		t.Fatalf("default = %v, want the first source", ids)
	}

	// A voted-up user source outranks the other user source but never climbs
	// above an official one.
	sourceVote(t, c, token, second.ID, 1)
	doc = fetchSources(t, c, token, track.ID)
	ids = sourceVariantIDs(doc)
	want := []string{spotify.ID.String(), ytmusic.ID.String(), second.ID.String(), first.ID.String()}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("order = %v, want %v", ids, want)
		}
	}
	voted := sourceFor(t, doc, second.ID.String())
	if voted.MyVote != 1 || voted.Upvotes != 1 || voted.Downvotes != 0 {
		t.Fatalf("voted source = %+v, want myVote 1 and 1 up vote", voted)
	}
	if voted.Official || !sourceFor(t, doc, spotify.ID.String()).Official {
		t.Fatalf("official flags wrong: %+v", voted)
	}

	// The votes reshape the ordering for everyone, but the vote itself stays
	// the caller's.
	guest := fetchSources(t, c, "", track.ID)
	if got := sourceVariantIDs(guest); got[2] != second.ID.String() {
		t.Fatalf("guest order = %v, want the same sources", got)
	}
	if sourceFor(t, guest, second.ID.String()).MyVote != 0 {
		t.Fatalf("guest myVote = %+v, want 0", guest.Sources)
	}
}

func TestVariantVoteChangeAndWithdraw(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	tokenA := c.register("kyle", "hunter2hunter2")
	tokenB := c.register("someone", "hunter2hunter2")
	track := seedTracks(t, c, "Song")[0]
	variant := seedSourceVariant(t, c, track.ID, "user", "u1")
	id := variant.ID.String()

	sourceVote(t, c, tokenA, variant.ID, 1)
	row := sourceFor(t, fetchSources(t, c, tokenA, track.ID), id)
	if row.Upvotes != 1 || row.Downvotes != 0 || row.MyVote != 1 {
		t.Fatalf("after vote = %+v, want 1 up and myVote 1", row)
	}

	// Each user has one vote of their own.
	sourceVote(t, c, tokenB, variant.ID, 1)
	row = sourceFor(t, fetchSources(t, c, tokenA, track.ID), id)
	if row.Upvotes != 2 {
		t.Fatalf("after second voter = %+v, want 2 up votes", row)
	}

	// Voting again changes that one vote instead of adding another.
	sourceVote(t, c, tokenA, variant.ID, -1)
	row = sourceFor(t, fetchSources(t, c, tokenA, track.ID), id)
	if row.Upvotes != 1 || row.Downvotes != 1 || row.MyVote != -1 {
		t.Fatalf("after change = %+v, want 1 up, 1 down and myVote -1", row)
	}

	// Withdrawing removes the caller's vote and leaves everyone else's.
	sourceVote(t, c, tokenA, variant.ID, 0)
	row = sourceFor(t, fetchSources(t, c, tokenA, track.ID), id)
	if row.Upvotes != 1 || row.Downvotes != 0 || row.MyVote != 0 {
		t.Fatalf("after withdraw = %+v, want 1 up left and myVote 0", row)
	}
	if row = sourceFor(t, fetchSources(t, c, tokenB, track.ID), id); row.MyVote != 1 {
		t.Fatalf("other voter = %+v, want myVote 1", row)
	}
	// Withdrawing twice is fine.
	sourceVote(t, c, tokenA, variant.ID, 0)

	// Only 1, -1 and 0 are votes.
	for _, body := range []map[string]int{{"value": 2}, {"value": -2}} {
		rec := c.do(http.MethodPost, "/api/v1/variants/"+id+"/vote", tokenA, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("vote %+v = %d, want 400", body, rec.Code)
		}
	}
	rec := c.do(http.MethodPost, "/api/v1/variants/"+id+"/vote", tokenA, map[string]int{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("vote without value = %d, want 400", rec.Code)
	}
	rec = c.do(http.MethodPost, "/api/v1/variants/"+uuid.NewString()+"/vote", tokenA, map[string]int{"value": 1})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("vote on missing variant = %d, want 404", rec.Code)
	}
}

func TestTrackPreferenceWinsForCallerOnly(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	token := c.register("kyle", "hunter2hunter2")
	other := c.register("someone", "hunter2hunter2")
	track := seedTracks(t, c, "Song")[0]

	ytmusic := seedSourceVariant(t, c, track.ID, "ytmusic", "o1")
	spotify := seedSourceVariant(t, c, track.ID, "spotify", "o2")
	upload := seedSourceVariant(t, c, track.ID, "user", "u1")

	// The shared default is the first source in the ordering; the caller's
	// preference wins over it and is reported back.
	rec := c.do(http.MethodPut, "/api/v1/me/tracks/"+track.ID.String()+"/preference", token,
		map[string]string{"variantId": upload.ID.String()})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preference = %d: %s", rec.Code, rec.Body.String())
	}
	doc := fetchSources(t, c, token, track.ID)
	if doc.PreferredVariantID != upload.ID.String() {
		t.Fatalf("preferredVariantId = %q, want %q", doc.PreferredVariantID, upload.ID)
	}
	if !sourceFor(t, doc, upload.ID.String()).Default {
		t.Fatalf("default = %v, want the preferred upload", sourceVariantIDs(doc))
	}

	// Saving another source replaces the choice.
	rec = c.do(http.MethodPut, "/api/v1/me/tracks/"+track.ID.String()+"/preference", token,
		map[string]string{"variantId": spotify.ID.String()})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preference = %d: %s", rec.Code, rec.Body.String())
	}
	doc = fetchSources(t, c, token, track.ID)
	if doc.PreferredVariantID != spotify.ID.String() || !sourceFor(t, doc, spotify.ID.String()).Default {
		t.Fatalf("preferred = %q, sources = %+v", doc.PreferredVariantID, doc.Sources)
	}

	// Everyone else keeps the shared default: the first source.
	for name, tok := range map[string]string{"other user": other, "guest": ""} {
		doc := fetchSources(t, c, tok, track.ID)
		if doc.PreferredVariantID != "" {
			t.Errorf("%s preferredVariantId = %q, want none", name, doc.PreferredVariantID)
		}
		if !sourceFor(t, doc, ytmusic.ID.String()).Default {
			t.Errorf("%s default = %v, want the first source %v", name, sourceVariantIDs(doc), ytmusic.ID)
		}
	}

	// The preference names a source of this very track.
	otherTrack := seedTracks(t, c, "Other")[0]
	otherSource := seedSourceVariant(t, c, otherTrack.ID, "ytmusic", "o3")
	rec = c.do(http.MethodPut, "/api/v1/me/tracks/"+track.ID.String()+"/preference", token,
		map[string]string{"variantId": otherSource.ID.String()})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("preference for another track's source = %d, want 400", rec.Code)
	}
	rec = c.do(http.MethodPut, "/api/v1/me/tracks/"+track.ID.String()+"/preference", token,
		map[string]string{"variantId": uuid.NewString()})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("preference for missing source = %d, want 404", rec.Code)
	}
	rec = c.do(http.MethodPut, "/api/v1/me/tracks/"+track.ID.String()+"/preference", token,
		map[string]string{"variantId": "not-a-uuid"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("preference with bad id = %d, want 400", rec.Code)
	}
}

func TestGuestsCannotWriteSources(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	track := seedTracks(t, c, "Song")[0]
	variant := seedSourceVariant(t, c, track.ID, "ytmusic", "o1")

	rec := c.do(http.MethodPost, "/api/v1/variants/"+variant.ID.String()+"/vote", "", map[string]int{"value": 1})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("guest vote = %d, want 401", rec.Code)
	}
	rec = c.do(http.MethodPut, "/api/v1/me/tracks/"+track.ID.String()+"/preference", "",
		map[string]string{"variantId": variant.ID.String()})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("guest preference = %d, want 401", rec.Code)
	}

	// Reading the picker stays open to guests.
	doc := fetchSources(t, c, "", track.ID)
	if len(doc.Sources) != 1 || doc.PreferredVariantID != "" {
		t.Fatalf("guest sources = %+v", doc)
	}
}

func TestTrackSourcesFillInUploader(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	token := c.register("kyle", "hunter2hunter2")
	rec := c.do(http.MethodPatch, "/api/v1/me", token, map[string]string{"displayName": "Kyle Ray"})
	if rec.Code != http.StatusOK {
		t.Fatalf("display name = %d: %s", rec.Code, rec.Body.String())
	}
	track := seedTracks(t, c, "Song")[0]
	official := seedSourceVariant(t, c, track.ID, "ytmusic", "o1")

	// The upload flow is what puts an uploader on a variant; the sources
	// listing reports it back with the account behind it.
	rec = c.do(http.MethodPost, "/api/v1/uploads", token, map[string]any{
		"filename":           "song.opus",
		"contentType":        "audio/ogg",
		"data":               base64.StdEncoding.EncodeToString([]byte("not actually audio")),
		"title":              "Mine",
		"artists":            []string{"Me"},
		"album":              "Demo",
		"durationMs":         1000,
		"artworkData":        "",
		"artworkContentType": "",
		"associateTrackId":   track.ID.String(),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload = %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Upload struct {
			VariantID string `json:"variantId"`
			Uploader  struct {
				UserID      string `json:"userId"`
				Username    string `json:"username"`
				DisplayName string `json:"displayName"`
			} `json:"uploader"`
		} `json:"upload"`
	}
	c.decode(rec, &created)
	if created.Upload.VariantID == "" {
		t.Fatalf("upload returned no variant: %s", rec.Body.String())
	}

	doc := fetchSources(t, c, token, track.ID)
	row := sourceFor(t, doc, created.Upload.VariantID)
	if row.Official {
		t.Fatalf("upload %s reported official: %+v", created.Upload.VariantID, row)
	}
	if row.Title != "Mine" || len(row.Artists) != 1 || row.Artists[0] != "Me" {
		t.Fatalf("upload metadata = %+v, want the upload's own", row)
	}
	if row.Uploader == nil {
		t.Fatalf("uploader = nil, want kyle's account: %+v", row)
	}
	if row.Uploader.Username != "kyle" || row.Uploader.DisplayName != "Kyle Ray" {
		t.Fatalf("uploader = %+v, want kyle / Kyle Ray", row.Uploader)
	}
	if sourceFor(t, doc, official.ID.String()).Uploader != nil {
		t.Fatalf("provider source reported an uploader: %+v", doc.Sources)
	}
}
