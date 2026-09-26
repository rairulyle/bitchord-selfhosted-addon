package jellyfintest

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

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/jellyfin"
)

const Token = "fake-jellyfin-key"

type Request struct {
	Method string
	Path   string
	Query  string
	Header http.Header
}

type Fake struct {
	URL          string
	Folders      []jellyfin.Folder
	Items        map[string][]jellyfin.Item
	Files        map[string][]byte
	Extra        map[string]http.HandlerFunc
	IgnorePaging bool

	mu       sync.Mutex
	status   int
	rawBody  string
	requests []Request
}

func New(t testing.TB) *Fake {
	f := &Fake{
		Folders: Folders(),
		Items:   Items(),
		Files:   Files(),
		Extra:   map[string]http.HandlerFunc{},
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

	if !strings.Contains(r.Header.Get("Authorization"), `Token="`+Token+`"`) {
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
	case path == "/Library/MediaFolders":
		writeJSON(w, map[string]any{"Items": f.Folders})
	case path == "/Items" && r.URL.Query().Has("Ids"):
		f.serveOne(w, r.URL.Query().Get("Ids"))
	case path == "/Items":
		f.servePage(w, r)
	case strings.HasPrefix(path, "/Items/") && strings.HasSuffix(path, "/File"):
		f.serveFile(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/Items/"), "/File"))
	case strings.HasPrefix(path, "/Items/") && strings.HasSuffix(path, "/Images/Primary"):
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("X-Jellyfin-Fake", "1")
		w.Write([]byte("jpeg:" + path + "?" + r.URL.RawQuery))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *Fake) servePage(w http.ResponseWriter, r *http.Request) {
	all := f.Items[r.URL.Query().Get("ParentId")]
	if f.IgnorePaging {
		writeJSON(w, map[string]any{"Items": all})
		return
	}
	start, _ := strconv.Atoi(r.URL.Query().Get("StartIndex"))
	limit, err := strconv.Atoi(r.URL.Query().Get("Limit"))
	if err != nil {
		limit = len(all)
	}
	start = min(start, len(all))
	end := min(start+limit, len(all))
	writeJSON(w, map[string]any{"Items": all[start:end]})
}

func (f *Fake) serveOne(w http.ResponseWriter, id string) {
	for _, items := range f.Items {
		for _, item := range items {
			if item.ID == id {
				writeJSON(w, map[string]any{"Items": []jellyfin.Item{item}})
				return
			}
		}
	}
	writeJSON(w, map[string]any{"Items": []jellyfin.Item{}})
}

func (f *Fake) serveFile(w http.ResponseWriter, r *http.Request, id string) {
	body, ok := f.Files[id]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "audio/flac")
	w.Header().Set("ETag", `"fake-etag"`)
	w.Header().Set("X-Jellyfin-Fake", "1")
	http.ServeContent(w, r, "", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), bytes.NewReader(body))
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(value)
}

const (
	MusicFolder      = "1a0c5aa7b8c94c1d9f8e2b3c4d5e6f70"
	AudiobooksFolder = "2b1d6bb8c9d05d2ea09f3c4d5e6f7081"
	MoviesFolder     = "3c2e7cc9d0e16e3fb1a04d5e6f708192"
	DashedID         = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
)

func Folders() []jellyfin.Folder {
	return []jellyfin.Folder{
		{ID: MoviesFolder, Name: "Movies", CollectionType: "movies"},
		{ID: MusicFolder, Name: "Music", CollectionType: "music"},
		{ID: AudiobooksFolder, Name: "Audiobooks", CollectionType: "music"},
	}
}

func item(id, title string, artists []string, albumArtist, album string, seconds float64, codec, container string, bps int, stream jellyfin.MediaStream) jellyfin.Item {
	stream.Type = "Audio"
	stream.Codec = codec
	return jellyfin.Item{
		ID:                   id,
		Name:                 title,
		RunTimeTicks:         int64(seconds * 10_000_000),
		Artists:              artists,
		AlbumArtist:          albumArtist,
		Album:                album,
		AlbumID:              "a" + id,
		ImageTags:            map[string]string{"Primary": "t" + id},
		AlbumPrimaryImageTag: "ta" + id,
		MediaSources:         []jellyfin.MediaSource{{Container: container, Bitrate: bps, MediaStreams: []jellyfin.MediaStream{stream}}},
	}
}

func Items() map[string][]jellyfin.Item {
	noArt := item("f104", "Stays Four the Same", nil, "The Ready Set", "Stays Four The Same", 201.378, "mp3", "mp3", 251000, jellyfin.MediaStream{})
	noArt.ImageTags, noArt.AlbumPrimaryImageTag = nil, ""
	albumArt := item("f105", "Small Town Girl", nil, "Never Shout Never", "Small Town Girl", 218.047, "aac", "mp4", 320000, jellyfin.MediaStream{SampleRate: 44100})
	albumArt.ImageTags = nil
	dashed := item(DashedID, "Dashed", []string{"Anberlin"}, "Anberlin", "Cities", 200, "flac", "flac", 900000, jellyfin.MediaStream{SampleRate: 44100, BitDepth: 16})
	return map[string][]jellyfin.Item{
		MusicFolder: {
			item("f101", "New Religion", []string{"All Time Low", "Teddy Swims"}, "All Time Low", "Tell Me I’m Alive", 184.054, "flac", "flac", 0, jellyfin.MediaStream{BitRate: 1875000, SampleRate: 48000, BitDepth: 24}),
			item("f102", "Endless Slaughter", nil, "Limp Bizkit", "Endless Slaughter", 336.823, "mp3", "mp3", 320000, jellyfin.MediaStream{SampleRate: 44100}),
			item("f103", "Closer", nil, "Anberlin", "As You Found Me", 230.520, "pcm", "wav", 1411000, jellyfin.MediaStream{SampleRate: 44100, BitDepth: 16}),
			noArt,
			albumArt,
			{ID: "f106", Name: "Broken, No Media"},
			dashed,
		},
		AudiobooksFolder: {
			item("f501", "Chapter One", nil, "Some Author", "Some Book", 1800, "mp3", "mp3", 64000, jellyfin.MediaStream{}),
		},
	}
}

func Files() map[string][]byte {
	return map[string][]byte{
		"f101":   []byte("0123456789abcdefghijklmnopqrstuvwxyz"),
		"f102":   []byte("mp3-bytes"),
		DashedID: []byte("dashed-bytes"),
	}
}
