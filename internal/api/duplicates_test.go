package api

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/musoak/internal/store"
)

// seedDuplicateUpload writes an upload straight through the store, bypassing
// the create endpoint's duplicate check, so a test can set up the copies that
// endpoint now refuses to make.
func seedDuplicateUpload(t *testing.T, c *testClient, userID uuid.UUID, sha, title string) store.Upload {
	t.Helper()
	ctx := context.Background()
	track := &store.Track{Title: title}
	if err := c.store.CreateTrack(ctx, track); err != nil {
		t.Fatalf("CreateTrack: %v", err)
	}
	upload := &store.Upload{UserID: userID, Filename: title + ".opus"}
	variant := &store.Variant{
		TrackID:    track.ID,
		Title:      title,
		Artists:    []string{"The Waves"},
		Album:      "Ocean Songs",
		DurationMs: 2500,
	}
	file := &store.MediaFile{
		Path:       filepath.Join(t.TempDir(), title+".opus"),
		SHA256:     sha,
		DurationMs: 2500,
		Bytes:      3,
	}
	if err := c.store.CreateUpload(ctx, upload, variant, file); err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	created, err := c.store.Upload(ctx, upload.ID)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	return *created
}

// listDuplicates reads GET /api/v1/uploads/duplicates.
func listDuplicates(t *testing.T, c *testClient, token string) []duplicateGroup {
	t.Helper()
	rec := c.do(http.MethodGet, "/api/v1/uploads/duplicates", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("duplicates = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Groups []duplicateGroup `json:"groups"`
	}
	c.decode(rec, &out)
	return out.Groups
}

func TestUploadCreateRefusesDuplicateBytes(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	kyle := c.register("kyle", "hunter2hunter2")
	ctx := context.Background()

	first := createUpload(t, c, kyle, uploadRequest("Blue Horizon", unreadableAudio, 2500))
	before, err := c.store.MediaFiles(ctx)
	if err != nil {
		t.Fatal(err)
	}

	rec := c.do(http.MethodPost, "/api/v1/uploads", kyle, uploadRequest("Blue Horizon Copy", unreadableAudio, 2500))
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate create = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	var body errorBody
	c.decode(rec, &body)
	if !strings.Contains(body.Error, "Blue Horizon") {
		t.Errorf("refusal = %q, want it to name what it duplicates", body.Error)
	}

	// Nothing reached disk and nothing was listed: the second copy does not
	// exist, so the LRU eviction cannot delete a file another variant needs.
	after, err := c.store.MediaFiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Errorf("media files = %d, want the %d already stored", len(after), len(before))
	}
	if mine := listUploads(t, c, kyle, "/api/v1/uploads"); len(mine) != 1 || mine[0].ID != first.ID {
		t.Errorf("uploads after the refusal = %+v", mine)
	}
}

func TestUploadDuplicatesListing(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	kyle := c.register("kyle", "hunter2hunter2")
	bob := c.register("bob", "hunter2hunter2")

	// One copy goes through the handler; the identical second one is seeded,
	// because the create endpoint now refuses it.
	first := createUpload(t, c, kyle, uploadRequest("Blue Horizon", unreadableAudio, 2500))
	owner := uuid.MustParse(first.Uploader.UserID)
	sha := sha256Hex(unreadableAudio)
	second := seedDuplicateUpload(t, c, owner, sha, "Blue Horizon Again")

	// Bob's own pair is a different file, and must never show in kyle's list.
	bobUpload := createUpload(t, c, bob, uploadRequest("Green Fields", uploadAudio("green fields"), 2500))
	bobOwner := uuid.MustParse(bobUpload.Uploader.UserID)
	seedDuplicateUpload(t, c, bobOwner, "bob-sum", "Green Fields Two")

	groups := listDuplicates(t, c, kyle)
	if len(groups) != 1 {
		t.Fatalf("groups = %+v, want exactly kyle's one pair", groups)
	}
	if groups[0].SHA256 != sha {
		t.Errorf("sha256 = %q, want %q", groups[0].SHA256, sha)
	}
	if len(groups[0].Uploads) != 2 {
		t.Fatalf("group uploads = %+v, want both copies", groups[0].Uploads)
	}
	got := map[string]bool{}
	for _, upload := range groups[0].Uploads {
		got[upload.ID] = true
	}
	if !got[first.ID] || !got[second.ID.String()] {
		t.Errorf("group uploads = %+v, want %s and %s", groups[0].Uploads, first.ID, second.ID)
	}

	// The listing is personal.
	if rec := c.do(http.MethodGet, "/api/v1/uploads/duplicates", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("guest duplicates = %d, want 401", rec.Code)
	}
}

