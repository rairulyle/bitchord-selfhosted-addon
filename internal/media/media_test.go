package media

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

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
		format := AudioFormat(tc.codec, tc.container)
		if format != tc.format || Lossless(format) != tc.lossless {
			t.Errorf("%s/%s = %s lossless=%v", tc.codec, tc.container, format, Lossless(format))
		}
	}
}

func TestPlayable(t *testing.T) {
	if !(Track{ID: "1", FileRef: "/f"}).Playable() {
		t.Error("a track with an id and a file must be playable")
	}
	if (Track{ID: "1"}).Playable() || (Track{FileRef: "/f"}).Playable() {
		t.Error("a track without an id or a file must not be playable")
	}
}

func TestAllPages(t *testing.T) {
	all := []string{"a", "b", "c", "d", "e"}
	page := func(honourStart bool) func(int) ([]string, error) {
		return func(start int) ([]string, error) {
			if !honourStart {
				start = 0
			}
			end := min(start+2, len(all))
			start = min(start, len(all))
			return all[start:end], nil
		}
	}
	id := func(s string) string { return s }
	got, err := AllPages(2, id, page(true))
	if err != nil || !reflect.DeepEqual(got, all) {
		t.Fatalf("paged = %v, %v", got, err)
	}
	got, err = AllPages(2, id, page(false))
	if err != nil || !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("ignored paging = %v, %v", got, err)
	}
	_, err = AllPages(2, id, func(int) ([]string, error) { return nil, errors.New("down") })
	if err == nil {
		t.Fatal("fetch errors must surface")
	}
}

func newClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	return NewClient(ClientOptions{
		BaseURL: upstream.URL, Name: "fake", TokenVar: "FAKE_TOKEN",
		Authorize: func(r *http.Request) { r.Header.Set("X-Fake", "1") },
	})
}

func TestClientKeepsABasePath(t *testing.T) {
	var seen string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.RequestURI()
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(upstream.Close)
	c := NewClient(ClientOptions{BaseURL: upstream.URL + "/jellyfin", Name: "fake", Authorize: func(*http.Request) {}})
	var into struct{}
	if err := c.GetJSON(context.Background(), "/Items?Ids=1", nil, &into); err != nil {
		t.Fatal(err)
	}
	if seen != "/jellyfin/Items?Ids=1" {
		t.Fatalf("request went to %s", seen)
	}
}

func TestClientMapsStatusesAndNamesTheVariable(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Fake") != "1" {
			t.Errorf("authorize was not applied: %v", r.Header)
		}
		switch r.URL.Path {
		case "/missing":
			w.WriteHeader(http.StatusNotFound)
		case "/locked":
			w.WriteHeader(http.StatusUnauthorized)
		case "/broken":
			w.WriteHeader(http.StatusInternalServerError)
		case "/xml":
			w.Write([]byte("<x/>"))
		default:
			w.Write([]byte(`{"ok":true}`))
		}
	})
	var into struct{ OK bool }
	if err := c.GetJSON(context.Background(), "/fine", nil, &into); err != nil || !into.OK {
		t.Fatalf("GetJSON = %v, %v", into, err)
	}
	if err := c.GetJSON(context.Background(), "/missing", nil, &into); !errors.Is(err, ErrNotFound) {
		t.Errorf("404: %v", err)
	}
	err := c.GetJSON(context.Background(), "/locked", nil, &into)
	if !errors.Is(err, ErrUnauthorized) || !strings.Contains(err.Error(), "fake rejected FAKE_TOKEN") {
		t.Errorf("401: %v", err)
	}
	var status StatusError
	if err := c.GetJSON(context.Background(), "/broken", nil, &into); !errors.As(err, &status) || status != 500 || !strings.Contains(err.Error(), "fake: unexpected status 500") {
		t.Errorf("500: %v", err)
	}
	if err := c.GetJSON(context.Background(), "/xml", nil, &into); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Errorf("xml: %v", err)
	}
	if _, err := c.Open(context.Background(), http.MethodGet, "/locked", nil); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("Open 401: %v", err)
	}
	res, err := c.Open(context.Background(), http.MethodGet, "/broken", nil)
	if err != nil || res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("Open passes other statuses through: %v, %v", res, err)
	}
	res.Body.Close()
}
