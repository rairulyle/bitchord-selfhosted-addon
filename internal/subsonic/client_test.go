package subsonic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/fakes"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
)

func newClient(fake *fakes.Navidrome) *Client {
	return New(Options{BaseURL: fake.URL, Username: fakes.NavidromeUser, Password: fakes.NavidromePassword})
}

func TestVersion(t *testing.T) {
	fake := fakes.NewNavidrome(t)
	version, err := newClient(fake).Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if version != "1.16.1" {
		t.Fatalf("version = %q, want 1.16.1", version)
	}
}

func TestWrongPasswordIsUnauthorized(t *testing.T) {
	fake := fakes.NewNavidrome(t)
	_, err := New(Options{BaseURL: fake.URL, Username: fakes.NavidromeUser, Password: "wrong-password"}).Version(context.Background())
	if err == nil || !isUnauthorized(err) {
		t.Fatalf("err = %v, want unauthorized", err)
	}
}

func TestLibraries(t *testing.T) {
	fake := fakes.NewNavidrome(t)
	libs, err := newClient(fake).Libraries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(libs) != 1 || libs[0].ID != fakes.NavidromeMusicFolder || libs[0].Name != fakes.NavidromeMusicFolderName {
		t.Fatalf("libraries = %+v", libs)
	}
}

