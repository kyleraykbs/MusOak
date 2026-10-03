package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/musoak/internal/match"
	"codeberg.org/kyleraykbs/musoak/internal/provider"
	"codeberg.org/kyleraykbs/musoak/internal/ranking"
	"codeberg.org/kyleraykbs/musoak/internal/store"
)

// trackResponse is the canonical track as clients see it.
type trackResponse struct {
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	DurationMs int64    `json:"durationMs"`
	Artists    []string `json:"artists"`
	// ArtistIDs is parallel to Artists, in the same order, so a client can open
	// an artist's page from a track: the names alone cannot name a page.
	ArtistIDs []string  `json:"artistIds"`
	Albums    []string  `json:"albums"`
	CreatedAt time.Time `json:"createdAt"`
	// ArtworkURL is a path on this server, empty when nothing is known yet.
	ArtworkURL string `json:"artworkUrl,omitempty"`
	// Plays is how many times the calling user played this track: 0 for a guest
	// and for a song they have never played, so a client can show it without
	// asking whether it means anything.
	Plays int `json:"plays"`
}

// variantResponse is one provider's rendition of a track, with its download
// state so a client knows whether it can play it right now.
type variantResponse struct {
	ID       string `json:"id"`
	TrackID  string `json:"trackId"`
	Provider string `json:"provider"`
	// Slot names the reserved source order position this variant fills for the
	// caller: "self" for their own upload, "uploaded" for somebody else's, and
	// "" for a provider's own rendition. A client ranks a variant by its slot
	// when it has one, so the two reserved positions decide playback.
	Slot            string              `json:"slot"`
	ProviderTrackID string              `json:"providerTrackId"`
	Title           string              `json:"title"`
	Artists         []string            `json:"artists"`
	Album           string              `json:"album"`
	DurationMs      int64               `json:"durationMs"`
	Downloadable    bool                `json:"downloadable"`
	ISRC            string              `json:"isrc,omitempty"`
	Media           mediaStatusResponse `json:"media"`
}

// buildTrack fills in the artists and albums of a track.
func (s *Server) buildTrack(ctx context.Context, track store.Track) (trackResponse, error) {
	artists, err := s.store.TrackArtists(ctx, track.ID)
	if err != nil {
		return trackResponse{}, err
	}
	albums, err := s.store.TrackAlbums(ctx, track.ID)
	if err != nil {
		return trackResponse{}, err
	}

	out := trackResponse{
		ID:         track.ID.String(),
		Title:      track.Title,
		DurationMs: track.DurationMs,
		Artists:    make([]string, 0, len(artists)),
		ArtistIDs:  make([]string, 0, len(artists)),
		Albums:     make([]string, 0, len(albums)),
		CreatedAt:  track.CreatedAt,
	}
	// The header may have been read before the cover was learned, so fall back
	// to a lookup rather than pretending the track has no artwork.
	artwork := track.ArtworkURL
	if artwork == "" {
		if url, err := s.store.TrackArtwork(ctx, track.ID); err == nil {
			artwork = url
		}
	}
	if artwork != "" {
		out.ArtworkURL = artworkPath(store.ArtworkTrack, track.ID)
	}
	for _, artist := range artists {
		out.Artists = append(out.Artists, artist.Name)
		out.ArtistIDs = append(out.ArtistIDs, artist.ID.String())
	}
	for _, album := range albums {
		out.Albums = append(out.Albums, album.Title)
	}
	return out, nil
}

