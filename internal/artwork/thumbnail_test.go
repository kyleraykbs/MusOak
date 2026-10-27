package artwork

import (
	"context"
	"image"
	_ "image/jpeg"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/kyleraykbs/MusOak/internal/ffmpeg"
)

// makeImage writes a square PNG of the given size, so a resize has something
// real to shrink. ffmpeg is what the fetcher uses for the resize itself, so it
// is the honest way to make the source too.
func makeImage(t *testing.T, dir string, size int, colour string) string {
	t.Helper()
	if len(ffmpeg.Missing()) > 0 {
		t.Skipf("ffmpeg unavailable: %v", ffmpeg.Missing())
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "cover.png")
	out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "color=c="+colour+":s="+strconv.Itoa(size)+"x"+strconv.Itoa(size)+":d=1",
		"-frames:v", "1", path).CombinedOutput()
	if err != nil {
		t.Fatalf("render cover: %v: %s", err, out)
	}
	return path
}

func imageSize(t *testing.T, path string) (int, int) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer file.Close()
	config, _, err := image.DecodeConfig(file)
	if err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return config.Width, config.Height
}

func TestThumbnailShrinksOnceAndCaches(t *testing.T) {
	dir := t.TempDir()
	f := &fixture{fetcher: New(dir, nil, slog.New(slog.DiscardHandler)), dir: dir}
	source := makeImage(t, dir, 400, "orange")
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	item := &Item{Path: source, ContentType: "image/png", Bytes: info.Size()}

	shot, err := f.fetcher.Thumbnail(context.Background(), item, 64)
	if err != nil {
		t.Fatalf("Thumbnail: %v", err)
	}
	if shot.Path != ThumbnailPath(source, 64) {
		t.Errorf("thumbnail at %q, want %q", shot.Path, ThumbnailPath(source, 64))
	}
	if shot.ContentType != "image/jpeg" {
		t.Errorf("content type = %q, want image/jpeg", shot.ContentType)
	}
	if shot.SHA256 == "" || shot.Bytes == 0 {
		t.Errorf("thumbnail item = %+v, want its own hash and size", shot)
	}
	if shot.Bytes >= item.Bytes {
		t.Errorf("thumbnail is %d bytes against the source's %d: it did not shrink", shot.Bytes, item.Bytes)
	}
	if width, height := imageSize(t, shot.Path); width != 64 || height != 64 {
		t.Errorf("thumbnail is %dx%d, want 64x64", width, height)
	}

	// The copy is kept: asking again answers with the file that is there, and
	// does not need the resize to work at all.
	ffmpeg.SetBinaries(filepath.Join(dir, "no-such-ffmpeg"), filepath.Join(dir, "no-such-ffprobe"))
	t.Cleanup(func() { ffmpeg.SetBinaries("ffmpeg", "ffprobe") })
	again, err := f.fetcher.Thumbnail(context.Background(), item, 64)
	if err != nil {
		t.Fatalf("a cached thumbnail needed the resize run again: %v", err)
	}
	if again.Path != shot.Path || again.SHA256 != shot.SHA256 {
		t.Errorf("cached thumbnail = %+v, want the one already made", again)
	}
}

func TestThumbnailNeverUpscalesASmallerCover(t *testing.T) {
	dir := t.TempDir()
	f := &fixture{fetcher: New(dir, nil, slog.New(slog.DiscardHandler)), dir: dir}
	source := makeImage(t, dir, 40, "teal")
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	item := &Item{Path: source, ContentType: "image/png", Bytes: info.Size()}

	shot, err := f.fetcher.Thumbnail(context.Background(), item, 256)
	if err != nil {
		t.Fatalf("Thumbnail: %v", err)
	}
	if width, _ := imageSize(t, shot.Path); width > 40 {
		t.Errorf("a 40px cover came back %dpx wide: the resize upscaled it", width)
	}
}

func TestThumbnailWidthSnapsToWhatTheAppDraws(t *testing.T) {
	cases := map[int]int{
		0:    0,
		-5:   0,
		1:    64,
		64:   64,
		100:  112,
		112:  112,
		113:  160,
		168:  256,
		300:  336,
		400:  512,
		5000: 512,
	}
	for asked, want := range cases {
		if got := ThumbnailWidth(asked); got != want {
			t.Errorf("ThumbnailWidth(%d) = %d, want %d", asked, got, want)
		}
	}
}

func TestThumbnailWithoutAnImageFails(t *testing.T) {
	dir := t.TempDir()
	f := &fixture{fetcher: New(dir, nil, slog.New(slog.DiscardHandler)), dir: dir}
	if _, err := f.fetcher.Thumbnail(context.Background(), nil, 64); err == nil {
		t.Error("no image should be an error, so the caller serves the original")
	}
	if _, err := f.fetcher.Thumbnail(context.Background(), &Item{Path: makeImage(t, dir, 40, "red")}, 0); err == nil {
		t.Error("no width should be an error, so the caller serves the original")
	}
}