func TestAllTracksSearch3(t *testing.T) {
	fake := fakes.NewNavidrome(t)
	tracks, err := newClient(fake).AllTracks(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 3 {
		t.Fatalf("got %d tracks, want 3: %+v", len(tracks), tracks)
	}
	byID := map[string]media.Track{}
	for _, tr := range tracks {
		byID[tr.ID] = tr
	}
	flac := byID["s-1"]
	if flac.Title != "New Religion" || flac.Codec != "flac" || !flac.Playable() {
		t.Fatalf("flac track = %+v", flac)
	}
	if !media.Lossless(flac.Codec) {
		t.Fatal("flac should be lossless")
	}
	if flac.SampleRate != 48000 || flac.BitDepth != 24 {
		t.Fatalf("hi-res metadata missing: %+v", flac)
	}
	if flac.AlbumArtist != "All Time Low" {
		t.Fatalf("album artist = %q, want All Time Low", flac.AlbumArtist)
	}
	mp3 := byID["s-2"]
	if mp3.Codec != "mp3" || mp3.ArtRef != "" {
		t.Fatalf("mp3 track = %+v", mp3)
	}
	m4a := byID["s-3"]
	if m4a.Codec != "aac" || media.Lossless(m4a.Codec) {
		t.Fatalf("m4a should map to aac (not lossless): %+v", m4a)
	}
}

func TestAllTracksFallbackViaAlbums(t *testing.T) {
	fake := fakes.NewNavidrome(t)
	fake.DisableSearch3 = true // Force fallback to album walk
	tracks, err := newClient(fake).AllTracks(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 3 {
		t.Fatalf("got %d tracks via album walk, want 3: %+v", len(tracks), tracks)
	}
	byID := map[string]media.Track{}
	for _, tr := range tracks {
		byID[tr.ID] = tr
	}
	if _, ok := byID["s-1"]; !ok {
		t.Errorf("missing s-1 from album walk")
	}
	if _, ok := byID["s-2"]; !ok {
		t.Errorf("missing s-2 from album walk")
	}
	if _, ok := byID["s-3"]; !ok {
		t.Errorf("missing s-3 from album walk")
	}
}

func TestTrack(t *testing.T) {
	fake := fakes.NewNavidrome(t)
	tr, err := newClient(fake).Track(context.Background(), "s-1")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Title != "New Religion" || tr.DurationSec != 184 {
		t.Fatalf("track = %+v", tr)
	}
	if _, err := newClient(fake).Track(context.Background(), "missing"); err != media.ErrNotFound {
		t.Fatalf("missing track err = %v, want ErrNotFound", err)
	}
}

func TestOpenFileAndArt(t *testing.T) {
	fake := fakes.NewNavidrome(t)
	c := newClient(fake)
	t.Cleanup(c.Close)
	tracks, err := c.AllTracks(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	var withArt media.Track
	for _, tr := range tracks {
		if tr.ArtRef != "" {
			withArt = tr
		}
	}
	file, err := c.OpenFile(context.Background(), withArt, http.MethodGet, nil)
	if err != nil {
		t.Fatal(err)
	}
	file.Body.Close()
	if file.StatusCode != http.StatusOK {
		t.Fatalf("file status = %d", file.StatusCode)
	}
	art, err := c.OpenArt(context.Background(), withArt)
	if err != nil {
		t.Fatal(err)
	}
	art.Body.Close()
	if art.StatusCode != http.StatusOK {
		t.Fatalf("art status = %d", art.StatusCode)
	}
}

func TestSplitJoinCredential(t *testing.T) {
	user, pass := SplitCredential("lyle:pa:ss")
	if user != "lyle" || pass != "pa:ss" {
		t.Fatalf("SplitCredential = %q, %q", user, pass)
	}
	if JoinCredential("lyle", "pa:ss") != "lyle:pa:ss" {
		t.Fatal("JoinCredential round trip failed")
	}
}

func TestResolveFolderByName(t *testing.T) {
	fake := fakes.NewNavidrome(t)
	if got := newClient(fake).resolveFolderID(context.Background(), "Music"); got != fakes.NavidromeMusicFolder {
		t.Fatalf("resolveFolderID(Music) = %q, want %s", got, fakes.NavidromeMusicFolder)
	}
	if got := newClient(fake).resolveFolderID(context.Background(), ""); got != "" {
		t.Fatalf("resolveFolderID(empty) = %q, want empty", got)
	}
}

func TestMusicFolderFlexibleUnmarshal(t *testing.T) {
	// Numeric ID (real Navidrome default folder id: 0)
	var f1 MusicFolder
	if err := json.Unmarshal([]byte(`{"id": 0, "name": "Music"}`), &f1); err != nil {
		t.Fatalf("unmarshal numeric 0 failed: %v", err)
	}
	if f1.ID != "0" || f1.Name != "Music" {
		t.Fatalf("got %+v, want ID:0 Name:Music", f1)
	}

	// Numeric ID > 0
	var f2 MusicFolder
	if err := json.Unmarshal([]byte(`{"id": 42, "name": "Hi-Res"}`), &f2); err != nil {
		t.Fatalf("unmarshal numeric 42 failed: %v", err)
	}
	if f2.ID != "42" || f2.Name != "Hi-Res" {
		t.Fatalf("got %+v, want ID:42 Name:Hi-Res", f2)
	}

	// String ID
	var f3 MusicFolder
	if err := json.Unmarshal([]byte(`{"id": "folder-abc", "name": "Rock"}`), &f3); err != nil {
		t.Fatalf("unmarshal string id failed: %v", err)
	}
	if f3.ID != "folder-abc" || f3.Name != "Rock" {
		t.Fatalf("got %+v, want ID:folder-abc Name:Rock", f3)
	}
}

func TestFlexibleLists(t *testing.T) {
	// MusicFolderList with single object
	var singleFolder MusicFolderList
	if err := json.Unmarshal([]byte(`{"id": 0, "name": "Single"}`), &singleFolder); err != nil {
		t.Fatalf("unmarshal single folder object failed: %v", err)
	}
	if len(singleFolder) != 1 || singleFolder[0].ID != "0" {
		t.Fatalf("single folder = %+v", singleFolder)
	}

	// MusicFolderList with array
	var multiFolder MusicFolderList
	if err := json.Unmarshal([]byte(`[{"id": 0, "name": "A"}, {"id": 1, "name": "B"}]`), &multiFolder); err != nil {
		t.Fatalf("unmarshal multi folder array failed: %v", err)
	}
	if len(multiFolder) != 2 || multiFolder[0].ID != "0" || multiFolder[1].ID != "1" {
		t.Fatalf("multi folder = %+v", multiFolder)
	}

	// ChildList with single song object
	var singleChild ChildList
	if err := json.Unmarshal([]byte(`{"id": "s-1", "title": "Song 1"}`), &singleChild); err != nil {
		t.Fatalf("unmarshal single child failed: %v", err)
	}
	if len(singleChild) != 1 || singleChild[0].Title != "Song 1" {
		t.Fatalf("single child = %+v", singleChild)
	}

	// AlbumList with single album object
	var singleAlbum AlbumList
	if err := json.Unmarshal([]byte(`{"id": "al-1", "name": "Album 1"}`), &singleAlbum); err != nil {
		t.Fatalf("unmarshal single album failed: %v", err)
	}
	if len(singleAlbum) != 1 || singleAlbum[0].Name != "Album 1" {
		t.Fatalf("single album = %+v", singleAlbum)
	}
}

func TestScrobble(t *testing.T) {
	fake := fakes.NewNavidrome(t)
	c := newClient(fake)
	if err := c.Scrobble(context.Background(), "s-1", false); err != nil {
		t.Fatalf("Scrobble(false) failed: %v", err)
	}
	if err := c.Scrobble(context.Background(), "s-1", true); err != nil {
		t.Fatalf("Scrobble(true) failed: %v", err)
	}
	var scrobbles []fakes.Request
	for _, req := range fake.Requests() {
		if req.Path == "/rest/scrobble.view" {
			scrobbles = append(scrobbles, req)
		}
	}
	if len(scrobbles) != 2 {
		t.Fatalf("expected 2 scrobbles, got %d", len(scrobbles))
	}
	if !strings.Contains(scrobbles[0].Query, "submission=false") {
		t.Errorf("scrobble 0 query = %q, want submission=false", scrobbles[0].Query)
	}
	if !strings.Contains(scrobbles[1].Query, "submission=true") {
		t.Errorf("scrobble 1 query = %q, want submission=true", scrobbles[1].Query)
	}
}

type fakeTimer struct {
	after   time.Duration
	fire    func()
	stopped bool
}

func (f *fakeTimer) Stop() bool {
	f.stopped = true
	return true
}

type playbackHarness struct {
	t          *testing.T
	fake       *fakes.Navidrome
	client     *Client
	now        time.Time
	timers     []*fakeTimer
	activities chan string
}

func newPlaybackHarness(t *testing.T, percent int) *playbackHarness {
	h := &playbackHarness{t: t, fake: fakes.NewNavidrome(t), now: time.Unix(1_800_000_000, 0), activities: make(chan string, 16)}
	h.client = New(Options{BaseURL: h.fake.URL, Username: fakes.NavidromeUser, Password: fakes.NavidromePassword, ScrobblePercent: percent})
	h.client.plays.now = func() time.Time { return h.now }
	h.client.plays.afterFunc = func(d time.Duration, f func()) stopper {
		timer := &fakeTimer{after: d, fire: f}
		h.timers = append(h.timers, timer)
		return timer
	}
	h.client.SetOnActivity(func(kind, message string) { h.activities <- kind + ": " + message })
	t.Cleanup(h.client.Close)
	return h
}

func (h *playbackHarness) get(id, rangeHeader string, readAll bool) {
	h.t.Helper()
	header := http.Header{}
	if rangeHeader != "" {
		header.Set("Range", rangeHeader)
	}
	track := media.Track{ID: id, Title: "Song " + id, Artist: "Artist", DurationSec: 200, FileRef: StreamPath(id)}
	res, err := h.client.OpenFile(context.Background(), track, http.MethodGet, header)
	if err != nil {
		h.t.Fatal(err)
	}
	if readAll {
		io.ReadAll(res.Body)
	} else {
		res.Body.Read(make([]byte, 2))
	}
	res.Body.Close()
}

func (h *playbackHarness) activity() string {
	h.t.Helper()
	select {
	case got := <-h.activities:
		return got
	case <-time.After(5 * time.Second):
		h.t.Fatal("no activity reported")
		return ""
	}
}

func (h *playbackHarness) submissions() int {
	count := 0
	for _, req := range h.fake.Requests() {
		if req.Path == "/rest/scrobble.view" && strings.Contains(req.Query, "submission=true") {
			count++
		}
	}
	return count
}

func TestAShortProbeAndASeekKeepTheScrobble(t *testing.T) {
	h := newPlaybackHarness(t, 50)
	h.get("s-1", "bytes=0-", false)
	if got := h.activity(); got != "now_playing: Now playing: Song s-1 — Artist" {
		t.Fatalf("activity = %q", got)
	}
	h.now = h.now.Add(2 * time.Second)
	h.get("s-1", "bytes=9000-", false)
	h.get("s-1", "", false)
	h.now = h.now.Add(30 * time.Second)
	h.get("s-1", "bytes=4000-", true)
	if len(h.timers) != 1 || h.timers[0].stopped || h.timers[0].after != 100*time.Second {
		t.Fatalf("timers = %+v, want one pending 100s timer", h.timers)
	}
	h.timers[0].fire()
	if got := h.activity(); got != "scrobble: Scrobbled: Song s-1 — Artist (50% played)" {
		t.Fatalf("activity = %q", got)
	}
	if got := h.submissions(); got != 1 {
		t.Fatalf("submissions = %d, want 1", got)
	}
	h.timers[0].fire()
	if got := h.submissions(); got != 1 {
		t.Fatalf("a timer fired twice submitted %d times", got)
	}
}

func TestASecondTrackDoesNotCancelTheFirst(t *testing.T) {
	h := newPlaybackHarness(t, 50)
	h.get("s-1", "", false)
	h.get("s-2", "", false)
	h.activity()
	h.activity()
	if len(h.timers) != 2 || h.timers[0].stopped {
		t.Fatalf("timers = %+v, want two pending", h.timers)
	}
	h.timers[0].fire()
	h.activity()
	if got := h.submissions(); got != 1 {
		t.Fatalf("submissions = %d, want 1", got)
	}
}

func TestAFinishedTrackPlayedFromTheStartIsAReplay(t *testing.T) {
	h := newPlaybackHarness(t, 50)
	h.get("s-1", "", true)
	h.activity()
	h.now = h.now.Add(100 * time.Second)
	h.timers[0].fire()
	h.activity()
	h.get("s-1", "bytes=5000-", false)
	if len(h.timers) != 1 {
		t.Fatalf("a seek after the scrobble started a new play: %d timers", len(h.timers))
	}
	h.get("s-1", "", false)
	h.activity()
	if len(h.timers) != 2 {
		t.Fatalf("a replay armed %d timers, want 2", len(h.timers))
	}
}

func TestDisabledScrobblingOnlyReportsNowPlaying(t *testing.T) {
	h := newPlaybackHarness(t, -1)
	h.get("s-1", "", true)
	if got := h.activity(); !strings.HasPrefix(got, "now_playing:") {
		t.Fatalf("activity = %q", got)
	}
	h.get("s-1", "bytes=0-", true)
	if len(h.timers) != 0 {
		t.Fatalf("disabled scrobbling armed %d timers", len(h.timers))
	}
	select {
	case got := <-h.activities:
		t.Fatalf("a repeated request reported %q", got)
	default:
	}
}

func TestTrackedPlaysAreBounded(t *testing.T) {
	h := newPlaybackHarness(t, 50)
	h.client.SetOnActivity(nil)
	for i := range maxTrackedPlays + 1 {
		h.now = h.now.Add(time.Second)
		h.client.plays.played(media.Track{ID: fmt.Sprintf("t-%d", i)}, "")
	}
	if got := len(h.client.plays.byID); got != maxTrackedPlays {
		t.Fatalf("tracked plays = %d, want %d", got, maxTrackedPlays)
	}
	if !h.timers[0].stopped || h.timers[1].stopped {
		t.Fatal("the oldest play was not the one evicted")
	}
}

func TestTransportErrorsCarryNoCredentials(t *testing.T) {
	c := New(Options{BaseURL: fakes.DeadURL(t), Username: fakes.NavidromeUser, Password: fakes.NavidromePassword})
	track := media.Track{ID: "s-1", FileRef: StreamPath("s-1"), ArtRef: ArtPath("al-1")}
	_, versionErr := c.Version(context.Background())
	_, fileErr := c.OpenFile(context.Background(), track, http.MethodGet, nil)
	_, artErr := c.OpenArt(context.Background(), track)
	for name, err := range map[string]error{"call": versionErr, "file": fileErr, "art": artErr} {
		if err == nil {
			t.Fatalf("%s: no error from a dead server", name)
		}
		for _, leak := range []string{"t=", "s=", "u=", "p=", fakes.NavidromeUser} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("%s error %q contains %q", name, err, leak)
			}
		}
	}
}

func TestFailedEnvelopesOnMediaEndpointsAreErrors(t *testing.T) {
	fake := fakes.NewNavidrome(t)
	good := newClient(fake)
	wrong := New(Options{BaseURL: fake.URL, Username: fakes.NavidromeUser, Password: "wrong-password"})
	song := media.Track{ID: "s-1", FileRef: StreamPath("s-1"), ArtRef: ArtPath("al-1")}
	missing := media.Track{ID: "gone", FileRef: StreamPath("gone"), ArtRef: ArtPath("gone")}
	cases := map[string]struct {
		open func() (*http.Response, error)
		want error
	}{
		"file, wrong password": {func() (*http.Response, error) { return wrong.OpenFile(context.Background(), song, http.MethodGet, nil) }, media.ErrUnauthorized},
		"file, missing song": {func() (*http.Response, error) {
			return good.OpenFile(context.Background(), missing, http.MethodGet, nil)
		}, media.ErrNotFound},
		"art, wrong password": {func() (*http.Response, error) { return wrong.OpenArt(context.Background(), song) }, media.ErrUnauthorized},
		"art, missing cover":  {func() (*http.Response, error) { return good.OpenArt(context.Background(), missing) }, media.ErrNotFound},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			res, err := tc.open()
			if res != nil {
				res.Body.Close()
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestFailedXMLEnvelopeIsAnError(t *testing.T) {
	fake := fakes.NewNavidrome(t)
	fake.Extra["/rest/getCoverArt.view"] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		w.Write([]byte(`<subsonic-response status="failed" version="1.16.1"><error code="70" message="Artwork not found"/></subsonic-response>`))
	}
	res, err := newClient(fake).OpenArt(context.Background(), media.Track{ID: "s-1", ArtRef: ArtPath("al-1")})
	if res != nil {
		res.Body.Close()
	}
	if !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("err = %v, want not found", err)
	}
}

func TestPagingStopsWhenTheServerIgnoresOffsets(t *testing.T) {
	for name, disableSearch3 := range map[string]bool{"search3": false, "album walk": true} {
		t.Run(name, func(t *testing.T) {
			fake := fakes.NewNavidrome(t)
			fake.IgnoreOffset, fake.DisableSearch3 = true, disableSearch3
			c := newClient(fake)
			c.pageSize = 2
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			tracks, err := c.AllTracks(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			if len(tracks) == 0 {
				t.Fatal("no tracks")
			}
		})
	}
}

func TestTheAlbumWalkStopsAtTheFirstFailure(t *testing.T) {
	fake := fakes.NewNavidrome(t)
	fake.DisableSearch3 = true
	albums := make([]map[string]any, 100)
	for i := range albums {
		albums[i] = map[string]any{"id": fmt.Sprintf("al-%d", i), "name": fmt.Sprintf("Album %d", i)}
	}
	fake.Extra["/rest/getAlbumList2.view"] = func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"subsonic-response": map[string]any{"status": "ok", "albumList2": map[string]any{"album": albums}}})
	}
	fake.Extra["/rest/getAlbum.view"] = func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"subsonic-response": map[string]any{"status": "failed", "error": map[string]any{"code": 0, "message": "boom"}}})
	}
	if _, err := newClient(fake).AllTracks(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want the album failure", err)
	}
	fetched := 0
	for _, req := range fake.Requests() {
		if req.Path == "/rest/getAlbum.view" {
			fetched++
		}
	}
	if fetched > 2*albumWorkers {
		t.Fatalf("fetched %d albums after the first failure", fetched)
	}
}

