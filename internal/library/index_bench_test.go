package library

import (
	"fmt"
	"math/rand/v2"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
)

const benchTracks = 100_000

var (
	benchSyllables = []string{"ka", "lo", "mi", "ren", "tor", "shi", "va", "dul", "be", "nor", "sa", "qui", "fen", "ga", "ru", "zel", "po", "tha", "wen", "cro", "di", "mar", "lu", "sten", "ny", "vor", "el", "ish", "bra", "ko"}
	benchCommon    = []string{"the", "love", "you", "me", "my", "of", "in", "a", "night", "heart", "baby", "time", "life", "light", "home", "girl", "world", "day", "fire", "dream"}
)

func benchWord(r *rand.Rand) string {
	var b strings.Builder
	for range 2 + r.IntN(3) {
		b.WriteString(benchSyllables[r.IntN(len(benchSyllables))])
	}
	return b.String()
}

func benchPhrase(r *rand.Rand, vocab []string, words int) string {
	parts := make([]string, words)
	for i := range parts {
		if r.IntN(3) == 0 {
			parts[i] = benchCommon[r.IntN(len(benchCommon))]
		} else {
			parts[i] = vocab[r.IntN(1+r.IntN(len(vocab)))]
		}
	}
	title := strings.Join(parts, " ")
	return strings.ToUpper(title[:1]) + title[1:]
}

func benchLibrary() []media.Track {
	r := rand.New(rand.NewPCG(1, 2))
	vocab := make([]string, 40_000)
	for i := range vocab {
		vocab[i] = benchWord(r)
	}
	artists := make([]string, 8000)
	for i := range artists {
		artists[i] = benchPhrase(r, vocab, 1+r.IntN(2))
	}
	codecs := []string{"flac", "mp3", "aac", "opus"}
	tracks := make([]media.Track, 0, benchTracks)
	for album := 0; len(tracks) < benchTracks; album++ {
		artist := artists[r.IntN(len(artists))]
		name := benchPhrase(r, vocab, 1+r.IntN(3))
		for range 10 {
			id := len(tracks) + 1
			trackArtist := artist
			if r.IntN(10) == 0 {
				trackArtist = artist + " • " + artists[r.IntN(len(artists))]
			}
			tracks = append(tracks, media.Track{
				ID:          fmt.Sprint(id),
				Title:       benchPhrase(r, vocab, 1+r.IntN(4)),
				Artist:      trackArtist,
				AlbumArtist: artist,
				Album:       name,
				DurationSec: 120 + r.IntN(300),
				Codec:       codecs[r.IntN(len(codecs))],
				Container:   "flac",
				BitrateKbps: 900,
				SampleRate:  44100,
				BitDepth:    16,
				FileRef:     fmt.Sprintf("/library/parts/%d/1700000000/file.flac", id),
				ArtRef:      fmt.Sprintf("/library/metadata/%d/thumb/1700000000", album+1),
			})
		}
	}
	return tracks
}

func typo(word string) string {
	b := []rune(word)
	mid := len(b) / 2
	b[mid-1], b[mid] = b[mid], b[mid-1]
	return string(b)
}

func longestWord(s string) string {
	longest := ""
	for _, w := range Tokens(s) {
		if len(w) > len(longest) {
			longest = w
		}
	}
	return longest
}

func benchHit(tracks []media.Track) media.Track {
	common := map[string]bool{}
	for _, w := range benchCommon {
		common[w] = true
	}
	return tracks[slices.IndexFunc(tracks[4242:], func(t media.Track) bool {
		words := Tokens(t.Title)
		return len(words) >= 2 && !slices.ContainsFunc(words, func(w string) bool { return common[w] })
	})+4242]
}

func BenchmarkSearch(b *testing.B) {
	tracks := benchLibrary()
	ix := NewIndex(tracks)
	hit := benchHit(tracks)
	word := longestWord(hit.Title)
	queries := map[string]string{
		"exact hit":   hit.Title + " " + hit.Artist,
		"typo miss":   strings.Replace(strings.ToLower(hit.Title), word, typo(word), 1) + " " + hit.AlbumArtist,
		"common word": "love",
	}
	for name, query := range queries {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				ix.Find(query, 50)
			}
		})
	}
}

func heapMB() float64 {
	var m runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m)
	return float64(m.HeapAlloc) / (1 << 20)
}

func BenchmarkNewIndex(b *testing.B) {
	start := heapMB()
	tracks := benchLibrary()
	loaded := heapMB()
	ix := NewIndex(tracks)
	indexed := heapMB()
	runtime.KeepAlive(ix)
	b.ReportAllocs()
	for b.Loop() {
		NewIndex(tracks)
	}
	b.ReportMetric(loaded-start, "tracks-MB")
	b.ReportMetric(indexed-loaded, "index-MB")
}
