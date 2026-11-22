package library

import (
	"container/heap"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
)

type Index struct {
	tracks          []media.Track
	byID            map[string]int
	postings        map[string][]int
	titlePosts      map[string][]int
	sortedKeys      []string
	sortedTitleKeys []string
	titleTokens     [][]string
	titleForms      [][]string
	normTitles      []string
	normArtists     []string
	normAlbums      []string
	normEverything  []string

	albums  []albumRecord
	artists []artistRecord
}

type albumRecord struct {
	name       string
	artist     string
	trackCount int
	year       int
	artRef     string
	sampleID   string
	normName   string
	normArtist string
}

type artistRecord struct {
	name       string
	trackCount int
	artRef     string
	sampleID   string
	normName   string
}

type AlbumMatch struct {
	Name       string `json:"name"`
	Artist     string `json:"artist"`
	ArtworkURL string `json:"artworkURL,omitempty"`
	ArtRef     string `json:"-"`
	SampleID   string `json:"id,omitempty"`
	TrackCount int    `json:"trackCount"`
	Year       int    `json:"year,omitempty"`
}

type ArtistMatch struct {
	Name       string `json:"name"`
	ArtworkURL string `json:"artworkURL,omitempty"`
	ArtRef     string `json:"-"`
	SampleID   string `json:"id,omitempty"`
	TrackCount int    `json:"trackCount"`
}

type Result struct {
	Tracks   []media.Track `json:"tracks"`
	Albums   []AlbumMatch  `json:"albums"`
	Artists  []ArtistMatch `json:"artists"`
	Strict   int           `json:"strict"`
	Fallback int           `json:"fallback"`
	Tokens   []string      `json:"tokens"`
}

func NewIndex(tracks []media.Track) *Index {
	ix := &Index{
		tracks:         tracks,
		byID:           make(map[string]int, len(tracks)),
		postings:       map[string][]int{},
		titlePosts:     map[string][]int{},
		titleTokens:    make([][]string, len(tracks)),
		titleForms:     make([][]string, len(tracks)),
		normTitles:     make([]string, len(tracks)),
		normArtists:    make([]string, len(tracks)),
		normAlbums:     make([]string, len(tracks)),
		normEverything: make([]string, len(tracks)),
	}

	albumMap := make(map[string]*albumRecord)
	artistMap := make(map[string]*artistRecord)

	for i, t := range tracks {
		ix.byID[t.ID] = i
		cleanTitle := CleanString(t.Title)
		cleanArtist := CleanString(t.Artist)
		cleanAlbumArtist := CleanString(t.AlbumArtist)
		cleanAlbum := CleanString(t.Album)

		ix.normTitles[i] = cleanTitle
		ix.normArtists[i] = cleanArtist
		if cleanAlbumArtist != "" && cleanAlbumArtist != cleanArtist {
			ix.normArtists[i] = cleanArtist + " " + cleanAlbumArtist
		}
		ix.normAlbums[i] = cleanAlbum
		ix.normEverything[i] = strings.TrimSpace(cleanTitle + " " + ix.normArtists[i] + " " + cleanAlbum)

		title := unique(Tokens(t.Title))
		ix.titleTokens[i] = title
		for _, token := range title {
			ix.titlePosts[token] = append(ix.titlePosts[token], i)
		}
		everything := strings.Join([]string{t.Artist, t.AlbumArtist, t.Album}, " ")
		hits := map[string]struct{}{}
		for _, token := range title {
			hits[token] = struct{}{}
		}
		for _, form := range bitchordWords(t.Title) {
			if _, known := hits[form]; !known {
				ix.titleForms[i] = append(ix.titleForms[i], form)
			}
			hits[form] = struct{}{}
		}
		for _, token := range unique(Tokens(everything)) {
			hits[token] = struct{}{}
		}
		for _, form := range bitchordWords(everything) {
			hits[form] = struct{}{}
		}
		synHits := map[string]struct{}{}
		for token := range hits {
			if syns, ok := tokenSynonyms[token]; ok {
				for _, syn := range syns {
					synHits[syn] = struct{}{}
				}
			}
		}
		for syn := range synHits {
			hits[syn] = struct{}{}
		}
		for token := range hits {
			ix.postings[token] = append(ix.postings[token], i)
		}

		// Track album groupings
		if t.Album != "" {
			albumArtist := t.AlbumArtist
			if albumArtist == "" {
				albumArtist = t.Artist
			}
			key := strings.ToLower(t.Album) + "\x00" + strings.ToLower(albumArtist)
			rec, ok := albumMap[key]
			if !ok {
				rec = &albumRecord{
					name:       t.Album,
					artist:     albumArtist,
					artRef:     t.ArtRef,
					sampleID:   t.ID,
					normName:   cleanAlbum,
					normArtist: CleanString(albumArtist),
				}
				albumMap[key] = rec
			}
			rec.trackCount++
			if rec.artRef == "" && t.ArtRef != "" {
				rec.artRef = t.ArtRef
				rec.sampleID = t.ID
			}
		}

		// Track artist groupings
		seenTrackArtists := make(map[string]struct{})
		for _, raw := range []string{t.Artist, t.AlbumArtist} {
			for _, artName := range SplitArtists(raw) {
				key := strings.ToLower(artName)
				if _, dup := seenTrackArtists[key]; dup {
					continue
				}
				seenTrackArtists[key] = struct{}{}
				rec, ok := artistMap[key]
				if !ok {
					rec = &artistRecord{
						name:     artName,
						artRef:   t.ArtRef,
						sampleID: t.ID,
						normName: CleanString(artName),
					}
					artistMap[key] = rec
				}
				rec.trackCount++
				if rec.artRef == "" && t.ArtRef != "" {
					rec.artRef = t.ArtRef
					rec.sampleID = t.ID
				}
			}
		}
	}

	sortedKeys := make([]string, 0, len(ix.postings))
	for k := range ix.postings {
		sortedKeys = append(sortedKeys, k)
	}
	slices.Sort(sortedKeys)
	ix.sortedKeys = sortedKeys

	sortedTitleKeys := make([]string, 0, len(ix.titlePosts))
	for k := range ix.titlePosts {
		sortedTitleKeys = append(sortedTitleKeys, k)
	}
	slices.Sort(sortedTitleKeys)
	ix.sortedTitleKeys = sortedTitleKeys

	ix.albums = make([]albumRecord, 0, len(albumMap))
	for _, rec := range albumMap {
		ix.albums = append(ix.albums, *rec)
	}
	ix.artists = make([]artistRecord, 0, len(artistMap))
	for _, rec := range artistMap {
		ix.artists = append(ix.artists, *rec)
	}

	return ix
}

