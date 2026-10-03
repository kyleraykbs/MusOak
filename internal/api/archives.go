package api

import (
	"archive/zip"
	"context"
	"io"
	"mime"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/musoak/internal/store"
)

// Downloading a playlist as one zip.
//
// The archive is written straight into the response: a download is the one
// request a browser waits for however long it takes, and streaming it means a
// cold playlist starts arriving before its last song has been fetched. What a
// download of many songs needs in return is a count, and a response cannot
// report one. So the request that asks for the archive reads the playlist and
// answers with a job; the download then follows the job's count of songs
// written so far.

// archiveTTL is how long a finished archive is remembered, so a page polling
// once a second still sees how it ended.
const archiveTTL = 10 * time.Minute

// playlistArchive is one playlist being written out as a zip.
type playlistArchive struct {
	mu     sync.Mutex
	state  string // ready, running, done, failed
	userID uuid.UUID
	name   string
	items  []store.PlaylistItem
	done   int
	err    string
	ended  time.Time
}

type playlistArchiveStatus struct {
	ArchiveID string `json:"archiveId"`
	State     string `json:"state"`
	Done      int    `json:"done"`
	Total     int    `json:"total"`
	FileName  string `json:"fileName"`
	Error     string `json:"error,omitempty"`
}

func (a *playlistArchive) status(id string) playlistArchiveStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return playlistArchiveStatus{
		ArchiveID: id,
		State:     a.state,
		Done:      a.done,
		Total:     len(a.items),
		FileName:  zipFileName(a.name),
		Error:     a.err,
	}
}

// begin claims the archive for one download. Writing it is the work, so a
// second reader would have to do all of it again; it is told to ask for a new
// archive instead.
func (a *playlistArchive) begin() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state != "ready" {
		return false
	}
	a.state = "running"
	return true
}

func (a *playlistArchive) progress(written int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.done = written
}

func (a *playlistArchive) finish(written int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state = "done"
	a.done = written
	a.ended = time.Now()
}

func (a *playlistArchive) fail(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state = "failed"
	a.err = err.Error()
	a.ended = time.Now()
}

func (a *playlistArchive) belongsTo(userID uuid.UUID) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.userID == userID
}

// handlePlaylistArchiveStart reads the playlist and remembers it as a job.
//
// The songs are counted here, so the caller knows how many to expect before a
// single byte of the archive moves.
func (s *Server) handlePlaylistArchiveStart(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	playlist, ok := s.ownedPlaylist(w, r, user.ID)
	if !ok {
		return
	}
	items, err := s.store.PlaylistItems(r.Context(), playlist.ID)
	if err != nil {
		writeStoreError(w, err, "playlist unavailable")
		return
	}
	if len(items) == 0 {
		writeError(w, http.StatusBadRequest, "that playlist has no songs yet")
		return
	}

	archive := &playlistArchive{state: "ready", userID: user.ID, name: playlist.Name, items: items}
	writeJSON(w, http.StatusAccepted, archive.status(s.archives.add(archive)))
}

// handlePlaylistArchiveStatus reports how much of the zip has been written.
func (s *Server) handlePlaylistArchiveStatus(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	archive, ok := s.ownArchive(w, r, user.ID)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, archive.status(r.PathValue("archiveId")))
}

// handlePlaylistArchiveFile streams the zip, counting the songs as it writes
// them.
func (s *Server) handlePlaylistArchiveFile(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	archive, ok := s.ownArchive(w, r, user.ID)
	if !ok {
		return
	}
	if !archive.begin() {
		writeError(w, http.StatusConflict, "that archive has already been downloaded; ask for a new one")
		return
	}

	archive.mu.Lock()
	name, items := archive.name, archive.items
	archive.mu.Unlock()

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{
		"filename": zipFileName(name),
	}))
	// The headers go out before the first song is fetched, so the browser shows
	// the download starting rather than an empty wait.
	w.WriteHeader(http.StatusOK)

	written, err := s.writeArchive(r.Context(), w, archive, items)
	if err != nil {
		archive.fail(err)
		s.logger.Warn("archive stopped early", "archive", r.PathValue("archiveId"), "songs", written, "error", err)
		return
	}
	archive.finish(written)
	s.logger.Info("playlist archived", "archive", r.PathValue("archiveId"), "songs", written, "of", len(items))
}

// writeArchive writes every song in the playlist into the response as a zip
// entry, reporting how many are done as it goes.
func (s *Server) writeArchive(ctx context.Context, w io.Writer, archive *playlistArchive, items []store.PlaylistItem) (int, error) {
	out := zip.NewWriter(w)
	used := map[string]int{}
	written := 0
	for _, item := range items {
		variant, err := s.playableVariant(ctx, item.TrackID)
		if err != nil {
			continue // nothing to download for this song yet
		}
		path, err := s.media.Ensure(ctx, variant.ID)
		if err != nil {
			s.logger.Warn("archive: could not prepare a song", "variant", variant.ID, "error", err)
			continue
		}
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		entry, err := out.CreateHeader(&zip.FileHeader{
			Name:   uniqueEntryName(used, opusFileName(strings.Join(variant.Artists, ", "), variant.Title, variant.ID)),
			Method: zip.Store, // already compressed audio: storing is the honest choice
		})
		if err != nil {
			file.Close()
			return written, err
		}
		_, err = io.Copy(entry, file)
		file.Close()
		if err != nil {
			return written, err
		}
		written++
		archive.progress(written)
		if err := ctx.Err(); err != nil {
			return written, err
		}
	}
	return written, out.Close()
}

// ownArchive reads the archive named in the path, refusing one that belongs to
// somebody else as if it did not exist.
func (s *Server) ownArchive(w http.ResponseWriter, r *http.Request, userID uuid.UUID) (*playlistArchive, bool) {
	archive, ok := s.archives.get(r.PathValue("archiveId"))
	if !ok || !archive.belongsTo(userID) {
		writeError(w, http.StatusNotFound, "no such archive")
		return nil, false
	}
	return archive, true
}

// playableVariant is the rendition of a track this server can actually hand
// over: the first one it could download.
func (s *Server) playableVariant(ctx context.Context, trackID uuid.UUID) (store.Variant, error) {
	variants, err := s.store.VariantsForTrack(ctx, trackID)
	if err != nil {
		return store.Variant{}, err
	}
	for _, variant := range variants {
		if variant.Downloadable {
			return variant, nil
		}
	}
	return store.Variant{}, store.ErrNotFound
}

// uniqueEntryName keeps two songs with the same name apart inside one archive.
func uniqueEntryName(used map[string]int, name string) string {
	count := used[name]
	used[name] = count + 1
	if count == 0 {
		return name
	}
	stem := strings.TrimSuffix(name, ".opus")
	return stem + " (" + itoa(count+1) + ").opus"
}

// zipFileName names the archive after the playlist, with an extension a file
// system and a browser both accept.
func zipFileName(name string) string {
	var cleaned strings.Builder
	for _, r := range strings.TrimSpace(name) {
		if r < 0x20 || strings.ContainsRune(`\/:*?"<>|`, r) {
			cleaned.WriteRune('_')
			continue
		}
		cleaned.WriteRune(r)
	}
	out := strings.TrimSpace(cleaned.String())
	if out == "" {
		out = "playlist"
	}
	return out + ".zip"
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
