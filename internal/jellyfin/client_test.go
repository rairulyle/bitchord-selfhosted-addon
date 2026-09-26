package jellyfin_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/fakes"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/jellyfin"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
)

func client(fake *fakes.Jellyfin, pageSize int) *jellyfin.Client {
	return jellyfin.New(jellyfin.Options{BaseURL: fake.URL, APIKey: fakes.JellyfinToken, Version: "1.2.3", PageSize: pageSize})
}

func keys(tracks []media.Track) string {
	out := make([]string, len(tracks))
	for i, t := range tracks {
		out[i] = t.ID
	}
	return strings.Join(out, ",")
}

const music = "f101,f102,f103,f104,f105,f106," + fakes.DashedID

func TestAllTracksPagesThroughEveryMusicLibrary(t *testing.T) {
	fake := fakes.NewJellyfin(t)
	tracks, err := client(fake, 2).AllTracks(context.Background(), "")
	if err != nil {
		t.Fatalf("AllTracks: %v", err)
	}
	if got := keys(tracks); got != music+",f501" {
		t.Fatalf("keys = %s", got)
	}
	pages := 0
	for _, r := range fake.Requests() {
		if r.Path == "/Items" && strings.Contains(r.Query, "ParentId="+fakes.MusicFolder) {
			pages++
		}
	}
	if pages != 4 {
		t.Errorf("the music library was fetched in %d pages, want 4 (2+2+2+1)", pages)
	}
}