func (ix *Index) Len() int { return len(ix.tracks) }

func (ix *Index) missingFrom(other *Index) int {
	if other == nil {
		return len(ix.tracks)
	}
	missing := 0
	for id := range ix.byID {
		if _, ok := other.byID[id]; !ok {
			missing++
		}
	}
	return missing
}

func (ix *Index) Get(id string) (media.Track, bool) {
	i, ok := ix.byID[id]
	if !ok {
		return media.Track{}, false
	}
	return ix.tracks[i], true
}

var tokenSynonyms = map[string][]string{
	"ft":         {"feat", "featuring"},
	"feat":       {"ft", "featuring"},
	"featuring":  {"feat", "ft"},
	"pt":         {"part"},
	"part":       {"pt"},
	"vol":        {"volume"},
	"volume":     {"vol"},
	"ost":        {"soundtrack"},
	"soundtrack": {"ost"},
	"vs":         {"versus"},
	"versus":     {"vs"},
}

var noiseWords = map[string]struct{}{
	"live": {}, "remaster": {}, "remastered": {}, "feat": {}, "ft": {}, "featuring": {},
	"deluxe": {}, "edition": {}, "bonus": {}, "track": {}, "acoustic": {}, "instrumental": {},
	"official": {}, "video": {}, "audio": {}, "version": {}, "edit": {}, "mix": {}, "remix": {},
	"pt": {}, "part": {}, "vol": {}, "volume": {},
}

func isYear(s string) bool {
	if len(s) != 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s >= "1900" && s <= "2099"
}

func stripNoise(q []string) []string {
	var cleaned []string
	for _, t := range q {
		if _, noise := noiseWords[t]; !noise && !isYear(t) {
			cleaned = append(cleaned, t)
		}
	}
	return cleaned
}

const maxPrefixExpansion = 30