func TestUploadsAssociateBulk(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	kyle := c.register("kyle", "hunter2hunter2")
	ctx := context.Background()

	target := seedWithVariant(t, c, "Target Song", "ytmusic")
	a := createUpload(t, c, kyle, uploadRequest("A", uploadAudio("a"), 2500))
	b := createUpload(t, c, kyle, uploadRequest("B", uploadAudio("b"), 2500))
	d := createUpload(t, c, kyle, uploadRequest("D", uploadAudio("d"), 2500))

	// A is already on the song; the bulk selection must release it rather than
	// silently keeping two of the caller's uploads associated.
	if rec := c.do(http.MethodPatch, "/api/v1/uploads/"+a.ID, kyle, map[string]any{"associateTrackId": target}); rec.Code != http.StatusOK {
		t.Fatalf("first association = %d: %s", rec.Code, rec.Body.String())
	}

	rec := c.do(http.MethodPost, "/api/v1/uploads/associate", kyle, map[string]any{
		"uploadIds": []string{b.ID, d.ID}, "associateTrackId": target,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("bulk associate = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Uploads  []uploadResponse `json:"uploads"`
		Released []uploadResponse `json:"released"`
	}
	c.decode(rec, &out)
	if len(out.Uploads) != 2 {
		t.Fatalf("associated = %+v, want both", out.Uploads)
	}
	for _, upload := range out.Uploads {
		if upload.AssociateTrackID != target || upload.TrackID != target {
			t.Errorf("associated upload = %+v, want the target song", upload)
		}
	}
	if len(out.Released) != 1 || out.Released[0].ID != a.ID {
		t.Fatalf("released = %+v, want %s", out.Released, a.ID)
	}
	if out.Released[0].AssociateTrackID != "" {
		t.Errorf("released upload = %+v, want No Association", out.Released[0])
	}

	keptA, err := c.store.Upload(ctx, uuid.MustParse(a.ID))
	if err != nil {
		t.Fatal(err)
	}
	if keptA.Variant.TrackID.String() == target {
		t.Errorf("A kept the association after being released: %+v", keptA.Variant)
	}

	// Bad batches are refused whole.
	if rec := c.do(http.MethodPost, "/api/v1/uploads/associate", kyle, map[string]any{"uploadIds": []string{}, "associateTrackId": target}); rec.Code != http.StatusBadRequest {
		t.Errorf("empty selection = %d, want 400", rec.Code)
	}
	many := make([]string, 101)
	for i := range many {
		many[i] = uuid.NewString()
	}
	if rec := c.do(http.MethodPost, "/api/v1/uploads/associate", kyle, map[string]any{"uploadIds": many, "associateTrackId": target}); rec.Code != http.StatusBadRequest {
		t.Errorf("101 ids = %d, want 400", rec.Code)
	}
}

func TestUploadsAssociateRejectsAnothersUpload(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	kyle := c.register("kyle", "hunter2hunter2")
	bob := c.register("bob", "hunter2hunter2")
	ctx := context.Background()

	target := seedWithVariant(t, c, "Target Song", "ytmusic")
	mine := createUpload(t, c, kyle, uploadRequest("Mine", uploadAudio("mine"), 2500))
	theirs := createUpload(t, c, bob, uploadRequest("Theirs", uploadAudio("theirs"), 2500))
	ownTrack := mine.TrackID

	rec := c.do(http.MethodPost, "/api/v1/uploads/associate", kyle, map[string]any{
		"uploadIds": []string{mine.ID, theirs.ID}, "associateTrackId": target,
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("batch with a stranger = %d, want 404: %s", rec.Code, rec.Body.String())
	}

	// Nothing was written: the caller's own upload in the refused batch did not
	// move.
	after, err := c.store.Upload(ctx, uuid.MustParse(mine.ID))
	if err != nil {
		t.Fatal(err)
	}
	if after.Variant.TrackID.String() != ownTrack {
		t.Errorf("the refused batch moved an upload: %+v", after.Variant)
	}
	if _, err := c.store.Track(ctx, uuid.MustParse(ownTrack)); err != nil {
		t.Errorf("the upload's own track was dropped: %v", err)
	}
}

func TestUploadsAssociateDissociatesWithNoTarget(t *testing.T) {
	c := newHTTPTestServer(t, nil)
	kyle := c.register("kyle", "hunter2hunter2")

	target := seedWithVariant(t, c, "Target Song", "ytmusic")
	a := createUpload(t, c, kyle, uploadRequest("A", uploadAudio("a"), 2500))
	b := createUpload(t, c, kyle, uploadRequest("B", uploadAudio("b"), 2500))

	if rec := c.do(http.MethodPost, "/api/v1/uploads/associate", kyle, map[string]any{
		"uploadIds": []string{a.ID, b.ID}, "associateTrackId": target,
	}); rec.Code != http.StatusOK {
		t.Fatalf("associate = %d: %s", rec.Code, rec.Body.String())
	}

	rec := c.do(http.MethodPost, "/api/v1/uploads/associate", kyle, map[string]any{
		"uploadIds": []string{a.ID, b.ID}, "associateTrackId": "",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("dissociate = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Uploads  []uploadResponse `json:"uploads"`
		Released []uploadResponse `json:"released"`
	}
	c.decode(rec, &out)
	if len(out.Uploads) != 2 {
		t.Fatalf("uploads = %+v, want both", out.Uploads)
	}
	tracks := map[string]bool{}
	for _, upload := range out.Uploads {
		if upload.AssociateTrackID != "" {
			t.Errorf("upload = %+v, want No Association", upload)
		}
		if upload.TrackID == target {
			t.Errorf("upload stayed on the target: %+v", upload)
		}
		tracks[upload.TrackID] = true
	}
	if len(tracks) != 2 {
		t.Errorf("tracks = %v, want one of its own per upload", tracks)
	}
	if len(out.Released) != 0 {
		t.Errorf("released = %+v, want none for a dissociation", out.Released)
	}
}
