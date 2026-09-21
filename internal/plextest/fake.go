package plextest

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/plex"
)

const Token = "fake-plex-token"

type Request struct {
	Method string
	Path   string
	Query  string
	Header http.Header
}

type Fake struct {
	URL      string
	Sections []plex.Section
	Tracks   map[string][]plex.Track
	Files    map[string][]byte
	Extra    map[string]http.HandlerFunc

	mu       sync.Mutex
	status   int
	rawBody  string
	requests []Request
}

func New(t testing.TB) *Fake {
	f := &Fake{
		Sections: Sections(),
		Tracks:   Tracks(),
		Files:    Files(),
		Extra:    map[string]http.HandlerFunc{},
	}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	f.URL = server.URL
	return f
}

func (f *Fake) FailWith(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
}

func (f *Fake) AnswerRaw(body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rawBody = body
}

func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.requests...)
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, Request{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone()})
	status, rawBody := f.status, f.rawBody
	f.mu.Unlock()

	if r.Header.Get("X-Plex-Token") != Token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	if rawBody != "" {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(rawBody))
		return
	}
	if extra, ok := f.Extra[r.URL.Path]; ok {
		extra(w, r)
		return
	}
	switch path := r.URL.Path; {
	case path == "/library/sections":
		writeJSON(w, map[string]any{"MediaContainer": map[string]any{"Directory": f.Sections}})
	case strings.HasPrefix(path, "/library/sections/") && strings.HasSuffix(path, "/all"):
		f.servePage(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/library/sections/"), "/all"))
	case strings.HasPrefix(path, "/library/metadata/"):
		f.serveTrack(w, strings.TrimPrefix(path, "/library/metadata/"))
	case path == "/photo/:/transcode":
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("X-Plex-Protocol", "1.0")
		w.Write([]byte("jpeg:" + r.URL.Query().Get("url")))
	default:
		f.serveFile(w, r)
	}
}

func (f *Fake) servePage(w http.ResponseWriter, r *http.Request, key string) {
	all := f.Tracks[key]
	start, _ := strconv.Atoi(r.Header.Get("X-Plex-Container-Start"))
	size, err := strconv.Atoi(r.Header.Get("X-Plex-Container-Size"))
	if err != nil {
		size = len(all)
	}
	start = min(start, len(all))
	end := min(start+size, len(all))
	writeJSON(w, map[string]any{"MediaContainer": map[string]any{
		"totalSize": len(all),
		"offset":    start,
		"Metadata":  all[start:end],
	}})
}

func (f *Fake) serveTrack(w http.ResponseWriter, id string) {
	for _, tracks := range f.Tracks {
		for _, track := range tracks {
			if track.RatingKey == id {
				writeJSON(w, map[string]any{"MediaContainer": map[string]any{"Metadata": []plex.Track{track}}})
				return
			}
		}
	}
	w.WriteHeader(http.StatusNotFound)
}

func (f *Fake) serveFile(w http.ResponseWriter, r *http.Request) {
	body, ok := f.Files[r.URL.Path]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "audio/flac")
	w.Header().Set("ETag", `"fake-etag"`)
	w.Header().Set("X-Plex-Protocol", "1.0")
	http.ServeContent(w, r, "", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), bytes.NewReader(body))
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(value)
}

func Sections() []plex.Section {
	return []plex.Section{
		{Key: "1", Type: "movie", Title: "Movies"},
		{Key: "3", Type: "artist", Title: "Music"},
		{Key: "5", Type: "artist", Title: "Audiobooks"},
	}
}

func track(id, title, artist, albumArtist, album string, ms int, codec, container string, kbps int, stream plex.Stream) plex.Track {
	stream.StreamType = 2
	stream.Codec = codec
	return plex.Track{
		RatingKey:        id,
		Title:            title,
		OriginalTitle:    artist,
		GrandparentTitle: albumArtist,
		ParentTitle:      album,
		Duration:         ms,
		Thumb:            "/library/metadata/" + id + "/thumb/1",
		ParentThumb:      "/library/metadata/9" + id + "/thumb/1",
		Media: []plex.Media{{
			AudioCodec: codec,
			Container:  container,
			Bitrate:    kbps,
			Part: []plex.Part{{
				Key:       "/library/parts/" + id + "/1/file." + container,
				Container: container,
				Stream:    []plex.Stream{stream},
			}},
		}},
	}
}

func Tracks() map[string][]plex.Track {
	noThumb := track("104", "No Cover", "", "Nobody", "Blank", 60000, "mp3", "mp3", 128, plex.Stream{})
	noThumb.Thumb, noThumb.ParentThumb = "", ""
	albumThumb := track("105", "Album Cover Only", "", "Somebody", "Covers", 61000, "aac", "mp4", 256, plex.Stream{SamplingRate: 44100})
	albumThumb.Thumb = ""
	return map[string][]plex.Track{
		"3": {
			track("101", "Paniyon Sa", "Atif Aslam, Tulsi Kumar", "Various Artists", "Satyameva Jayate", 249400, "flac", "flac", 2890, plex.Stream{SamplingRate: 96000, BitDepth: 24}),
			track("102", "Tum Hi Ho", "", "Arijit Singh", "Aashiqui 2", 261600, "mp3", "mp3", 320, plex.Stream{SamplingRate: 44100}),
			track("103", "Home", "", "Michael Bublé", "It's Time", 225000, "pcm", "wav", 1411, plex.Stream{SamplingRate: 44100, BitDepth: 16}),
			noThumb,
			albumThumb,
			{RatingKey: "106", Title: "Broken, No Media"},
		},
		"5": {
			track("501", "Chapter One", "", "Some Author", "Some Book", 1800000, "mp3", "mp3", 64, plex.Stream{}),
		},
	}
}

func Files() map[string][]byte {
	return map[string][]byte{
		"/library/parts/101/1/file.flac": []byte("0123456789abcdefghijklmnopqrstuvwxyz"),
		"/library/parts/102/1/file.mp3":  []byte("mp3-bytes"),
	}
}