// completions lists the keys that start with token, most-used first, so a
// short prefix keeps the words a library actually has many tracks for.
func completions(sortedKeys []string, posts map[string][]int, token string) []string {
	if utf8.RuneCountInString(token) < 2 {
		return nil
	}
	start := sort.SearchStrings(sortedKeys, token)
	end := start
	for end < len(sortedKeys) && strings.HasPrefix(sortedKeys[end], token) {
		end++
	}
	keys := slices.Clone(sortedKeys[start:end])
	slices.SortStableFunc(keys, func(a, b string) int { return len(posts[b]) - len(posts[a]) })
	return keys[:min(len(keys), maxPrefixExpansion)]
}

func (ix *Index) lookupPostings(token string, prefix bool) []int {
	if !prefix {
		return ix.postings[token]
	}
	lists := [][]int{ix.postings[token]}
	for _, key := range completions(ix.sortedKeys, ix.postings, token) {
		if key != token {
			lists = append(lists, ix.postings[key])
		}
	}
	return union(lists)
}

func (ix *Index) Search(query string, limit int) []media.Track { return ix.Find(query, limit).Tracks }

func (ix *Index) Find(query string, limit int) Result {
	q := unique(Tokens(query))
	if len(q) == 0 || limit <= 0 {
		return Result{Tokens: q}
	}
	cleanQ := CleanString(query)
	first, second, exact := ix.match(q)
	ix.rank(first, cleanQ, q)
	ix.rank(second, cleanQ, q)
	result := Result{Fallback: len(first) + len(second), Tokens: q}
	if exact {
		result.Strict, result.Fallback = len(first), len(second)
	}
	ordered := append(first, second...)
	if len(ordered) > limit {
		ordered = ordered[:limit]
	}
	out := make([]media.Track, len(ordered))
	for n, i := range ordered {
		out[n] = ix.tracks[i]
	}
	result.Tracks = out
	result.Albums = ix.findAlbums(cleanQ, q, ordered)
	result.Artists = ix.findArtists(cleanQ, q, ordered)
	return result
}

// match stops at the first tier that finds anything. Only the first tier
// counts as strict: the search log reads strict as every word matched as typed.
func (ix *Index) match(q []string) (first, second []int, exact bool) {
	if first, second = ix.tier(q, false); len(first)+len(second) > 0 {
		return first, second, true
	}
	if first, second = ix.tier(q, true); len(first)+len(second) > 0 {
		return first, second, false
	}
	if cleaned := stripNoise(q); len(cleaned) > 0 && len(cleaned) < len(q) {
		for _, prefix := range []bool{false, true} {
			if first, second = ix.tier(cleaned, prefix); len(first)+len(second) > 0 {
				return first, second, false
			}
		}
	}
	if substringable(q) {
		if found := ix.substring(q); len(found) > 0 {
			return nil, found, false
		}
	}
	return nil, ix.fuzzy(q), false
}

func (ix *Index) tier(q []string, prefix bool) (strict, loose []int) {
	strict = ix.strict(q, prefix)
	for _, i := range ix.fallback(q, prefix) {
		if _, found := slices.BinarySearch(strict, i); !found {
			loose = append(loose, i)
		}
	}
	return strict, loose
}

func (ix *Index) substring(q []string) []int {
	var out []int
	for i, haystack := range ix.normEverything {
		if !slices.ContainsFunc(q, func(token string) bool { return !strings.Contains(haystack, token) }) {
			out = append(out, i)
		}
	}
	return out
}