// buildVariant attaches the current download state.
func (s *Server) buildVariant(ctx context.Context, variant store.Variant) variantResponse {
	status := s.media.Status(ctx, variant.ID)
	out := variantResponse{
		ID:              variant.ID.String(),
		TrackID:         variant.TrackID.String(),
		Provider:        variant.Provider,
		ProviderTrackID: variant.ProviderTrackID,
		Title:           variant.Title,
		Artists:         variant.Artists,
		Album:           variant.Album,
		DurationMs:      variant.DurationMs,
		Downloadable:    variant.Downloadable,
		ISRC:            variant.ISRC,
		Media: mediaStatusResponse{
			VariantID:  status.VariantID.String(),
			State:      string(status.State),
			Progress:   status.Progress,
			Error:      status.Err,
			DurationMs: status.DurationMs,
			Bytes:      status.Bytes,
		},
	}
	if out.Artists == nil {
		out.Artists = []string{}
	}
	return out
}

func (s *Server) buildVariants(ctx context.Context, variants []store.Variant) []variantResponse {
	out := make([]variantResponse, 0, len(variants))
	for _, variant := range variants {
		out = append(out, s.buildVariant(ctx, variant))
	}
	return out
}

// withPlays annotates tracks with how often the caller played each one. One
// query covers a whole list, and a guest costs none: a play count belongs to an
// account, and a client without one has nothing to count.
func (s *Server) withPlays(ctx context.Context, user *store.User, tracks []*trackResponse) {
	if user == nil || len(tracks) == 0 {
		return
	}
	ids := make([]uuid.UUID, 0, len(tracks))
	at := make(map[uuid.UUID][]*trackResponse, len(tracks))
	for _, track := range tracks {
		if track == nil {
			continue
		}
		id, err := uuid.Parse(track.ID)
		if err != nil {
			continue // an id this server built is always a uuid
		}
		if _, seen := at[id]; !seen {
			ids = append(ids, id)
		}
		at[id] = append(at[id], track)
	}
	if len(ids) == 0 {
		return
	}
	counts, err := s.store.PlayCounts(ctx, user.ID, ids)
	if err != nil {
		// The count is a label beside a title: losing it is better than losing
		// the page it sits on.
		s.logger.Warn("plays: count failed", "error", err)
		return
	}
	for id, targets := range at {
		plays := counts[id].Plays
		for _, track := range targets {
			track.Plays = plays
		}
	}
}

// trackRefs points at each track in a list, so a slice built as values can
// still be annotated in place by withPlays.
func trackRefs(tracks []trackResponse) []*trackResponse {
	out := make([]*trackResponse, len(tracks))
	for i := range tracks {
		out[i] = &tracks[i]
	}
	return out
}

// trackIDFromPath parses the {trackId} path value.
func trackIDFromPath(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	raw := r.PathValue("trackId")
	if len(raw) == 0 || len(raw) > maxMediaVariantIDLen {
		writeError(w, http.StatusBadRequest, "invalid track id")
		return uuid.Nil, false
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid track id")
		return uuid.Nil, false
	}
	return id, true
}

// writeStoreError maps repository failures onto status codes.
func writeStoreError(w http.ResponseWriter, err error, notFound string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, notFound)
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

// handleTrack returns one canonical track.
func (s *Server) handleTrack(w http.ResponseWriter, r *http.Request) {
	id, ok := trackIDFromPath(w, r)
	if !ok {
		return
	}
	track, err := s.store.Track(r.Context(), id)
	if err != nil {
		writeStoreError(w, err, "track not found")
		return
	}
	response, err := s.buildTrack(r.Context(), *track)
	if err != nil {
		writeStoreError(w, err, "track not found")
		return
	}
	s.withPlays(r.Context(), s.currentUser(r), []*trackResponse{&response})
	writeJSON(w, http.StatusOK, response)
}

