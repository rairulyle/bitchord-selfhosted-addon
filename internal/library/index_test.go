package library

import (
	"reflect"
	"testing"
)

func fixtureIndex() *Index {
	return NewIndex([]Track{
		{ID: "1", Title: "emo girl", Artist: "Machine Gun Kelly & WILLOW", AlbumArtist: "mgk", Album: "mainstream sellout (life in pink deluxe)"},
		{ID: "2", Title: "One Step Closer", Artist: "Linkin Park", AlbumArtist: "Linkin Park", Album: "Hybrid Theory"},
		{ID: "3", Title: "One Step Closer (live)", Artist: "Linkin Park", AlbumArtist: "Linkin Park", Album: "Underground 4.0"},
		{ID: "4", Title: "Adventure", Artist: "[Alexandros]", AlbumArtist: "[Alexandros]", Album: "Where's My History"},
		{ID: "5", Title: "I Write Sins Not Tragedies", Artist: "Panic! at the Disco", AlbumArtist: "Panic! at the Disco", Album: "A Fever You Can’t Sweat Out"},
		{ID: "6", Title: "Dance, Dance Christa Päffgen", Artist: "Anberlin", AlbumArtist: "Anberlin", Album: "Never Take Friendship Personal"},
		{ID: "7", Title: "Closer", Artist: "Anberlin", AlbumArtist: "Anberlin", Album: "As You Found Me"},
	})
}

func ids(tracks []Track) []string {
	out := make([]string, len(tracks))
	for i, t := range tracks {
		out[i] = t.ID
	}
	return out
}

func TestSearch(t *testing.T) {
	cases := map[string]struct {
		query string
		limit int
		want  []string
	}{
		"title and artist in one string":   {"emo girl Machine Gun Kelly", 50, []string{"1"}},
		"title alone":                      {"emo girl", 50, []string{"1"}},
		"album artist is searchable":       {"emo girl mgk", 50, []string{"1"}},
		"album is searchable":              {"adventure wheres my history", 50, []string{"4"}},
		"accents ignored":                  {"anberlin dance dance christa paffgen", 50, []string{"6"}},
		"shorter title ranks first":        {"one step closer", 50, []string{"2", "3"}},
		"title hits outrank other fields":  {"closer", 50, []string{"7", "2", "3"}},
		"fallback when artist not in tags": {"i write sins not tragedies brendon urie", 50, []string{"5"}},
		"one word titles skip fallback":    {"adventure brendon urie", 50, []string{}},
		"strict before fallback":           {"one step closer live", 50, []string{"3", "2"}},
		"limit applies":                    {"one step closer", 1, []string{"2"}},
		"repeated words":                   {"sins not tragedies sins", 50, []string{"5"}},
		"no match":                         {"zzz", 50, []string{}},
		"blank query":                      {"   ", 50, []string{}},
		"zero limit":                       {"closer", 0, []string{}},
	}
	ix := fixtureIndex()
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := ids(ix.Search(tc.query, tc.limit))
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Search(%q) = %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}

func TestSearchDoesNotCorruptTheIndex(t *testing.T) {
	ix := fixtureIndex()
	ix.Search("closer", 50)
	got := ids(ix.Search("one step closer", 50))
	if !reflect.DeepEqual(got, []string{"2", "3"}) {
		t.Fatalf("second search = %v, want [2 3]", got)
	}
}

func TestGetAndLen(t *testing.T) {
	ix := fixtureIndex()
	if ix.Len() != 7 {
		t.Errorf("Len = %d", ix.Len())
	}
	track, ok := ix.Get("6")
	if !ok || track.Title != "Dance, Dance Christa Päffgen" {
		t.Errorf("Get(6) = %+v, %v", track, ok)
	}
	if _, ok := ix.Get("999"); ok {
		t.Error("Get(999) should miss")
	}
}

func TestSearchAnswersTheWordFormsBitChordSends(t *testing.T) {
	ix := NewIndex([]Track{
		{ID: "1", Title: "Time‐Bomb", Artist: "All Time Low", AlbumArtist: "All Time Low", Album: "Dirty Work"},
		{ID: "2", Title: "Naïve Orleans", Artist: "Anberlin", AlbumArtist: "Anberlin", Album: "Blueprints for the Black Market"},
		{ID: "3", Title: "11:11 PM", Artist: "The All‐American Rejects", AlbumArtist: "The All‐American Rejects", Album: "Move Along"},
		{ID: "4", Title: "ワンテンポ遅れたMonster ain't dead", Artist: "[Alexandros]", AlbumArtist: "[Alexandros]", Album: "ALXD"},
		{ID: "5", Title: "Weightless", Artist: "All Time Low", AlbumArtist: "All Time Low", Album: "Nothing Personal"},
	})
	cases := map[string]struct {
		query string
		want  []string
	}{
		"joined hyphenated title":            {"timebomb all time low", []string{"1"}},
		"joined title alone":                 {"timebomb", []string{"1"}},
		"spelled apart still works":          {"time bomb all time low", []string{"1"}},
		"accent deleted":                     {"nave orleans anberlin", []string{"2"}},
		"accent folded still works":          {"naive orleans", []string{"2"}},
		"digits joined across punctuation":   {"1111 pm the all‐american rejects", []string{"3"}},
		"latin words of a mixed script":      {"monster aint dead [alexandros]", []string{"4"}},
		"fallback counts only real words":    {"time bomb brendon urie", []string{"1"}},
		"a joined form alone is no fallback": {"timebomb brendon urie", []string{}},
		"title hit outranks an artist hit":   {"time", []string{"1", "5"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := ids(ix.Search(tc.query, 50))
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Search(%q) = %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}
