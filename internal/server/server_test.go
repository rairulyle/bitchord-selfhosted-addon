package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/fakes"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/jellyfin"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/library"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/plex"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/registry"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/store"
)

const (
	testSecret   = "s3cr3t-s3cr3t-s3cr3t"
	testPublic   = "https://music.example.com"
	testBase     = testPublic + "/plex-1/" + testSecret
	jellyfinBase = testPublic + "/jellyfin-1/" + testSecret
)

// stubRegistry holds entries built by hand, so a test decides whether an
// index is loaded instead of waiting on a refresher.
type stubRegistry struct {
	mu      sync.Mutex
	entries map[string]*registry.Entry
}

func (s *stubRegistry) Lookup(slug string) (*registry.Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[slug]
	return entry, ok
}

func (s *stubRegistry) Healthy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, entry := range s.entries {
		if !entry.Library.Ready() {
			return false
		}
	}
	return true
}

func newEntry(t *testing.T, slug string, kind store.Kind, label string, backend media.Backend, log *slog.Logger, load bool) *registry.Entry {
	t.Helper()
	log = log.With("server", slug, "source", string(kind))
	lib := library.NewLibrary(backend, "Music", time.Hour, log)
	if load {
		if err := lib.Refresh(context.Background()); err != nil {
			t.Fatalf("Refresh: %v", err)
		}
	}
	return &registry.Entry{Slug: slug, Kind: kind, Label: label, Backend: backend, Library: lib, Log: log}
}

