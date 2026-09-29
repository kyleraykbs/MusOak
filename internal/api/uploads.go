package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/prismusic/internal/artwork"
	"codeberg.org/kyleraykbs/prismusic/internal/ffmpeg"
	"codeberg.org/kyleraykbs/prismusic/internal/store"
)

// uploadCreateRequest is a song upload: the audio, base64, with the metadata
// the dialog collected. A JSON body keeps the upload to the endpoints already
// here rather than adding multipart parsing for one feature.
type uploadCreateRequest struct {
	Filename    string   `json:"filename"`
	ContentType string   `json:"contentType"`
	Data        string   `json:"data"`
	Title       string   `json:"title"`
	Artists     []string `json:"artists"`
	Album       string   `json:"album"`
	DurationMs  int64    `json:"durationMs"`
	// ArtworkData is an optional cover, base64, with the type it claims to be.
	ArtworkData        string `json:"artworkData"`
	ArtworkContentType string `json:"artworkContentType"`
	// AssociateTrackID names the canonical track the upload is a source of; ""
	// means No Association and gives the upload a track of its own.
	AssociateTrackID string `json:"associateTrackId"`
}

// uploadAssociationRequest re-points an upload at a canonical track. The field
// is a pointer so "absent" is not silently read as No Association.
type uploadAssociationRequest struct {
	AssociateTrackID *string `json:"associateTrackId"`
}

