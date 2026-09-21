package plex_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/plex"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/plextest"
)

func client(fake *plextest.Fake, pageSize int) *plex.Client {
	return plex.New(plex.Options{BaseURL: fake.URL, Token: plextest.Token, PageSize: pageSize})
}

func keys(tracks []plex.Track) string {
	out := make([]string, len(tracks))
	for i, t := range tracks {
		out[i] = t.RatingKey
	}
	return strings.Join(out, ",")
}

func TestAllTracksPagesThroughEveryMusicSection(t *testing.T) {
	fake := plextest.New(t)
	tracks, err := client(fake, 2).AllTracks(context.Background(), "")
	if err != nil {
		t.Fatalf("AllTracks: %v", err)
	}
	if got := keys(tracks); got != "101,102,103,104,105,106,501" {
		t.Fatalf("keys = %s", got)
	}
	pages := 0
	for _, r := range fake.Requests() {
		if r.Path == "/library/sections/3/all" {
			pages++
		}
	}
	if pages != 4 {
		t.Errorf("section 3 was fetched in %d pages, want 4 (2+2+2+0)", pages)
	}
}

func TestAllTracksFiltersSections(t *testing.T) {
	cases := map[string]struct{ filter, want string }{
		"by id":    {"5", "501"},
		"by title": {"Music", "101,102,103,104,105,106"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tracks, err := client(plextest.New(t), 1000).AllTracks(context.Background(), tc.filter)
			if err != nil {
				t.Fatalf("AllTracks: %v", err)
			}
			if got := keys(tracks); got != tc.want {
				t.Fatalf("keys = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestAllTracksFailsWhenNoSectionMatches(t *testing.T) {
	for _, filter := range []string{"Nope", "Movies", "1"} {
		_, err := client(plextest.New(t), 1000).AllTracks(context.Background(), filter)
		if err == nil || !strings.Contains(err.Error(), filter) {
			t.Errorf("filter %q: err = %v, want it to name the filter", filter, err)
		}
	}
}

func TestEveryRequestCarriesTheTokenAsAHeaderOnly(t *testing.T) {
	fake := plextest.New(t)
	c := client(fake, 1000)
	if _, err := c.AllTracks(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Track(context.Background(), "101"); err != nil {
		t.Fatal(err)
	}
	for _, r := range fake.Requests() {
		if r.Header.Get("X-Plex-Token") != plextest.Token {
			t.Errorf("%s: token header missing", r.Path)
		}
		if strings.Contains(r.Query, plextest.Token) || strings.Contains(r.Path, plextest.Token) {
			t.Errorf("%s?%s: token leaked into the URL", r.Path, r.Query)
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("%s: Accept = %q", r.Path, r.Header.Get("Accept"))
		}
	}
}

func TestTrackReturnsStreamDetails(t *testing.T) {
	track, err := client(plextest.New(t), 1000).Track(context.Background(), "101")
	if err != nil {
		t.Fatalf("Track: %v", err)
	}
	_, part, ok := track.FirstPart()
	if !ok || part.Key != "/library/parts/101/1/file.flac" {
		t.Fatalf("part = %+v, %v", part, ok)
	}
	stream, ok := part.AudioStream()
	if !ok || stream.SamplingRate != 96000 || stream.BitDepth != 24 {
		t.Fatalf("stream = %+v, %v", stream, ok)
	}
}

func TestErrors(t *testing.T) {
	t.Run("unknown track", func(t *testing.T) {
		_, err := client(plextest.New(t), 1000).Track(context.Background(), "999")
		if !errors.Is(err, plex.ErrNotFound) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("rejected token", func(t *testing.T) {
		fake := plextest.New(t)
		c := plex.New(plex.Options{BaseURL: fake.URL, Token: "wrong"})
		_, err := c.AllTracks(context.Background(), "")
		if !errors.Is(err, plex.ErrUnauthorized) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("server error", func(t *testing.T) {
		fake := plextest.New(t)
		fake.FailWith(http.StatusInternalServerError)
		_, err := client(fake, 1000).AllTracks(context.Background(), "")
		if err == nil || !strings.Contains(err.Error(), "500") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("malformed answer", func(t *testing.T) {
		fake := plextest.New(t)
		fake.AnswerRaw("<MediaContainer/>")
		_, err := client(fake, 1000).AllTracks(context.Background(), "")
		if err == nil || !strings.Contains(err.Error(), "malformed") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		dead := httptest.NewServer(http.NotFoundHandler())
		dead.Close()
		c := plex.New(plex.Options{BaseURL: dead.URL, Token: plextest.Token})
		if _, err := c.AllTracks(context.Background(), ""); err == nil {
			t.Fatal("expected an error")
		}
	})
}

func TestAudioFormatAndLossless(t *testing.T) {
	cases := []struct {
		codec, container, format string
		lossless                 bool
	}{
		{"flac", "flac", "flac", true},
		{"pcm", "wav", "wav", true},
		{"pcm", "aiff", "aiff", true},
		{"alac", "mp4", "alac", true},
		{"mp3", "mp3", "mp3", false},
		{"aac", "mp4", "aac", false},
		{"", "ogg", "ogg", false},
	}
	for _, tc := range cases {
		format := plex.AudioFormat(tc.codec, tc.container)
		if format != tc.format || plex.Lossless(format) != tc.lossless {
			t.Errorf("%s/%s = %s lossless=%v", tc.codec, tc.container, format, plex.Lossless(format))
		}
	}
}

func TestArtPathEscapesTheThumb(t *testing.T) {
	got := plex.ArtPath("/library/metadata/1/thumb/2?x=1")
	want := "/photo/:/transcode?width=600&height=600&minSize=1&upscale=1&url=%2Flibrary%2Fmetadata%2F1%2Fthumb%2F2%3Fx%3D1"
	if got != want {
		t.Fatalf("ArtPath = %s", got)
	}
}
