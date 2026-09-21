package library

import (
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

type Track struct {
	ID          string
	Title       string
	Artist      string
	AlbumArtist string
	Album       string
	DurationSec int
	Codec       string
	Container   string
	BitrateKbps int
	PartKey     string
	Thumb       string
}

type Index struct {
	tracks      []Track
	byID        map[string]int
	postings    map[string][]int
	titlePosts  map[string][]int
	titleCounts []int
}

func NewIndex(tracks []Track) *Index {
	ix := &Index{
		tracks:      tracks,
		byID:        make(map[string]int, len(tracks)),
		postings:    map[string][]int{},
		titlePosts:  map[string][]int{},
		titleCounts: make([]int, len(tracks)),
	}
	for i, t := range tracks {
		ix.byID[t.ID] = i
		title := unique(Tokens(t.Title))
		ix.titleCounts[i] = len(title)
		for _, token := range title {
			ix.titlePosts[token] = append(ix.titlePosts[token], i)
		}
		everything := strings.Join([]string{t.Title, t.Artist, t.AlbumArtist, t.Album}, " ")
		for _, token := range unique(Tokens(everything)) {
			ix.postings[token] = append(ix.postings[token], i)
		}
	}
	return ix
}

func (ix *Index) Len() int { return len(ix.tracks) }

func (ix *Index) Get(id string) (Track, bool) {
	i, ok := ix.byID[id]
	if !ok {
		return Track{}, false
	}
	return ix.tracks[i], true
}

func (ix *Index) Search(query string, limit int) []Track {
	q := unique(Tokens(query))
	if len(q) == 0 || limit <= 0 {
		return nil
	}
	strict := ix.strict(q)
	seen := make(map[int]struct{}, len(strict))
	for _, i := range strict {
		seen[i] = struct{}{}
	}
	var loose []int
	for _, i := range ix.fallback(q) {
		if _, dup := seen[i]; !dup {
			loose = append(loose, i)
		}
	}
	ix.rank(strict, q)
	ix.rank(loose, q)
	ordered := append(strict, loose...)
	if len(ordered) > limit {
		ordered = ordered[:limit]
	}
	out := make([]Track, len(ordered))
	for n, i := range ordered {
		out[n] = ix.tracks[i]
	}
	return out
}

func (ix *Index) strict(q []string) []int {
	lists := make([][]int, len(q))
	for n, token := range q {
		lists[n] = ix.postings[token]
		if len(lists[n]) == 0 {
			return nil
		}
	}
	sort.Slice(lists, func(a, b int) bool { return len(lists[a]) < len(lists[b]) })
	result := slices.Clone(lists[0])
	for _, next := range lists[1:] {
		result = intersect(result, next)
		if len(result) == 0 {
			return nil
		}
	}
	return result
}

func (ix *Index) fallback(q []string) []int {
	hits := map[int]int{}
	for _, token := range q {
		for _, i := range ix.titlePosts[token] {
			hits[i]++
		}
	}
	var out []int
	for i, n := range hits {
		if ix.titleCounts[i] >= 2 && n == ix.titleCounts[i] {
			out = append(out, i)
		}
	}
	return out
}

func (ix *Index) rank(candidates []int, q []string) {
	wanted := make(map[string]struct{}, len(q))
	for _, token := range q {
		wanted[token] = struct{}{}
	}
	titleHits := make(map[int]int, len(candidates))
	for _, i := range candidates {
		for _, token := range unique(Tokens(ix.tracks[i].Title)) {
			if _, ok := wanted[token]; ok {
				titleHits[i]++
			}
		}
	}
	sort.Slice(candidates, func(a, b int) bool {
		ia, ib := candidates[a], candidates[b]
		ta, tb := ix.tracks[ia], ix.tracks[ib]
		if titleHits[ia] != titleHits[ib] {
			return titleHits[ia] > titleHits[ib]
		}
		if la, lb := utf8.RuneCountInString(ta.Title), utf8.RuneCountInString(tb.Title); la != lb {
			return la < lb
		}
		if ta.Album != tb.Album {
			return ta.Album < tb.Album
		}
		return ta.ID < tb.ID
	})
}

func intersect(a, b []int) []int {
	out := a[:0]
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			out = append(out, a[i])
			i++
			j++
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	return out
}

func unique(tokens []string) []string {
	seen := make(map[string]struct{}, len(tokens))
	out := tokens[:0]
	for _, token := range tokens {
		if _, dup := seen[token]; !dup {
			seen[token] = struct{}{}
			out = append(out, token)
		}
	}
	return out
}