func (ix *Index) strict(q []string, prefix bool) []int {
	lists := make([][]int, len(q))
	for n, token := range q {
		lists[n] = ix.lookupPostings(token, prefix && n == len(q)-1)
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

func (ix *Index) titleKeys(token string, prefix bool) []string {
	keys := append([]string{token}, tokenSynonyms[token]...)
	if prefix && utf8.RuneCountInString(token) >= 2 {
		keys = append(keys, completions(ix.sortedTitleKeys, ix.titlePosts, token)...)
	}
	return keys
}

func (ix *Index) fallback(q []string, prefix bool) []int {
	covered := map[string]struct{}{}
	var lists [][]int
	for n, token := range q {
		for _, key := range ix.titleKeys(token, prefix && n == len(q)-1) {
			covered[key] = struct{}{}
			lists = append(lists, ix.titlePosts[key])
		}
	}
	var out []int
	for _, i := range union(lists) {
		title := ix.titleTokens[i]
		n := 0
		for _, token := range title {
			if _, ok := covered[token]; ok {
				n++
			}
		}
		if len(title) >= 2 && n == len(title) {
			out = append(out, i)
		}
	}
	return out
}

func damerauLevenshtein(as, bs string, maxDist int) int {
	a, b := []rune(as), []rune(bs)
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	diff := la - lb
	if diff < 0 {
		diff = -diff
	}
	if diff > maxDist {
		return maxDist + 1
	}

	r0 := make([]int, lb+1)
	r1 := make([]int, lb+1)
	r2 := make([]int, lb+1)

	for j := 0; j <= lb; j++ {
		r1[j] = j
	}

	for i := 1; i <= la; i++ {
		r2[0] = i
		minInRow := i
		for j := 1; j <= lb; j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			del := r1[j] + 1
			ins := r2[j-1] + 1
			sub := r1[j-1] + cost
			val := min(del, min(ins, sub))

			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				trans := r0[j-2] + 1
				if trans < val {
					val = trans
				}
			}
			r2[j] = val
			if val < minInRow {
				minInRow = val
			}
		}
		if minInRow > maxDist {
			return maxDist + 1
		}
		copy(r0, r1)
		copy(r1, r2)
	}
	return r1[lb]
}

const minSubstring = 3

func substringable(q []string) bool {
	return !slices.ContainsFunc(q, func(token string) bool { return utf8.RuneCountInString(token) < minSubstring })
}

// maxTypos stays at zero below five characters: one edit turns most short
// words into other real words (love, live, lose, dove).
func maxTypos(s string) int {
	switch n := utf8.RuneCountInString(s); {
	case n > 8:
		return 2
	case n >= 5:
		return 1
	default:
		return 0
	}
}

func (ix *Index) fuzzyKeys(token string) []string {
	maxDist := maxTypos(token)
	if maxDist == 0 {
		return nil
	}
	type match struct {
		key  string
		dist int
	}
	var matches []match
	for _, key := range ix.sortedKeys {
		d := damerauLevenshtein(token, key, maxDist)
		if d <= maxDist {
			matches = append(matches, match{key: key, dist: d})
		}
	}
	if len(matches) == 0 {
		return nil
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].dist != matches[j].dist {
			return matches[i].dist < matches[j].dist
		}
		return len(matches[i].key) < len(matches[j].key)
	})
	limit := 10
	if len(matches) < limit {
		limit = len(matches)
	}
	out := make([]string, limit)
	for i := 0; i < limit; i++ {
		out[i] = matches[i].key
	}
	return out
}

