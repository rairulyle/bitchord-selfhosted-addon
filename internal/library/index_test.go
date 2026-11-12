package library

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
)

func fixtureIndex() *Index {
	return NewIndex([]media.Track{
		{ID: "1", Title: "emo girl", Artist: "Machine Gun Kelly & WILLOW", AlbumArtist: "mgk", Album: "mainstream sellout (life in pink deluxe)"},
		{ID: "2", Title: "One Step Closer", Artist: "Linkin Park", AlbumArtist: "Linkin Park", Album: "Hybrid Theory"},
		{ID: "3", Title: "One Step Closer (live)", Artist: "Linkin Park", AlbumArtist: "Linkin Park", Album: "Underground 4.0"},
		{ID: "4", Title: "Adventure", Artist: "[Alexandros]", AlbumArtist: "[Alexandros]", Album: "Where's My History"},
		{ID: "5", Title: "I Write Sins Not Tragedies", Artist: "Panic! at the Disco", AlbumArtist: "Panic! at the Disco", Album: "A Fever You Can’t Sweat Out"},
		{ID: "6", Title: "Dance, Dance Christa Päffgen", Artist: "Anberlin", AlbumArtist: "Anberlin", Album: "Never Take Friendship Personal"},
		{ID: "7", Title: "Closer", Artist: "Anberlin", AlbumArtist: "Anberlin", Album: "As You Found Me"},
	})
}

