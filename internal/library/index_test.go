package library

import (
	"reflect"
	"testing"
)

func fixtureIndex() *Index {
	return NewIndex([]Track{
		{ID: "1", Title: "Paniyon Sa", Artist: "Atif Aslam, Tulsi Kumar", AlbumArtist: "Various Artists", Album: "Satyameva Jayate"},
		{ID: "2", Title: "Tum Hi Ho", Artist: "Arijit Singh", AlbumArtist: "Arijit Singh", Album: "Aashiqui 2"},
		{ID: "3", Title: "Tum Hi Ho (Live)", Artist: "Arijit Singh", AlbumArtist: "Arijit Singh", Album: "MTV Unplugged"},
		{ID: "4", Title: "Home", Artist: "Michael Bublé", AlbumArtist: "Michael Bublé", Album: "It's Time"},
		{ID: "5", Title: "Party On My Mind", Artist: "KK, Shefali Alvares", AlbumArtist: "Various Artists", Album: "Race 2"},
		{ID: "6", Title: "Back In Black", Artist: "AC/DC", AlbumArtist: "AC/DC", Album: "Back In Black"},
		{ID: "7", Title: "Tum", Artist: "Someone Else", AlbumArtist: "Someone Else", Album: "Zed"},
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
		"title and artist in one string":   {"Paniyon Sa Atif Aslam", 50, []string{"1"}},
		"title alone":                      {"paniyon sa", 50, []string{"1"}},
		"album artist is searchable":       {"paniyon sa various artists", 50, []string{"1"}},
		"album is searchable":              {"home its time", 50, []string{"4"}},
		"accents ignored":                  {"michael buble home", 50, []string{"4"}},
		"shorter title ranks first":        {"tum hi ho", 50, []string{"2", "3"}},
		"title hits outrank other fields":  {"tum", 50, []string{"7", "2", "3"}},
		"fallback when artist not in tags": {"party on my mind pritam", 50, []string{"5"}},
		"one word titles skip fallback":    {"home pritam", 50, []string{}},
		"strict before fallback":           {"tum hi ho live", 50, []string{"3", "2"}},
		"limit applies":                    {"tum hi ho", 1, []string{"2"}},
		"repeated words":                   {"back in black back", 50, []string{"6"}},
		"no match":                         {"zzz", 50, []string{}},
		"blank query":                      {"   ", 50, []string{}},
		"zero limit":                       {"tum", 0, []string{}},
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
	ix.Search("tum", 50)
	got := ids(ix.Search("tum hi ho", 50))
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
	if !ok || track.Title != "Back In Black" {
		t.Errorf("Get(6) = %+v, %v", track, ok)
	}
	if _, ok := ix.Get("999"); ok {
		t.Error("Get(999) should miss")
	}
}