func (ix *Index) fuzzy(q []string) []int {
	lists := make([][]int, len(q))
	for n, token := range q {
		if exact, ok := ix.postings[token]; ok && len(exact) > 0 {
			lists[n] = exact
			continue
		}
		if n == len(q)-1 {
			if pfx := ix.lookupPostings(token, true); len(pfx) > 0 {
				lists[n] = pfx
				continue
			}
		}
		if maxTypos(token) == 0 {
			return nil
		}
		matchedKeys := ix.fuzzyKeys(token)
		if len(matchedKeys) == 0 {
			return nil
		}
		keyLists := make([][]int, len(matchedKeys))
		for k, key := range matchedKeys {
			keyLists[k] = ix.postings[key]
		}
		lists[n] = union(keyLists)
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

func (ix *Index) rank(candidates []int, cleanQ string, q []string) {
	wanted := make(map[string]struct{}, len(q))
	for _, token := range q {
		wanted[token] = struct{}{}
	}
	titleScores := make(map[int]int, len(candidates))
	for _, i := range candidates {
		normTitle := ix.normTitles[i]
		normArtist := ix.normArtists[i]
		normAlbum := ix.normAlbums[i]

		if normTitle == cleanQ {
			titleScores[i] += 500
		} else if strings.HasPrefix(normTitle, cleanQ) {
			titleScores[i] += 250
		} else if strings.Contains(normTitle, cleanQ) {
			titleScores[i] += 120
		}

		if normArtist == cleanQ {
			titleScores[i] += 200
		} else if strings.HasPrefix(normArtist, cleanQ) {
			titleScores[i] += 100
		} else if strings.Contains(normArtist, cleanQ) {
			titleScores[i] += 50
		}

		if strings.Contains(normAlbum, cleanQ) {
			titleScores[i] += 30
		}

		if maxD := maxTypos(cleanQ); titleScores[i] == 0 && maxD > 0 {
			if d := damerauLevenshtein(cleanQ, normTitle, maxD); d <= maxD {
				titleScores[i] += 150 - (d * 30)
			} else if d := damerauLevenshtein(cleanQ, normArtist, maxD); d <= maxD {
				titleScores[i] += 100 - (d * 20)
			} else if d := damerauLevenshtein(cleanQ, normAlbum, maxD); d <= maxD {
				titleScores[i] += 50 - (d * 10)
			}
		}

		for _, tokens := range [][]string{ix.titleTokens[i], ix.titleForms[i]} {
			for _, token := range tokens {
				if _, ok := wanted[token]; ok {
					titleScores[i] += 20
					continue
				}
				for _, qToken := range q {
					if utf8.RuneCountInString(qToken) >= 2 && strings.HasPrefix(token, qToken) {
						titleScores[i] += 10
						break
					}
				}
			}
		}
	}

	sort.Slice(candidates, func(a, b int) bool {
		ia, ib := candidates[a], candidates[b]
		ta, tb := ix.tracks[ia], ix.tracks[ib]
		if titleScores[ia] != titleScores[ib] {
			return titleScores[ia] > titleScores[ib]
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

func (ix *Index) findAlbums(cleanQ string, q []string, matchedTracks []int) []AlbumMatch {
	if len(ix.albums) == 0 || len(cleanQ) == 0 {
		return nil
	}
	albumScore := make(map[int]int)
	matchedAlbumNames := make(map[string]struct{})
	for _, trackIdx := range matchedTracks {
		alb := ix.tracks[trackIdx].Album
		if alb != "" {
			matchedAlbumNames[strings.ToLower(alb)] = struct{}{}
		}
	}

	for i, alb := range ix.albums {
		score := 0
		if alb.normName == cleanQ {
			score += 1000
		} else if strings.HasPrefix(alb.normName, cleanQ) {
			score += 600
		} else if strings.Contains(alb.normName, cleanQ) {
			score += 400
		}
		if alb.normArtist == cleanQ {
			score += 500
		} else if strings.HasPrefix(alb.normArtist, cleanQ) {
			score += 300
		} else if strings.Contains(alb.normArtist, cleanQ) {
			score += 200
		}

		allTokensMatch := true
		for _, tk := range q {
			matched := strings.Contains(alb.normName+" "+alb.normArtist, tk)
			if !matched {
				if syns, ok := tokenSynonyms[tk]; ok {
					for _, syn := range syns {
						if strings.Contains(alb.normName+" "+alb.normArtist, syn) {
							matched = true
							break
						}
					}
				}
			}
			if !matched {
				allTokensMatch = false
				break
			}
		}
		if allTokensMatch && len(q) > 0 {
			score += 250
		}

		if maxD := maxTypos(cleanQ); score == 0 && maxD > 0 {
			if d := damerauLevenshtein(cleanQ, alb.normName, maxD); d <= maxD {
				score += 300 - (d * 50)
			}
		}

		// Match against album name or artist.
		if score > 0 {
			if _, ok := matchedAlbumNames[strings.ToLower(alb.name)]; ok {
				score += 100
			}
			albumScore[i] = score
		}
	}

	var candidates []int
	for idx := range albumScore {
		candidates = append(candidates, idx)
	}
	sort.Slice(candidates, func(a, b int) bool {
		ia, ib := candidates[a], candidates[b]
		sa, sb := albumScore[ia], albumScore[ib]
		if sa != sb {
			return sa > sb
		}
		return ix.albums[ia].name < ix.albums[ib].name
	})

	limit := 10
	if len(candidates) < limit {
		limit = len(candidates)
	}
	out := make([]AlbumMatch, limit)
	for i := 0; i < limit; i++ {
		alb := ix.albums[candidates[i]]
		out[i] = AlbumMatch{
			Name:       alb.name,
			Artist:     alb.artist,
			ArtRef:     alb.artRef,
			SampleID:   alb.sampleID,
			TrackCount: alb.trackCount,
			Year:       alb.year,
		}
	}
	return out
}

func (ix *Index) findArtists(cleanQ string, q []string, matchedTracks []int) []ArtistMatch {
	if len(ix.artists) == 0 || len(cleanQ) == 0 {
		return nil
	}
	artistScore := make(map[int]int)
	matchedArtistNames := make(map[string]struct{})
	for _, trackIdx := range matchedTracks {
		for _, name := range SplitArtists(ix.tracks[trackIdx].Artist) {
			matchedArtistNames[strings.ToLower(name)] = struct{}{}
		}
		for _, name := range SplitArtists(ix.tracks[trackIdx].AlbumArtist) {
			matchedArtistNames[strings.ToLower(name)] = struct{}{}
		}
	}

	for i, art := range ix.artists {
		score := 0
		if art.normName == cleanQ {
			score += 1000
		} else if strings.HasPrefix(art.normName, cleanQ) {
			score += 600
		} else if strings.Contains(art.normName, cleanQ) {
			score += 400
		}

		allTokensMatch := true
		for _, tk := range q {
			matched := strings.Contains(art.normName, tk)
			if !matched {
				if syns, ok := tokenSynonyms[tk]; ok {
					for _, syn := range syns {
						if strings.Contains(art.normName, syn) {
							matched = true
							break
						}
					}
				}
			}
			if !matched {
				allTokensMatch = false
				break
			}
		}
		if allTokensMatch && len(q) > 0 {
			score += 250
		}

		if maxD := maxTypos(cleanQ); score == 0 && maxD > 0 {
			if d := damerauLevenshtein(cleanQ, art.normName, maxD); d <= maxD {
				score += 300 - (d * 50)
			}
		}

		// Match against artist name.
		if score > 0 {
			if _, ok := matchedArtistNames[strings.ToLower(art.name)]; ok {
				score += 100
			}
			artistScore[i] = score
		}
	}

	var candidates []int
	for idx := range artistScore {
		candidates = append(candidates, idx)
	}
	sort.Slice(candidates, func(a, b int) bool {
		ia, ib := candidates[a], candidates[b]
		sa, sb := artistScore[ia], artistScore[ib]
		if sa != sb {
			return sa > sb
		}
		if ix.artists[ia].trackCount != ix.artists[ib].trackCount {
			return ix.artists[ia].trackCount > ix.artists[ib].trackCount
		}
		return ix.artists[ia].name < ix.artists[ib].name
	})

	limit := 10
	if len(candidates) < limit {
		limit = len(candidates)
	}
	out := make([]ArtistMatch, limit)
	for i := 0; i < limit; i++ {
		art := ix.artists[candidates[i]]
		out[i] = ArtistMatch{
			Name:       art.name,
			ArtRef:     art.artRef,
			SampleID:   art.sampleID,
			TrackCount: art.trackCount,
		}
	}
	return out
}

type postingHeap [][]int

func (h postingHeap) Len() int           { return len(h) }
func (h postingHeap) Less(a, b int) bool { return h[a][0] < h[b][0] }
func (h postingHeap) Swap(a, b int)      { h[a], h[b] = h[b], h[a] }
func (h *postingHeap) Push(x any)        { *h = append(*h, x.([]int)) }
func (h *postingHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}

// union merges sorted posting lists into one sorted list without repeats.
// With a single list it returns that list itself, which callers must not
// modify.
func union(lists [][]int) []int {
	h := make(postingHeap, 0, len(lists))
	total := 0
	for _, list := range lists {
		if len(list) > 0 {
			h = append(h, list)
			total += len(list)
		}
	}
	switch len(h) {
	case 0:
		return nil
	case 1:
		return h[0]
	}
	heap.Init(&h)
	out := make([]int, 0, total)
	for len(h) > 0 {
		if next := h[0][0]; len(out) == 0 || out[len(out)-1] != next {
			out = append(out, next)
		}
		if h[0] = h[0][1:]; len(h[0]) == 0 {
			heap.Pop(&h)
		} else {
			heap.Fix(&h, 0)
		}
	}
	return out
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

// SplitArtists splits a composite artist string into individual artist names
// by common multi-artist delimiters (; • | and " / ").
// Delimiters without surrounding whitespace (like AC/DC) or band name
// connectors (like & or , in "Tyler, The Creator") are preserved.
func SplitArtists(s string) []string {
	if s == "" {
		return nil
	}
	clean := s
	for _, delim := range []string{";", "•", " / ", "|"} {
		clean = strings.ReplaceAll(clean, delim, "\x00")
	}

	parts := strings.Split(clean, "\x00")
	var artists []string
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		lower := strings.ToLower(name)
		if _, dup := seen[lower]; !dup {
			seen[lower] = struct{}{}
			artists = append(artists, name)
		}
	}
	return artists
}