// handleTrackVariants lists every provider rendition of a track, in the
// caller's effective order, each naming the reserved slot it fills when it is
// a user's upload rather than a provider's rendition.
func (s *Server) handleTrackVariants(w http.ResponseWriter, r *http.Request) {
	id, ok := trackIDFromPath(w, r)
	if !ok {
		return
	}
	if _, err := s.store.Track(r.Context(), id); err != nil {
		writeStoreError(w, err, "track not found")
		return
	}
	variants, err := s.store.VariantsForTrack(r.Context(), id)
	if err != nil {
		writeStoreError(w, err, "track not found")
		return
	}
	counts, err := s.store.VariantVoteCounts(r.Context(), id)
	if err != nil {
		writeStoreError(w, err, "variants unavailable")
		return
	}
	uploaders, err := s.store.VariantUploaders(r.Context(), id)
	if err != nil {
		writeStoreError(w, err, "variants unavailable")
		return
	}
	user := s.currentUser(r)
	order, err := s.effectiveOrder(r.Context(), user)
	if err != nil {
		writeStoreError(w, err, "variants unavailable")
		return
	}
	var caller uuid.UUID
	if user != nil {
		caller = user.ID
	}
	ordered := ranking.OrderVariants(order, variants, uploaders, counts, caller)
	out := s.buildVariants(r.Context(), ordered)
	for i := range out {
		out[i].Slot = slotFor(ordered[i].ID, uploaders, caller)
	}
	writeJSON(w, http.StatusOK, map[string]any{"variants": out})
}

// handleTrackResolve makes sure a track has a downloadable variant, matching
// one from a downloadable provider when it only has metadata-only renditions.
func (s *Server) handleTrackResolve(w http.ResponseWriter, r *http.Request) {
	id, ok := trackIDFromPath(w, r)
	if !ok {
		return
	}
	if _, err := s.store.Track(r.Context(), id); err != nil {
		writeStoreError(w, err, "track not found")
		return
	}

	variants, err := s.matcher.Resolve(r.Context(), id)
	if err != nil && !errors.Is(err, match.ErrNoPlayableVariant) {
		writeStoreError(w, err, "track not found")
		return
	}
	response := map[string]any{"variants": s.buildVariants(r.Context(), variants)}
	if errors.Is(err, match.ErrNoPlayableVariant) {
		response["playable"] = false
		writeJSON(w, http.StatusOK, response)
		return
	}
	response["playable"] = true
	writeJSON(w, http.StatusOK, response)
}

// sourceAttachRequest is a provider pick from the "no source found" dialog:
// the metadata the provider reported for the rendition the user chose.
type sourceAttachRequest struct {
	Provider        string   `json:"provider"`
	ProviderTrackID string   `json:"providerTrackId"`
	Title           string   `json:"title"`
	Artists         []string `json:"artists"`
	Album           string   `json:"album"`
	DurationMs      int64    `json:"durationMs"`
	ArtworkURL      string   `json:"artworkUrl"`
}

// handleTrackSourceAttach attaches a provider pick to a canonical track the
// caller chose. The pick is deliberate, so it is never re-matched against the
// library. An unregistered provider or an incomplete body is a 400, an
// unknown track a 404, and a guest a 401.
func (s *Server) handleTrackSourceAttach(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r); !ok {
		return
	}
	id, ok := trackIDFromPath(w, r)
	if !ok {
		return
	}

	var req sourceAttachRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Provider) == "" || strings.TrimSpace(req.ProviderTrackID) == "" || strings.TrimSpace(req.Title) == "" {
		writeError(w, http.StatusBadRequest, "provider, providerTrackId and title are required")
		return
	}

	_, _, err := s.matcher.AttachToTrack(r.Context(), id, req.Provider, provider.Track{
		ProviderTrackID: req.ProviderTrackID,
		Title:           req.Title,
		Artists:         req.Artists,
		Album:           req.Album,
		DurationMs:      req.DurationMs,
		ArtworkURL:      req.ArtworkURL,
	})
	switch {
	case errors.Is(err, match.ErrUnknownProvider):
		writeError(w, http.StatusBadRequest, "unknown provider")
		return
	case err != nil:
		writeStoreError(w, err, "track not found")
		return
	}

	variants, err := s.store.VariantsForTrack(r.Context(), id)
	if err != nil {
		writeStoreError(w, err, "track not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"variants": s.buildVariants(r.Context(), variants)})
}
