package api

import (
	"errors"
	"math"
	"net/http"

	"github.com/google/uuid"

	"github.com/kyleraykbs/MusOak/internal/store"
)

type favoriteRequest struct {
	TrackID string   `json:"trackId"`
	Rating  *float64 `json:"rating"`
}

func favoriteTenths(rating *float64) (int, error) {
	if rating == nil {
		return 0, nil
	}
	value := int(math.Round(*rating * 10))
	if value < 1 || value > 50 || math.Abs(*rating-float64(value)/10) > 1e-9 {
		return 0, errors.New("rating must be between 0.1 and 5.0 in tenths")
	}
	return value, nil
}

// handleFavoritesList returns the caller's favorited tracks.
func (s *Server) handleFavoritesList(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	ids, err := s.store.Favorites(r.Context(), user.ID)
	if err != nil {
		writeStoreError(w, err, "favorites unavailable")
		return
	}
	ratings, err := s.store.FavoriteRatings(r.Context(), user.ID)
	if err != nil {
		writeStoreError(w, err, "favorites unavailable")
		return
	}
	tracks := make([]trackResponse, 0, len(ids))
	for _, id := range ids {
		track, err := s.store.Track(r.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			writeStoreError(w, err, "favorites unavailable")
			return
		}
		response, err := s.buildTrack(r.Context(), *track)
		if err != nil {
			writeStoreError(w, err, "favorites unavailable")
			return
		}
		response.Rating = float64(ratings[id]) / 10
		tracks = append(tracks, response)
	}
	s.withPlays(r.Context(), user, trackRefs(tracks))
	writeJSON(w, http.StatusOK, map[string]any{"tracks": tracks})
}

// handleFavoriteAdd favorites a track, upserting its optional rating.
func (s *Server) handleFavoriteAdd(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	var req favoriteRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	trackID, err := uuid.Parse(req.TrackID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid trackId")
		return
	}
	if _, err := s.store.Track(r.Context(), trackID); err != nil {
		writeStoreError(w, err, "track not found")
		return
	}
	rating, err := favoriteTenths(req.Rating)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.AddFavorite(r.Context(), user.ID, trackID, rating); err != nil {
		writeStoreError(w, err, "could not add favorite")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleFavoriteRemove removes a track from the caller's favorites.
func (s *Server) handleFavoriteRemove(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	trackID, err := uuid.Parse(r.PathValue("trackId"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid track id")
		return
	}
	if err := s.store.RemoveFavorite(r.Context(), user.ID, trackID); err != nil {
		writeStoreError(w, err, "favorite not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
