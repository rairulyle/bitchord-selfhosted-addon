package library

import (
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
		titleHits := map[string]struct{}{}
		for _, token := range title {
			titleHits[token] = struct{}{}
			if syns, ok := tokenSynonyms[token]; ok {
				for _, syn := range syns {
					titleHits[syn] = struct{}{}
				}
			}
		}
		for token := range titleHits {
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

func prefixMatches(sortedKeys []string, token string, maxKeys int) []string {
	if len(token) < 2 {
		return nil
	}
	idx := sort.SearchStrings(sortedKeys, token)
	var matched []string
	for i := idx; i < len(sortedKeys); i++ {
		key := sortedKeys[i]
		if !strings.HasPrefix(key, token) {
			break
		}
		matched = append(matched, key)
		if len(matched) >= maxKeys {
			break
		}
	}
	return matched
}

func (ix *Index) lookupPostings(token string, prefix bool) []int {
	exact := ix.postings[token]
	if !prefix || len(token) < 2 {
		return exact
	}
	keys := prefixMatches(ix.sortedKeys, token, maxPrefixExpansion)
	if len(keys) == 0 {
		return exact
	}
	if len(keys) == 1 && keys[0] == token {
		return exact
	}
	var matched []int
	seen := make(map[int]struct{})
	for _, id := range exact {
		seen[id] = struct{}{}
		matched = append(matched, id)
	}
	for _, key := range keys {
		if key == token {
			continue
		}
		for _, id := range ix.postings[key] {
			if _, ok := seen[id]; !ok {
				seen[id] = struct{}{}
				matched = append(matched, id)
			}
		}
	}
	sort.Ints(matched)
	return matched
}

func (ix *Index) lookupTitle(token string, prefix bool) []int {
	exact := ix.titlePosts[token]
	if !prefix || len(token) < 2 {
		return exact
	}
	keys := prefixMatches(ix.sortedTitleKeys, token, maxPrefixExpansion)
	if len(keys) == 0 {
		return exact
	}
	if len(keys) == 1 && keys[0] == token {
		return exact
	}
	var matched []int
	seen := make(map[int]struct{})
	for _, id := range exact {
		seen[id] = struct{}{}
		matched = append(matched, id)
	}
	for _, key := range keys {
		if key == token {
			continue
		}
		for _, id := range ix.titlePosts[key] {
			if _, ok := seen[id]; !ok {
				seen[id] = struct{}{}
				matched = append(matched, id)
			}
		}
	}
	sort.Ints(matched)
	return matched
}

func (ix *Index) Search(query string, limit int) []media.Track { return ix.Find(query, limit).Tracks }

func (ix *Index) Find(query string, limit int) Result {
	cleanQ := CleanString(query)
	q := unique(Tokens(query))
	if len(q) == 0 || limit <= 0 {
		return Result{Tokens: q}
	}

	strict := ix.strict(q, false)
	seen := make(map[int]struct{}, len(strict))
	for _, i := range strict {
		seen[i] = struct{}{}
	}

	hasPrefixTokens := false
	for _, tk := range q {
		if len(tk) >= 2 {
			hasPrefixTokens = true
			break
		}
	}
	if hasPrefixTokens {
		for _, i := range ix.strict(q, true) {
			if _, dup := seen[i]; !dup {
				seen[i] = struct{}{}
				strict = append(strict, i)
			}
		}
	}

	var loose []int
	for _, i := range ix.fallback(q, false) {
		if _, dup := seen[i]; !dup {
			seen[i] = struct{}{}
			loose = append(loose, i)
		}
	}
	if hasPrefixTokens {
		for _, i := range ix.fallback(q, true) {
			if _, dup := seen[i]; !dup {
				seen[i] = struct{}{}
				loose = append(loose, i)
			}
		}
	}

	// Fallback without variant tokens (live, remaster, feat, etc.)
	if len(strict) == 0 && len(loose) == 0 {
		cleaned := stripNoise(q)
		if len(cleaned) > 0 && len(cleaned) < len(q) {
			for _, i := range ix.strict(cleaned, true) {
				if _, dup := seen[i]; !dup {
					seen[i] = struct{}{}
					strict = append(strict, i)
				}
			}
			for _, i := range ix.fallback(cleaned, true) {
				if _, dup := seen[i]; !dup {
					seen[i] = struct{}{}
					loose = append(loose, i)
				}
			}
		}
	}

	// Substring fallback when no token matches exist
	if len(strict) == 0 && len(loose) == 0 && len(cleanQ) >= 2 {
		for i := range ix.tracks {
			allMatch := true
			for _, token := range q {
				if !strings.Contains(ix.normEverything[i], token) {
					allMatch = false
					break
				}
			}
			if allMatch {
				if _, dup := seen[i]; !dup {
					seen[i] = struct{}{}
					loose = append(loose, i)
				}
			}
		}
	}

	// Fuzzy / Typo fallback when all exact, prefix, noise-stripped, and substring searches yielded nothing
	if len(strict) == 0 && len(loose) == 0 {
		for _, i := range ix.fuzzy(q) {
			if _, dup := seen[i]; !dup {
				seen[i] = struct{}{}
				loose = append(loose, i)
			}
		}
	}

	ix.rank(strict, cleanQ, q)
	ix.rank(loose, cleanQ, q)
	result := Result{
		Strict:   len(strict),
		Fallback: len(loose),
		Tokens:   q,
	}
	ordered := append(strict, loose...)
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

func (ix *Index) strict(q []string, prefix bool) []int {
	lists := make([][]int, len(q))
	for n, token := range q {
		lists[n] = ix.lookupPostings(token, prefix)
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

func (ix *Index) fallback(q []string, prefix bool) []int {
	hits := map[int]int{}
	for _, token := range q {
		for _, i := range ix.lookupTitle(token, prefix) {
			hits[i]++
		}
	}
	var out []int
	for i, n := range hits {
		if count := len(ix.titleTokens[i]); count >= 2 && n >= count {
			out = append(out, i)
		}
	}
	return out
}

func damerauLevenshtein(a, b string, maxDist int) int {
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

func (ix *Index) fuzzyKeys(token string) []string {
	if len(token) < 4 {
		return nil
	}
	maxDist := 1
	if len(token) >= 8 {
		maxDist = 2
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
		if len(token) >= 2 {
			if pfx := ix.lookupPostings(token, true); len(pfx) > 0 {
				lists[n] = pfx
				continue
			}
		}
		if len(token) < 4 {
			return nil
		}
		matchedKeys := ix.fuzzyKeys(token)
		if len(matchedKeys) == 0 {
			return nil
		}
		var list []int
		seen := make(map[int]struct{})
		for _, key := range matchedKeys {
			for _, id := range ix.postings[key] {
				if _, ok := seen[id]; !ok {
					seen[id] = struct{}{}
					list = append(list, id)
				}
			}
		}
		sort.Ints(list)
		lists[n] = list
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

		if titleScores[i] == 0 && len(cleanQ) >= 4 {
			maxD := 1
			if len(cleanQ) >= 8 {
				maxD = 2
			}
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
					if len(qToken) >= 2 && strings.HasPrefix(token, qToken) {
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

		if score == 0 && len(cleanQ) >= 4 {
			maxD := 1
			if len(cleanQ) >= 8 {
				maxD = 2
			}
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

		if score == 0 && len(cleanQ) >= 4 {
			maxD := 1
			if len(cleanQ) >= 8 {
				maxD = 2
			}
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

// FormatArtists formats a multi-artist string consistently using " • ".
func FormatArtists(s string) string {
	parts := SplitArtists(s)
	if len(parts) <= 1 {
		return s
	}
	return strings.Join(parts, " • ")
}