func newServerWith(log *slog.Logger, entries ...*registry.Entry) *server {
	stub := &stubRegistry{entries: map[string]*registry.Entry{}}
	for _, entry := range entries {
		stub.entries[entry.Slug] = entry
	}
	return newServer(Options{
		Site:     func() Site { return Site{PublicURL: testPublic, Secret: testSecret} },
		Registry: stub, Version: "1.2.3", Log: log,
	})
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type harness struct {
	fake    *fakes.Plex
	entry   *registry.Entry
	server  *server
	handler http.Handler
	logs    *syncBuffer
}

func newHarness(t *testing.T) *harness {
	return newHarnessWith(t, plex.Options{}, true)
}

func newHarnessWith(t *testing.T, options plex.Options, load bool) *harness {
	t.Helper()
	fake := fakes.NewPlex(t)
	options.BaseURL = fake.URL
	if options.Token == "" {
		options.Token = fakes.PlexToken
	}
	logs := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	options.Log = log
	entry := newEntry(t, "plex-1", store.Plex, "Home", plex.New(options), log, load)
	s := newServerWith(log, entry)
	return &harness{fake: fake, entry: entry, server: s, handler: s.handler(), logs: logs}
}

func do(handler http.Handler, method, path string, header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for key, values := range header {
		req.Header[key] = values
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func (h *harness) do(method, path string, header http.Header) *httptest.ResponseRecorder {
	return do(h.handler, method, path, header)
}

func (h *harness) get(path string) *httptest.ResponseRecorder {
	return h.do(http.MethodGet, path, nil)
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, rec.Body.String())
	}
	return out
}

func TestManifest(t *testing.T) {
	h := newHarness(t)
	rec := h.get("/plex-1/" + testSecret + "/manifest.json")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status %d, type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	got := decode[map[string]any](t, rec)
	want := map[string]any{
		"id": "app.bitchord-selfhosted-addon.plex-1", "name": "Plex - Home", "version": "1.2.3",
		"description": "Your Plex music library", "contentType": "music",
		"resources": []any{"search", "stream"}, "types": []any{"track"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("manifest = %v", got)
	}
	h.entry.Label = ""
	if got := decode[map[string]any](t, h.get("/plex-1/"+testSecret+"/manifest.json")); got["name"] != "Plex" {
		t.Errorf("name without a label = %v, want the kind alone", got["name"])
	}
}

func TestTwoServersUnderOneSecretAnswerSeparately(t *testing.T) {
	plexFake, jellyfinFake := fakes.NewPlex(t), fakes.NewJellyfin(t)
	logs := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	plexEntry := newEntry(t, "plex-1", store.Plex, "Home", plex.New(plex.Options{BaseURL: plexFake.URL, Token: fakes.PlexToken}), log, true)
	jellyfinEntry := newEntry(t, "jellyfin-1", store.Jellyfin, "", jellyfin.New(jellyfin.Options{BaseURL: jellyfinFake.URL, APIKey: fakes.JellyfinToken}), log, true)
	handler := newServerWith(log, plexEntry, jellyfinEntry).handler()
	plexManifest := decode[map[string]any](t, do(handler, http.MethodGet, "/plex-1/"+testSecret+"/manifest.json", nil))
	jellyfinManifest := decode[map[string]any](t, do(handler, http.MethodGet, "/jellyfin-1/"+testSecret+"/manifest.json", nil))
	if plexManifest["id"] != "app.bitchord-selfhosted-addon.plex-1" || jellyfinManifest["id"] != "app.bitchord-selfhosted-addon.jellyfin-1" || jellyfinManifest["name"] != "Jellyfin" {
		t.Fatalf("manifests = %v, %v", plexManifest, jellyfinManifest)
	}
	if rec := do(handler, http.MethodGet, "/plex-1/"+testSecret+"/stream/101", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), testBase+"/file/101") {
		t.Fatalf("plex stream: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(handler, http.MethodGet, "/jellyfin-1/"+testSecret+"/stream/f101", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), jellyfinBase+"/file/f101") {
		t.Fatalf("jellyfin stream: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(handler, http.MethodGet, "/jellyfin-1/"+testSecret+"/stream/101", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("a plex id on the jellyfin source answered %d", rec.Code)
	}
	if logs := logs.String(); !strings.Contains(logs, `"server":"plex-1"`) || !strings.Contains(logs, `"server":"jellyfin-1"`) {
		t.Fatalf("logs lack the server slugs:\n%s", logs)
	}
}

func TestWrongSecretAndUnknownRoutesAnswerTheSameEmpty404(t *testing.T) {
	h := newHarness(t)
	reference := h.get("/plex-1/not-the-secret-at-all/manifest.json")
	if reference.Code != http.StatusNotFound || reference.Body.Len() != 0 {
		t.Fatalf("wrong secret: status %d, body %q", reference.Code, reference.Body.String())
	}
	requests := map[string][2]string{
		"wrong secret search":   {http.MethodGet, "/plex-1/wrong/search?q=closer"},
		"wrong secret stream":   {http.MethodGet, "/plex-1/wrong/stream/101"},
		"wrong secret file":     {http.MethodGet, "/plex-1/wrong/file/101"},
		"wrong secret art":      {http.MethodGet, "/plex-1/wrong/art/101"},
		"wrong secret options":  {http.MethodOptions, "/plex-1/wrong/search"},
		"secret as a prefix":    {http.MethodGet, "/plex-1/" + testSecret + "x/manifest.json"},
		"wrong slug":            {http.MethodGet, "/plex-9/" + testSecret + "/manifest.json"},
		"slug of another kind":  {http.MethodGet, "/jellyfin-1/" + testSecret + "/manifest.json"},
		"reserved word as slug": {http.MethodGet, "/setup/" + testSecret + "/manifest.json"},
		"health as slug":        {http.MethodGet, "/health/" + testSecret + "/manifest.json"},
		"the 0.4 url shape":     {http.MethodGet, "/" + testSecret + "/manifest.json"},
		"secret before slug":    {http.MethodGet, "/" + testSecret + "/plex-1/manifest.json"},
		"slug alone":            {http.MethodGet, "/plex-1"},
		"slug and secret alone": {http.MethodGet, "/plex-1/" + testSecret},
		"unknown route":         {http.MethodGet, "/plex-1/" + testSecret + "/nope"},
		"unknown nested route":  {http.MethodGet, "/plex-1/" + testSecret + "/search/extra"},
		"root":                  {http.MethodGet, "/"},
		"wrong method":          {http.MethodPost, "/plex-1/" + testSecret + "/search"},
		"the old healthz path":  {http.MethodGet, "/healthz"},
		"health under secret":   {http.MethodGet, "/plex-1/" + testSecret + "/health"},
		"track id with a slash": {http.MethodGet, "/plex-1/" + testSecret + "/stream/1%2F2"},
		"track id with a dot":   {http.MethodGet, "/plex-1/" + testSecret + "/stream/1.2"},
		"non hex track id":      {http.MethodGet, "/plex-1/" + testSecret + "/stream/xyz"},
		"overlong track id":     {http.MethodGet, "/plex-1/" + testSecret + "/file/" + strings.Repeat("a", 37)},
		"unknown track in plex": {http.MethodGet, "/plex-1/" + testSecret + "/stream/999"},
		"double slash secret":   {http.MethodGet, "//plex-1/" + testSecret + "/manifest.json"},
		"dot segment secret":    {http.MethodGet, "/./plex-1/" + testSecret + "/manifest.json"},
		"dot dot secret":        {http.MethodGet, "/plex-1/" + testSecret + "/../" + testSecret + "/manifest.json"},
		"trailing slash secret": {http.MethodGet, "/plex-1/" + testSecret + "/"},
		"double slash unknown":  {http.MethodGet, "//nope"},
	}
	for name, request := range requests {
		t.Run(name, func(t *testing.T) {
			rec := h.do(request[0], request[1], nil)
			if rec.Code != http.StatusNotFound || rec.Body.Len() != 0 {
				t.Fatalf("status %d, body %q", rec.Code, rec.Body.String())
			}
			if !reflect.DeepEqual(rec.Header(), reference.Header()) {
				t.Fatalf("headers differ from a wrong-secret answer:\n%v\n%v", rec.Header(), reference.Header())
			}
		})
	}
}

func TestCORS(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/plex-1/" + testSecret + "/manifest.json", "/plex-1/wrong/manifest.json", "/health"} {
		if got := h.get(path).Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("%s: Access-Control-Allow-Origin = %q", path, got)
		}
	}
	rec := h.do(http.MethodOptions, "/plex-1/"+testSecret+"/file/101", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got != "GET, HEAD, OPTIONS" {
		t.Errorf("Allow-Methods = %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(got, "Range") {
		t.Errorf("Allow-Headers = %q", got)
	}
	if got := rec.Header().Get("Access-Control-Expose-Headers"); !strings.Contains(got, "Content-Range") {
		t.Errorf("Expose-Headers = %q", got)
	}
}

func TestSearchMapsTracks(t *testing.T) {
	h := newHarness(t)
	rec := h.get("/plex-1/" + testSecret + "/search?q=New+Religion+Teddy+Swims&quality=LOW")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	got := decode[map[string]any](t, rec)
	want := map[string]any{
		"tracks": []any{map[string]any{
			"id": "101", "title": "New Religion", "artist": "All Time Low feat. Teddy Swims", "album": "Tell Me I’m Alive",
			"duration": float64(184), "artworkURL": testBase + "/art/101", "format": "flac", "audioQuality": "LOSSLESS",
		}},
		"albums": []any{}, "artists": []any{}, "playlists": []any{},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("search = %v", got)
	}
}

func TestSearchQualityFormatAndArtwork(t *testing.T) {
	h := newHarness(t)
	cases := map[string]struct {
		query, format, quality string
		artwork                bool
	}{
		"mp3 falls back to album artist": {"endless slaughter limp bizkit", "mp3", "HIGH", true},
		"pcm in wav is reported as wav":  {"closer anberlin", "wav", "LOSSLESS", true},
		"album thumb is enough":          {"small town girl", "aac", "HIGH", true},
		"no thumb, no artworkURL":        {"stays four the same", "mp3", "HIGH", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := h.get("/plex-1/" + testSecret + "/search?q=" + strings.ReplaceAll(tc.query, " ", "+"))
			tracks := decode[map[string][]map[string]any](t, rec)["tracks"]
			if len(tracks) != 1 {
				t.Fatalf("tracks = %v", tracks)
			}
			track := tracks[0]
			if track["format"] != tc.format || track["audioQuality"] != tc.quality {
				t.Errorf("format %v, quality %v", track["format"], track["audioQuality"])
			}
			if _, has := track["artworkURL"]; has != tc.artwork {
				t.Errorf("artworkURL present = %v", has)
			}
		})
	}
}

func TestSearchAnswersEmptyArraysNeverNull(t *testing.T) {
	const empty = `{"tracks":[],"albums":[],"artists":[],"playlists":[]}`
	loaded := newHarness(t)
	notLoaded := newHarnessWith(t, plex.Options{}, false)
	cases := map[string]*httptest.ResponseRecorder{
		"blank query":      loaded.get("/plex-1/" + testSecret + "/search?q="),
		"missing query":    loaded.get("/plex-1/" + testSecret + "/search"),
		"no match":         loaded.get("/plex-1/" + testSecret + "/search?q=zzzz"),
		"index not loaded": notLoaded.get("/plex-1/" + testSecret + "/search?q=closer"),
	}
	for name, rec := range cases {
		if got := strings.TrimSpace(rec.Body.String()); rec.Code != http.StatusOK || got != empty {
			t.Errorf("%s: status %d, body %s", name, rec.Code, got)
		}
	}
}

func TestHealth(t *testing.T) {
	if rec := newHarnessWith(t, plex.Options{}, false).get("/health"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("before the first load: %d", rec.Code)
	}
	if rec := newHarness(t).get("/health"); rec.Code != http.StatusOK {
		t.Errorf("after the first load: %d", rec.Code)
	}
}

func TestLogsRedactTheSecretAndNeverCarryTheToken(t *testing.T) {
	h := newHarness(t)
	h.get("/plex-1/" + testSecret + "/search?q=closer")
	h.get("/plex-1/" + testSecret + "/stream/101")
	h.get("/health")
	h.get("//plex-1/" + testSecret + "/manifest.json")
	h.get("/./plex-1/" + testSecret + "/search?q=closer")
	h.get("/" + testSecret + "/search?q=closer")
	h.get("/health/" + testSecret + "/manifest.json")
	h.get("/Plex-1/" + testSecret + "/search")
	logs := h.logs.String()
	if strings.Contains(logs, testSecret) || strings.Contains(logs, fakes.PlexToken) {
		t.Fatalf("logs leak a credential:\n%s", logs)
	}
	for _, want := range []string{
		`"path":"/plex-1/***/search"`, `"path":"/plex-1/***/stream/101"`, `"path":"/***/search"`, `"path":"/health"`, `"status":200`,
		`"path":"/***/***/manifest.json"`, `"path":"/***/***/search"`,
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %s:\n%s", want, logs)
		}
	}
}

func TestRedact(t *testing.T) {
	cases := map[string]string{
		"/health": "/health", "/": "/", "/abc": "/***", "/abc/search": "/***/search", "/abc/file/12": "/***/file/12",
		"//x/y": "/***", "/./x/y": "/***", "/x/../y": "/***", "/x/": "/***",
		"/plex-1/abc/search": "/plex-1/***/search", "/jellyfin-2/abc/file/12": "/jellyfin-2/***/file/12",
		"/plex-1/abc": "/plex-1/***", "/plex-1": "/***", "/setup/login": "/***/login",
	}
	for in, want := range cases {
		if got := redact(in, ""); got != want {
			t.Errorf("redact(%q, \"\") = %q, want %q", in, got, want)
		}
	}

	const secret = "topsecret"
	withSecret := map[string]string{
		"/health/" + secret + "/manifest.json": "/***/***/manifest.json",
		"/setup/" + secret + "/login":          "/***/***/login",
		"/Plex-1/" + secret + "/search":        "/***/***/search",
		"/plex/" + secret + "/search":          "/***/***/search",
	}
	for in, want := range withSecret {
		if got := redact(in, secret); got != want || strings.Contains(got, secret) {
			t.Errorf("redact(%q, %q) = %q, want %q", in, secret, got, want)
		}
	}
}

func TestSearchesAreLoggedWithTheirOutcome(t *testing.T) {
	h := newHarness(t)
	h.get("/plex-1/" + testSecret + "/search?q=New+Religion+Teddy+Swims")
	h.get("/plex-1/" + testSecret + "/search?q=zzzz")
	h.get("/plex-1/" + testSecret + "/search?q=" + strings.Repeat("a", 500))
	logs := h.logs.String()
	for _, want := range []string{
		`"msg":"search","server":"plex-1","source":"plex","q":"New Religion Teddy Swims","strict":1,"fallback":0,"returned":1,"top":"New Religion — All Time Low feat. Teddy Swims"`,
		`"msg":"search miss","server":"plex-1","source":"plex","q":"zzzz","strict":0,"fallback":0,"returned":0`,
		`"q":"` + strings.Repeat("a", 200) + `…"`,
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %s:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, testSecret) {
		t.Error("the secret reached the logs")
	}
}

func TestRequestLinesAreDebugOnly(t *testing.T) {
	fake := fakes.NewPlex(t)
	logs := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	entry := newEntry(t, "plex-1", store.Plex, "", plex.New(plex.Options{BaseURL: fake.URL, Token: fakes.PlexToken}), log, true)
	handler := newServerWith(log, entry).handler()
	req := httptest.NewRequest(http.MethodGet, "/plex-1/"+testSecret+"/manifest.json", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)
	if strings.Contains(logs.String(), `"msg":"request"`) {
		t.Fatalf("request line logged at info:\n%s", logs.String())
	}
}
