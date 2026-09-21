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

	"github.com/rairulyle/eclipse-plex-addon/internal/library"
	"github.com/rairulyle/eclipse-plex-addon/internal/plex"
	"github.com/rairulyle/eclipse-plex-addon/internal/plextest"
)

const (
	testSecret = "s3cr3t-s3cr3t-s3cr3t"
	testPublic = "https://music.example.com"
	testBase   = testPublic + "/" + testSecret
)

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
	fake    *plextest.Fake
	server  *server
	handler http.Handler
	logs    *syncBuffer
}

func newHarness(t *testing.T) *harness {
	return newHarnessWith(t, plex.Options{}, true)
}

func newHarnessWith(t *testing.T, options plex.Options, load bool) *harness {
	t.Helper()
	fake := plextest.New(t)
	options.BaseURL, options.Token = fake.URL, plextest.Token
	client := plex.New(options)
	logs := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	lib := library.NewLibrary(client, "Music", time.Hour, log)
	if load {
		if err := lib.Refresh(context.Background()); err != nil {
			t.Fatalf("Refresh: %v", err)
		}
	}
	s := newServer(Options{
		Secret: testSecret, PublicURL: testPublic, AddonName: "Home Plex", Version: "1.2.3",
		Library: lib, Plex: client, Log: log,
	})
	return &harness{fake: fake, server: s, handler: s.handler(), logs: logs}
}

func (h *harness) do(method, path string, header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for key, values := range header {
		req.Header[key] = values
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
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
	rec := newHarness(t).get("/" + testSecret + "/manifest.json")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status %d, type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	got := decode[map[string]any](t, rec)
	want := map[string]any{
		"id": "app.eclipse-plex-addon", "name": "Home Plex", "version": "1.2.3",
		"description": "Your Plex music library", "contentType": "music",
		"resources": []any{"search", "stream"}, "types": []any{"track"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("manifest = %v", got)
	}
}

func TestWrongSecretAndUnknownRoutesAnswerTheSameEmpty404(t *testing.T) {
	h := newHarness(t)
	reference := h.get("/not-the-secret-at-all/manifest.json")
	if reference.Code != http.StatusNotFound || reference.Body.Len() != 0 {
		t.Fatalf("wrong secret: status %d, body %q", reference.Code, reference.Body.String())
	}
	requests := map[string][2]string{
		"wrong secret search":   {http.MethodGet, "/wrong/search?q=tum"},
		"wrong secret stream":   {http.MethodGet, "/wrong/stream/101"},
		"wrong secret file":     {http.MethodGet, "/wrong/file/101"},
		"wrong secret art":      {http.MethodGet, "/wrong/art/101"},
		"wrong secret options":  {http.MethodOptions, "/wrong/search"},
		"secret as a prefix":    {http.MethodGet, "/" + testSecret + "x/manifest.json"},
		"unknown route":         {http.MethodGet, "/" + testSecret + "/nope"},
		"unknown nested route":  {http.MethodGet, "/" + testSecret + "/search/extra"},
		"root":                  {http.MethodGet, "/"},
		"wrong method":          {http.MethodPost, "/" + testSecret + "/search"},
		"healthz under secret":  {http.MethodGet, "/" + testSecret + "/healthz"},
		"non numeric track id":  {http.MethodGet, "/" + testSecret + "/stream/abc"},
		"overlong track id":     {http.MethodGet, "/" + testSecret + "/file/123456789012345678901"},
		"unknown track in plex": {http.MethodGet, "/" + testSecret + "/stream/999"},
		"double slash secret":   {http.MethodGet, "//" + testSecret + "/manifest.json"},
		"dot segment secret":    {http.MethodGet, "/./" + testSecret + "/manifest.json"},
		"dot dot secret":        {http.MethodGet, "/" + testSecret + "/../" + testSecret + "/manifest.json"},
		"trailing slash secret": {http.MethodGet, "/" + testSecret + "/"},
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
	for _, path := range []string{"/" + testSecret + "/manifest.json", "/wrong/manifest.json", "/healthz"} {
		if got := h.get(path).Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("%s: Access-Control-Allow-Origin = %q", path, got)
		}
	}
	rec := h.do(http.MethodOptions, "/"+testSecret+"/file/101", nil)
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
	rec := h.get("/" + testSecret + "/search?q=Paniyon+Sa+Atif+Aslam&quality=LOW")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	got := decode[map[string]any](t, rec)
	want := map[string]any{
		"tracks": []any{map[string]any{
			"id": "101", "title": "Paniyon Sa", "artist": "Atif Aslam, Tulsi Kumar", "album": "Satyameva Jayate",
			"duration": float64(249), "artworkURL": testBase + "/art/101", "format": "flac", "audioQuality": "LOSSLESS",
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
		"mp3 falls back to album artist": {"tum hi ho arijit singh", "mp3", "HIGH", true},
		"pcm in wav is reported as wav":  {"home michael buble", "wav", "LOSSLESS", true},
		"album thumb is enough":          {"album cover only", "aac", "HIGH", true},
		"no thumb, no artworkURL":        {"no cover", "mp3", "HIGH", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := h.get("/" + testSecret + "/search?q=" + strings.ReplaceAll(tc.query, " ", "+"))
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
		"blank query":      loaded.get("/" + testSecret + "/search?q="),
		"missing query":    loaded.get("/" + testSecret + "/search"),
		"no match":         loaded.get("/" + testSecret + "/search?q=zzzz"),
		"index not loaded": notLoaded.get("/" + testSecret + "/search?q=tum"),
	}
	for name, rec := range cases {
		if got := strings.TrimSpace(rec.Body.String()); rec.Code != http.StatusOK || got != empty {
			t.Errorf("%s: status %d, body %s", name, rec.Code, got)
		}
	}
}

func TestHealthz(t *testing.T) {
	if rec := newHarnessWith(t, plex.Options{}, false).get("/healthz"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("before the first load: %d", rec.Code)
	}
	if rec := newHarness(t).get("/healthz"); rec.Code != http.StatusOK {
		t.Errorf("after the first load: %d", rec.Code)
	}
}

func TestLogsRedactTheSecretAndNeverCarryTheToken(t *testing.T) {
	h := newHarness(t)
	h.get("/" + testSecret + "/search?q=tum")
	h.get("/" + testSecret + "/stream/101")
	h.get("/healthz")
	h.get("//" + testSecret + "/manifest.json")
	h.get("/./" + testSecret + "/search?q=tum")
	logs := h.logs.String()
	if strings.Contains(logs, testSecret) || strings.Contains(logs, plextest.Token) {
		t.Fatalf("logs leak a credential:\n%s", logs)
	}
	for _, want := range []string{`"path":"/***/search"`, `"path":"/***/stream/101"`, `"path":"/healthz"`, `"status":200`} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %s:\n%s", want, logs)
		}
	}
}

func TestRedact(t *testing.T) {
	cases := map[string]string{
		"/healthz": "/healthz", "/": "/", "/abc": "/***", "/abc/search": "/***/search", "/abc/file/12": "/***/file/12",
		"//x/y": "/***", "/./x/y": "/***", "/x/../y": "/***", "/x/": "/***",
	}
	for in, want := range cases {
		if got := redact(in); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
	}
}
