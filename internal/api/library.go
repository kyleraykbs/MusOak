package api

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/kyleraykbs/MusOak/internal/store"
)

type importRequest struct {
	// Path imports a single file.
	Path string `json:"path"`
	// Dir imports every audio file under a directory.
	Dir string `json:"dir"`
}

type importResponse struct {
	Imported []importedTrack `json:"imported"`
	Failures []string        `json:"failures,omitempty"`
}

type importedTrack struct {
	Track   trackResponse   `json:"track"`
	Variant variantResponse `json:"variant"`
}

// importAllowed reports whether path lies inside one of the roots. Both are
// resolved first, so a link inside a root cannot name a place outside it, and
// a path that does not exist is nobody's to import.
func importAllowed(roots []string, path string) bool {
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	if target, err = filepath.Abs(target); err != nil {
		return false
	}
	for _, root := range roots {
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		if resolved, err = filepath.Abs(resolved); err != nil {
			continue
		}
		rel, err := filepath.Rel(resolved, target)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// handleLibraryImport adds local files to the library, matching them onto
// canonical tracks like any other provider's rendition.
func (s *Server) handleLibraryImport(w http.ResponseWriter, r *http.Request) {
	var req importRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.Path == "" && req.Dir == "" {
		writeError(w, http.StatusBadRequest, "path or dir is required")
		return
	}
	if req.Path != "" && req.Dir != "" {
		writeError(w, http.StatusBadRequest, "path and dir are mutually exclusive")
		return
	}
	// The import reads the path it is given as the server's own user: a file
	// to probe and transcode, or a whole directory to walk. Over the network
	// that is for somebody with an account, inside the directories the owner
	// named - not for whoever can reach the port, and not for "/".
	if !s.localOwner {
		if _, ok := s.requireUser(w, r); !ok {
			return
		}
		if !importAllowed(s.cfg.Media.ImportDirs, req.Path+req.Dir) {
			writeError(w, http.StatusForbidden,
				"that path is not inside a directory this server imports from (media.importDirs)")
			return
		}
	}

	var (
		variants []*store.Variant
		failures []string
		err      error
	)
	if req.Path != "" {
		variant, importErr := s.media.Import(r.Context(), req.Path)
		if importErr != nil {
			writeError(w, http.StatusBadRequest, importErr.Error())
			return
		}
		variants = append(variants, variant)
	} else {
		variants, err = s.media.ScanDir(r.Context(), req.Dir)
		if err != nil {
			// Successful imports still come back; the failures are reported.
			for _, line := range strings.Split(err.Error(), "\n") {
				if strings.TrimSpace(line) != "" {
					failures = append(failures, line)
				}
			}
		}
	}

	response := importResponse{Imported: make([]importedTrack, 0, len(variants)), Failures: failures}
	for _, variant := range variants {
		// An imported file joins the canonical library: it is matched against
		// existing tracks exactly like a provider hit would be.
		track, err := s.matcher.Adopt(r.Context(), variant.ID)
		if err != nil {
			writeStoreError(w, err, "import failed")
			return
		}
		variant.TrackID = track.ID
		trackResponse, err := s.buildTrack(r.Context(), *track)
		if err != nil {
			writeStoreError(w, err, "import failed")
			return
		}
		response.Imported = append(response.Imported, importedTrack{
			Track:   trackResponse,
			Variant: s.buildVariant(r.Context(), *variant),
		})
	}

	// Local files can join tracks that only exist as provider metadata; a
	// resolution pass keeps playback possible from either side.
	for _, variant := range variants {
		if _, err := s.matcher.Resolve(r.Context(), variant.TrackID); err != nil {
			s.logger.Warn("library import: resolve", "track", variant.TrackID, "error", err)
		}
	}

	writeJSON(w, http.StatusOK, response)
}
