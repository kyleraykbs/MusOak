package store

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

// seedVariantForFile makes the rows a media file hangs off.
func seedVariantForFile(t *testing.T, db *DB) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	track := &Track{Title: "Song"}
	if err := db.CreateTrack(ctx, track); err != nil {
		t.Fatal(err)
	}
	variant := &Variant{TrackID: track.ID, Provider: "ytmusic", ProviderTrackID: uuid.NewString(), Title: "Song", Downloadable: true}
	if err := db.CreateVariant(ctx, variant); err != nil {
		t.Fatal(err)
	}
	return variant.ID
}

// A media path is stored relative to the storage directory and read back as a
// path on this disk. Absolute paths in the database are what made moving the
// library leave every row pointing at where it used to be.
func TestMediaPathsAreRelativeToTheStorageDirectory(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "musoak.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	variantID := seedVariantForFile(t, db)
	onDisk := filepath.Join(dir, "media", variantID.String()+".opus")
	if err := db.UpsertMediaFile(ctx, &MediaFile{VariantID: variantID, Path: onDisk, SHA256: "sum"}); err != nil {
		t.Fatalf("UpsertMediaFile: %v", err)
	}

	// What the row holds: relative, and free of this machine's paths.
	var stored string
	if err := db.db.QueryRowContext(ctx,
		`SELECT path FROM media_files WHERE variant_id = ?`, variantID.String()).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("media", variantID.String()+".opus"); stored != want {
		t.Errorf("stored path = %q, want %q", stored, want)
	}

	// What a reader gets: the path to use here.
	file, err := db.MediaFile(ctx, variantID)
	if err != nil {
		t.Fatalf("MediaFile: %v", err)
	}
	if file.Path != onDisk {
		t.Errorf("read path = %q, want %q", file.Path, onDisk)
	}
}

// The point of the whole arrangement: move the storage directory - database and
// media together - and the library still resolves, with no rows to rewrite.
func TestMovingTheStorageDirectoryMovesTheLibrary(t *testing.T) {
	first := t.TempDir()
	firstDB := filepath.Join(first, "musoak.db")
	db, err := Open(firstDB)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	ctx := context.Background()
	variantID := seedVariantForFile(t, db)
	name := variantID.String() + ".opus"
	if err := os.MkdirAll(filepath.Join(first, "media"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first, "media", name), []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertMediaFile(ctx, &MediaFile{VariantID: variantID, Path: filepath.Join(first, "media", name), SHA256: "sum"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// The move: the database and the media go to a new directory together.
	second := t.TempDir()
	if err := copyFile(firstDB, filepath.Join(second, "musoak.db")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(second, "media"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(filepath.Join(first, "media", name), filepath.Join(second, "media", name)); err != nil {
		t.Fatal(err)
	}

	moved, err := Open(filepath.Join(second, "musoak.db"))
	if err != nil {
		t.Fatalf("Open after the move: %v", err)
	}
	t.Cleanup(func() { _ = moved.Close() })

	file, err := moved.MediaFile(ctx, variantID)
	if err != nil {
		t.Fatalf("MediaFile after the move: %v", err)
	}
	if want := filepath.Join(second, "media", name); file.Path != want {
		t.Errorf("path after the move = %q, want %q", file.Path, want)
	}
	if _, err := os.Stat(file.Path); err != nil {
		t.Errorf("the file the library points at is not there: %v", err)
	}
}

// A database written before paths were relative carries absolute ones. Opening
// it rewrites them, so a library that has already moved is found again.
func TestMigrationMakesExistingPathsRelative(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "musoak.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	variantID := seedVariantForFile(t, db)

	// The old form: an absolute path from wherever the library used to live.
	elsewhere := filepath.Join("/home/somebody/.local/share/prismusic/media", variantID.String()+".opus")
	if err := db.UpsertMediaFile(ctx, &MediaFile{VariantID: variantID, Path: elsewhere, SHA256: "sum"}); err != nil {
		t.Fatal(err)
	}
	// Put it back the way it was, so the migration has something to rewrite.
	if _, err := db.db.ExecContext(ctx,
		`UPDATE media_files SET path = ? WHERE variant_id = ?`, elsewhere, variantID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = 20`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(filepath.Join(dir, "musoak.db"))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	var stored string
	if err := reopened.db.QueryRowContext(ctx,
		`SELECT path FROM media_files WHERE variant_id = ?`, variantID.String()).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("media", variantID.String()+".opus"); stored != want {
		t.Errorf("after the migration the row holds %q, want %q", stored, want)
	}
	// And it now resolves here, which is the point: the file was moved into
	// this storage directory, whatever path the row used to name.
	file, err := reopened.MediaFile(ctx, variantID)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "media", variantID.String()+".opus"); file.Path != want {
		t.Errorf("resolved path = %q, want %q", file.Path, want)
	}
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
