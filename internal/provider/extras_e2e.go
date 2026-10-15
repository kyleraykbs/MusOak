//go:build e2e

package provider

// Extras returns the providers a test build has and a released one does not.
//
// The browser suite needs a download it can watch: a provider that fetches
// nothing but takes seconds and reports its own progress, so a test can assert
// that a bar moves rather than that a fake status struct holds the right
// number. Build with -tags e2e to get it; the plain binary has none of it.
func Extras() []Provider { return []Provider{NewE2E()} }
