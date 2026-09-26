package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/fakes"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/jellyfin"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/library"
)

type jellyfinHarness struct {
	fake    *fakes.Jellyfin
	server  *server
	handler http.Handler
	logs    *syncBuffer
}

func newJellyfinHarness(t *testing.T, key string, load bool) *jellyfinHarness {
	t.Helper()
	fake := fakes.NewJellyfin(t)
	client := jellyfin.New(jellyfin.Options{BaseURL: fake.URL, APIKey: key, Version: "1.2.3"})
	logs := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	lib := library.NewLibrary(client, "Music", time.Hour, log)
	if load {
		if err := lib.Refresh(context.Background()); err != nil {
			t.Fatalf("Refresh: %v", err)
		}
	}
	s := newServer(Options{Secret: testSecret, PublicURL: testPublic, Version: "1.2.3", Library: lib, Backend: client, Log: log})
	return &jellyfinHarness{fake: fake, server: s, handler: s.handler(), logs: logs}
}

func (h *jellyfinHarness) do(method, path string, header http.Header) *httptest.ResponseRecorder {
	return do(h.handler, method, path, header)
}

func (h *jellyfinHarness) get(path string) *httptest.ResponseRecorder {
	return h.do(http.MethodGet, path, nil)
}

func TestJellyfinManifestUsesTheBackendName(t *testing.T) {
	h := newJellyfinHarness(t, fakes.JellyfinToken, true)
	got := decode[map[string]any](t, h.get("/"+testSecret+"/manifest.json"))
	want := map[string]any{
		"id": "app.bitchord-selfhosted-addon.jellyfin", "name": "Jellyfin", "version": "1.2.3",
		"description": "Your Jellyfin music library", "contentType": "music",
		"resources": []any{"search", "stream"}, "types": []any{"track"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("manifest = %v", got)
	}
	h.server.AddonName = "Home Jellyfin"
	if got := decode[map[string]any](t, h.get("/"+testSecret+"/manifest.json")); got["name"] != "Home Jellyfin" {
		t.Errorf("ADDON_NAME not applied: %v", got["name"])
	}
}

func TestJellyfinSearchMapsTracks(t *testing.T) {
	h := newJellyfinHarness(t, fakes.JellyfinToken, true)
	rec := h.get("/" + testSecret + "/search?q=New+Religion+Teddy+Swims")
	got := decode[map[string]any](t, rec)
	want := map[string]any{
		"tracks": []any{map[string]any{
			"id": "f101", "title": "New Religion", "artist": "All Time Low, Teddy Swims", "album": "Tell Me I’m Alive",
			"duration": float64(184), "artworkURL": testBase + "/art/f101", "format": "flac", "audioQuality": "LOSSLESS",
		}},
		"albums": []any{}, "artists": []any{}, "playlists": []any{},
	}
	if rec.Code != http.StatusOK || !reflect.DeepEqual(got, want) {
		t.Fatalf("status %d, search = %v", rec.Code, got)
	}
}

func TestJellyfinStreamDescriptors(t *testing.T) {
	h := newJellyfinHarness(t, fakes.JellyfinToken, true)
	cases := map[string]struct {
		id   string
		want map[string]any
	}{
		"hi-res flac from the stream bitrate": {"f101", map[string]any{
			"url": testBase + "/file/f101", "format": "flac", "quality": "lossless 24-bit 48kHz", "codec": "flac",
			"container": "flac", "manifest": "none", "sampleRate": float64(48000), "bitDepth": float64(24), "bitrate": float64(1875000),
		}},
		"mp3 from the source bitrate": {"f104", map[string]any{
			"url": testBase + "/file/f104", "format": "mp3", "quality": "251kbps", "codec": "mp3",
			"container": "mp3", "manifest": "none", "bitrate": float64(251000),
		}},
		"pcm in wav": {"f103", map[string]any{
			"url": testBase + "/file/f103", "format": "wav", "quality": "lossless 16-bit 44.1kHz", "codec": "wav",
			"container": "wav", "manifest": "none", "sampleRate": float64(44100), "bitDepth": float64(16), "bitrate": float64(1411000),
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := h.get("/" + testSecret + "/stream/" + tc.id)
			if got := decode[map[string]any](t, rec); rec.Code != http.StatusOK || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("status %d, descriptor = %v", rec.Code, got)
			}
		})
	}
	for name, id := range map[string]string{"unknown": "f999", "no media": "f106", "movie": fakes.MovieID} {
		if rec := h.get("/" + testSecret + "/stream/" + id); rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d", name, rec.Code)
		}
	}
}

