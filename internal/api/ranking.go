package api

import (
	"errors"
	"net/http"

	"codeberg.org/kyleraykbs/prismusic/internal/ranking"
)

type rankingResponse struct {
	// Ranking is what the user submitted, empty when they never ranked.
	Ranking []string `json:"ranking"`
	// Effective is what actually decides variant selection for this user.
	Effective []string `json:"effective"`
	// Aggregate is the mean order across all ranked users.
	Aggregate []string `json:"aggregate"`
	// Default is the configured fallback order.
	Default []string `json:"default"`
}

// handleRankingGet reports the caller's ranking, the aggregate and the default.
func (s *Server) handleRankingGet(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	own, err := s.store.Ranking(r.Context(), user.ID)
	if err != nil {
		writeStoreError(w, err, "ranking unavailable")
		return
	}
	effective, err := s.ranking.User(r.Context(), user.ID)
	if err != nil {
		writeStoreError(w, err, "ranking unavailable")
		return
	}
	aggregate, err := s.ranking.Aggregate(r.Context())
	if err != nil {
		writeStoreError(w, err, "ranking unavailable")
		return
	}
	writeJSON(w, http.StatusOK, rankingResponse{
		Ranking:   nonNilStrings(own),
		Effective: nonNilStrings(effective),
		Aggregate: nonNilStrings(aggregate),
		Default:   nonNilStrings(s.cfg.DefaultProviderOrder),
	})
}

type rankingRequest struct {
	Ranking []string `json:"ranking"`
}

// handleRankingPut replaces the caller's provider order.
func (s *Server) handleRankingPut(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	var req rankingRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.ranking.Set(r.Context(), user.ID, req.Ranking); err != nil {
		if errors.Is(err, ranking.ErrInvalidRanking) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeStoreError(w, err, "could not store ranking")
		return
	}
	effective, err := s.ranking.User(r.Context(), user.ID)
	if err != nil {
		writeStoreError(w, err, "ranking unavailable")
		return
	}
	aggregate, err := s.ranking.Aggregate(r.Context())
	if err != nil {
		writeStoreError(w, err, "ranking unavailable")
		return
	}
	writeJSON(w, http.StatusOK, rankingResponse{
		Ranking:   nonNilStrings(req.Ranking),
		Effective: nonNilStrings(effective),
		Aggregate: nonNilStrings(aggregate),
		Default:   nonNilStrings(s.cfg.DefaultProviderOrder),
	})
}

func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
