package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"codeberg.org/kyleraykbs/prismusic/internal/store"
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
		track, err := s.store.Track(r.Context(), variant.TrackID)
		if err != nil {
			writeStoreError(w, err, "imported variant lost its track")
			return
		}
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
