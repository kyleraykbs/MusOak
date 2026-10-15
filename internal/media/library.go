package media

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dhowden/tag"
	"github.com/google/uuid"

	"github.com/kyleraykbs/MusOak/internal/ffmpeg"
	"github.com/kyleraykbs/MusOak/internal/provider"
	"github.com/kyleraykbs/MusOak/internal/store"
)

// maxImportedArtworkBytes bounds one cover an imported file carries. It is the
// same cap the upload endpoint puts on an uploaded image (maxArtworkBytes in
// internal/api): a cover is a few hundred kilobytes, and a picture past this is
// not one. A picture this large is skipped, never the import.
const maxImportedArtworkBytes = 8 << 20

// ArtworkStorer files a cover an imported file carried, rather than one fetched
// from a provider, and answers the value to record on the track. That value is
// the artwork cache's source for the image, which is what the server's own
// /api/v1/artwork/track/<id> path resolves to. An image this server cannot serve
// is an error, and leaves the import without a cover rather than failing it.
type ArtworkStorer interface {
	StoreImportedArtwork(trackID uuid.UUID, contentType string, data []byte) (string, error)
}

// audioExtensions are the file types ScanDir picks up.
var audioExtensions = map[string]bool{
	".opus": true,
	".ogg":  true,
	".flac": true,
	".mp3":  true,
	".m4a":  true,
	".aac":  true,
	".wav":  true,
	".webm": true,
}

// Import adds a file from disk to the library as a local variant: it reads the
// tags, stores the audio as Ogg/Opus and records the file. Importing the same
// content twice returns the existing variant.
func (m *Manager) Import(ctx context.Context, path string) (*store.Variant, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("media: import %s: %w", path, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("media: import %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("media: import %s: not a regular file", path)
	}

	// Content identity: re-importing the same bytes is a no-op.
	sum, _, err := hashFile(abs)
	if err != nil {
		return nil, fmt.Errorf("media: hash %s: %w", path, err)
	}
	if existing, err := m.db.MediaFileBySHA256(ctx, sum); err == nil {
		return m.db.Variant(ctx, existing.VariantID)
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	// Duration comes from the stored file, not the source: one probe, and it
	// describes exactly what is served.
	meta := readTags(abs)

	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return nil, fmt.Errorf("media: create dir: %w", err)
	}

	// The variant's id is minted before its file exists so the conversion can
	// report against the variant a client will read once the import lands: an
	// import that converts for a minute used to show nothing at all.
	variantID := uuid.New()
	tmp := filepath.Join(m.dir, ".import-"+uuid.NewString()+".opus.part")
	m.setStatus(Status{
		VariantID: variantID,
		State:     StateDownloading,
		Stage:     provider.StageConvert,
		Progress:  progressUnknown,
	})
	if err := ffmpeg.ToOpusWithProgress(ctx, abs, tmp, func(done, total time.Duration) {
		ms, totalMs := done.Milliseconds(), total.Milliseconds()
		m.setStatus(Status{
			VariantID:  variantID,
			State:      StateDownloading,
			Stage:      provider.StageConvert,
			Bytes:      ms,
			TotalBytes: totalMs,
			Progress:   fraction(ms, totalMs),
		})
	}); err != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("media: import %s: %w", path, err)
	}

	stored, err := probeStored(tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}

	track := &store.Track{Title: meta.Title, DurationMs: stored.DurationMs}
	if err := m.db.CreateTrack(ctx, track); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	variant := &store.Variant{
		ID:              variantID,
		TrackID:         track.ID,
		Provider:        store.LocalProvider,
		ProviderTrackID: "sha256:" + sum,
		Title:           meta.Title,
		Artists:         meta.Artists,
		Album:           meta.Album,
		DurationMs:      stored.DurationMs,
		Downloadable:    true,
	}
	if err := m.db.CreateVariant(ctx, variant); err != nil {
		_ = os.Remove(tmp)
		if errors.Is(err, store.ErrConflict) {
			if existing, lookupErr := m.db.VariantByProviderTrack(ctx, store.LocalProvider, variant.ProviderTrackID); lookupErr == nil {
				return existing, nil
			}
		}
		return nil, err
	}

	final := m.pathFor(variant.ID)
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("media: import %s: move into place: %w", path, err)
	}

	file := &store.MediaFile{
		VariantID:  variant.ID,
		Path:       final,
		SHA256:     stored.SHA256,
		DurationMs: stored.DurationMs,
		Bytes:      stored.Bytes,
	}
	if err := m.db.UpsertMediaFile(ctx, file); err != nil {
		return nil, err
	}
	if err := m.db.SetTrackArtists(ctx, track.ID, meta.Artists); err != nil {
		return nil, err
	}
	if meta.Album != "" {
		if err := m.db.SetTrackAlbums(ctx, track.ID, []string{meta.Album}); err != nil {
			return nil, err
		}
	}
	// The file's own cover, filed and recorded exactly as an uploaded one is.
	if meta.Cover != nil {
		m.storeCover(ctx, track.ID, meta.Cover)
	}

	m.setStatus(Status{
		VariantID:  variant.ID,
		State:      StateReady,
		Progress:   1,
		DurationMs: file.DurationMs,
		Bytes:      file.Bytes,
		Path:       final,
	})
	m.evict(ctx)
	return variant, nil
}