func TestAllTracksFiltersLibraries(t *testing.T) {
	cases := map[string]struct{ filter, want string }{
		"by id":   {fakes.AudiobooksFolder, "f501"},
		"by name": {"Music", music},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tracks, err := client(fakes.NewJellyfin(t), 1000).AllTracks(context.Background(), tc.filter)
			if err != nil {
				t.Fatalf("AllTracks: %v", err)
			}
			if got := keys(tracks); got != tc.want {
				t.Fatalf("keys = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestAllTracksMatchesTheCollectionTypeIgnoringCase(t *testing.T) {
	fake := fakes.NewJellyfin(t)
	fake.Folders[1].CollectionType = "Music"
	tracks, err := client(fake, 1000).AllTracks(context.Background(), "Music")
	if err != nil {
		t.Fatalf("AllTracks: %v", err)
	}
	if got := keys(tracks); got != music {
		t.Fatalf("keys = %s", got)
	}
}

func TestAllTracksFailsWhenNoLibraryMatches(t *testing.T) {
	for _, filter := range []string{"Nope", "Movies", fakes.MoviesFolder} {
		_, err := client(fakes.NewJellyfin(t), 1000).AllTracks(context.Background(), filter)
		if err == nil || !strings.Contains(err.Error(), filter) {
			t.Errorf("filter %q: err = %v, want it to name the filter", filter, err)
		}
	}
}

func TestAllTracksTerminatesWhenJellyfinIgnoresPaging(t *testing.T) {
	fake := fakes.NewJellyfin(t)
	fake.IgnorePaging = true
	tracks, err := client(fake, 2).AllTracks(context.Background(), "Music")
	if err != nil {
		t.Fatalf("AllTracks: %v", err)
	}
	if got := keys(tracks); got != music {
		t.Fatalf("keys = %s", got)
	}
}

func TestEveryRequestCarriesTheKeyInTheHeaderOnlyAndNoUserID(t *testing.T) {
	fake := fakes.NewJellyfin(t)
	c := client(fake, 1000)
	if _, err := c.AllTracks(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Track(context.Background(), "f101"); err != nil {
		t.Fatal(err)
	}
	want := `MediaBrowser Client="bitchord-selfhosted-addon", Device="server", DeviceId="bitchord-selfhosted-addon", Version="1.2.3", Token="` + fakes.JellyfinToken + `"`
	for _, r := range fake.Requests() {
		if got := r.Header.Get("Authorization"); got != want {
			t.Errorf("%s: Authorization = %q", r.Path, got)
		}
		if r.Header.Get("X-Emby-Token") != "" {
			t.Errorf("%s: legacy X-Emby-Token header sent", r.Path)
		}
		if strings.Contains(r.Query, fakes.JellyfinToken) || strings.Contains(r.Path, fakes.JellyfinToken) {
			t.Errorf("%s?%s: key leaked into the URL", r.Path, r.Query)
		}
		if strings.Contains(strings.ToLower(r.Query), "userid") {
			t.Errorf("%s?%s: a user id was sent", r.Path, r.Query)
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("%s: Accept = %q", r.Path, r.Header.Get("Accept"))
		}
	}
}

func TestItemQueriesAskForMediaSources(t *testing.T) {
	fake := fakes.NewJellyfin(t)
	if _, err := client(fake, 1000).AllTracks(context.Background(), "Music"); err != nil {
		t.Fatal(err)
	}
	requests := fake.Requests()
	last := requests[len(requests)-1]
	for _, want := range []string{"IncludeItemTypes=Audio", "Recursive=true", "Fields=MediaSources", "SortBy=SortName", "EnableTotalRecordCount=false", "StartIndex=0", "Limit=1000"} {
		if !strings.Contains(last.Query, want) {
			t.Errorf("query lacks %s: %s", want, last.Query)
		}
	}
}

func TestTrackFetchesOneItemAsAFilteredList(t *testing.T) {
	fake := fakes.NewJellyfin(t)
	c := client(fake, 1000)
	track, err := c.Track(context.Background(), "f101")
	if err != nil {
		t.Fatalf("Track: %v", err)
	}
	if track.FileRef != "/Items/f101/File" || track.SampleRate != 48000 || track.BitDepth != 24 || track.BitrateKbps != 1875 {
		t.Fatalf("track = %+v", track)
	}
	requests := fake.Requests()
	if last := requests[len(requests)-1]; last.Path != "/Items" || !strings.Contains(last.Query, "Ids=f101") || !strings.Contains(last.Query, "IncludeItemTypes=Audio") {
		t.Errorf("single item fetched through %s?%s", last.Path, last.Query)
	}
	dashed, err := c.Track(context.Background(), fakes.DashedID)
	if err != nil || dashed.ID != fakes.DashedID || dashed.FileRef != "/Items/"+fakes.DashedID+"/File" {
		t.Fatalf("dashed id: %+v, %v", dashed, err)
	}
	if _, err := c.Track(context.Background(), fakes.MovieID); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("movie id: %v, want %v", err, media.ErrNotFound)
	}
}

func TestErrors(t *testing.T) {
	t.Run("unknown track", func(t *testing.T) {
		_, err := client(fakes.NewJellyfin(t), 1000).Track(context.Background(), "f999")
		if !errors.Is(err, media.ErrNotFound) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("id jellyfin cannot parse", func(t *testing.T) {
		fake := fakes.NewJellyfin(t)
		fake.FailWith(http.StatusBadRequest)
		_, err := client(fake, 1000).Track(context.Background(), "abc")
		if !errors.Is(err, media.ErrNotFound) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("rejected key names the variable", func(t *testing.T) {
		fake := fakes.NewJellyfin(t)
		c := jellyfin.New(jellyfin.Options{BaseURL: fake.URL, APIKey: "wrong"})
		_, err := c.AllTracks(context.Background(), "")
		if !errors.Is(err, media.ErrUnauthorized) || !strings.Contains(err.Error(), "JELLYFIN_API_KEY") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("server error", func(t *testing.T) {
		fake := fakes.NewJellyfin(t)
		fake.FailWith(http.StatusInternalServerError)
		_, err := client(fake, 1000).AllTracks(context.Background(), "")
		if err == nil || !strings.Contains(err.Error(), "500") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("malformed answer", func(t *testing.T) {
		fake := fakes.NewJellyfin(t)
		fake.AnswerRaw("<html/>")
		_, err := client(fake, 1000).AllTracks(context.Background(), "")
		if err == nil || !strings.Contains(err.Error(), "malformed") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestOpenFileStreamsWithRangeAndIdentityEncoding(t *testing.T) {
	fake := fakes.NewJellyfin(t)
	track := media.Track{ID: "f101", FileRef: jellyfin.FilePath("f101")}
	res, err := client(fake, 1000).OpenFile(context.Background(), track, http.MethodGet, http.Header{"Range": {"bytes=10-19"}})
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusPartialContent || string(body) != "abcdefghij" || res.Header.Get("Content-Range") != "bytes 10-19/36" {
		t.Fatalf("status %d, body %q, range %q", res.StatusCode, body, res.Header.Get("Content-Range"))
	}
	requests := fake.Requests()
	last := requests[len(requests)-1]
	if last.Path != "/Items/f101/File" || last.Header.Get("Range") != "bytes=10-19" || last.Header.Get("Accept-Encoding") != "identity" {
		t.Errorf("upstream request = %+v", last)
	}
}

func TestOpenFileAndArtMapMissingAndRejected(t *testing.T) {
	fake := fakes.NewJellyfin(t)
	c := client(fake, 1000)
	gone := media.Track{ID: "f103", FileRef: jellyfin.FilePath("f103"), ArtRef: jellyfin.ArtPath("f103", "t103")}
	if _, err := c.OpenFile(context.Background(), gone, http.MethodGet, nil); !errors.Is(err, media.ErrNotFound) {
		t.Errorf("missing file: %v", err)
	}
	res, err := c.OpenArt(context.Background(), gone)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("OpenArt = %v, %v", res, err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(body) != "jpeg:/Items/f103/Images/Primary?fillWidth=600&fillHeight=600&quality=90&tag=t103" {
		t.Errorf("art body = %q", body)
	}
	wrong := jellyfin.New(jellyfin.Options{BaseURL: fake.URL, APIKey: "wrong"})
	if _, err := wrong.OpenFile(context.Background(), gone, http.MethodGet, nil); !errors.Is(err, media.ErrUnauthorized) {
		t.Errorf("rejected key: %v", err)
	}
}
