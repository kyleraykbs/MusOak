package api

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"os"
	"testing"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/musoak/internal/provider"
	"codeberg.org/kyleraykbs/musoak/internal/store"
)

// unreadableAudio is a payload no probe can read: a create with it pins the
// client-reported duration regardless of the tooling on the machine.
var unreadableAudio = []byte("definitely not audio")

// uploadAudio is an unreadable payload distinct to tag. The create endpoint
// refuses bytes it already holds, so uploads sharing one test need their own.
func uploadAudio(tag string) []byte {
	return []byte("definitely not audio: " + tag)
}

// uploadRequest is the create body for a song upload.
func uploadRequest(title string, data []byte, durationMs int64) map[string]any {
	return map[string]any{
		"filename":    title + ".opus",
		"contentType": "audio/ogg",
		"data":        base64.StdEncoding.EncodeToString(data),
		"title":       title,
		"artists":     []string{"The Waves"},
		"album":       "Ocean Songs",
		"durationMs":  durationMs,
	}
}

func createUpload(t *testing.T, c *testClient, token string, body map[string]any) uploadResponse {
	t.Helper()
	rec := c.do(http.MethodPost, "/api/v1/uploads", token, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Upload uploadResponse `json:"upload"`
	}
	c.decode(rec, &out)
	return out.Upload
}

func listUploads(t *testing.T, c *testClient, token, path string) []uploadResponse {
	t.Helper()
	rec := c.do(http.MethodGet, path, token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list %s = %d: %s", path, rec.Code, rec.Body.String())
	}
	var out struct {
		Uploads []uploadResponse `json:"uploads"`
	}
	c.decode(rec, &out)
	return out.Uploads
}

func TestUploadCreateAndList(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	kyle := c.register("kyle", "hunter2hunter2")
	bob := c.register("bob", "hunter2hunter2")

	upload := createUpload(t, c, kyle, uploadRequest("Blue Horizon", unreadableAudio, 2500))
	if upload.ID == "" || upload.TrackID == "" || upload.VariantID == "" {
		t.Fatalf("upload = %+v, want ids", upload)
	}
	if upload.Title != "Blue Horizon" || upload.Album != "Ocean Songs" ||
		len(upload.Artists) != 1 || upload.Artists[0] != "The Waves" {
		t.Errorf("metadata = %+v", upload)
	}
	if upload.DurationMs != 2500 {
		t.Errorf("durationMs = %d, want the reported 2500", upload.DurationMs)
	}
	if upload.Uploader.Username != "kyle" || upload.Uploader.UserID == "" {
		t.Errorf("uploader = %+v", upload.Uploader)
	}
	if upload.Official {
		t.Error("an upload is never official")
	}
	if upload.AssociateTrackID != "" {
		t.Errorf("associateTrackId = %q, want No Association", upload.AssociateTrackID)
	}

	// No Association creates a fresh canonical track credited from the upload.
	trackID := uuid.MustParse(upload.TrackID)
	if artists, err := c.store.TrackArtists(context.Background(), trackID); err != nil || len(artists) != 1 || artists[0].Name != "The Waves" {
		t.Errorf("track artists = %v, %v", artists, err)
	}
	if albums, err := c.store.TrackAlbums(context.Background(), trackID); err != nil || len(albums) != 1 || albums[0].Title != "Ocean Songs" {
		t.Errorf("track albums = %v, %v", albums, err)
	}

	// The bytes are served unchanged, exactly like a downloaded rendition.
	rec := c.do(http.MethodGet, "/api/v1/media/"+upload.VariantID, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("media = %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != string(unreadableAudio) {
		t.Errorf("media body = %q, want the uploaded bytes", rec.Body.String())
	}

	// The Manage tab lists the caller's uploads; everyone sees them all.
	if mine := listUploads(t, c, kyle, "/api/v1/uploads"); len(mine) != 1 || mine[0].ID != upload.ID {
		t.Fatalf("my uploads = %+v", mine)
	}
	theirs := createUpload(t, c, bob, uploadRequest("Green Fields", uploadAudio("green fields"), 2500))
	if mine := listUploads(t, c, bob, "/api/v1/uploads"); len(mine) != 1 || mine[0].ID != theirs.ID {
		t.Fatalf("bob's uploads = %+v", mine)
	}
	all := listUploads(t, c, bob, "/api/v1/uploads/all")
	if len(all) != 2 {
		t.Fatalf("all uploads = %+v, want both", all)
	}
	if guest := listUploads(t, c, "", "/api/v1/uploads/all"); len(guest) != 2 {
		t.Errorf("a guest sees %d uploads, want 2", len(guest))
	}

	// The caller's own listing is personal; a guest has no caller.
	if rec := c.do(http.MethodGet, "/api/v1/uploads", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("guest listing = %d, want 401", rec.Code)
	}
}

func TestUploadCreateNeedsAuthentication(t *testing.T) {
	c := newHTTPTestServer(t, nil)

	if rec := c.do(http.MethodPost, "/api/v1/uploads", "", uploadRequest("Blue Horizon", unreadableAudio, 2500)); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous create = %d, want 401", rec.Code)
	}
	id := uuid.NewString()
	for _, request := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPatch, "/api/v1/uploads/" + id, map[string]any{"associateTrackId": ""}},
		{http.MethodDelete, "/api/v1/uploads/" + id, nil},
	} {
		if rec := c.do(request.method, request.path, "", request.body); rec.Code != http.StatusUnauthorized {
			t.Errorf("anonymous %s = %d, want 401", request.method, rec.Code)
		}
	}
}

