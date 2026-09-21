package library

import (
	"reflect"
	"testing"
)

func TestTokens(t *testing.T) {
	cases := map[string]struct {
		in   string
		want []string
	}{
		"lowercases":          {"One Step Closer", []string{"one", "step", "closer"}},
		"strips accents":      {"Päffgen Blasé", []string{"paffgen", "blase"}},
		"punctuation splits":  {"Panic! at the Disco — Death of a Bachelor", []string{"panic", "at", "the", "disco", "death", "of", "a", "bachelor"}},
		"apostrophes vanish":  {"Don't Panic: It’s Longer Now!", []string{"dont", "panic", "its", "longer", "now"}},
		"brackets":            {"[Alexandros] spit! (live)", []string{"alexandros", "spit", "live"}},
		"compatibility forms": {"ＯＮＥ ＯＫ ＲＯＣＫ ﬁre", []string{"one", "ok", "rock", "fire"}},
		"digits kept":         {"Punk Goes 90’s, Volume 2", []string{"punk", "goes", "90s", "volume", "2"}},
		"empty":               {"", []string{}},
		"only punctuation":    {" -- !! ", []string{}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := Tokens(tc.in)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Tokens(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestTokensKeepNonLatinWordsWhole(t *testing.T) {
	for _, in := range []string{"夜に駆ける", "ショコラカタブラ"} {
		got := Tokens(in)
		if len(got) != 1 || got[0] == "" {
			t.Errorf("Tokens(%q) = %q, want one non-empty token", in, got)
		}
	}
}
