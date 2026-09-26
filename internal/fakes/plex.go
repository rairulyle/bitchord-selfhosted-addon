package fakes

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/plex"
)

const PlexToken = "fake-plex-token"

type Plex struct {
	recorder
	URL          string
	Sections     []plex.Section
	Tracks       map[string][]plex.Track
	Files        map[string][]byte
	Extra        map[string]http.HandlerFunc
	IgnorePaging bool
}

func NewPlex(t testing.TB) *Plex {
	f := &Plex{
		Sections: PlexSections(),
		Tracks:   PlexTracks(),
		Files:    PlexFiles(),
		Extra:    map[string]http.HandlerFunc{},
	}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	f.URL = server.URL
	return f
}

func (f *Plex) serve(w http.ResponseWriter, r *http.Request) {
	status, rawBody := f.record(r)
	if r.Header.Get("X-Plex-Token") != PlexToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if f.override(w, status, rawBody) {
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

func (f *Plex) servePage(w http.ResponseWriter, r *http.Request, key string) {
	all := f.Tracks[key]
	if f.IgnorePaging {
		writeJSON(w, map[string]any{"MediaContainer": map[string]any{
			"totalSize": len(all),
			"offset":    0,
			"Metadata":  all,
		}})
		return
	}
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

func (f *Plex) serveTrack(w http.ResponseWriter, id string) {
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

func (f *Plex) serveFile(w http.ResponseWriter, r *http.Request) {
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

func PlexSections() []plex.Section {
	return []plex.Section{
		{Key: "1", Type: "movie", Title: "Movies"},
		{Key: "3", Type: "artist", Title: "Music"},
		{Key: "5", Type: "artist", Title: "Audiobooks"},
	}
}

func plexTrack(id, title, artist, albumArtist, album string, ms int, codec, container string, kbps int, stream plex.Stream) plex.Track {
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

func PlexTracks() map[string][]plex.Track {
	noThumb := plexTrack("104", "Stays Four the Same", "", "The Ready Set", "Stays Four The Same", 201378, "mp3", "mp3", 251, plex.Stream{})
	noThumb.Thumb, noThumb.ParentThumb = "", ""
	albumThumb := plexTrack("105", "Small Town Girl", "", "Never Shout Never", "Small Town Girl", 218047, "aac", "mp4", 320, plex.Stream{SamplingRate: 44100})
	albumThumb.Thumb = ""
	return map[string][]plex.Track{
		"3": {
			plexTrack("101", "New Religion", "All Time Low feat. Teddy Swims", "All Time Low", "Tell Me I’m Alive", 184054, "flac", "flac", 1875, plex.Stream{SamplingRate: 48000, BitDepth: 24}),
			plexTrack("102", "Endless Slaughter", "", "Limp Bizkit", "Endless Slaughter", 336823, "mp3", "mp3", 320, plex.Stream{SamplingRate: 44100}),
			plexTrack("103", "Closer", "", "Anberlin", "As You Found Me", 230520, "pcm", "wav", 1411, plex.Stream{SamplingRate: 44100, BitDepth: 16}),
			noThumb,
			albumThumb,
			{RatingKey: "106", Title: "Broken, No Media"},
		},
		"5": {
			plexTrack("501", "Chapter One", "", "Some Author", "Some Book", 1800000, "mp3", "mp3", 64, plex.Stream{}),
		},
	}
}

func PlexFiles() map[string][]byte {
	return map[string][]byte{
		"/library/parts/101/1/file.flac": []byte("0123456789abcdefghijklmnopqrstuvwxyz"),
		"/library/parts/102/1/file.mp3":  []byte("mp3-bytes"),
	}
}