func TestJellyfinFileWithRangeAndDashedID(t *testing.T) {
	h := newJellyfinHarness(t, fakes.JellyfinToken, true)
	rec := h.do(http.MethodGet, "/"+testSecret+"/file/f101", http.Header{"Range": {"bytes=10-19"}})
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "abcdefghij" || rec.Header().Get("Content-Range") != "bytes 10-19/36" {
		t.Fatalf("status %d, body %q, headers %v", rec.Code, rec.Body.String(), rec.Header())
	}
	if rec.Header().Get("X-Jellyfin-Fake") != "" {
		t.Error("an upstream header outside the whitelist reached the client")
	}
	requests := h.fake.Requests()
	if last := requests[len(requests)-1]; last.Path != "/Items/f101/File" {
		t.Errorf("file fetched through %s", last.Path)
	}
	dashed := h.get("/" + testSecret + "/file/" + fakes.DashedID)
	if dashed.Code != http.StatusOK || dashed.Body.String() != "dashed-bytes" {
		t.Fatalf("dashed id: status %d, body %q", dashed.Code, dashed.Body.String())
	}
	if gone := h.get("/" + testSecret + "/file/f103"); gone.Code != http.StatusNotFound {
		t.Errorf("file gone from jellyfin: status %d", gone.Code)
	}
	cold := newJellyfinHarness(t, fakes.JellyfinToken, false)
	if rec := cold.get("/" + testSecret + "/file/f102"); rec.Code != http.StatusOK || rec.Body.String() != "mp3-bytes" {
		t.Errorf("index miss: status %d, body %q", rec.Code, rec.Body.String())
	}
	head := h.do(http.MethodHead, "/"+testSecret+"/file/f101", nil)
	if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Length") != "36" {
		t.Errorf("HEAD: status %d, body %q, length %q", head.Code, head.Body.String(), head.Header().Get("Content-Length"))
	}
	requests = h.fake.Requests()
	if last := requests[len(requests)-1]; last.Method != http.MethodHead {
		t.Errorf("HEAD was forwarded as %s", last.Method)
	}
}

func TestJellyfinArtCarriesTheTag(t *testing.T) {
	h := newJellyfinHarness(t, fakes.JellyfinToken, true)
	own := h.get("/" + testSecret + "/art/f101")
	if own.Code != http.StatusOK || own.Body.String() != "jpeg:/Items/f101/Images/Primary?fillWidth=600&fillHeight=600&quality=90&tag=tf101" {
		t.Fatalf("own art: status %d, body %q", own.Code, own.Body.String())
	}
	if own.Header().Get("Cache-Control") != "public, max-age=86400" || own.Header().Get("Content-Type") != "image/jpeg" {
		t.Errorf("headers = %v", own.Header())
	}
	album := h.get("/" + testSecret + "/art/f105")
	if album.Body.String() != "jpeg:/Items/af105/Images/Primary?fillWidth=600&fillHeight=600&quality=90&tag=taf105" {
		t.Errorf("album art fallback: %q", album.Body.String())
	}
	if none := h.get("/" + testSecret + "/art/f104"); none.Code != http.StatusNotFound {
		t.Errorf("track without art: status %d", none.Code)
	}
}

func TestJellyfinRejectedKeyIsNamedInTheLog(t *testing.T) {
	cold := newJellyfinHarness(t, "wrong", false)
	if rec := cold.get("/" + testSecret + "/stream/f101"); rec.Code != http.StatusBadGateway {
		t.Fatalf("stream: status = %d", rec.Code)
	}
	revoked := newJellyfinHarness(t, fakes.JellyfinToken, true)
	revoked.server.Backend = jellyfin.New(jellyfin.Options{BaseURL: revoked.fake.URL, APIKey: "wrong"})
	if rec := revoked.get("/" + testSecret + "/file/f101"); rec.Code != http.StatusBadGateway {
		t.Fatalf("file after the key was revoked: status = %d", rec.Code)
	}
	for _, logs := range []string{cold.logs.String(), revoked.logs.String()} {
		if !strings.Contains(logs, "JELLYFIN_API_KEY") || strings.Contains(logs, fakes.JellyfinToken) || strings.Contains(logs, "wrong") {
			t.Fatalf("logs = %s", logs)
		}
	}
}
