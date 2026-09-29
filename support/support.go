// Package support embeds the Python helper scripts that providers shell out to.
package support

import (
	"embed"
	"fmt"
	"io/fs"
)

//go:embed ytmusic/*.py
var scripts embed.FS

// YTMusicScript returns the YouTube Music helper source. The provider feeds it
// to `python3 -` on stdin and passes the command and its arguments as argv.
//
//	search songs|albums|artists QUERY [LIMIT]
//	album BROWSE_ID
//	radio VIDEO_ID [LIMIT]
func YTMusicScript() (string, error) {
	b, err := fs.ReadFile(scripts, "ytmusic/ytmusic.py")
	if err != nil {
		return "", fmt.Errorf("read embedded ytmusic helper: %w", err)
	}
	return string(b), nil
}
