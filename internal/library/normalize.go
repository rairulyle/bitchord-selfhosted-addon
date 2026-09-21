package library

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

func Tokens(s string) []string {
	var b strings.Builder
	for _, r := range norm.NFKD.String(s) {
		switch {
		case unicode.Is(unicode.M, r), r == '\'', r == '’', r == '‘', r == '`':
		case unicode.IsLetter(r), unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
		default:
			b.WriteByte(' ')
		}
	}
	return strings.Fields(b.String())
}

// bitchordWords spells s the way BitChord's TrackMatcher spells a title before
// it searches: split on spaces and dots, then everything outside a-z0-9 deleted
// from each word. "Time‐Bomb" is "timebomb" there, "Naïve" is "nave".
func bitchordWords(s string) []string {
	var words []string
	var word strings.Builder
	flush := func() {
		if word.Len() > 0 {
			words = append(words, word.String())
			word.Reset()
		}
	}
	composed := strings.ReplaceAll(strings.ToLower(norm.NFC.String(s)), "&", " and ")
	for _, r := range composed {
		switch {
		case unicode.IsSpace(r), r == '.', r == '·':
			flush()
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			word.WriteRune(r)
		}
	}
	flush()
	return words
}
