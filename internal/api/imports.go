package api

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/musoak/internal/store"
)

// Importing a playlist.
//
// One track is a provider search, which takes longer than an HTTP client will
// wait: the request starts the work and answers with a job id, and the client
// watches the job for how far it has got. That count is the whole story of an
// import, which is why it is worth a job of its own.

const (
	// importTimeout bounds a job so a provider that hangs does not hold one for
	// ever. A hundred tracks is a couple of minutes; this is generous.
	importTimeout = 30 * time.Minute
	// importTTL is how long a finished import is remembered, so a client
	// polling every second still sees how it ended.
	importTTL = 10 * time.Minute
)

// playlistImport is one import while it runs.
type playlistImport struct {
	mu       sync.Mutex
	state    string // running, done, failed
	done     int
	total    int
	playable int
	playlist *store.Playlist
	err      string
	ended    time.Time
}

type playlistImportStatus struct {
	JobID    string            `json:"jobId"`
	State    string            `json:"state"`
	Done     int               `json:"done"`
	Total    int               `json:"total"`
	Playable int               `json:"playable"`
	Playlist *playlistResponse `json:"playlist,omitempty"`
	Error    string            `json:"error,omitempty"`
}

// handlePlaylistImportStart starts an import and answers with its id.
func (s *Server) handlePlaylistImportStart(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	var req playlistImportRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	providerName := strings.TrimSpace(req.Provider)
	if providerName == "" {
		writeError(w, http.StatusBadRequest, "provider is required")
		return
	}
	providerPlaylistID := providerPlaylistIDFrom(providerName, req.ID, req.URL)
	if providerPlaylistID == "" {
		writeError(w, http.StatusBadRequest, "a playlist id or link is required")
		return
	}

	job := &playlistImport{state: "running"}
	id := s.imports.add(job)
	go s.runPlaylistImport(job, user.ID, providerName, providerPlaylistID, req.Name)
	writeJSON(w, http.StatusAccepted, playlistImportStatus{JobID: id, State: job.state})
}

// runPlaylistImport does the work the request could not wait for, reporting how
// far it has got as it goes.
func (s *Server) runPlaylistImport(job *playlistImport, userID uuid.UUID, providerName, providerPlaylistID, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), importTimeout)
	defer cancel()

	result, err := s.library.ImportPlaylist(ctx, userID, providerName, providerPlaylistID, name, func(done, total int) {
		job.mu.Lock()
		job.done, job.total = done, total
		job.mu.Unlock()
	})

	job.mu.Lock()
	defer job.mu.Unlock()
	job.ended = time.Now()
	if err != nil {
		job.state = "failed"
		job.err = err.Error()
		s.logger.Warn("playlist import failed", "provider", providerName, "playlist", providerPlaylistID, "error", err)
		return
	}
	job.state = "done"
	job.playlist = result.Playlist
	job.playable = result.Playable
	job.done, job.total = result.Added, result.Added
	s.logger.Info("playlist imported", "playlist", result.Playlist.ID, "tracks", result.Added, "playable", result.Playable)
	// The playlist is new, so its songs are what to fetch next.
	s.nudgePrefetch()
}

// handlePlaylistImportStatus reports how far an import has got.
func (s *Server) handlePlaylistImportStatus(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	jobID := r.PathValue("jobId")
	job, ok := s.imports.get(jobID)
	if !ok {
		writeError(w, http.StatusNotFound, "no such import")
		return
	}

	job.mu.Lock()
	defer job.mu.Unlock()
	out := playlistImportStatus{
		JobID:    jobID,
		State:    job.state,
		Done:     job.done,
		Total:    job.total,
		Playable: job.playable,
		Error:    job.err,
	}
	if job.playlist != nil {
		summary := playlistSummary(job.playlist, user.ID)
		summary.TrackCount = job.done
		out.Playlist = &summary
	}
	writeJSON(w, http.StatusOK, out)
}
