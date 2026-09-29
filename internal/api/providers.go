package api

import (
	"net/http"
	"sort"
)

type providerResponse struct {
	Name         string       `json:"name"`
	Capabilities providerCaps `json:"capabilities"`
	MissingDeps  []string     `json:"missingDeps,omitempty"`
}

type providerCaps struct {
	Search   bool `json:"search"`
	Download bool `json:"download"`
}

// handleProviders lists the enabled providers and what they can do. Clients
// use the capability flags to know which providers are worth ranking.
func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	missing := s.providers.MissingDeps()

	out := make([]providerResponse, 0)
	for _, p := range s.providers.All() {
		caps := p.Capabilities()
		entry := providerResponse{
			Name:         p.Name(),
			Capabilities: providerCaps{Search: caps.Search, Download: caps.Download},
		}
		if deps := missing[p.Name()]; len(deps) > 0 {
			entry.MissingDeps = append([]string(nil), deps...)
			sort.Strings(entry.MissingDeps)
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": out})
}
