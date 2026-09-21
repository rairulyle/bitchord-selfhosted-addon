package server

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rairulyle/eclipse-plex-addon/internal/plex"
	"github.com/rairulyle/eclipse-plex-addon/internal/plextest"
)

func TestStreamDescriptors(t *testing.T) {
	h := newHarness(t)
	cases := map[string]struct {
		id   string
		want map[string]any
	}{
		"hi-res flac": {"101", map[string]any{
			"url": testBase + "/file/101", "format": "flac", "quality": "lossless 24-bit 48kHz", "codec": "flac",
			"container": "flac", "manifest": "none", "sampleRate": float64(48000), "bitDepth": float64(24), "bitrate": float64(1875000),
		}},
		"mp3 omits bit depth": {"102", map[string]any{
			"url": testBase + "/file/102", "format": "mp3", "quality": "320kbps", "codec": "mp3",
			"container": "mp3", "manifest": "none", "sampleRate": float64(44100), "bitrate": float64(320000),
		}},
		"pcm in wav": {"103", map[string]any{
			"url": testBase + "/file/103", "format": "wav", "quality": "lossless 16-bit 44.1kHz", "codec": "wav",
			"container": "wav", "manifest": "none", "sampleRate": float64(44100), "bitDepth": float64(16), "bitrate": float64(1411000),
		}},
		"no stream details": {"104", map[string]any{
			"url": testBase + "/file/104", "format": "mp3", "quality": "251kbps", "codec": "mp3",
			"container": "mp3", "manifest": "none", "bitrate": float64(251000),
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := h.get("/" + testSecret + "/stream/" + tc.id + "?quality=HIGH")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			if got := decode[map[string]any](t, rec); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("descriptor = %v", got)
			}
		})
	}
}

func TestStreamAsksPlexEvenWhenTheIndexIsEmpty(t *testing.T) {
	h := newHarnessWith(t, plex.Options{}, false)
	if rec := h.get("/" + testSecret + "/stream/101"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestStreamMisses(t *testing.T) {
	h := newHarness(t)
	for name, id := range map[string]string{"unknown": "999", "track without media": "106"} {
		if rec := h.get("/" + testSecret + "/stream/" + id); rec.Code != http.StatusNotFound || rec.Body.Len() != 0 {
			t.Errorf("%s: status %d, body %q", name, rec.Code, rec.Body.String())
		}
	}
	before := len(h.fake.Requests())
	h.get("/" + testSecret + "/stream/abc")
	if len(h.fake.Requests()) != before {
		t.Error("a malformed id must not reach Plex")
	}
}

func TestStreamAnswers502WhenPlexFails(t *testing.T) {
	t.Run("plex 500", func(t *testing.T) {
		h := newHarness(t)
		h.fake.FailWith(http.StatusInternalServerError)
		if rec := h.get("/" + testSecret + "/stream/101"); rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d", rec.Code)
		}
	})
	t.Run("rejected token is named in the log", func(t *testing.T) {
		h := newHarness(t)
		h.server.Plex = plex.New(plex.Options{BaseURL: h.fake.URL, Token: "wrong"})
		rec := h.get("/" + testSecret + "/stream/101")
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d", rec.Code)
		}
		if logs := h.logs.String(); !strings.Contains(logs, "PLEX_TOKEN") || strings.Contains(logs, plextest.Token) {
			t.Fatalf("logs = %s", logs)
		}
	})
	t.Run("slow plex hits the lookup timeout", func(t *testing.T) {
		h := newHarness(t)
		h.fake.Extra["/library/metadata/101"] = func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
		}
		h.server.lookupTimeout = 50 * time.Millisecond
		started := time.Now()
		rec := h.get("/" + testSecret + "/stream/101")
		if rec.Code != http.StatusBadGateway || time.Since(started) > time.Second {
			t.Fatalf("status %d after %v", rec.Code, time.Since(started))
		}
	})
}
