package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
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

// providerNames reads the optional "providers" query parameter: a
// comma-separated list of enabled provider names, e.g. ?providers=ytmusic,spotify.
// Absent or empty means every enabled provider. A name that is not an enabled
// provider is an error naming it, which callers turn into a 400.
func (s *Server) providerNames(r *http.Request) ([]string, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("providers"))
	if raw == "" {
		return nil, nil
	}
	names := make([]string, 0, strings.Count(raw, ",")+1)
	for _, part := range strings.Split(raw, ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		if !s.providerKnown(name) {
			return nil, fmt.Errorf("unknown provider %s", name)
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil, nil
	}
	return names, nil
}
