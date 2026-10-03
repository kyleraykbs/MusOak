package api

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/musoak/internal/ranking"
	"codeberg.org/kyleraykbs/musoak/internal/store"
)

// rather than a rendition found on a service.

// sourceUploader is who uploaded a user source.
type sourceUploader struct {
	UserID      string `json:"userId"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
}

// sourceResponse is one entry of the playbar source picker: a variant of the
// track with its vote tally and whether it plays by default.
type sourceResponse struct {
	VariantID string `json:"variantId"`
	Provider  string `json:"provider"`
	// Slot names the reserved source order position this variant fills for the
	// caller: "self" for their own upload, "uploaded" for somebody else's, and
	// "" for a provider's own rendition. Clients rank a variant by its slot
	// when it has one, so the two reserved positions actually decide playback.
	Slot         string          `json:"slot"`
	Title        string          `json:"title"`
	Artists      []string        `json:"artists"`
	Album        string          `json:"album"`
	DurationMs   int64           `json:"durationMs"`
	Downloadable bool            `json:"downloadable"`
	MediaState   string          `json:"mediaState"`
	Official     bool            `json:"official"`
	Uploader     *sourceUploader `json:"uploader"`
	Upvotes      int             `json:"upvotes"`
	Downvotes    int             `json:"downvotes"`
	MyVote       int             `json:"myVote"`
	Default      bool            `json:"default"`
}

// trackSourcesResponse is the picker document: every source of a track and
// the one the caller has chosen to play it from.
type trackSourcesResponse struct {
	Sources            []sourceResponse `json:"sources"`
	PreferredVariantID string           `json:"preferredVariantId"`
}

// handleTrackSources lists every source of a track for the playbar picker.
// The caller's effective order decides: the reserved self and uploaded slots
// come first, each standing for the variant it resolves to, then the providers
// in their places, with ties broken by net votes and the variant id so the
// order never wobbles. Exactly one source is marked default: the caller's
// saved preference when it is among the sources, otherwise the first one.
func (s *Server) handleTrackSources(w http.ResponseWriter, r *http.Request) {
	trackID, ok := trackIDFromPath(w, r)
	if !ok {
		return
	}
	if _, err := s.store.Track(r.Context(), trackID); err != nil {
		writeStoreError(w, err, "track not found")
		return
	}
	variants, err := s.store.VariantsForTrack(r.Context(), trackID)
	if err != nil {
		writeStoreError(w, err, "track not found")
		return
	}
	counts, err := s.store.VariantVoteCounts(r.Context(), trackID)
	if err != nil {
		writeStoreError(w, err, "sources unavailable")
		return
	}
	uploaders, err := s.store.VariantUploaders(r.Context(), trackID)
	if err != nil {
		writeStoreError(w, err, "sources unavailable")
		return
	}

	// Guests have no votes and no saved preference; their sources are the
	// shared ordering only.
	user := s.currentUser(r)
	var (
		myVotes   map[uuid.UUID]int
		preferred uuid.UUID
	)
	if user != nil {
		if myVotes, err = s.store.UserVariantVotes(r.Context(), user.ID, trackID); err != nil {
			writeStoreError(w, err, "sources unavailable")
			return
		}
		if preferred, err = s.store.TrackPreference(r.Context(), user.ID, trackID); err != nil {
			writeStoreError(w, err, "sources unavailable")
			return
		}
	}

	// A guest has no ranking of their own, so the aggregate order decides,
	// the same fallback rooms use for anonymous members.
	order, err := s.effectiveOrder(r.Context(), user)
	if err != nil {
		writeStoreError(w, err, "sources unavailable")
		return
	}
	caller := uuid.Nil
	if user != nil {
		caller = user.ID
	}
	ordered := ranking.OrderVariants(order, variants, uploaders, counts, caller)

	sources := make([]sourceResponse, 0, len(ordered))
	for _, variant := range ordered {
		votes := counts[variant.ID]
		status := s.media.Status(r.Context(), variant.ID)
		source := sourceResponse{
			VariantID:    variant.ID.String(),
			Provider:     variant.Provider,
			Slot:         slotFor(variant.ID, uploaders, caller),
			Title:        variant.Title,
			Artists:      variant.Artists,
			Album:        variant.Album,
			DurationMs:   variant.DurationMs,
			Downloadable: variant.Downloadable,
			MediaState:   string(status.State),
			Official:     variant.Provider != store.UploadProvider,
			Upvotes:      votes.Up,
			Downvotes:    votes.Down,
			MyVote:       myVotes[variant.ID],
		}
		if source.Artists == nil {
			source.Artists = []string{}
		}
		if uploader, ok := uploaders[variant.ID]; ok {
			source.Uploader = &sourceUploader{
				UserID:      uploader.UserID.String(),
				Username:    uploader.Username,
				DisplayName: uploader.DisplayName,
			}
		}
		sources = append(sources, source)
	}

	preferredID := ""
	if preferred != uuid.Nil {
		preferredID = preferred.String()
	}
	defaultIndex := 0
	if preferredID != "" {
		for i := range sources {
			if sources[i].VariantID == preferredID {
				defaultIndex = i
				break
			}
		}
	}
	if len(sources) > 0 {
		sources[defaultIndex].Default = true
	}

	writeJSON(w, http.StatusOK, trackSourcesResponse{
		Sources:            sources,
		PreferredVariantID: preferredID,
	})
}

// effectiveOrder is the order that decides a caller's rendition: their own
// ranking, or the aggregate for a guest, either way carrying the reserved
// slots at its head.
func (s *Server) effectiveOrder(ctx context.Context, user *store.User) ([]string, error) {
	if user != nil {
		return s.ranking.User(ctx, user.ID)
	}
	return s.ranking.Aggregate(ctx)
}

// slotFor names the reserved order position a variant fills for the caller:
// "self" when they uploaded it, "uploaded" when somebody else did, and "" for
// a provider's own rendition. A guest has no self, so every upload they see is
// somebody else's.
func slotFor(variantID uuid.UUID, uploaders map[uuid.UUID]store.VariantUploader, caller uuid.UUID) string {
	uploader, ok := uploaders[variantID]
	if !ok {
		return ""
	}
	if caller != uuid.Nil && uploader.UserID == caller {
		return ranking.SlotSelf
	}
	return ranking.SlotUploaded
}


type variantVoteRequest struct {
	Value *int `json:"value"`
}

// handleVariantVote records the caller's vote for a variant. One vote per
// user per variant: voting again changes it, 0 withdraws it. The ordering
// these votes reshape is shared by everyone.
func (s *Server) handleVariantVote(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	id, ok := variantIDFromPath(w, r)
	if !ok {
		return
	}
	var req variantVoteRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Value == nil || *req.Value < -1 || *req.Value > 1 {
		writeError(w, http.StatusBadRequest, "value must be 1, -1 or 0")
		return
	}
	if _, err := s.store.Variant(r.Context(), id); err != nil {
		writeStoreError(w, err, "variant not found")
		return
	}
	if err := s.store.SetVariantVote(r.Context(), user.ID, id, *req.Value); err != nil {
		writeStoreError(w, err, "could not store vote")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type trackPreferenceRequest struct {
	VariantID string `json:"variantId"`
}

// handleTrackPreference saves the source the caller wants to play a track
// from. It is personal — other listeners keep the shared default — and wins
// over the vote ordering when this caller plays the song.
func (s *Server) handleTrackPreference(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	trackID, ok := trackIDFromPath(w, r)
	if !ok {
		return
	}
	var req trackPreferenceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	variantID, err := uuid.Parse(req.VariantID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid variantId")
		return
	}
	if _, err := s.store.Track(r.Context(), trackID); err != nil {
		writeStoreError(w, err, "track not found")
		return
	}
	variant, err := s.store.Variant(r.Context(), variantID)
	if err != nil {
		writeStoreError(w, err, "variant not found")
		return
	}
	if variant.TrackID != trackID {
		writeError(w, http.StatusBadRequest, "variant does not belong to track")
		return
	}
	if err := s.store.SetTrackPreference(r.Context(), user.ID, trackID, variantID); err != nil {
		writeStoreError(w, err, "could not store preference")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
