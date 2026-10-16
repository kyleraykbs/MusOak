//go:build e2e

package provider

import (
	"context"
	"errors"
	"strings"
)

// The second fake: a source that can be searched and nothing else.
//
// A song whose only rendition cannot be served is a state the product has - a
// playlist imported from a service buys names, not files - and the browser
// suite needs one to drive the dialogue that asks what the song is. This
// provider answers a query that says "ghost" with a hit nobody can fetch, which
// is exactly that state: a canonical track with nothing playable behind it.
type ghostProvider struct{}

// GhostProviderName is what the second fake registers under. It is deliberately
// not a provider a released binary has, and a test that searches for it is
// testing the no-source state itself.
const GhostProviderName = "e2e-ghost"

// GhostTitle is the hit's title: searchable, and findable in the DOM. It says
// "e2e" so the slow fake answers the same query - which is what gives the
// dialogue something playable to offer for it.
const GhostTitle = "E2E Ghost Tone"

// GhostTrackID is the one provider track id this fake answers with.
const GhostTrackID = "e2e-ghost-1"

// GhostDurationMs is deliberately nowhere near the slow fake's 30 s: matching
// refuses two renditions whose lengths differ by more than five seconds, so a
// song of this length can never be silently attached to that one. The point of
// the fake is a song nothing can be found for, and a resolution that quietly
// succeeds would hide the state being tested.
const GhostDurationMs = 280_000

func (ghostProvider) Name() string { return GhostProviderName }

// Capabilities: it searches. Nothing else - a source that cannot be fetched is
// the point of it.
func (ghostProvider) Capabilities() Caps { return Caps{Search: true} }

// Search answers a query that says "ghost" and nothing else, so the fake never
// turns up in a search the suite is about something else.
func (ghostProvider) Search(ctx context.Context, q string, opts SearchOpts) ([]Track, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !strings.Contains(strings.ToLower(q), "ghost") {
		return nil, nil
	}
	return []Track{{
		ProviderTrackID: GhostTrackID,
		Title:           GhostTitle,
		Artists:         []string{E2EArtist},
		DurationMs:      GhostDurationMs,
	}}, nil
}

// Download is never called: the capability set has no Download, and this is the
// refusal a caller would get if it asked anyway.
func (ghostProvider) Download(context.Context, string, string, ProgressFunc) error {
	return errors.New("e2e-ghost: this source cannot be fetched")
}
