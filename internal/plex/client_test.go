package plex_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/fakes"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/plex"
)

const flacBody = "0123456789abcdefghijklmnopqrstuvwxyz"

func client(fake *fakes.Plex, pageSize int) *plex.Client {
	return plex.New(plex.Options{BaseURL: fake.URL, Token: fakes.PlexToken, PageSize: pageSize})
}

func keys(tracks []media.Track) string {
	out := make([]string, len(tracks))
	for i, t := range tracks {
		out[i] = t.ID
	}
	return strings.Join(out, ",")
}

func flac() media.Track { return media.Track{ID: "101", FileRef: "/library/parts/101/1/file.flac"} }

func TestAllTracksPagesThroughEveryMusicSection(t *testing.T) {
	fake := fakes.NewPlex(t)
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

func TestAllTracksKeepsUnplayableTracksForTheCount(t *testing.T) {
	tracks, err := client(fakes.NewPlex(t), 1000).AllTracks(context.Background(), "Music")
	if err != nil {
		t.Fatalf("AllTracks: %v", err)
	}
	playable := 0
	for _, track := range tracks {
		if track.Playable() {
			playable++
		}
	}
	if len(tracks) != 6 || playable != 5 {
		t.Fatalf("%d tracks, %d playable, want 6 and 5", len(tracks), playable)
	}
}

func TestAllTracksFiltersSections(t *testing.T) {
	cases := map[string]struct{ filter, want string }{
		"by id":    {"5", "501"},
		"by title": {"Music", "101,102,103,104,105,106"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tracks, err := client(fakes.NewPlex(t), 1000).AllTracks(context.Background(), tc.filter)
			if err != nil {
				t.Fatalf("AllTracks: %v", err)
			}
			if got := keys(tracks); got != tc.want {
				t.Fatalf("keys = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestAllTracksTerminatesWhenPlexIgnoresThePagingHeaders(t *testing.T) {
	fake := fakes.NewPlex(t)
	fake.IgnorePaging = true
	tracks, err := client(fake, 2).AllTracks(context.Background(), "Music")
	if err != nil {
		t.Fatalf("AllTracks: %v", err)
	}
	if got := keys(tracks); got != "101,102,103,104,105,106" {
		t.Fatalf("keys = %s", got)
	}
}

func TestAllTracksTerminatesWhenPageSizeEqualsTheSectionLength(t *testing.T) {
	fake := fakes.NewPlex(t)
	fake.IgnorePaging = true
	tracks, err := client(fake, 6).AllTracks(context.Background(), "Music")
	if err != nil {
		t.Fatalf("AllTracks: %v", err)
	}
	if got := keys(tracks); got != "101,102,103,104,105,106" {
		t.Fatalf("keys = %s", got)
	}
}

func TestAllTracksFailsWhenNoSectionMatches(t *testing.T) {
	for _, filter := range []string{"Nope", "Movies", "1"} {
		_, err := client(fakes.NewPlex(t), 1000).AllTracks(context.Background(), filter)
		if err == nil || !strings.Contains(err.Error(), filter) {
			t.Errorf("filter %q: err = %v, want it to name the filter", filter, err)
		}
	}
}

func TestEveryRequestCarriesTheTokenAsAHeaderOnly(t *testing.T) {
	fake := fakes.NewPlex(t)
	c := client(fake, 1000)
	if _, err := c.AllTracks(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Track(context.Background(), "101"); err != nil {
		t.Fatal(err)
	}
	for _, r := range fake.Requests() {
		if r.Header.Get("X-Plex-Token") != fakes.PlexToken {
			t.Errorf("%s: token header missing", r.Path)
		}
		if strings.Contains(r.Query, fakes.PlexToken) || strings.Contains(r.Path, fakes.PlexToken) {
			t.Errorf("%s?%s: token leaked into the URL", r.Path, r.Query)
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("%s: Accept = %q", r.Path, r.Header.Get("Accept"))
		}
	}
}

func TestTrackReturnsStreamDetails(t *testing.T) {
	track, err := client(fakes.NewPlex(t), 1000).Track(context.Background(), "101")
	if err != nil {
		t.Fatalf("Track: %v", err)
	}
	if track.FileRef != "/library/parts/101/1/file.flac" || track.SampleRate != 48000 || track.BitDepth != 24 {
		t.Fatalf("track = %+v", track)
	}
}

func TestErrors(t *testing.T) {
	t.Run("unknown track", func(t *testing.T) {
		_, err := client(fakes.NewPlex(t), 1000).Track(context.Background(), "999")
		if !errors.Is(err, media.ErrNotFound) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("rejected token names the variable", func(t *testing.T) {
		fake := fakes.NewPlex(t)
		c := plex.New(plex.Options{BaseURL: fake.URL, Token: "wrong"})
		_, err := c.AllTracks(context.Background(), "")
		if !errors.Is(err, media.ErrUnauthorized) || !strings.Contains(err.Error(), "PLEX_TOKEN") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("server error", func(t *testing.T) {
		fake := fakes.NewPlex(t)
		fake.FailWith(http.StatusInternalServerError)
		_, err := client(fake, 1000).AllTracks(context.Background(), "")
		if err == nil || !strings.Contains(err.Error(), "500") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("malformed answer", func(t *testing.T) {
		fake := fakes.NewPlex(t)
		fake.AnswerRaw("<MediaContainer/>")
		_, err := client(fake, 1000).AllTracks(context.Background(), "")
		if err == nil || !strings.Contains(err.Error(), "malformed") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		dead := httptest.NewServer(http.NotFoundHandler())
		dead.Close()
		c := plex.New(plex.Options{BaseURL: dead.URL, Token: fakes.PlexToken})
		if _, err := c.AllTracks(context.Background(), ""); err == nil {
			t.Fatal("expected an error")
		}
	})
}

func TestArtPathEscapesTheThumb(t *testing.T) {
	got := plex.ArtPath("/library/metadata/1/thumb/2?x=1")
	want := "/photo/:/transcode?width=600&height=600&minSize=1&upscale=1&url=%2Flibrary%2Fmetadata%2F1%2Fthumb%2F2%3Fx%3D1"
	if got != want {
		t.Fatalf("ArtPath = %s", got)
	}
}

func TestOpenFileStreamsWithRangeAndIdentityEncoding(t *testing.T) {
	fake := fakes.NewPlex(t)
	c := client(fake, 1000)
	res, err := c.OpenFile(context.Background(), flac(), http.MethodGet, http.Header{"Range": {"bytes=10-19"}})
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusPartialContent {
		t.Fatalf("StatusCode = %d, want %d", res.StatusCode, http.StatusPartialContent)
	}
	body, _ := io.ReadAll(res.Body)
	if got := string(body); got != "abcdefghij" {
		t.Fatalf("body = %q, want %q", got, "abcdefghij")
	}
	if got := res.Header.Get("Content-Range"); got != "bytes 10-19/36" {
		t.Fatalf("Content-Range = %q, want %q", got, "bytes 10-19/36")
	}
	reqs := fake.Requests()
	req := reqs[len(reqs)-1]
	if req.Header.Get("Accept-Encoding") != "identity" {
		t.Errorf("Accept-Encoding = %q, want %q", req.Header.Get("Accept-Encoding"), "identity")
	}
	if req.Header.Get("X-Plex-Token") != fakes.PlexToken {
		t.Errorf("X-Plex-Token = %q, want %q", req.Header.Get("X-Plex-Token"), fakes.PlexToken)
	}
	if req.Header.Get("Range") != "bytes=10-19" {
		t.Errorf("Range = %q, want %q", req.Header.Get("Range"), "bytes=10-19")
	}
	if strings.Contains(req.Path, fakes.PlexToken) || strings.Contains(req.Query, fakes.PlexToken) {
		t.Errorf("token leaked into the URL: %s?%s", req.Path, req.Query)
	}
}

func TestOpenFileRetriesAsADownloadWhenPlexRefusesDirectPlay(t *testing.T) {
	fake := fakes.NewPlex(t)
	fake.Extra[flac().FileRef] = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("download") != "1" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		http.ServeContent(w, r, "", time.Time{}, strings.NewReader(flacBody))
	}
	res, err := client(fake, 1000).OpenFile(context.Background(), flac(), http.MethodGet, http.Header{"Range": {"bytes=10-19"}})
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusPartialContent || string(body) != "abcdefghij" {
		t.Fatalf("status %d, body %q", res.StatusCode, body)
	}
}

func TestOpenFileAndArtMapMissingAndRejected(t *testing.T) {
	fake := fakes.NewPlex(t)
	c := client(fake, 1000)
	gone := media.Track{ID: "103", FileRef: "/library/parts/103/1/file.wav", ArtRef: "/library/metadata/103/thumb/1"}
	if _, err := c.OpenFile(context.Background(), gone, http.MethodGet, nil); !errors.Is(err, media.ErrNotFound) {
		t.Errorf("missing file: %v", err)
	}
	res, err := c.OpenArt(context.Background(), gone)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("OpenArt = %v, %v", res, err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(body) != "jpeg:/library/metadata/103/thumb/1" {
		t.Errorf("art body = %q", body)
	}
	wrong := plex.New(plex.Options{BaseURL: fake.URL, Token: "wrong"})
	if _, err := wrong.OpenFile(context.Background(), flac(), http.MethodGet, nil); !errors.Is(err, media.ErrUnauthorized) {
		t.Errorf("rejected token: %v", err)
	}
}
