package artwork

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kyleraykbs/MusOak/internal/ffmpeg"
)

// ThumbnailWidths is the set of widths a client may ask for, smallest first.
//
// A fixed set rather than any number a query string carries: every width is a
// file on disk, and a page that asked for 63 and 65 would have two of them for
// one row. The list covers what the app draws - a 32px song row at twice the
// screen density, an avatar, an album card - and nothing wider than the art a
// provider supplies.
var ThumbnailWidths = []int{64, 112, 160, 256, 336, 512}

// ThumbnailWidth answers the width a client asked for, or 0 for none.
//
// A width that is not one of ThumbnailWidths is taken to the nearest one that
// is at least as wide, so a page asking for 100 gets 112 rather than a refusal:
// a cover is not worth failing over a rounding.
func ThumbnailWidth(requested int) int {
	if requested <= 0 {
		return 0
	}
	for _, width := range ThumbnailWidths {
		if requested <= width {
			return width
		}
	}
	return ThumbnailWidths[len(ThumbnailWidths)-1]
}

// Thumbnail returns a downscaled copy of an item's image, made once and cached
// beside it.
//
// The derivative is a JPEG named for the source's own name and the width, so a
// cover that changes under a stable entity id cannot serve a stale thumbnail -
// the source is content-addressed already. A failure is returned rather than
// cached: the caller serves the original instead, because a cover that cannot
// be shrunk is still a cover.
func (f *Fetcher) Thumbnail(ctx context.Context, item *Item, width int) (*Item, error) {
	width = ThumbnailWidth(width)
	if item == nil || item.Path == "" || width == 0 {
		return nil, errors.New("artwork: no image to shrink")
	}
	path := ThumbnailPath(item.Path, width)
	// Already made: stat is the whole of the cost, and hashFile is what the
	// ETag needs anyway.
	if info, err := os.Stat(path); err == nil && info.Size() > 0 {
		sum, size, err := hashFile(path)
		if err != nil {
			return nil, err
		}
		return &Item{Path: path, ContentType: "image/jpeg", SHA256: sum, Bytes: size, Source: item.Source}, nil
	}

	// One resize per (source, width) however many rows ask at once: a playlist
	// of thirty songs asks for the same cover thirty times.
	value, err, _ := f.flight.Do("thumb:"+path, func() (any, error) {
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			return path, nil // another caller just made it
		}
		tmp := path + ".part"
		_ = os.Remove(tmp)
		if err := ffmpeg.ScaleImage(ctx, item.Path, tmp, width); err != nil {
			_ = os.Remove(tmp)
			return nil, err
		}
		if err := os.Rename(tmp, path); err != nil {
			_ = os.Remove(tmp)
			return nil, err
		}
		return path, nil
	})
	if err != nil {
		return nil, err
	}
	sum, size, err := hashFile(value.(string))
	if err != nil {
		return nil, err
	}
	return &Item{Path: value.(string), ContentType: "image/jpeg", SHA256: sum, Bytes: size, Source: item.Source}, nil
}

// ThumbnailPath is where a width's copy of a stored image lives: the source's
// path with the width in its name, beside it.
func ThumbnailPath(source string, width int) string {
	base := strings.TrimSuffix(source, filepath.Ext(source))
	return fmt.Sprintf("%s@%d.jpg", base, width)
}
