package client

import (
	"context"
	"net/http"
)

// RadioResult is a generated station.
type RadioResult struct {
	Seed           Track           `json:"seed"`
	Providers      []string        `json:"providers"`
	Tracks         []Track         `json:"tracks"`
	ProviderErrors []ProviderError `json:"providerErrors,omitempty"`
	// Playlist is set when the station was saved.
	Playlist *Playlist `json:"playlist,omitempty"`
}

// RadioOptions tunes a station.
type RadioOptions struct {
	// Providers selects what the station is made of; empty means every enabled
	// provider that can build one.
	Providers []string
	// Length is how many tracks the station should hold; zero means the
	// server's default.
	Length int
	// Save stores the station as a playlist. It is a pointer so that "unset"
	// (the server default: save) is distinguishable from an explicit false.
	Save *bool
	// Name overrides the playlist name.
	Name string
}

// Radio starts a station from a seed track, mixing the providers it names.
func (c *Client) Radio(ctx context.Context, seedTrackID string, opts RadioOptions) (*RadioResult, error) {
	body := map[string]any{"seedTrackId": seedTrackID}
	if len(opts.Providers) > 0 {
		body["providers"] = opts.Providers
	}
	if opts.Length > 0 {
		body["length"] = opts.Length
	}
	if opts.Save != nil {
		body["save"] = *opts.Save
	}
	if opts.Name != "" {
		body["name"] = opts.Name
	}

	var out RadioResult
	if err := c.do(ctx, http.MethodPost, "/api/v1/radio", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Bool returns a pointer to v, for RadioOptions.Save.
func Bool(v bool) *bool { return &v }
