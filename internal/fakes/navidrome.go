package fakes

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	NavidromeUser            = "lyle"
	NavidromePassword        = "correct horse"
	NavidromeToken           = "lyle:correct horse"
	NavidromeMusicFolder     = "1"
	NavidromeMusicFolderName = "Music"
)

type Navidrome struct {
	recorder
	URL            string
	DisableSearch3 bool
	Extra          map[string]http.HandlerFunc
	server         *httptest.Server
}

func NewNavidrome(t testing.TB) *Navidrome {
	f := &Navidrome{
		Extra: map[string]http.HandlerFunc{},
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	f.URL = f.server.URL
	return f
}

func (f *Navidrome) serve(w http.ResponseWriter, r *http.Request) {
	status, rawBody := f.record(r)
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	if rawBody != "" {
		w.Write([]byte(rawBody))
		return
	}
	if extra, ok := f.Extra[r.URL.Path]; ok {
		extra(w, r)
		return
	}

	q := r.URL.Query()
	if r.URL.Path == "/rest/stream.view" {
		if !f.authorized(q) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "audio/flac")
		w.Write([]byte("FAKEAUDIOBYTES"))
		return
	}
	if r.URL.Path == "/rest/getCoverArt.view" {
		if !f.authorized(q) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte("FAKEIMAGEBYTES"))
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/rest/") {
		http.NotFound(w, r)
		return
	}
	if !f.authorized(q) {
		writeSub(w, map[string]any{
			"status": "failed",
			"error":  map[string]any{"code": 40, "message": "Wrong username or password."},
		})
		return
	}

	method := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/rest/"), ".view")
	switch method {
	case "ping":
		writeSub(w, map[string]any{"status": "ok", "version": "1.16.1"})
	case "getMusicFolders":
		writeSub(w, map[string]any{
			"status": "ok",
			"musicFolders": map[string]any{
				"musicFolder": []map[string]any{{"id": 1, "name": NavidromeMusicFolderName}},
			},
		})
	case "search3":
		if f.DisableSearch3 {
			writeSub(w, map[string]any{
				"status": "failed",
				"error":  map[string]any{"code": 10, "message": "Required parameter is missing: query"},
			})
			return
		}
		offset := atoi(q.Get("songOffset"))
		count := atoi(q.Get("songCount"))
		if count <= 0 {
			count = 20
		}
		all := navidromeAllSongs()
		var paged []map[string]any
		if offset < len(all) {
			end := offset + count
			if end > len(all) {
				end = len(all)
			}
			paged = all[offset:end]
		}
		writeSub(w, map[string]any{
			"status": "ok",
			"searchResult3": map[string]any{
				"song": paged,
			},
		})
	case "getAlbumList2":
		offset := atoi(q.Get("offset"))
		albums := []map[string]any{
			{"id": "al-1", "name": "Tell Me I’m Alive", "artist": "All Time Low"},
			{"id": "al-2", "name": "Endless Slaughter", "artist": "Limp Bizkit"},
		}
		if offset > 0 {
			albums = nil
		}
		writeSub(w, map[string]any{"status": "ok", "albumList2": map[string]any{"album": albums}})
	case "getAlbum":
		id := q.Get("id")
		songs := map[string][]map[string]any{
			"al-1": {
				{
					"id":                 "s-1",
					"title":              "New Religion",
					"album":              "Tell Me I’m Alive",
					"artist":             "All Time Low",
					"displayAlbumArtist": "All Time Low",
					"duration":           184,
					"suffix":             "flac",
					"bitRate":            1875,
					"samplingRate":       48000,
					"bitDepth":           24,
					"coverArt":           "al-1",
				},
			},
			"al-2": {
				{
					"id":           "s-2",
					"title":        "Endless Slaughter",
					"album":        "Endless Slaughter",
					"artist":       "Limp Bizkit",
					"duration":     336,
					"suffix":       "mp3",
					"bitRate":      320,
					"samplingRate": 44100,
					"bitDepth":     16,
				},
				{
					"id":           "s-3",
					"title":        "Small Town Girl",
					"album":        "Small Town Girl",
					"artist":       "Never Shout Never",
					"duration":     218,
					"suffix":       "m4a",
					"contentType":  "audio/mp4",
					"bitRate":      256,
					"samplingRate": 44100,
					"bitDepth":     16,
				},
			},
		}[id]
		if songs == nil {
			writeSub(w, map[string]any{"status": "failed", "error": map[string]any{"code": 70, "message": "not found"}})
			return
		}
		writeSub(w, map[string]any{"status": "ok", "album": map[string]any{"song": songs}})
	case "getSong":
		id := q.Get("id")
		for _, s := range navidromeAllSongs() {
			if s["id"] == id {
				writeSub(w, map[string]any{"status": "ok", "song": s})
				return
			}
		}
		writeSub(w, map[string]any{"status": "failed", "error": map[string]any{"code": 70, "message": "not found"}})
	case "scrobble":
		writeSub(w, map[string]any{"status": "ok"})
	default:
		writeSub(w, map[string]any{"status": "failed", "error": map[string]any{"code": 0, "message": "unknown method"}})
	}
}

func (f *Navidrome) authorized(q map[string][]string) bool {
	get := func(k string) string {
		if v, ok := q[k]; ok && len(v) > 0 {
			return v[0]
		}
		return ""
	}
	if get("u") != NavidromeUser {
		return false
	}
	salt := get("s")
	want := get("t")
	sum := md5.Sum([]byte(NavidromePassword + salt))
	return hex.EncodeToString(sum[:]) == want
}

func writeSub(w http.ResponseWriter, inner map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"subsonic-response": inner})
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func navidromeAllSongs() []map[string]any {
	return []map[string]any{
		{
			"id":                 "s-1",
			"title":              "New Religion",
			"album":              "Tell Me I’m Alive",
			"artist":             "All Time Low",
			"displayAlbumArtist": "All Time Low",
			"duration":           184,
			"suffix":             "flac",
			"bitRate":            1875,
			"samplingRate":       48000,
			"bitDepth":           24,
			"coverArt":           "al-1",
		},
		{
			"id":           "s-2",
			"title":        "Endless Slaughter",
			"album":        "Endless Slaughter",
			"artist":       "Limp Bizkit",
			"duration":     336,
			"suffix":       "mp3",
			"bitRate":      320,
			"samplingRate": 44100,
			"bitDepth":     16,
		},
		{
			"id":           "s-3",
			"title":        "Small Town Girl",
			"album":        "Small Town Girl",
			"artist":       "Never Shout Never",
			"duration":     218,
			"suffix":       "m4a",
			"contentType":  "audio/mp4",
			"bitRate":      256,
			"samplingRate": 44100,
			"bitDepth":     16,
		},
	}
}
