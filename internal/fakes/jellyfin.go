package fakes

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/jellyfin"
)

const JellyfinToken = "fake-jellyfin-key"

type Jellyfin struct {
	recorder
	URL          string
	Folders      []jellyfin.Folder
	Items        map[string][]jellyfin.Item
	Files        map[string][]byte
	Extra        map[string]http.HandlerFunc
	IgnorePaging bool
}

func NewJellyfin(t testing.TB) *Jellyfin {
	f := &Jellyfin{
		Folders: JellyfinFolders(),
		Items:   JellyfinItems(),
		Files:   JellyfinFiles(),
		Extra:   map[string]http.HandlerFunc{},
	}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	f.URL = server.URL
	return f
}

func (f *Jellyfin) serve(w http.ResponseWriter, r *http.Request) {
	status, rawBody := f.record(r)
	if !strings.Contains(r.Header.Get("Authorization"), `Token="`+JellyfinToken+`"`) {
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
	case path == "/Library/MediaFolders":
		writeJSON(w, map[string]any{"Items": f.Folders})
	case path == "/Items" && r.URL.Query().Has("Ids"):
		f.serveOne(w, r)
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

func (f *Jellyfin) servePage(w http.ResponseWriter, r *http.Request) {
	all := ofType(f.Items[r.URL.Query().Get("ParentId")], r.URL.Query().Get("IncludeItemTypes"))
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

func (f *Jellyfin) serveOne(w http.ResponseWriter, r *http.Request) {
	id, types := r.URL.Query().Get("Ids"), r.URL.Query().Get("IncludeItemTypes")
	for _, items := range f.Items {
		for _, item := range ofType(items, types) {
			if item.ID == id {
				writeJSON(w, map[string]any{"Items": []jellyfin.Item{item}})
				return
			}
		}
	}
	writeJSON(w, map[string]any{"Items": []jellyfin.Item{}})
}

func ofType(items []jellyfin.Item, types string) []jellyfin.Item {
	if types == "" {
		return items
	}
	wanted := strings.Split(types, ",")
	return slices.DeleteFunc(slices.Clone(items), func(item jellyfin.Item) bool {
		return !slices.ContainsFunc(wanted, func(t string) bool { return strings.EqualFold(t, item.Type) })
	})
}

func (f *Jellyfin) serveFile(w http.ResponseWriter, r *http.Request, id string) {
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

const (
	MusicFolder      = "1a0c5aa7b8c94c1d9f8e2b3c4d5e6f70"
	AudiobooksFolder = "2b1d6bb8c9d05d2ea09f3c4d5e6f7081"
	MoviesFolder     = "3c2e7cc9d0e16e3fb1a04d5e6f708192"
	DashedID         = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
	MovieID          = "e201"
)

func JellyfinFolders() []jellyfin.Folder {
	return []jellyfin.Folder{
		{ID: MoviesFolder, Name: "Movies", CollectionType: "movies"},
		{ID: MusicFolder, Name: "Music", CollectionType: "music"},
		{ID: AudiobooksFolder, Name: "Audiobooks", CollectionType: "music"},
	}
}

func jellyfinItem(id, title string, artists []string, albumArtist, album string, seconds float64, codec, container string, bps int, stream jellyfin.MediaStream) jellyfin.Item {
	stream.Type = "Audio"
	stream.Codec = codec
	return jellyfin.Item{
		ID:                   id,
		Type:                 "Audio",
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

func JellyfinItems() map[string][]jellyfin.Item {
	noArt := jellyfinItem("f104", "Stays Four the Same", nil, "The Ready Set", "Stays Four The Same", 201.378, "mp3", "mp3", 251000, jellyfin.MediaStream{})
	noArt.ImageTags, noArt.AlbumPrimaryImageTag = nil, ""
	albumArt := jellyfinItem("f105", "Small Town Girl", nil, "Never Shout Never", "Small Town Girl", 218.047, "aac", "mp4", 320000, jellyfin.MediaStream{SampleRate: 44100})
	albumArt.ImageTags = nil
	dashed := jellyfinItem(DashedID, "Dashed", []string{"Anberlin"}, "Anberlin", "Cities", 200, "flac", "flac", 900000, jellyfin.MediaStream{SampleRate: 44100, BitDepth: 16})
	return map[string][]jellyfin.Item{
		MusicFolder: {
			jellyfinItem("f101", "New Religion", []string{"All Time Low", "Teddy Swims"}, "All Time Low", "Tell Me I’m Alive", 184.054, "flac", "flac", 0, jellyfin.MediaStream{BitRate: 1875000, SampleRate: 48000, BitDepth: 24}),
			jellyfinItem("f102", "Endless Slaughter", nil, "Limp Bizkit", "Endless Slaughter", 336.823, "mp3", "mp3", 320000, jellyfin.MediaStream{SampleRate: 44100}),
			jellyfinItem("f103", "Closer", nil, "Anberlin", "As You Found Me", 230.520, "pcm", "wav", 1411000, jellyfin.MediaStream{SampleRate: 44100, BitDepth: 16}),
			noArt,
			albumArt,
			{ID: "f106", Type: "Audio", Name: "Broken, No Media"},
			dashed,
		},
		AudiobooksFolder: {
			jellyfinItem("f501", "Chapter One", nil, "Some Author", "Some Book", 1800, "mp3", "mp3", 64000, jellyfin.MediaStream{}),
		},
		MoviesFolder: {
			{ID: MovieID, Type: "Movie", Name: "Some Movie", RunTimeTicks: 5400 * 10_000_000, MediaSources: []jellyfin.MediaSource{{Container: "mkv", Bitrate: 8000000, MediaStreams: []jellyfin.MediaStream{{Type: "Audio", Codec: "aac", SampleRate: 48000}}}}},
		},
	}
}

func JellyfinFiles() map[string][]byte {
	return map[string][]byte{
		"f101":   []byte("0123456789abcdefghijklmnopqrstuvwxyz"),
		"f102":   []byte("mp3-bytes"),
		DashedID: []byte("dashed-bytes"),
		MovieID:  []byte("movie-bytes"),
	}
}
