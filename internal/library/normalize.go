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
