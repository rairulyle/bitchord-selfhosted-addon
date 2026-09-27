package fakes

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const (
	PlexTVToken    = "fake-account-token"
	PlexTVUsername = "lyle"
)

// PlexTV stands in for plex.tv: it hands out PINs, reports a PIN as claimed
// once the test says so, and lists the account's servers.
type PlexTV struct {
	recorder
	URL       string
	Resources []map[string]any
	mu        sync.Mutex
	nextPIN   int
	claimed   map[int]bool
}

func NewPlexTV(t testing.TB) *PlexTV {
	dead := DeadURL(t)
	f := &PlexTV{claimed: map[int]bool{}, Resources: PlexTVResources(dead, dead)}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	f.URL = server.URL
	return f
}

// Claim marks a PIN as signed in, the way app.plex.tv would.
func (f *PlexTV) Claim(id int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claimed[id] = true
}

func (f *PlexTV) serve(w http.ResponseWriter, r *http.Request) {
	status, rawBody := f.record(r)
	if r.Header.Get("X-Plex-Client-Identifier") == "" || r.Header.Get("X-Plex-Product") == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if f.override(w, status, rawBody) {
		return
	}
	switch path := r.URL.Path; {
	case path == "/api/v2/pins" && r.Method == http.MethodPost:
		f.mu.Lock()
		f.nextPIN++
		id := f.nextPIN
		f.mu.Unlock()
		writeJSON(w, map[string]any{"id": id, "code": "CODE" + strconv.Itoa(id), "authToken": nil})
	case strings.HasPrefix(path, "/api/v2/pins/"):
		id, err := strconv.Atoi(strings.TrimPrefix(path, "/api/v2/pins/"))
		f.mu.Lock()
		claimed, known := f.claimed[id], err == nil && id <= f.nextPIN
		f.mu.Unlock()
		if !known {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		answer := map[string]any{"id": id, "code": "CODE" + strconv.Itoa(id), "authToken": nil}
		if claimed {
			answer["authToken"] = PlexTVToken
		}
		writeJSON(w, answer)
	case r.Header.Get("X-Plex-Token") != PlexTVToken:
		w.WriteHeader(http.StatusUnauthorized)
	case path == "/api/v2/user":
		writeJSON(w, map[string]any{"username": PlexTVUsername})
	case path == "/api/v2/resources":
		writeJSON(w, f.Resources)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// PlexTVResources lists two servers and a player: "Home" at homeURL behind
// a dead local address, and "Parents" behind dead addresses only.
func PlexTVResources(homeURL, deadURL string) []map[string]any {
	return []map[string]any{
		{"name": "Living Room TV", "provides": "player", "connections": []map[string]any{}},
		{"name": "Home", "provides": "server", "connections": []map[string]any{
			{"uri": "https://1-2-3-4.abc.plex.direct:32400", "local": false, "relay": false},
			{"uri": "https://relay.plex.direct:8443", "local": false, "relay": true},
			{"uri": deadURL, "local": true, "relay": false},
			{"uri": homeURL, "local": true, "relay": false},
		}},
		{"name": "Parents", "provides": "server", "connections": []map[string]any{
			{"uri": deadURL, "local": true, "relay": false},
		}},
	}
}