func ids(tracks []media.Track) []string {
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
		"prefix match incomplete word":     {"emo gi", 50, []string{"1"}},
		"noise word stripped":              {"adventure acoustic", 50, []string{"4"}},
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
	ix := NewIndex([]media.Track{
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

func TestFindReportsHowEachTierDid(t *testing.T) {
	ix := fixtureIndex()
	cases := map[string]struct {
		query            string
		limit            int
		strict, fallback int
		returned         int
	}{
		"strict only":         {"emo girl mgk", 50, 1, 0, 1},
		"strict and fallback": {"one step closer live", 50, 1, 1, 2},
		"fallback only":       {"i write sins not tragedies brendon urie", 50, 0, 1, 1},
		"counts ignore limit": {"closer", 1, 3, 0, 1},
		"nothing":             {"zzz", 50, 0, 0, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := ix.Find(tc.query, tc.limit)
			if got.Strict != tc.strict || got.Fallback != tc.fallback || len(got.Tracks) != tc.returned {
				t.Fatalf("Find(%q) = strict %d, fallback %d, returned %d", tc.query, got.Strict, got.Fallback, len(got.Tracks))
			}
		})
	}
}

func TestSearchPrefixAndNoiseWords(t *testing.T) {
	ix := NewIndex([]media.Track{
		{ID: "1", Title: "On Kanna Vachi", Artist: "Saurav Srisan", Album: "On Kanna Vachi"},
		{ID: "2", Title: "Lose Control", Artist: "Teddy Swims", Album: "I've Tried Everything But Therapy"},
		{ID: "3", Title: "In the End", Artist: "Linkin Park", Album: "Hybrid Theory"},
	})

	// 1. Prefix match: "on kann" matches "On Kanna Vachi"
	if got := ids(ix.Search("on kann", 10)); !reflect.DeepEqual(got, []string{"1"}) {
		t.Errorf("prefix search 'on kann' = %v, want [1]", got)
	}

	// 2. Prefix match on single incomplete word
	if got := ids(ix.Search("teddy swi", 10)); !reflect.DeepEqual(got, []string{"2"}) {
		t.Errorf("prefix search 'teddy swi' = %v, want [2]", got)
	}

	// 3. Noise word stripping: "Lose Control (Live)" -> matches "Lose Control"
	if got := ids(ix.Search("teddy swims lose control live", 10)); !reflect.DeepEqual(got, []string{"2"}) {
		t.Errorf("noise stripping search = %v, want [2]", got)
	}

	// 4. Year + Remaster stripping: "In the End 2020 Remaster" -> matches "In the End"
	if got := ids(ix.Search("in the end 2020 remaster", 10)); !reflect.DeepEqual(got, []string{"3"}) {
		t.Errorf("year+remaster stripping search = %v, want [3]", got)
	}
}

func TestPrefixRankingAndExpansionCap(t *testing.T) {
	// 1. Title prefix match outranks artist/album prefix match, exact title match outranks prefix
	ix := NewIndex([]media.Track{
		{ID: "1", Title: "Something Else", Artist: "Kannada Singer", Album: "Great Hits"},
		{ID: "2", Title: "Kanna Love", Artist: "Unknown", Album: "Various"},
		{ID: "3", Title: "Kann", Artist: "Exact Matcher", Album: "Short Title"},
	})

	// When searching "kann", ID 3 (exact title hit) outranks ID 2 (prefix title hit) and ID 1 (artist hit)
	res := ids(ix.Search("kann", 10))
	if len(res) == 0 || res[0] != "3" {
		t.Fatalf("search 'kann' top result = %v, want ID 3", res)
	}

	// When searching "kan", ID 3 and ID 2 (prefix in title) outrank ID 1 (prefix only in artist)
	resPrefix := ids(ix.Search("kan", 10))
	if !reflect.DeepEqual(resPrefix, []string{"3", "2", "1"}) {
		t.Fatalf("search 'kan' results = %v, want [3 2 1]", resPrefix)
	}

	// 2. Expansion cap test: create 40 tracks each with a different prefix word (theme00, theme01, ..., theme39)
	var manyTracks []media.Track
	for i := 0; i < 40; i++ {
		manyTracks = append(manyTracks, media.Track{
			ID:    fmt.Sprintf("t%02d", i),
			Title: fmt.Sprintf("Theme%02d", i),
		})
	}
	ixMany := NewIndex(manyTracks)
	gotMany := ixMany.Search("them", 50)
	if len(gotMany) > 30 {
		t.Errorf("prefix search expanded %d tracks, want <= 30 (capped)", len(gotMany))
	}
}

func TestVideoSearchBehaviors(t *testing.T) {
	ix := NewIndex([]media.Track{
		{ID: "t1", Title: "SWEET / I THOUGHT YOU WANTED TO DANCE", Artist: "Tyler, The Creator", Album: "Call Me If You Get Lost"},
		{ID: "t2", Title: "Sweet Life", Artist: "Frank Ocean", Album: "Channel Orange"},
		{ID: "t3", Title: "Sweet", Artist: "Cigarettes After Sex", Album: "Cigarettes After Sex"},
		{ID: "t4", Title: "Better Together", Artist: "Jack Johnson", Album: "In Between Dreams"},
		{ID: "t5", Title: "Where This Flower Blooms", Artist: "Tyler, The Creator feat. Frank Ocean", Album: "Flower Boy"},
		{ID: "t6", Title: "Amsham", Artist: "Aksomaniac", Album: "Amsham"},
	})

	// 1. Partial/substring "wee" matches Sweet, Sweet Life, SWEET..., and In Between Dreams
	resWee := ix.Find("wee", 10)
	weeIDs := ids(resWee.Tracks)
	if len(weeIDs) < 4 {
		t.Fatalf("expected at least 4 tracks for 'wee', got %v", weeIDs)
	}
	// "Sweet" and "Sweet Life" should be near top
	if weeIDs[0] != "t3" && weeIDs[0] != "t2" && weeIDs[0] != "t1" {
		t.Errorf("expected a Sweet song on top, got %s", weeIDs[0])
	}
	// Album match check: "In Between Dreams" should be in Albums
	foundAlbum := false
	for _, alb := range resWee.Albums {
		if alb.Name == "In Between Dreams" {
			foundAlbum = true
			break
		}
	}
	if !foundAlbum {
		t.Errorf("expected 'In Between Dreams' in Albums for query 'wee'")
	}

	// 2. Multi-word prefix "frank oce" matches Frank Ocean tracks & artist
	resFrank := ix.Find("frank oce", 10)
	frankIDs := ids(resFrank.Tracks)
	if len(frankIDs) < 2 {
		t.Fatalf("expected Frank Ocean tracks, got %v", frankIDs)
	}
	foundArtist := false
	for _, art := range resFrank.Artists {
		if art.Name == "Frank Ocean" {
			foundArtist = true
			break
		}
	}
	if !foundArtist {
		t.Errorf("expected 'Frank Ocean' in Artists for query 'frank oce'")
	}

	// 3. Partial word "amsh" matches Amsham
	resAmsh := ix.Find("amsh", 10)
	amshIDs := ids(resAmsh.Tracks)
	if len(amshIDs) == 0 || amshIDs[0] != "t6" {
		t.Fatalf("expected 'Amsham' (t6) on top for 'amsh', got %v", amshIDs)
	}
}

func TestLoseAndLoserPrefixAndEntityIsolation(t *testing.T) {
	ix := NewIndex([]media.Track{
		{ID: "t1", Title: "Loser", Artist: "Tame Impala", Album: "Deadbeat"},
		{ID: "t2", Title: "Lose My Mind (feat. Doja Cat)", Artist: "Don Toliver", Album: "Lose My Mind"},
	})

	// 1. Searching "lose" should match BOTH "Lose My Mind" AND "Loser" (prefix of loser)
	resLose := ix.Find("lose", 10)
	loseIDs := ids(resLose.Tracks)
	if len(loseIDs) != 2 {
		t.Fatalf("expected both tracks for 'lose', got %v", loseIDs)
	}

	// For "lose", album "Lose My Mind" should match, but album "Deadbeat" must NOT match
	for _, alb := range resLose.Albums {
		if alb.Name == "Deadbeat" {
			t.Errorf("album 'Deadbeat' should not match query 'lose'")
		}
	}
	// For "lose", artist "Don Toliver" or "Tame Impala" must NOT match since neither has "lose"
	for _, art := range resLose.Artists {
		if art.Name == "Tame Impala" || art.Name == "Don Toliver" {
			t.Errorf("artist '%s' should not match query 'lose'", art.Name)
		}
	}

	// Searching "loser" matches "Loser", but not "Deadbeat" album or "Tame Impala" artist
	resLoser := ix.Find("loser", 10)
	if len(resLoser.Tracks) != 1 || resLoser.Tracks[0].ID != "t1" {
		t.Fatalf("expected 'Loser' (t1) for 'loser', got %v", ids(resLoser.Tracks))
	}
	if len(resLoser.Albums) != 0 {
		t.Errorf("expected 0 albums matching 'loser', got %v", resLoser.Albums)
	}
	if len(resLoser.Artists) != 0 {
		t.Errorf("expected 0 artists matching 'loser', got %v", resLoser.Artists)
	}

	// Searching "deadbeat" matches album "Deadbeat"
	resDeadbeat := ix.Find("deadbeat", 10)
	if len(resDeadbeat.Albums) != 1 || resDeadbeat.Albums[0].Name != "Deadbeat" {
		t.Errorf("expected album 'Deadbeat' for query 'deadbeat', got %v", resDeadbeat.Albums)
	}

	// Searching "tame" matches artist "Tame Impala"
	resTame := ix.Find("tame", 10)
	if len(resTame.Artists) != 1 || resTame.Artists[0].Name != "Tame Impala" {
		t.Errorf("expected artist 'Tame Impala' for query 'tame', got %v", resTame.Artists)
	}
}

func TestSplitArtists(t *testing.T) {
	cases := []struct {
		input string
		want  []string
	}{
		{"Metro Boomin; The Weeknd; 21 Savage", []string{"Metro Boomin", "The Weeknd", "21 Savage"}},
		{"The Weeknd • Gesaffelstein", []string{"The Weeknd", "Gesaffelstein"}},
		{"The Weeknd • JENNIE • Lily-Rose Depp", []string{"The Weeknd", "JENNIE", "Lily-Rose Depp"}},
		{"The Weeknd; Anitta", []string{"The Weeknd", "Anitta"}},
		{"Artist A / Artist B", []string{"Artist A", "Artist B"}},
		{"AC/DC", []string{"AC/DC"}},
		{"Tyler, The Creator", []string{"Tyler, The Creator"}},
		{"Earth, Wind & Fire", []string{"Earth, Wind & Fire"}},
		{"", nil},
	}

	for _, tc := range cases {
		got := SplitArtists(tc.input)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("SplitArtists(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestFormatArtists(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"Metro Boomin; The Weeknd; 21 Savage", "Metro Boomin • The Weeknd • 21 Savage"},
		{"The Weeknd • Gesaffelstein", "The Weeknd • Gesaffelstein"},
		{"The Weeknd; Anitta", "The Weeknd • Anitta"},
		{"The Weeknd", "The Weeknd"},
		{"Tyler, The Creator", "Tyler, The Creator"},
		{"", ""},
	}

	for _, tc := range cases {
		got := FormatArtists(tc.input)
		if got != tc.want {
			t.Errorf("FormatArtists(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestMultiArtistGroupingInIndex(t *testing.T) {
	tracks := []media.Track{
		{ID: "1", Title: "Starboy", Artist: "The Weeknd", Album: "Starboy"},
		{ID: "2", Title: "Creepin'", Artist: "Metro Boomin; The Weeknd; 21 Savage", Album: "Heroes & Villains"},
		{ID: "3", Title: "Lost in the Fire", Artist: "The Weeknd • Gesaffelstein", Album: "Hyperion"},
		{ID: "4", Title: "Popular", Artist: "The Weeknd • Playboi Carti • Madonna", Album: "The Idol"},
		{ID: "5", Title: "São Paulo", Artist: "The Weeknd; Anitta", Album: "Hurry Up Tomorrow"},
	}

	ix := NewIndex(tracks)

	// Verify multi-artist tracks consolidate into a single artist entry.
	res := ix.Find("weeknd", 10)
	if len(res.Artists) == 0 {
		t.Fatalf("expected to find artist for 'weeknd'")
	}
	var weeknd *ArtistMatch
	for i := range res.Artists {
		if res.Artists[i].Name == "The Weeknd" {
			weeknd = &res.Artists[i]
			break
		}
	}
	if weeknd == nil {
		t.Fatalf("expected 'The Weeknd' in artists, got: %+v", res.Artists)
	}
	if weeknd.TrackCount != 5 {
		t.Errorf("expected The Weeknd to have 5 tracks, got %d", weeknd.TrackCount)
	}

	// Make sure combo strings like "The Weeknd • Gesaffelstein" are NOT in the artists list
	for _, art := range res.Artists {
		if strings.Contains(art.Name, "•") || strings.Contains(art.Name, ";") {
			t.Errorf("found un-split combo artist in index: %s", art.Name)
		}
	}

	// Searching "anitta" should find "Anitta"
	resAnitta := ix.Find("anitta", 10)
	if len(resAnitta.Artists) != 1 || resAnitta.Artists[0].Name != "Anitta" {
		t.Errorf("expected 'Anitta' for query 'anitta', got %+v", resAnitta.Artists)
	}

	// Searching "gesaffelstein" should find "Gesaffelstein"
	resGes := ix.Find("gesaffelstein", 10)
	if len(resGes.Artists) != 1 || resGes.Artists[0].Name != "Gesaffelstein" {
		t.Errorf("expected 'Gesaffelstein' for query 'gesaffelstein', got %+v", resGes.Artists)
	}
}

func TestDamerauLevenshtein(t *testing.T) {
	cases := []struct {
		a, b    string
		maxDist int
		want    int
	}{
		{"", "", 2, 0},
		{"a", "", 2, 1},
		{"", "b", 2, 1},
		{"coldplay", "coldplay", 2, 0},
		{"coldpay", "coldplay", 2, 1},     // omission
		{"coldplayy", "coldplay", 2, 1},   // insertion
		{"koldplay", "coldplay", 2, 1},    // substitution
		{"clodplay", "coldplay", 2, 1},    // transposition
		{"bohemain", "bohemian", 2, 1},    // transposition
		{"completely", "different", 1, 2}, // exceeded maxDist
	}
	for _, tc := range cases {
		got := damerauLevenshtein(tc.a, tc.b, tc.maxDist)
		if got != tc.want {
			t.Errorf("damerauLevenshtein(%q, %q, %d) = %d, want %d", tc.a, tc.b, tc.maxDist, got, tc.want)
		}
	}
}

func TestSynonymExpansion(t *testing.T) {
	ix := NewIndex([]media.Track{
		{ID: "1", Title: "Star Wars: The Empire Strikes Back (Original Soundtrack)", Artist: "John Williams", Album: "Original Soundtrack"},
		{ID: "2", Title: "Cyberpunk 2077 OST", Artist: "Marcin Przybyłowicz", Album: "Cyberpunk 2077 OST"},
		{ID: "3", Title: "Guardians of the Galaxy: Awesome Mix Vol. 1", Artist: "Various Artists", Album: "Awesome Mix Vol. 1"},
		{ID: "4", Title: "Classical Favorites Volume 2", Artist: "London Philharmonic", Album: "Classical Favorites Volume 2"},
		{ID: "5", Title: "Shine On You Crazy Diamond (Pts. 1-5)", Artist: "Pink Floyd", Album: "Wish You Were Here"},
		{ID: "6", Title: "Another Brick in the Wall (Part 2)", Artist: "Pink Floyd", Album: "The Wall"},
		{ID: "7", Title: "Godzilla vs Kong", Artist: "Junkie XL", Album: "Battle of the Titans"},
	})

	// 1. "ost" matches "Original Soundtrack"
	ostRes := ix.Find("star wars ost", 10)
	if len(ostRes.Tracks) == 0 || ostRes.Tracks[0].ID != "1" {
		t.Fatalf("search 'star wars ost' want ID 1, got %v", ids(ostRes.Tracks))
	}

	// 2. "soundtrack" matches "Cyberpunk 2077 OST"
	stRes := ix.Find("cyberpunk soundtrack", 10)
	if len(stRes.Tracks) == 0 || stRes.Tracks[0].ID != "2" {
		t.Fatalf("search 'cyberpunk soundtrack' want ID 2, got %v", ids(stRes.Tracks))
	}

	// 3. "volume 1" matches "Awesome Mix Vol. 1"
	volRes := ix.Find("awesome mix volume 1", 10)
	if len(volRes.Tracks) == 0 || volRes.Tracks[0].ID != "3" {
		t.Fatalf("search 'awesome mix volume 1' want ID 3, got %v", ids(volRes.Tracks))
	}

	// 4. "vol 2" matches "Volume 2"
	vol2Res := ix.Find("classical favorites vol 2", 10)
	if len(vol2Res.Tracks) == 0 || vol2Res.Tracks[0].ID != "4" {
		t.Fatalf("search 'classical favorites vol 2' want ID 4, got %v", ids(vol2Res.Tracks))
	}

	// 5. "part 1" matches "Pts. 1-5"
	ptRes := ix.Find("crazy diamond part 1", 10)
	if len(ptRes.Tracks) == 0 || ptRes.Tracks[0].ID != "5" {
		t.Fatalf("search 'crazy diamond part 1' want ID 5, got %v", ids(ptRes.Tracks))
	}

	// 6. "pt 2" matches "Part 2"
	pt2Res := ix.Find("another brick pt 2", 10)
	if len(pt2Res.Tracks) == 0 || pt2Res.Tracks[0].ID != "6" {
		t.Fatalf("search 'another brick pt 2' want ID 6, got %v", ids(pt2Res.Tracks))
	}

	// 7. "versus" matches "vs"
	vsRes := ix.Find("godzilla versus kong", 10)
	if len(vsRes.Tracks) == 0 || vsRes.Tracks[0].ID != "7" {
		t.Fatalf("search 'godzilla versus kong' want ID 7, got %v", ids(vsRes.Tracks))
	}
}

func TestFuzzyTypoSearch(t *testing.T) {
	ix := NewIndex([]media.Track{
		{ID: "1", Title: "Yellow", Artist: "Coldplay", Album: "Parachutes"},
		{ID: "2", Title: "Creep", Artist: "Radiohead", Album: "Pablo Honey"},
		{ID: "3", Title: "Bohemian Rhapsody", Artist: "Queen", Album: "A Night at the Opera"},
		{ID: "4", Title: "HUMBLE.", Artist: "Kendrick Lamar", Album: "DAMN."},
	})

	// 1. Single token typo: "coldpay" -> finds "Coldplay" track & artist
	res1 := ix.Find("coldpay", 10)
	if len(res1.Tracks) == 0 || res1.Tracks[0].ID != "1" {
		t.Fatalf("search 'coldpay' want track ID 1, got %v", ids(res1.Tracks))
	}
	if len(res1.Artists) == 0 || res1.Artists[0].Name != "Coldplay" {
		t.Fatalf("search 'coldpay' want artist Coldplay, got %+v", res1.Artists)
	}

	// 2. Multi-word typo: "radiohed creep" -> finds "Radiohead - Creep"
	res2 := ix.Find("radiohed creep", 10)
	if len(res2.Tracks) == 0 || res2.Tracks[0].ID != "2" {
		t.Fatalf("search 'radiohed creep' want track ID 2, got %v", ids(res2.Tracks))
	}

	// 3. Adjacent transposition: "bohemain" -> finds "Bohemian Rhapsody"
	res3 := ix.Find("bohemain", 10)
	if len(res3.Tracks) == 0 || res3.Tracks[0].ID != "3" {
		t.Fatalf("search 'bohemain' want track ID 3, got %v", ids(res3.Tracks))
	}

	// 4. Typo in artist name: "kendrick lamr" -> finds "HUMBLE."
	res4 := ix.Find("kendrick lamr", 10)
	if len(res4.Tracks) == 0 || res4.Tracks[0].ID != "4" {
		t.Fatalf("search 'kendrick lamr' want track ID 4, got %v", ids(res4.Tracks))
	}

	// 5. Typo in album search: "parachuts" -> finds "Parachutes" album
	res5 := ix.Find("parachuts", 10)
	foundAlb := false
	for _, alb := range res5.Albums {
		if alb.Name == "Parachutes" {
			foundAlb = true
			break
		}
	}
	if !foundAlb {
		t.Fatalf("search 'parachuts' want album Parachutes, got %+v", res5.Albums)
	}

	// 6. Very short non-matching word does not trigger false positive
	res6 := ix.Find("zzz", 10)
	if len(res6.Tracks) != 0 {
		t.Fatalf("search 'zzz' should return 0 tracks, got %v", ids(res6.Tracks))
	}
}
