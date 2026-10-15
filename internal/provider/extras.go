//go:build !e2e

package provider

// Extras returns providers that only a test build has.
//
// The browser suite needs a download it can watch: a provider that fetches
// nothing but takes seconds and reports its own progress, so a test can assert
// that a bar moves rather than that a fake status struct holds the right
// number. It is behind the `e2e` build tag, and the released binary has none of
// it - a tested-seam-in-production is a seam a user can trip over.
func Extras() []Provider { return nil }