func TestTrackKeepsAnAuthFailureApartFromAMissingTrack(t *testing.T) {
	fake := fakes.NewNavidrome(t)
	_, err := New(Options{BaseURL: fake.URL, Username: fakes.NavidromeUser, Password: "wrong-password"}).Track(context.Background(), "s-1")
	if !errors.Is(err, media.ErrUnauthorized) {
		t.Fatalf("err = %v, want unauthorized", err)
	}
}

func TestErrorCodes(t *testing.T) {
	cases := map[int]error{40: media.ErrUnauthorized, 41: media.ErrUnauthorized, 50: media.ErrUnauthorized, 70: media.ErrNotFound}
	for code, want := range cases {
		if err := navidromeError(&apiError{Code: code, Message: "m"}); !errors.Is(err, want) {
			t.Errorf("code %d: %v, want %v", code, err, want)
		}
	}
	missing := navidromeError(&apiError{Code: 10, Message: "Required parameter is missing"})
	if errors.Is(missing, media.ErrUnauthorized) || errors.Is(missing, media.ErrNotFound) {
		t.Fatalf("a missing parameter maps to %v", missing)
	}
}

func TestLosslessFormatLabels(t *testing.T) {
	cases := map[string]string{"aif": "aiff", "aiff": "aiff", "wav": "wav", "wv": "wavpack", "ape": "ape", "dsf": "dsd", "dff": "dsd", "flac": "flac"}
	for suffix, want := range cases {
		track := Child{ID: "s", Suffix: suffix}.convert()
		if track.Codec != want || !media.Lossless(track.Codec) {
			t.Errorf("%s: codec %q, lossless %v; want %q, lossless", suffix, track.Codec, media.Lossless(track.Codec), want)
		}
	}
}
