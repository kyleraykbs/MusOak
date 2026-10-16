//go:build e2e

package provider

// Extras returns the providers a test build has and a released one does not.
//
// The browser suite needs a download it can watch: a provider that fetches
// nothing but takes seconds and reports its own progress, so a test can assert
// that a bar moves rather than that a fake status struct holds the right
// number. Build with -tags e2e to get it; the plain binary has none of it.
//
// It also needs a song with nothing playable behind it - the state a playlist
// imported from a service leaves when no file can be found for a name - which
// is what the search-only fake answers with. Same rule: a test build has both,
// a released binary has neither.
func Extras() []Provider { return []Provider{NewE2E(), ghostProvider{}} }
