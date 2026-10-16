package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kyleraykbs/MusOak/internal/match"
	"github.com/kyleraykbs/MusOak/internal/media"
	"github.com/kyleraykbs/MusOak/internal/provider"
	"github.com/kyleraykbs/MusOak/internal/ranking"
	"github.com/kyleraykbs/MusOak/internal/store"
)

// trackResponse is the canonical track as clients see it.
type trackResponse struct {
	ID         string    `json:"id"`
	Title      string    `json:"title"`
	DurationMs int64     `json:"durationMs"`
	Artists    []string  `json:"artists"`
	ArtistIDs  []string  `json:"artistIds"`
	Albums     []string  `json:"albums"`
	CreatedAt  time.Time `json:"createdAt"`
	ArtworkURL string    `json:"artworkUrl,omitempty"`
	Plays      int       `json:"plays"`
	Rating     float64   `json:"rating"`
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
	// The header may have been read before the cover was learned, so fall back
	// to a lookup rather than pretending the track has no artwork.
	artwork := track.ArtworkURL
	if artwork == "" {
		if url, err := s.store.TrackArtwork(ctx, track.ID); err == nil {
			artwork = url
		}
	}
	return trackResponseFor(track, artists, albums, artwork), nil
}

// buildTracks fills in the artists, albums and artwork of a whole listing in a
// few queries rather than one set per row: a page of two hundred songs must not
// cost six hundred round trips to render.
func (s *Server) buildTracks(ctx context.Context, tracks []store.Track) ([]trackResponse, error) {
	ids := make([]uuid.UUID, len(tracks))
	for i := range tracks {
		ids[i] = tracks[i].ID
	}
	artists, err := s.store.TrackArtistsFor(ctx, ids)
	if err != nil {
		return nil, err
	}
	albums, err := s.store.TrackAlbumsFor(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]trackResponse, 0, len(tracks))
	for i := range tracks {
		track := tracks[i]
		out = append(out, trackResponseFor(track, artists[track.ID], albums[track.ID], track.ArtworkURL))
	}
	return out, nil
}

// trackResponseFor shapes one track from credits and artwork already read.
func trackResponseFor(track store.Track, artists []store.Artist, albums []store.Album, artwork string) trackResponse {
	out := trackResponse{
		ID:         track.ID.String(),
		Title:      track.Title,
		DurationMs: track.DurationMs,
		Artists:    make([]string, 0, len(artists)),
		ArtistIDs:  make([]string, 0, len(artists)),
		Albums:     make([]string, 0, len(albums)),
		CreatedAt:  track.CreatedAt,
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
	return out
}

// buildVariant attaches the current download state.
func (s *Server) buildVariant(ctx context.Context, variant store.Variant) variantResponse {
	return variantResponseFrom(variant, mediaStatusResponseFrom(s.media.Status(ctx, variant.ID)))
}

// variantResponseFrom shapes one variant from a download state already known.
func variantResponseFrom(variant store.Variant, status mediaStatusResponse) variantResponse {
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
		Media:           status,
	}
	if out.Artists == nil {
		out.Artists = []string{}
	}
	return out
}

// buildVariants renders a whole rendition list. One read answers which of them
// already have a file on disk; only the rest ask the media manager, where a
// transfer in flight is known, so a list is not a query per row.
func (s *Server) buildVariants(ctx context.Context, variants []store.Variant) []variantResponse {
	ready, err := s.store.MediaFilesFor(ctx, variantIDs(variants))
	if err != nil {
		// A listing is still worth answering; the manager below fills every row.
		ready = nil
	}
	out := make([]variantResponse, 0, len(variants))
	for _, variant := range variants {
		out = append(out, variantResponseFrom(variant, s.variantMediaStatus(ctx, variant.ID, ready)))
	}
	return out
}

// variantMediaStatus answers one rendition's download state, preferring the
// stored file already read for the listing over a second look.
func (s *Server) variantMediaStatus(ctx context.Context, variantID uuid.UUID, ready map[uuid.UUID]store.MediaFile) mediaStatusResponse {
	if file, ok := ready[variantID]; ok {
		return mediaStatusResponse{
			VariantID:  file.VariantID.String(),
			State:      string(media.StateReady),
			Progress:   1,
			DurationMs: file.DurationMs,
			Bytes:      file.Bytes,
		}
	}
	return mediaStatusResponseFrom(s.media.Status(ctx, variantID))
}

// mediaStatusResponseFrom renders a manager status in the API's shape.
func mediaStatusResponseFrom(status media.Status) mediaStatusResponse {
	return mediaStatusResponse{
		VariantID:  status.VariantID.String(),
		State:      string(status.State),
		Progress:   status.Progress,
		Error:      status.Err,
		DurationMs: status.DurationMs,
		Bytes:      status.Bytes,
	}
}

// variantIDs collects the ids of a rendition list.
func variantIDs(variants []store.Variant) []uuid.UUID {
	ids := make([]uuid.UUID, len(variants))
	for i := range variants {
		ids[i] = variants[i].ID
	}
	return ids
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