func TestUploadProbesTheStoredDuration(t *testing.T) {
	data, err := os.ReadFile(toneFile(t, t.TempDir(), "tone.opus"))
	if err != nil {
		t.Fatal(err)
	}
	c := newHTTPTestServer(t, nil)
	token := c.register("kyle", "hunter2hunter2")

	// The stored bytes decide the duration; the request's number does not.
	body := uploadRequest("Blue Horizon", data, 1)
	upload := createUpload(t, c, token, body)
	if upload.DurationMs < 900 || upload.DurationMs > 1100 {
		t.Errorf("durationMs = %d, want the probed ~1000", upload.DurationMs)
	}
}

func TestUploadAssociationPatch(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	kyle := c.register("kyle", "hunter2hunter2")
	bob := c.register("bob", "hunter2hunter2")

	upload := createUpload(t, c, kyle, uploadRequest("Blue Horizon", unreadableAudio, 2500))
	ownTrack := upload.TrackID
	variantID := uuid.MustParse(upload.VariantID)

	// The target is a song with a rendition of its own, like a search pick.
	target := seedWithVariant(t, c, "Target Song", "ytmusic")

	rec := c.do(http.MethodPatch, "/api/v1/uploads/"+upload.ID, kyle, map[string]any{"associateTrackId": target})
	if rec.Code != http.StatusOK {
		t.Fatalf("associate = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Upload uploadResponse `json:"upload"`
	}
	c.decode(rec, &out)
	if out.Upload.TrackID != target || out.Upload.AssociateTrackID != target {
		t.Fatalf("association = %+v", out.Upload)
	}
	// The association decides which song it is a source of; the upload keeps
	// its own metadata.
	if out.Upload.Title != "Blue Horizon" || out.Upload.Album != "Ocean Songs" ||
		len(out.Upload.Artists) != 1 || out.Upload.Artists[0] != "The Waves" {
		t.Errorf("association clobbered the upload: %+v", out.Upload)
	}
	variant, err := c.store.Variant(context.Background(), variantID)
	if err != nil {
		t.Fatal(err)
	}
	if variant.TrackID.String() != target || variant.Title != "Blue Horizon" {
		t.Errorf("variant = %+v", variant)
	}
	// The track the upload stood alone on is gone with its last rendition.
	if _, err := c.store.Track(context.Background(), uuid.MustParse(ownTrack)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the emptied track survived: %v", err)
	}

	// Re-association to No Association builds a fresh track of its own again.
	rec = c.do(http.MethodPatch, "/api/v1/uploads/"+upload.ID, kyle, map[string]any{"associateTrackId": ""})
	if rec.Code != http.StatusOK {
		t.Fatalf("No Association = %d: %s", rec.Code, rec.Body.String())
	}
	c.decode(rec, &out)
	if out.Upload.AssociateTrackID != "" {
		t.Fatalf("associateTrackId = %q, want No Association", out.Upload.AssociateTrackID)
	}
	if out.Upload.TrackID == target || out.Upload.TrackID == ownTrack {
		t.Errorf("track = %v, want a fresh one", out.Upload.TrackID)
	}
	if out.Upload.Title != "Blue Horizon" || out.Upload.Album != "Ocean Songs" {
		t.Errorf("re-association clobbered the upload: %+v", out.Upload)
	}
	if variant, err := c.store.Variant(context.Background(), variantID); err != nil ||
		variant.TrackID.String() != out.Upload.TrackID || variant.Artists[0] != "The Waves" {
		t.Errorf("variant = %+v, %v", variant, err)
	}

	// Bad input, other people's uploads and unknown tracks are refused.
	if rec := c.do(http.MethodPatch, "/api/v1/uploads/"+upload.ID, kyle, map[string]any{}); rec.Code != http.StatusBadRequest {
		t.Errorf("missing associateTrackId = %d, want 400", rec.Code)
	}
	if rec := c.do(http.MethodPatch, "/api/v1/uploads/"+upload.ID, kyle, map[string]any{"associateTrackId": "not-a-uuid"}); rec.Code != http.StatusBadRequest {
		t.Errorf("bad track id = %d, want 400", rec.Code)
	}
	if rec := c.do(http.MethodPatch, "/api/v1/uploads/"+upload.ID, kyle, map[string]any{"associateTrackId": uuid.NewString()}); rec.Code != http.StatusNotFound {
		t.Errorf("unknown track = %d, want 404", rec.Code)
	}
	if rec := c.do(http.MethodPatch, "/api/v1/uploads/"+upload.ID, bob, map[string]any{"associateTrackId": target}); rec.Code != http.StatusNotFound {
		t.Errorf("somebody else's upload = %d, want 404", rec.Code)
	}
}

// TestOneAssociationPerUserPerSong is the rule: a second upload of a song takes
// the association, and the one that had it stands on its own again. The lookup
// endpoint reports which upload would be replaced, so a dialog can say so first.
func TestOneAssociationPerUserPerSong(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	kyle := c.register("kyle", "hunter2hunter2")
	bob := c.register("bob", "hunter2hunter2")
	ctx := context.Background()

	target := seedWithVariant(t, c, "Target Song", "ytmusic")

	lookup := func(token string) *uploadResponse {
		t.Helper()
		rec := c.do(http.MethodGet, "/api/v1/uploads/association?trackId="+target, token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("lookup = %d: %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Upload *uploadResponse `json:"upload"`
		}
		c.decode(rec, &out)
		return out.Upload
	}

	// Nothing associated yet.
	if found := lookup(kyle); found != nil {
		t.Fatalf("lookup = %+v, want nothing before the first association", found)
	}

	first := createUpload(t, c, kyle, uploadRequest("Blue Horizon", unreadableAudio, 2500))
	if rec := c.do(http.MethodPatch, "/api/v1/uploads/"+first.ID, kyle, map[string]any{"associateTrackId": target}); rec.Code != http.StatusOK {
		t.Fatalf("first association = %d: %s", rec.Code, rec.Body.String())
	}
	if found := lookup(kyle); found == nil || found.ID != first.ID {
		t.Fatalf("lookup = %+v, want the first upload", found)
	}

	// A second upload of the same song takes the association.
	second := createUpload(t, c, kyle, uploadRequest("Blue Horizon Again", uploadAudio("blue horizon again"), 2600))
	if rec := c.do(http.MethodPatch, "/api/v1/uploads/"+second.ID, kyle, map[string]any{"associateTrackId": target}); rec.Code != http.StatusOK {
		t.Fatalf("second association = %d: %s", rec.Code, rec.Body.String())
	}

	released, err := c.store.Upload(ctx, uuid.MustParse(first.ID))
	if err != nil {
		t.Fatal(err)
	}
	if released.Variant.TrackID.String() == target {
		t.Fatalf("the first upload kept the association: %+v", released.Variant)
	}
	// Released, not mangled: it stands on its own with its own metadata.
	if released.Variant.Title != "Blue Horizon" || len(released.Variant.Artists) != 1 ||
		released.Variant.Artists[0] != "The Waves" {
		t.Errorf("the released upload lost its metadata: %+v", released.Variant)
	}
	if found := lookup(kyle); found == nil || found.ID != second.ID {
		t.Fatalf("lookup = %+v, want the second upload", found)
	}

	// Another user's association on the same song is theirs to keep.
	other := createUpload(t, c, bob, uploadRequest("Bob's Take", uploadAudio("bob's take"), 2500))
	if rec := c.do(http.MethodPatch, "/api/v1/uploads/"+other.ID, bob, map[string]any{"associateTrackId": target}); rec.Code != http.StatusOK {
		t.Fatalf("bob's association = %d: %s", rec.Code, rec.Body.String())
	}
	kept, err := c.store.Upload(ctx, uuid.MustParse(second.ID))
	if err != nil {
		t.Fatal(err)
	}
	if kept.Variant.TrackID.String() != target {
		t.Errorf("bob's association released kyle's: %+v", kept.Variant)
	}

	// An upload created straight onto the song replaces one the same way.
	third := uploadRequest("Blue Horizon Third", uploadAudio("blue horizon third"), 2700)
	third["associateTrackId"] = target
	created := createUpload(t, c, kyle, third)
	if created.TrackID != target {
		t.Fatalf("create with association = %+v", created)
	}
	if found := lookup(kyle); found == nil || found.ID != created.ID {
		t.Fatalf("lookup = %+v, want the upload just created", found)
	}
	afterCreate, err := c.store.Upload(ctx, uuid.MustParse(second.ID))
	if err != nil {
		t.Fatal(err)
	}
	if afterCreate.Variant.TrackID.String() == target {
		t.Errorf("creating an upload on the song left the earlier association: %+v", afterCreate.Variant)
	}

	// The lookup is the caller's own: nobody else's uploads, and no guests.
	if rec := c.do(http.MethodGet, "/api/v1/uploads/association?trackId="+target, "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous lookup = %d, want 401", rec.Code)
	}
	if rec := c.do(http.MethodGet, "/api/v1/uploads/association?trackId=nope", kyle, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad trackId = %d, want 400", rec.Code)
	}
}

func TestUploadDeleteFreesTheRows(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	kyle := c.register("kyle", "hunter2hunter2")
	bob := c.register("bob", "hunter2hunter2")

	upload := createUpload(t, c, kyle, uploadRequest("Blue Horizon", unreadableAudio, 2500))
	variantID := uuid.MustParse(upload.VariantID)
	trackID := uuid.MustParse(upload.TrackID)
	file, err := c.store.MediaFile(context.Background(), variantID)
	if err != nil {
		t.Fatal(err)
	}

	if rec := c.do(http.MethodDelete, "/api/v1/uploads/"+upload.ID, kyle, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", rec.Code, rec.Body.String())
	}

	ctx := context.Background()
	if _, err := c.store.Upload(ctx, uuid.MustParse(upload.ID)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("upload row survived: %v", err)
	}
	if _, err := c.store.Variant(ctx, variantID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("variant row survived: %v", err)
	}
	if _, err := c.store.MediaFile(ctx, variantID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("media row survived: %v", err)
	}
	if _, err := os.Stat(file.Path); !os.IsNotExist(err) {
		t.Errorf("the bytes are still on disk: %v", err)
	}
	if _, err := c.store.Track(ctx, trackID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the emptied track survived: %v", err)
	}
	if mine := listUploads(t, c, kyle, "/api/v1/uploads"); len(mine) != 0 {
		t.Errorf("uploads after delete = %+v", mine)
	}

	if rec := c.do(http.MethodDelete, "/api/v1/uploads/"+upload.ID, kyle, nil); rec.Code != http.StatusNotFound {
		t.Errorf("second delete = %d, want 404", rec.Code)
	}

	// A remove action only ever removes the caller's own uploads.
	theirs := createUpload(t, c, bob, uploadRequest("Green Fields", uploadAudio("green fields"), 2500))
	if rec := c.do(http.MethodDelete, "/api/v1/uploads/"+theirs.ID, kyle, nil); rec.Code != http.StatusNotFound {
		t.Errorf("deleting somebody else's upload = %d, want 404", rec.Code)
	}
}

// uploadStubProvider answers every search with one hit of its own.
type uploadStubProvider struct {
	name  string
	title string
}

func (p *uploadStubProvider) Name() string { return p.name }
func (p *uploadStubProvider) Capabilities() provider.Caps {
	return provider.Caps{Search: true, Download: true}
}
func (p *uploadStubProvider) Download(ctx context.Context, id, dest string) error { return nil }
func (p *uploadStubProvider) Search(ctx context.Context, q string, opts provider.SearchOpts) ([]provider.Track, error) {
	return []provider.Track{{
		ProviderTrackID: p.name + "-1",
		Title:           p.title,
		Artists:         []string{"Some Artist"},
		DurationMs:      200_000,
	}}, nil
}

func TestSearchPinsUserUploadsFirst(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	c.providers.Register(&uploadStubProvider{name: "stub", title: "Provider Song"})
	token := c.register("kyle", "hunter2hunter2")
	createUpload(t, c, token, uploadRequest("Blue Horizon", unreadableAudio, 2500))

	rec := c.do(http.MethodGet, "/api/v1/search?q=blue", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("search = %d: %s", rec.Code, rec.Body.String())
	}
	var search searchResponse
	c.decode(rec, &search)
	if len(search.Groups) != 2 {
		t.Fatalf("groups = %+v, want the upload above the provider hit", search.Groups)
	}
	if !search.Groups[0].UserUpload || search.Groups[0].Track.Title != "Blue Horizon" {
		t.Errorf("first group = %+v, want the user upload", search.Groups[0])
	}
	if len(search.Groups[0].Variants) != 1 || search.Groups[0].Variants[0].Provider != store.UploadProvider {
		t.Errorf("first group variants = %+v", search.Groups[0].Variants)
	}
	if search.Groups[1].UserUpload || search.Groups[1].Track.Title != "Provider Song" {
		t.Errorf("second group = %+v, want the provider hit", search.Groups[1])
	}

	// A query matching no upload carries no user-upload group at all. A fresh
	// document decodes into a fresh value: omitted flags are not zeros.
	rec = c.do(http.MethodGet, "/api/v1/search?q=zebra", "", nil)
	var noUploads searchResponse
	c.decode(rec, &noUploads)
	for _, group := range noUploads.Groups {
		if group.UserUpload {
			t.Errorf("zebra matched an upload: %+v", group)
		}
	}
}

func TestUserUploadsListing(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	kyle := c.register("kyle", "hunter2hunter2")
	bob := c.register("bob", "hunter2hunter2")

	kyleUpload := createUpload(t, c, kyle, uploadRequest("Blue Horizon", unreadableAudio, 2500))
	bobUpload := createUpload(t, c, bob, uploadRequest("Green Fields", uploadAudio("green fields"), 2500))

	// The owner sees their own.
	mine := listUploads(t, c, kyle, "/api/v1/users/"+kyleUpload.Uploader.UserID+"/uploads")
	if len(mine) != 1 || mine[0].ID != kyleUpload.ID {
		t.Fatalf("kyle's uploads = %+v", mine)
	}

	// Someone else sees the same list: uploaded songs are visible to every user.
	theirs := listUploads(t, c, kyle, "/api/v1/users/"+bobUpload.Uploader.UserID+"/uploads")
	if len(theirs) != 1 || theirs[0].ID != bobUpload.ID {
		t.Fatalf("bob's uploads as kyle = %+v", theirs)
	}

	// A guest sees them too: the endpoint needs no account.
	guest := listUploads(t, c, "", "/api/v1/users/"+bobUpload.Uploader.UserID+"/uploads")
	if len(guest) != 1 || guest[0].ID != bobUpload.ID {
		t.Fatalf("bob's uploads as guest = %+v", guest)
	}

	// An id that names nobody is a 404.
	rec := c.do(http.MethodGet, "/api/v1/users/"+uuid.NewString()+"/uploads", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown user = %d, want 404", rec.Code)
	}
}
