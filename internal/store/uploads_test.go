package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

// mustUpload stores an upload like the API does: a user-sourced variant with a
// media file on disk. createdAt is fixed so listings have a defined order.
func mustUpload(t *testing.T, db *DB, dir string, user *User, track *Track, title string, createdAt time.Time) *Upload {
	t.Helper()
	variantID := uuid.New()
	path := filepath.Join(dir, variantID.String()+".opus")
	if err := os.WriteFile(path, []byte("bytes of "+title), 0o644); err != nil {
		t.Fatal(err)
	}
	u := &Upload{UserID: user.ID, Filename: title + ".opus", CreatedAt: createdAt}
	v := &Variant{
		ID:         variantID,
		TrackID:    track.ID,
		Title:      title,
		Artists:    []string{"The " + title},
		Album:      "Album of " + title,
		DurationMs: 120_000,
	}
	m := &MediaFile{Path: path, SHA256: "sum of " + title, DurationMs: 120_000, Bytes: int64(len("bytes of " + title))}
	if err := db.CreateUpload(context.Background(), u, v, m); err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	return u
}

func TestUploadCreateWiresTheRows(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := mustUserNamed(t, db, "kyle")
	track := mustTrack(t, db, "Song")
	dir := t.TempDir()

	upload := mustUpload(t, db, dir, user, track, "Blue", time.UnixMilli(1000))

	// The variant's identity follows from the upload, whatever the caller
	// passed in.
	if upload.VariantID == uuid.Nil || upload.ID == uuid.Nil {
		t.Fatalf("CreateUpload did not fill the ids: %+v", upload)
	}
	if upload.VariantID == upload.ID {
		t.Errorf("upload id and variant id must differ: %v", upload.ID)
	}
	variant, err := db.Variant(ctx, upload.VariantID)
	if err != nil {
		t.Fatalf("Variant: %v", err)
	}
	if variant.Provider != UploadProvider || variant.ProviderTrackID != upload.ID.String() {
		t.Errorf("variant identity = %q/%q, want %q/%q",
			variant.Provider, variant.ProviderTrackID, UploadProvider, upload.ID)
	}
	if !variant.Downloadable {
		t.Error("an upload is downloadable")
	}

	// uploader_user_id is what the source picker reads back.
	var uploaderID string
	if err := db.db.QueryRowContext(ctx,
		`SELECT uploader_user_id FROM variants WHERE id = ?`, upload.VariantID.String()).
		Scan(&uploaderID); err != nil {
		t.Fatal(err)
	}
	if uploaderID != user.ID.String() {
		t.Errorf("uploader_user_id = %q, want %q", uploaderID, user.ID)
	}

	file, err := db.MediaFile(ctx, upload.VariantID)
	if err != nil {
		t.Fatalf("MediaFile: %v", err)
	}
	if file.VariantID != upload.VariantID || file.SHA256 == "" || file.Bytes == 0 {
		t.Errorf("media file = %+v", file)
	}

	// An upload for an account that does not exist cannot be stored.
	ghost := &Upload{UserID: uuid.New(), Filename: "ghost.opus"}
	if err := db.CreateUpload(ctx, ghost,
		&Variant{TrackID: track.ID, Title: "Ghost"},
		&MediaFile{Path: filepath.Join(dir, "ghost"), SHA256: "x", Bytes: 1}); !errors.Is(err, ErrConflict) {
		t.Errorf("unknown user: err = %v, want ErrConflict", err)
	}
}

func TestUploadListsAndLookup(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	dir := t.TempDir()
	kyle := mustUserNamed(t, db, "kyle")
	other := mustUserNamed(t, db, "someone")

	trackA := mustTrack(t, db, "A")
	trackB := mustTrack(t, db, "B")

	first := mustUpload(t, db, dir, kyle, trackA, "First", time.UnixMilli(1000))
	second := mustUpload(t, db, dir, kyle, trackB, "Second", time.UnixMilli(2000))
	mine := mustUpload(t, db, dir, other, trackB, "Theirs", time.UnixMilli(3000))

	got, err := db.Upload(ctx, first.ID)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if got.Filename != "First.opus" || got.UserID != kyle.ID || got.Variant.Title != "First" {
		t.Errorf("upload = %+v", got)
	}
	if got.Variant.Artists[0] != "The First" || got.Variant.Album != "Album of First" {
		t.Errorf("variant metadata = %+v", got.Variant)
	}
	if got.Uploader.UserID != kyle.ID || got.Uploader.Username != "kyle" {
		t.Errorf("uploader = %+v", got.Uploader)
	}

	if _, err := db.Upload(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing upload: err = %v", err)
	}
	byVariant, err := db.UploadByVariant(ctx, second.VariantID)
	if err != nil {
		t.Fatalf("UploadByVariant: %v", err)
	}
	if byVariant.ID != second.ID {
		t.Errorf("UploadByVariant = %v, want %v", byVariant.ID, second.ID)
	}
	if _, err := db.UploadByVariant(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown variant: err = %v", err)
	}

	// A listing is newest first and scoped to its owner; AllUploads is not.
	mineList, err := db.UploadsForUser(ctx, kyle.ID)
	if err != nil {
		t.Fatalf("UploadsForUser: %v", err)
	}
	if len(mineList) != 2 || mineList[0].ID != second.ID || mineList[1].ID != first.ID {
		t.Fatalf("UploadsForUser = %+v", mineList)
	}
	all, err := db.AllUploads(ctx)
	if err != nil {
		t.Fatalf("AllUploads: %v", err)
	}
	if len(all) != 3 || all[0].ID != mine.ID {
		t.Fatalf("AllUploads = %+v", all)
	}

	// An upload standing alone on its track is No Association; one sharing a
	// track with another rendition is associated. Solo is a scanned fact, so
	// the rows are read back.
	if got, _ := db.Upload(ctx, first.ID); !got.Solo {
		t.Error("an upload alone on its track should be solo")
	}
	shared := mustUpload(t, db, dir, other, trackB, "Second Again", time.UnixMilli(4000))
	for _, id := range []uuid.UUID{shared.ID, second.ID} {
		if got, _ := db.Upload(ctx, id); got.Solo {
			t.Error("uploads sharing a track are not solo")
		}
	}
}

