// Package support embeds the Python helper scripts that providers shell out to.
package support

import (
	"embed"
	"fmt"
	"io/fs"
)

//go:embed ytmusic/*.py
var scripts embed.FS

// YTMusicSearchScript returns the YouTube Music search helper source. The
// provider feeds it to `python3 -` on stdin with the query as argv[1] and the
// result limit as argv[2].
func YTMusicSearchScript() (string, error) {
	b, err := fs.ReadFile(scripts, "ytmusic/search.py")
	if err != nil {
		return "", fmt.Errorf("read embedded search script: %w", err)
	}
	return string(b), nil
}
