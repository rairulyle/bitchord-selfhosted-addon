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
		"lowercases":          {"Tum Hi Ho", []string{"tum", "hi", "ho"}},
		"strips accents":      {"Beyoncé Bublé", []string{"beyonce", "buble"}},
		"punctuation splits":  {"AC/DC — Back In Black!", []string{"ac", "dc", "back", "in", "black"}},
		"apostrophes vanish":  {"Don't Stop Believin’", []string{"dont", "stop", "believin"}},
		"brackets":            {"Song (feat. Someone) [Live]", []string{"song", "feat", "someone", "live"}},
		"compatibility forms": {"ﬁre Ｆｕｌｌ", []string{"fire", "full"}},
		"digits kept":         {"1979 - 2011 Remaster", []string{"1979", "2011", "remaster"}},
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
	for _, in := range []string{"夜に駆ける", "पानी"} {
		got := Tokens(in)
		if len(got) != 1 || got[0] == "" {
			t.Errorf("Tokens(%q) = %q, want one non-empty token", in, got)
		}
	}
}
