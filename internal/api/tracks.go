package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/prismusic/internal/match"
	"codeberg.org/kyleraykbs/prismusic/internal/store"
)

// trackResponse is the canonical track as clients see it.
type trackResponse struct {
	ID         string    `json:"id"`
	Title      string    `json:"title"`
	DurationMs int64     `json:"durationMs"`
	Artists    []string  `json:"artists"`
	Albums     []string  `json:"albums"`
	CreatedAt  time.Time `json:"createdAt"`
	// ArtworkURL is a path on this server, empty when nothing is known yet.
	ArtworkURL string `json:"artworkUrl,omitempty"`
}

// variantResponse is one provider's rendition of a track, with its download
// state so a client knows whether it can play it right now.
type variantResponse struct {
	ID              string              `json:"id"`
	TrackID         string              `json:"trackId"`
	Provider        string              `json:"provider"`
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
	writeJSON(w, http.StatusOK, response)
}

// handleTrackVariants lists every provider rendition of a track.
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
	writeJSON(w, http.StatusOK, map[string]any{"variants": s.buildVariants(r.Context(), variants)})
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