// ScanDir imports every audio file under dir. Successful imports are returned
// alongside a joined error for the files that failed.
func (m *Manager) ScanDir(ctx context.Context, dir string) ([]*store.Variant, error) {
	var (
		imported []*store.Variant
		failures []error
	)
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !audioExtensions[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		variant, err := m.Import(ctx, path)
		if err != nil {
			failures = append(failures, err)
			return nil
		}
		imported = append(imported, variant)
		return nil
	})
	if walkErr != nil {
		failures = append(failures, walkErr)
	}
	return imported, errors.Join(failures...)
}

type storedMedia struct {
	SHA256     string
	DurationMs int64
	Bytes      int64
}

func probeStored(path string) (storedMedia, error) {
	sum, size, err := hashFile(path)
	if err != nil {
		return storedMedia{}, err
	}
	duration, err := ffmpeg.Duration(context.Background(), path)
	if err != nil {
		return storedMedia{}, err
	}
	return storedMedia{SHA256: sum, DurationMs: duration.Milliseconds(), Bytes: size}, nil
}

type fileTags struct {
	Title   string
	Artists []string
	Album   string
	// Cover is the picture the file carries, kept until the track exists to
	// record it on. Nil when the file carries none.
	Cover *tag.Picture
}

// readTags reads artist, title and album, falling back to the file name.
func readTags(path string) fileTags {
	fallback := fileTags{Title: titleFromPath(path)}

	f, err := os.Open(path)
	if err != nil {
		return fallback
	}
	defer f.Close()

	md, err := tag.ReadFrom(f)
	if err != nil {
		return fallback
	}

	out := fileTags{
		Title: strings.TrimSpace(md.Title()),
		Album: strings.TrimSpace(md.Album()),
	}
	if picture := md.Picture(); picture != nil && len(picture.Data) > 0 {
		out.Cover = picture
	}
	if artist := strings.TrimSpace(md.Artist()); artist != "" {
		out.Artists = []string{artist}
	}
	if out.Title == "" {
		out.Title = fallback.Title
	}
	return out
}

func titleFromPath(path string) string {
	base := filepath.Base(path)
	return strings.TrimSpace(strings.TrimSuffix(base, filepath.Ext(base)))
}

// storeCover files the picture an imported file carried and records where it
// landed on the track, the way the upload path files a cover somebody sent.
//
// A picture this server cannot serve is a track without a cover, not a song
// that could not be imported: every failure here is logged and dropped.
func (m *Manager) storeCover(ctx context.Context, trackID uuid.UUID, cover *tag.Picture) {
	if m.artwork == nil {
		return
	}
	if len(cover.Data) > maxImportedArtworkBytes {
		m.logger.Debug("import: cover skipped, too large",
			"track", trackID, "bytes", len(cover.Data), "limit", maxImportedArtworkBytes)
		return
	}
	source, err := m.artwork.StoreImportedArtwork(trackID, cover.MIMEType, cover.Data)
	if err != nil {
		m.logger.Debug("import: cover skipped", "track", trackID, "error", err)
		return
	}
	if source == "" {
		return
	}
	if err := m.db.SetTrackArtwork(ctx, trackID, source); err != nil {
		m.logger.Debug("import: cover not recorded", "track", trackID, "error", err)
	}
}