func TestUploadAssociationKeepsItsOwnMetadata(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	dir := t.TempDir()
	user := mustUserNamed(t, db, "kyle")
	own := mustTrack(t, db, "Own")
	target := mustTrack(t, db, "Target")
	if err := db.SetTrackArtists(ctx, target.ID, []string{"Target Artist"}); err != nil {
		t.Fatal(err)
	}
	// The target is a song with a rendition of its own, like a search pick.
	mustVariant(t, db, target.ID, "ytmusic", "yt-target")

	upload := mustUpload(t, db, dir, user, own, "Blue", time.UnixMilli(1000))

	if err := db.SetUploadTrack(ctx, upload.ID, target.ID); err != nil {
		t.Fatalf("SetUploadTrack: %v", err)
	}
	variant, err := db.Variant(ctx, upload.VariantID)
	if err != nil {
		t.Fatal(err)
	}
	if variant.TrackID != target.ID {
		t.Errorf("track = %v, want %v", variant.TrackID, target.ID)
	}
	// The association decides which song it is a source of, nothing else.
	if variant.Title != "Blue" || variant.Artists[0] != "The Blue" || variant.Album != "Album of Blue" {
		t.Errorf("association clobbered the upload: %+v", variant)
	}
	if got, _ := db.Upload(ctx, upload.ID); got.Solo {
		t.Error("an upload that joined a track is not solo")
	}

	if err := db.SetUploadTrack(ctx, uuid.New(), target.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("re-pointing a missing upload: err = %v", err)
	}
}

func TestUploadDeleteFreesRowsAndFile(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	dir := t.TempDir()
	user := mustUserNamed(t, db, "kyle")
	track := mustTrack(t, db, "Song")
	upload := mustUpload(t, db, dir, user, track, "Blue", time.UnixMilli(1000))

	file, err := db.MediaFile(ctx, upload.VariantID)
	if err != nil {
		t.Fatal(err)
	}

	if err := db.DeleteUpload(ctx, upload.ID); err != nil {
		t.Fatalf("DeleteUpload: %v", err)
	}

	if _, err := os.Stat(file.Path); !os.IsNotExist(err) {
		t.Errorf("the bytes are still on disk: %v", err)
	}
	if _, err := db.Upload(ctx, upload.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("upload row survived: %v", err)
	}
	if _, err := db.Variant(ctx, upload.VariantID); !errors.Is(err, ErrNotFound) {
		t.Errorf("variant row survived: %v", err)
	}
	if _, err := db.MediaFile(ctx, upload.VariantID); !errors.Is(err, ErrNotFound) {
		t.Errorf("media row survived: %v", err)
	}

	if err := db.DeleteUpload(ctx, upload.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: err = %v", err)
	}
}

func TestSearchUploads(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	dir := t.TempDir()
	user := mustUserNamed(t, db, "kyle")

	blue := mustUpload(t, db, dir, user, mustTrack(t, db, "Blue Horizon"), "Blue Horizon", time.UnixMilli(1000))
	mustUpload(t, db, dir, user, mustTrack(t, db, "Green Fields"), "Green Fields", time.UnixMilli(2000))

	// The match covers the fields the upload dialog collects, and is not
	// case-sensitive.
	for _, query := range []string{"blue", "Horizon", "The Blue", "Blue Horizon.opus"} {
		found, err := db.SearchUploads(ctx, query, 10)
		if err != nil {
			t.Fatalf("SearchUploads(%q): %v", query, err)
		}
		if len(found) != 1 || found[0].ID != blue.ID {
			t.Errorf("SearchUploads(%q) = %+v", query, found)
		}
	}

	if found, _ := db.SearchUploads(ctx, "nothing here", 10); len(found) != 0 {
		t.Errorf("unmatched search returned %+v", found)
	}
	// Wildcards in the query match literally.
	if found, _ := db.SearchUploads(ctx, "%", 10); len(found) != 0 {
		t.Errorf("a literal %% matched %+v", found)
	}

	// The limit bounds the result; a caller without one still gets results.
	if found, _ := db.SearchUploads(ctx, "e", 1); len(found) != 1 {
		t.Errorf("limited search returned %d hits, want 1", len(found))
	}
	if found, _ := db.SearchUploads(ctx, "e", 0); len(found) != 2 {
		t.Errorf("unbounded search returned %d hits, want 2", len(found))
	}
}
