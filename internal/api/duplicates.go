package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/musoak/internal/store"
)

// maxAssociateUploads bounds one bulk association: a selection is a handful of
// files, and the release rule scans the caller's uploads for each of them.
const maxAssociateUploads = 100

// uploadAssociateRequest is a bulk association: the uploads to point at one
// canonical song. An empty associateTrackId gives each upload a track of its
// own, the way PATCH with no association does.
type uploadAssociateRequest struct {
	UploadIDs        []string `json:"uploadIds"`
	AssociateTrackID string   `json:"associateTrackId"`
}

// duplicateGroup is a set of the caller's uploads whose stored bytes are the
// same file, keyed by the content hash they share.
type duplicateGroup struct {
	SHA256  string           `json:"sha256"`
	Uploads []uploadResponse `json:"uploads"`
}

// handleUploadDuplicates lists the caller's uploads that are the same bytes as
// another of their uploads, grouped by content hash. The create endpoint
// refuses a second copy, so these are the ones stored before that check.
func (s *Server) handleUploadDuplicates(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	groups, err := s.store.DuplicateUploads(r.Context(), user.ID)
	if err != nil {
		writeStoreError(w, err, "duplicates unavailable")
		return
	}
	out := make([]duplicateGroup, 0, len(groups))
	for _, group := range groups {
		uploads := make([]uploadResponse, 0, len(group.Uploads))
		for i := range group.Uploads {
			uploads = append(uploads, s.uploadResponseFor(r.Context(), &group.Uploads[i]))
		}
		out = append(out, duplicateGroup{SHA256: group.SHA256, Uploads: uploads})
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": out})
}

// handleUploadsAssociate points several of the caller's uploads at one
// canonical song, or back to a track of their own each. It applies the same
// rule as the single-upload PATCH: the uploads taking the association keep it,
// and the caller's other uploads already on the target are released. Nothing is
// written unless every id names the caller's own upload and the target exists,
// so a batch that names a stranger changes nothing at all.
func (s *Server) handleUploadsAssociate(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	var req uploadAssociateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.UploadIDs) == 0 {
		writeError(w, http.StatusBadRequest, "uploadIds is required")
		return
	}
	if len(req.UploadIDs) > maxAssociateUploads {
		writeError(w, http.StatusBadRequest, "no more than 100 uploads at a time")
		return
	}

	// Every upload has to be the caller's before anything is written.
	uploads := make([]*store.Upload, 0, len(req.UploadIDs))
	seen := make(map[uuid.UUID]struct{}, len(req.UploadIDs))
	for _, raw := range req.UploadIDs {
		id, err := uuid.Parse(strings.TrimSpace(raw))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid upload id")
			return
		}
		if _, repeat := seen[id]; repeat {
			continue
		}
		seen[id] = struct{}{}
		upload, err := s.store.Upload(r.Context(), id)
		if err != nil || upload.UserID != user.ID {
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				writeStoreError(w, err, "upload not found")
				return
			}
			writeError(w, http.StatusNotFound, "upload not found")
			return
		}
		uploads = append(uploads, upload)
	}

	// The target is resolved before the first write, so a bad track id leaves
	// the caller's uploads exactly as they were.
	var target *store.Track
	if strings.TrimSpace(req.AssociateTrackID) != "" {
		found, ok := s.associationTarget(w, r, req.AssociateTrackID)
		if !ok {
			return
		}
		target = found
	}

	for _, upload := range uploads {
		if err := s.setUploadAssociation(r.Context(), upload, target); err != nil {
			writeStoreError(w, err, "the association could not be changed")
			return
		}
	}

	var released []store.Upload
	if target != nil {
		keep := make([]uuid.UUID, 0, len(uploads))
		for _, upload := range uploads {
			keep = append(keep, upload.ID)
		}
		released = s.releaseOtherAssociations(r.Context(), user.ID, target.ID, keep...)
	}

	out := make([]uploadResponse, 0, len(uploads))
	for _, upload := range uploads {
		updated, err := s.store.Upload(r.Context(), upload.ID)
		if err != nil {
			writeStoreError(w, err, "the association could not be changed")
			return
		}
		out = append(out, s.uploadResponseFor(r.Context(), updated))
	}
	releasedOut := make([]uploadResponse, 0, len(released))
	for i := range released {
		releasedOut = append(releasedOut, s.uploadResponseFor(r.Context(), &released[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"uploads": out, "released": releasedOut})
}