type uploaderResponse struct {
	UserID      string `json:"userId"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
}

// uploadResponse is an upload as the Manage tab lists it.
type uploadResponse struct {
	ID         string           `json:"id"`
	TrackID    string           `json:"trackId"`
	VariantID  string           `json:"variantId"`
	Title      string           `json:"title"`
	Artists    []string         `json:"artists"`
	Album      string           `json:"album"`
	DurationMs int64            `json:"durationMs"`
	ArtworkURL string           `json:"artworkUrl,omitempty"`
	Uploader   uploaderResponse `json:"uploader"`
	Official   bool             `json:"official"`
	CreatedAt  string           `json:"createdAt"`
	// AssociateTrackID mirrors the association the dialog edits: "" is No
	// Association (TrackID is then the upload's own track), otherwise it is
	// the canonical track the upload joined, equal to TrackID.
	AssociateTrackID string `json:"associateTrackId"`
}

// maxUploadBytes is the decoded audio cap; maxUploadBodyBytes bounds the JSON
// envelope around it (base64 runs a third larger, plus the metadata).
const (
	maxUploadBytes     = 64 << 20
	maxUploadBodyBytes = 96 << 20
)

// decodeUploadJSON reads the upload envelope, which carries the audio and so
// is far bigger than any other body on the server.
func decodeUploadJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	body := http.MaxBytesReader(w, r.Body, maxUploadBodyBytes)
	err := json.NewDecoder(body).Decode(v)
	if err == nil {
		return true
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "the upload is too large")
		return false
	}
	writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
	return false
}

// handleUploadCreate stores a song somebody uploaded. Every registered user
// may upload, and the song is then visible to every user.
//
// The bytes land where the media manager keeps renditions and are recorded as
// a media file exactly like a download, so /api/v1/media/{variantId} serves
// them unchanged.
func (s *Server) handleUploadCreate(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	var req uploadCreateRequest
	if !decodeUploadJSON(w, r, &req) {
		return
	}

	filename := strings.TrimSpace(req.Filename)
	if filename == "" {
		writeError(w, http.StatusBadRequest, "filename is required")
		return
	}
	if len(filename) > 255 {
		writeError(w, http.StatusBadRequest, "filename is too long")
		return
	}
	// An unlabelled upload is named after its file, the way an import reads
	// its tags.
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(filename), filepath.Ext(filename))
	}
	if title == "" {
		writeError(w, http.StatusBadRequest, "title is required")
		return
	}
	if len(title) > 200 {
		writeError(w, http.StatusBadRequest, "title is too long")
		return
	}
	artists := cleanNames(req.Artists)
	album := strings.TrimSpace(req.Album)

	data, err := base64.StdEncoding.DecodeString(req.Data)
	if err != nil {
		writeError(w, http.StatusBadRequest, "data must be base64")
		return
	}
	if len(data) == 0 {
		writeError(w, http.StatusBadRequest, "the audio is empty")
		return
	}
	if len(data) > maxUploadBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "the upload is too large")
		return
	}

	cover, coverExt, ok := decodeUploadCover(w, req)
	if !ok {
		return
	}

	// A missing association target is reported before anything is stored.
	var target *store.Track
	if strings.TrimSpace(req.AssociateTrackID) != "" {
		target, ok = s.associationTarget(w, r, req.AssociateTrackID)
		if !ok {
			return
		}
	}

	variantID := uuid.New()
	path := filepath.Join(s.media.Dir(), variantID.String()+".opus")
	if err := os.MkdirAll(s.media.Dir(), 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "the upload could not be stored")
		return
	}
	// Written under a temporary name and moved into place, so nothing ever
	// serves a partial file.
	tmp := path + ".part"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		writeError(w, http.StatusInternalServerError, "the upload could not be stored")
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		writeError(w, http.StatusInternalServerError, "the upload could not be stored")
		return
	}

	// The stored bytes decide the duration: one probe describing exactly what
	// is served, the way the download pipeline and the library import record
	// it. When the probe cannot read the file, the client's own number stands.
	durationMs := req.DurationMs
	if probed, err := ffmpeg.Duration(r.Context(), path); err == nil {
		durationMs = probed.Milliseconds()
	}
	if durationMs < 0 {
		durationMs = 0
	}

	if target == nil {
		// No Association: the upload stands alone on a fresh track of its own.
		target, err = s.ownTrack(r.Context(), title, artists, album, durationMs)
	} else if target.DurationMs == 0 && durationMs > 0 {
		// A canonical track with no duration of its own inherits the first one
		// any source teaches it; a known duration is never overwritten.
		err = s.store.SetTrackDuration(r.Context(), target.ID, durationMs)
	}
	if err != nil {
		_ = os.Remove(path)
		writeStoreError(w, err, "the upload could not be stored")
		return
	}

	upload := &store.Upload{UserID: user.ID, Filename: filename}
	variant := &store.Variant{
		TrackID:    target.ID,
		Title:      title,
		Artists:    artists,
		Album:      album,
		DurationMs: durationMs,
	}
	file := &store.MediaFile{
		Path:       path,
		SHA256:     sha256Hex(data),
		DurationMs: durationMs,
		Bytes:      int64(len(data)),
	}
	if err := s.store.CreateUpload(r.Context(), upload, variant, file); err != nil {
		_ = os.Remove(path)
		writeStoreError(w, err, "the upload could not be stored")
		return
	}

	s.storeUploadCover(r.Context(), upload.ID, target.ID, cover, coverExt)

	created, err := s.store.Upload(r.Context(), upload.ID)
	if err != nil {
		writeStoreError(w, err, "the upload could not be stored")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"upload": s.uploadResponseFor(r.Context(), created)})
}

// decodeUploadCover reads the optional cover. ok is false and the request is
// answered when one was sent but is not usable.
func decodeUploadCover(w http.ResponseWriter, req uploadCreateRequest) (data []byte, extension string, ok bool) {
	if req.ArtworkData == "" {
		return nil, "", true
	}
	contentType := artwork.ContentType(req.ArtworkContentType)
	extension, known := artwork.ExtensionFor(contentType)
	if !known {
		writeError(w, http.StatusBadRequest, "an image is needed: png, jpeg, webp, gif or avif")
		return nil, "", false
	}
	data, err := base64.StdEncoding.DecodeString(req.ArtworkData)
	if err != nil {
		writeError(w, http.StatusBadRequest, "artworkData must be base64")
		return nil, "", false
	}
	if len(data) == 0 {
		writeError(w, http.StatusBadRequest, "the image is empty")
		return nil, "", false
	}
	if len(data) > maxArtworkBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "the image is too large")
		return nil, "", false
	}
	return data, extension, true
}

// storeUploadCover files a cover the way the artwork cache files any other:
// under its own source, recorded on the track. Only an empty slot is filled,
// so a song that already has a cover keeps it; a cover that cannot be stored
// never fails the upload itself.
func (s *Server) storeUploadCover(ctx context.Context, uploadID, trackID uuid.UUID, data []byte, extension string) {
	if len(data) == 0 {
		return
	}
	source := uploadSource(uploadID)
	if err := s.artwork.Store(source, data, extension); err != nil {
		s.logger.Warn("upload: artwork not stored", "upload", uploadID, "error", err)
		return
	}
	if err := s.store.SetTrackArtwork(ctx, trackID, source); err != nil {
		s.logger.Warn("upload: artwork not recorded", "upload", uploadID, "error", err)
	}
}

// associationTarget resolves the canonical track an upload joins.
func (s *Server) associationTarget(w http.ResponseWriter, r *http.Request, raw string) (*store.Track, bool) {
	id, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid associateTrackId")
		return nil, false
	}
	track, err := s.store.Track(r.Context(), id)
	if err != nil {
		writeStoreError(w, err, "track not found")
		return nil, false
	}
	return track, true
}

// ownTrack creates the canonical track an unassociated upload stands alone on,
// credited from the upload's own metadata.
func (s *Server) ownTrack(ctx context.Context, title string, artists []string, album string, durationMs int64) (*store.Track, error) {
	track := &store.Track{Title: title, DurationMs: durationMs}
	if err := s.store.CreateTrack(ctx, track); err != nil {
		return nil, err
	}
	if err := s.store.SetTrackArtists(ctx, track.ID, artists); err != nil {
		return nil, err
	}
	if album != "" {
		if err := s.store.SetTrackAlbums(ctx, track.ID, []string{album}); err != nil {
			return nil, err
		}
	}
	return track, nil
}

// dropEmptyTrack removes a canonical track an upload left without a single
// rendition, so deleting an upload leaves no dead song behind. A track other
// sources still point at is left alone.
func (s *Server) dropEmptyTrack(ctx context.Context, trackID uuid.UUID) {
	err := s.store.DeleteTrack(ctx, trackID)
	if err != nil && !errors.Is(err, store.ErrConflict) && !errors.Is(err, store.ErrNotFound) {
		s.logger.Warn("upload: could not remove the emptied track", "track", trackID, "error", err)
	}
}

// uploadResponseFor shapes one upload for the API.
func (s *Server) uploadResponseFor(ctx context.Context, u *store.Upload) uploadResponse {
	out := uploadResponse{
		ID:         u.ID.String(),
		TrackID:    u.Variant.TrackID.String(),
		VariantID:  u.Variant.ID.String(),
		Title:      u.Variant.Title,
		Artists:    u.Variant.Artists,
		Album:      u.Variant.Album,
		DurationMs: u.Variant.DurationMs,
		Uploader: uploaderResponse{
			UserID:      u.Uploader.UserID.String(),
			Username:    u.Uploader.Username,
			DisplayName: u.Uploader.DisplayName,
		},
		Official:  false, // an upload is never an official source
		CreatedAt: u.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	if out.Artists == nil {
		out.Artists = []string{}
	}
	if u.Solo {
		out.AssociateTrackID = ""
	} else {
		out.AssociateTrackID = out.TrackID
	}
	if url, err := s.store.TrackArtwork(ctx, u.Variant.TrackID); err == nil && url != "" {
		out.ArtworkURL = artworkPath(store.ArtworkTrack, u.Variant.TrackID)
	}
	return out
}

// writeUploads renders an upload listing.
func (s *Server) writeUploads(w http.ResponseWriter, r *http.Request, uploads []store.Upload) {
	out := make([]uploadResponse, 0, len(uploads))
	for i := range uploads {
		out = append(out, s.uploadResponseFor(r.Context(), &uploads[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"uploads": out})
}

// handleUploadList lists the caller's uploads.
func (s *Server) handleUploadList(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	uploads, err := s.store.UploadsForUser(r.Context(), user.ID)
	if err != nil {
		writeStoreError(w, err, "uploads unavailable")
		return
	}
	s.writeUploads(w, r, uploads)
}

// handleUploadListAll lists every user's uploads: uploaded songs are visible
// to every user, and this is the inspection view of them.
func (s *Server) handleUploadListAll(w http.ResponseWriter, r *http.Request) {
	uploads, err := s.store.AllUploads(r.Context())
	if err != nil {
		writeStoreError(w, err, "uploads unavailable")
		return
	}
	s.writeUploads(w, r, uploads)
}

// ownedUpload returns the upload when it is the caller's. Somebody else's
// upload is reported as missing, so the endpoint does not leak who uploaded
// what.
func (s *Server) ownedUpload(w http.ResponseWriter, r *http.Request, userID uuid.UUID) (*store.Upload, bool) {
	raw := r.PathValue("uploadId")
	if len(raw) == 0 || len(raw) > maxMediaVariantIDLen {
		writeError(w, http.StatusBadRequest, "invalid upload id")
		return nil, false
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid upload id")
		return nil, false
	}

	upload, err := s.store.Upload(r.Context(), id)
	if err != nil || upload.UserID != userID {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			writeStoreError(w, err, "upload not found")
			return nil, false
		}
		writeError(w, http.StatusNotFound, "upload not found")
		return nil, false
	}
	return upload, true
}

// handleUploadPatch re-points an upload at another canonical track, or back to
// No Association. The upload keeps its own title, artists and album: the
// association only decides which song it is a source of.
func (s *Server) handleUploadPatch(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	upload, ok := s.ownedUpload(w, r, user.ID)
	if !ok {
		return
	}
	var req uploadAssociationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.AssociateTrackID == nil {
		writeError(w, http.StatusBadRequest, "associateTrackId is required")
		return
	}

	left := upload.Variant.TrackID
	var target *store.Track
	if strings.TrimSpace(*req.AssociateTrackID) == "" {
		// No Association: the upload stands alone again, on a fresh track
		// credited from its own metadata.
		created, err := s.ownTrack(r.Context(), upload.Variant.Title, upload.Variant.Artists, upload.Variant.Album, upload.Variant.DurationMs)
		if err != nil {
			writeStoreError(w, err, "the association could not be changed")
			return
		}
		target = created
	} else {
		found, ok := s.associationTarget(w, r, *req.AssociateTrackID)
		if !ok {
			return
		}
		target = found
	}

	if err := s.store.SetUploadTrack(r.Context(), upload.ID, target.ID); err != nil {
		writeStoreError(w, err, "the association could not be changed")
		return
	}
	s.dropEmptyTrack(r.Context(), left)

	updated, err := s.store.Upload(r.Context(), upload.ID)
	if err != nil {
		writeStoreError(w, err, "the association could not be changed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"upload": s.uploadResponseFor(r.Context(), updated)})
}

// handleUploadDelete removes an upload: its rows, its bytes, and the canonical
// track when the upload was the last rendition on it.
func (s *Server) handleUploadDelete(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	upload, ok := s.ownedUpload(w, r, user.ID)
	if !ok {
		return
	}
	if err := s.store.DeleteUpload(r.Context(), upload.ID); err != nil {
		writeStoreError(w, err, "the upload could not be deleted")
		return
	}
	s.dropEmptyTrack(r.Context(), upload.Variant.TrackID)
	w.WriteHeader(http.StatusNoContent)
}

// cleanNames drops the blank entries of an artist list.
func cleanNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
