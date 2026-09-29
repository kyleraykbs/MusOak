package artwork

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/prismusic/internal/store"
)

// imageServer serves a tiny PNG and counts the requests.
type imageServer struct {
	server *httptest.Server
	hits   atomic.Int32
	status int
	body   []byte
	ctype  string
}

// pngBytes is a 1x1 PNG, which is enough for the cache to treat it as an image.
var pngBytes = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0a, 0x49, 0x44, 0x41,
	0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00,
	0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
	0x42, 0x60, 0x82,
}

func newImageServer(t *testing.T) *imageServer {
	t.Helper()
	image := &imageServer{status: http.StatusOK, body: pngBytes, ctype: "image/png"}
	image.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		image.hits.Add(1)
		if r.Header.Get("User-Agent") == "" {
			http.Error(w, "no user agent", http.StatusForbidden)
			return
		}
		if image.status != http.StatusOK {
			http.Error(w, "nope", image.status)
			return
		}
		w.Header().Set("Content-Type", image.ctype)
		_, _ = w.Write(image.body)
	}))
	t.Cleanup(image.server.Close)
	return image
}

type fixture struct {
	fetcher *Fetcher
	db      *store.DB
	dir     string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := store.Open("file:artwork-" + uuid.NewString() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	dir := filepath.Join(t.TempDir(), "artwork")
	return &fixture{fetcher: New(dir, db, slog.New(slog.DiscardHandler)), db: db, dir: dir}
}

func (f *fixture) trackWithArtwork(t *testing.T, url string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	track := &store.Track{Title: "Song"}
	if err := f.db.CreateTrack(ctx, track); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetTrackArtwork(ctx, track.ID, url); err != nil {
		t.Fatal(err)
	}
	return track.ID
}

func TestFetchDownloadsOnceAndCaches(t *testing.T) {
	image := newImageServer(t)
	f := newFixture(t)
	ctx := context.Background()
	trackID := f.trackWithArtwork(t, image.server.URL+"/cover.png")

	item, err := f.fetcher.Fetch(ctx, store.ArtworkTrack, trackID)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if item.ContentType != "image/png" {
		t.Errorf("content type = %q", item.ContentType)
	}
	if item.Bytes != int64(len(pngBytes)) {
		t.Errorf("bytes = %d, want %d", item.Bytes, len(pngBytes))
	}
	if item.SHA256 == "" {
		t.Error("the cached image was not hashed")
	}
	if _, err := os.Stat(item.Path); err != nil {
		t.Fatalf("the image is not on disk: %v", err)
	}

	// The second call is served from disk.
	again, err := f.fetcher.Fetch(ctx, store.ArtworkTrack, trackID)
	if err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if again.Path != item.Path {
		t.Errorf("paths differ: %s vs %s", again.Path, item.Path)
	}
	if hits := image.hits.Load(); hits != 1 {
		t.Errorf("provider requests = %d, want 1", hits)
	}
}

func TestFetchSharesOneURLBetweenEntities(t *testing.T) {
	image := newImageServer(t)
	f := newFixture(t)
	ctx := context.Background()

	source := image.server.URL + "/cover.png"
	first := f.trackWithArtwork(t, source)
	second := f.trackWithArtwork(t, source)

	one, err := f.fetcher.Fetch(ctx, store.ArtworkTrack, first)
	if err != nil {
		t.Fatal(err)
	}
	two, err := f.fetcher.Fetch(ctx, store.ArtworkTrack, second)
	if err != nil {
		t.Fatal(err)
	}
	if one.Path != two.Path {
		t.Errorf("the same source was stored twice: %s vs %s", one.Path, two.Path)
	}
	if hits := image.hits.Load(); hits != 1 {
		t.Errorf("provider requests = %d, want 1", hits)
	}
}

func TestFetchReportsMissingAndBrokenArtwork(t *testing.T) {
	image := newImageServer(t)
	f := newFixture(t)
	ctx := context.Background()

	// Unknown entity.
	if _, err := f.fetcher.Fetch(ctx, store.ArtworkTrack, uuid.New()); !errors.Is(err, ErrNoArtwork) {
		t.Errorf("unknown entity: err = %v, want ErrNoArtwork", err)
	}
	// Unknown kind.
	if _, err := f.fetcher.Fetch(ctx, store.ArtworkKind("nonsense"), uuid.New()); !errors.Is(err, ErrNoArtwork) {
		t.Errorf("unknown kind: err = %v, want ErrNoArtwork", err)
	}

	// A provider that refuses.
	image.status = http.StatusForbidden
	trackID := f.trackWithArtwork(t, image.server.URL+"/gone.png")
	if _, err := f.fetcher.Fetch(ctx, store.ArtworkTrack, trackID); !errors.Is(err, ErrFetch) {
		t.Errorf("refused image: err = %v, want ErrFetch", err)
	}

	// A source that is not a URL at all.
	broken := f.trackWithArtwork(t, "not-a-url")
	if _, err := f.fetcher.Fetch(ctx, store.ArtworkTrack, broken); !errors.Is(err, ErrFetch) {
		t.Errorf("bad URL: err = %v, want ErrFetch", err)
	}

	// Nothing was cached for the failures.
	entries, err := os.ReadDir(f.dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, entry := range entries {
		t.Errorf("a failed fetch left %s behind", entry.Name())
	}
}

func TestArtworkIsOnlySetOnce(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	track := &store.Track{Title: "Song"}
	if err := f.db.CreateTrack(ctx, track); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetTrackArtwork(ctx, track.ID, "https://first.example/cover.jpg"); err != nil {
		t.Fatal(err)
	}
	// A later provider must not overwrite an image the entity already has.
	if err := f.db.SetTrackArtwork(ctx, track.ID, "https://second.example/cover.jpg"); err != nil {
		t.Fatal(err)
	}
	got, err := f.db.TrackArtwork(ctx, track.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://first.example/cover.jpg" {
		t.Errorf("artwork = %q, want the first one", got)
	}

	// The provider order decides which alternative a caller prefers.
	album, err := f.db.EnsureAlbum(ctx, "Album", []string{"Artist"})
	if err != nil {
		t.Fatal(err)
	}
	for _, variant := range []struct{ provider, url string }{
		{"ytmusic", "https://yt.example/cover.jpg"},
		{"spotify", "https://sp.example/cover.jpg"},
	} {
		attached, _, err := f.db.AttachAlbumVariant(ctx, &store.AlbumVariant{
			AlbumID: album, Provider: variant.provider, ProviderAlbumID: variant.provider + "-1",
			Title: "Album",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.db.SetAlbumVariantArtwork(ctx, attached.ID, variant.url); err != nil {
			t.Fatal(err)
		}
	}

	sources, err := f.db.ArtworkSources(ctx, store.ArtworkAlbum, album, []string{"spotify", "ytmusic"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 2 || sources[0].Provider != "spotify" {
		t.Fatalf("sources = %+v, want spotify first", sources)
	}
}
