package match

import (
	"regexp"
	"strings"
	"unicode"
)

// The noise providers add to titles. Everything removed here describes the
// release, not the recording. Markers that do change the recording — live,
// remix, acoustic, instrumental, slowed, sped up — are deliberately kept, so
// those versions never merge with the studio cut.
var (
	noiseBrackets = regexp.MustCompile(`(?i)[\(\[]\s*(official\s*(music\s*)?(video|audio)|music\s*video|lyric\s*video|lyrics?|visuali[sz]er|audio|video|full\s*album|hd|hq|4k|explicit|clean|(\d{4}\s*)?remaster(ed)?(\s*\d{4})?)\s*[\)\]]`)
	noiseSuffix   = regexp.MustCompile(`(?i)\s*[-–—]\s*(official\s*(music\s*)?(video|audio)|music\s*video|lyric\s*video|lyrics?|visuali[sz]er|(\d{4}\s*)?remaster(ed)?(\s*\d{4})?|audio|video|hd|hq|4k)\s*$`)
	featBrackets  = regexp.MustCompile(`(?i)[\(\[]\s*(feat\.?|ft\.?|featuring|with|prod\.?)\s[^\)\]]*[\)\]]`)
	featInfix     = regexp.MustCompile(`(?i)\s+(feat\.?|ft\.?|featuring)\s+.*$`)
	topicSuffix   = regexp.MustCompile(`(?i)\s*[-–—]\s*topic\s*$`)

	// YouTube Music appends a romanisation to a title that is not written in
	// Latin letters - "トーキョーレギー - Tokyo Reggie" - which is a second
	// spelling of the same name, not part of it.
	romanisation = regexp.MustCompile(`\s*[-–—]\s*[\p{Latin}\p{N}\s]+$`)
	latinLetter  = regexp.MustCompile(`\p{Latin}`)
)

// NormalizeTitle reduces a title to its comparison key.
func NormalizeTitle(s string) string {
	s = strings.ToLower(s)
	s = noiseBrackets.ReplaceAllString(s, " ")
	s = noiseSuffix.ReplaceAllString(s, " ")
	s = featBrackets.ReplaceAllString(s, " ")
	s = featInfix.ReplaceAllString(s, " ")
	s = dropRomanisation(s)
	return collapse(s)
}

// dropRomanisation removes a trailing romanisation from a title that has none
// of its own Latin letters, so a Japanese song found on YouTube Music compares
// equal to the same song from somewhere that spells it only once. A title that
// is already written in Latin letters is left alone: "Song - Live at Wembley"
// says which recording it is, and stripping that would merge versions that are
// genuinely different.
func dropRomanisation(s string) string {
	match := romanisation.FindStringIndex(s)
	if match == nil {
		return s
	}
	head := strings.TrimSpace(s[:match[0]])
	if head == "" || latinLetter.MatchString(head) {
		return s
	}
	return head
}

// NormalizeArtist reduces an artist name to its comparison key. "&" and "and"
// collapse to the same thing because punctuation becomes a separator.
func NormalizeArtist(s string) string {
	return collapse(topicSuffix.ReplaceAllString(strings.ToLower(s), ""))
}

// collapse lowercases away punctuation: every run of non-letter, non-digit
// runes becomes a single space.
func collapse(s string) string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	return strings.Join(fields, " ")
}

// tokens splits a collapsed key into its words.
func tokens(key string) map[string]bool {
	out := make(map[string]bool)
	for _, word := range strings.Fields(key) {
		out[word] = true
	}
	return out
}
