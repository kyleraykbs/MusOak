package api

import (
	"context"
	"encoding/base64"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/musoak/internal/provider"
	"codeberg.org/kyleraykbs/musoak/internal/store"
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

// uploadSource stores a real upload of trackID for the token's user and
// returns its variant id. It goes through the upload flow because that is what
// records the uploader the reserved slots resolve against.
func uploadSourceVariant(t *testing.T, c *testClient, token string, trackID uuid.UUID, title string) uuid.UUID {
	t.Helper()
	rec := c.do(http.MethodPost, "/api/v1/uploads", token, map[string]any{
		"filename":         title + ".opus",
		"contentType":      "audio/ogg",
		"data":             base64.StdEncoding.EncodeToString([]byte("not actually audio: " + title)),
		"title":            title,
		"artists":          []string{"Uploader"},
		"album":            "Demo",
		"durationMs":       1000,
		"associateTrackId": trackID.String(),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload %q = %d: %s", title, rec.Code, rec.Body.String())
	}
	var created struct {
		Upload struct {
			VariantID string `json:"variantId"`
		} `json:"upload"`
	}
	c.decode(rec, &created)
	id, err := uuid.Parse(created.Upload.VariantID)
	if err != nil {
		t.Fatalf("upload %q variantId = %q: %v", title, created.Upload.VariantID, err)
	}
	return id
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

// capsStubProvider is a registry entry with a fixed capability set.
type capsStubProvider struct {
	name string
	caps provider.Caps
}

func (p *capsStubProvider) Name() string                { return p.name }
func (p *capsStubProvider) Capabilities() provider.Caps { return p.caps }
func (p *capsStubProvider) Search(context.Context, string, provider.SearchOpts) ([]provider.Track, error) {
	return nil, nil
}
func (p *capsStubProvider) Download(context.Context, string, string) error { return nil }

func TestAttachTrackSource(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	c.providers.Register(&capsStubProvider{name: "ytmusic", caps: provider.Caps{Search: true, Download: true}})
	c.providers.Register(&capsStubProvider{name: "spotify", caps: provider.Caps{Search: true}})
	token := c.register("kyle", "hunter2hunter2")
	track := seedTracks(t, c, "Blue Horizon")[0]
	path := "/api/v1/tracks/" + track.ID.String() + "/sources"

	pick := map[string]any{
		"provider":        "ytmusic",
		"providerTrackId": "abc123",
		"title":           "Blue Horizon",
		"artists":         []string{"The Waves"},
		"album":           "Ocean Songs",
		"durationMs":      213000,
		"artworkUrl":      "https://example.test/cover.jpg",
	}

	rec := c.do(http.MethodPost, path, token, pick)
	if rec.Code != http.StatusOK {
		t.Fatalf("attach = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Variants []variantResponse `json:"variants"`
	}
	c.decode(rec, &out)
	if len(out.Variants) != 1 {
		t.Fatalf("variants = %+v, want the pick", out.Variants)
	}
	picked := out.Variants[0]
	if picked.Provider != "ytmusic" || picked.ProviderTrackID != "abc123" || !picked.Downloadable {
		t.Fatalf("pick = %+v, want a downloadable ytmusic variant", picked)
	}

	// It shows up among the track's variants over HTTP...
	rec = c.do(http.MethodGet, "/api/v1/tracks/"+track.ID.String()+"/variants", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("variants = %d: %s", rec.Code, rec.Body.String())
	}
	c.decode(rec, &out)
	if len(out.Variants) != 1 || out.Variants[0].ID != picked.ID {
		t.Fatalf("listed variants = %+v", out.Variants)
	}

	// ...and in the store.
	stored, err := c.store.VariantsForTrack(context.Background(), track.ID)
	if err != nil || len(stored) != 1 || stored[0].ProviderTrackID != "abc123" {
		t.Fatalf("stored variants = %+v, %v", stored, err)
	}

	// The pick fills metadata the track never had.
	if artists, err := c.store.TrackArtists(context.Background(), track.ID); err != nil || len(artists) != 1 || artists[0].Name != "The Waves" {
		t.Fatalf("track artists = %+v, %v", artists, err)
	}
	if albums, err := c.store.TrackAlbums(context.Background(), track.ID); err != nil || len(albums) != 1 || albums[0].Title != "Ocean Songs" {
		t.Fatalf("track albums = %+v, %v", albums, err)
	}

	// Posting the same pick twice is idempotent: the existing variant returns.
	rec = c.do(http.MethodPost, path, token, pick)
	if rec.Code != http.StatusOK {
		t.Fatalf("second attach = %d: %s", rec.Code, rec.Body.String())
	}
	c.decode(rec, &out)
	if len(out.Variants) != 1 || out.Variants[0].ID != picked.ID {
		t.Fatalf("second attach variants = %+v, want the same one", out.Variants)
	}
	if stored, _ = c.store.VariantsForTrack(context.Background(), track.ID); len(stored) != 1 {
		t.Fatalf("store has %d variants after a repeat, want 1", len(stored))
	}

	// The provider's real capability is honoured: spotify cannot be downloaded.
	rec = c.do(http.MethodPost, path, token, map[string]any{
		"provider": "spotify", "providerTrackId": "sp-1", "title": "Blue Horizon",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("spotify attach = %d: %s", rec.Code, rec.Body.String())
	}
	c.decode(rec, &out)
	for _, v := range out.Variants {
		if v.Provider == "spotify" && v.Downloadable {
			t.Errorf("spotify variant reported downloadable: %+v", v)
		}
	}

	// An unregistered provider is a 400, an incomplete body too.
	rec = c.do(http.MethodPost, path, token, map[string]any{
		"provider": "nope", "providerTrackId": "x", "title": "Blue Horizon",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown provider = %d, want 400", rec.Code)
	}
	rec = c.do(http.MethodPost, path, token, map[string]any{"provider": "ytmusic"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("incomplete body = %d, want 400", rec.Code)
	}

	// A guest has no account, an unknown track does not exist.
	rec = c.do(http.MethodPost, path, "", pick)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("guest attach = %d, want 401", rec.Code)
	}
	rec = c.do(http.MethodPost, "/api/v1/tracks/"+uuid.NewString()+"/sources", token, pick)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown track = %d, want 404", rec.Code)
	}
}

// variantsByID reads a track's variants and keys them by variant id.
func variantsByID(t *testing.T, c *testClient, token string, trackID uuid.UUID) map[string]variantResponse {
	t.Helper()
	rec := c.do(http.MethodGet, "/api/v1/tracks/"+trackID.String()+"/variants", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("variants = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Variants []variantResponse `json:"variants"`
	}
	c.decode(rec, &out)
	byID := make(map[string]variantResponse, len(out.Variants))
	for _, variant := range out.Variants {
		byID[variant.ID] = variant
	}
	return byID
}

// TestSourceSlotsSelfAndGuest: the self slot fills with the caller's own
// upload, and a guest has none, so the same upload falls to the uploaded slot.
func TestSourceSlotsSelfAndGuest(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	kyle := c.register("kyle", "hunter2hunter2")
	track := seedTracks(t, c, "Song")[0]
	ytmusic := seedSourceVariant(t, c, track.ID, "ytmusic", "o1")
	mine := uploadSourceVariant(t, c, kyle, track.ID, "Mine")

	doc := fetchSources(t, c, kyle, track.ID)
	ids := sourceVariantIDs(doc)
	if len(ids) != 2 || ids[0] != mine.String() || ids[1] != ytmusic.ID.String() {
		t.Fatalf("sources = %v, want the caller's upload then the provider", ids)
	}
	if got := sourceFor(t, doc, mine.String()).Slot; got != "self" {
		t.Fatalf("self slot = %q, want self", got)
	}
	if got := sourceFor(t, doc, ytmusic.ID.String()).Slot; got != "" {
		t.Fatalf("provider slot = %q, want empty", got)
	}
	if !sourceFor(t, doc, mine.String()).Default {
		t.Fatalf("default = %v, want the caller's own upload", ids)
	}

	// Variants carry the same slot, which is what a client ranks by.
	byID := variantsByID(t, c, kyle, track.ID)
	if byID[mine.String()].Slot != "self" || byID[ytmusic.ID.String()].Slot != "" {
		t.Fatalf("variant slots = %+v", byID)
	}
	if byID[mine.String()].Provider != "user" {
		t.Fatalf("upload provider = %q, want user", byID[mine.String()].Provider)
	}

	// A guest never gets self: the same upload is somebody else's, and with no
	// other upload it fills the household's uploaded slot.
	guest := fetchSources(t, c, "", track.ID)
	if ids = sourceVariantIDs(guest); ids[0] != mine.String() {
		t.Fatalf("guest sources = %v, want the upload first", ids)
	}
	if got := sourceFor(t, guest, mine.String()).Slot; got != "uploaded" {
		t.Fatalf("guest slot = %q, want uploaded", got)
	}
	guestVariants := variantsByID(t, c, "", track.ID)
	if guestVariants[mine.String()].Slot != "uploaded" {
		t.Fatalf("guest variant slot = %+v", guestVariants)
	}
}

// TestSourceSlotUploadedPicksBestOtherUpload: the uploaded slot is the
// best-liked upload by somebody else, and it is skipped when nobody else has
// uploaded the track.
func TestSourceSlotUploadedPicksBestOtherUpload(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	kyle := c.register("kyle", "hunter2hunter2")
	early := c.register("early", "hunter2hunter2")
	late := c.register("late", "hunter2hunter2")
	track := seedTracks(t, c, "Song")[0]
	ytmusic := seedSourceVariant(t, c, track.ID, "ytmusic", "o1")
	// Two uploaders, because one person has one association per song.
	a := uploadSourceVariant(t, c, early, track.ID, "A")
	time.Sleep(2 * time.Millisecond)
	b := uploadSourceVariant(t, c, late, track.ID, "B")

	// Tied votes: the store's own order (oldest first) fills the slot with the
	// earlier upload; the rest fall in behind the providers.
	doc := fetchSources(t, c, kyle, track.ID)
	ids := sourceVariantIDs(doc)
	if len(ids) != 3 || ids[0] != a.String() || ids[1] != ytmusic.ID.String() || ids[2] != b.String() {
		t.Fatalf("sources = %v, want the earliest upload, the provider, then the other", ids)
	}
	if sourceFor(t, doc, a.String()).Slot != "uploaded" || sourceFor(t, doc, b.String()).Slot != "uploaded" {
		t.Fatalf("upload slots = %+v", doc.Sources)
	}

	// A vote promotes the best-liked upload into the slot.
	sourceVote(t, c, kyle, b, 1)
	doc = fetchSources(t, c, kyle, track.ID)
	if ids = sourceVariantIDs(doc); ids[0] != b.String() {
		t.Fatalf("sources = %v, want the voted-up upload first", ids)
	}

	// With no other upload the slot is skipped, exactly like a provider with
	// no rendition.
	lonely := seedTracks(t, c, "Lonely")[0]
	official := seedSourceVariant(t, c, lonely.ID, "ytmusic", "o2")
	doc = fetchSources(t, c, kyle, lonely.ID)
	if ids = sourceVariantIDs(doc); len(ids) != 1 || ids[0] != official.ID.String() {
		t.Fatalf("sources = %v, want only the provider", ids)
	}
	if sourceFor(t, doc, official.ID.String()).Slot != "" {
		t.Fatalf("provider reported a slot: %+v", doc.Sources)
	}
}
