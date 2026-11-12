package subsonic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
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

func TestOpenFileNowPlayingAndSkipping(t *testing.T) {
	fake := fakes.NewNavidrome(t)
	c := newClient(fake)
	track := media.Track{ID: "s-1", FileRef: StreamPath("s-1"), DurationSec: 180}
	res, err := c.OpenFile(context.Background(), track, http.MethodGet, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(res.Body)
	_ = res.Body.Close()

	// Wait briefly for background Now Playing goroutine
	time.Sleep(50 * time.Millisecond)

	var scrobbles []fakes.Request
	for _, req := range fake.Requests() {
		if req.Path == "/rest/scrobble.view" {
			scrobbles = append(scrobbles, req)
		}
	}
	// Exactly 1 scrobble (now playing), NO completed scrobble because elapsed time was only 50ms
	if len(scrobbles) != 1 {
		t.Fatalf("expected exactly 1 scrobble (now playing), got %d: %+v", len(scrobbles), scrobbles)
	}
	if !strings.Contains(scrobbles[0].Query, "submission=false") {
		t.Errorf("scrobble query = %q, want submission=false", scrobbles[0].Query)
	}

	// Now start a second track - should cancel any pending timer for s-1
	track2 := media.Track{ID: "s-2", FileRef: StreamPath("s-2"), DurationSec: 180}
	res2, err := c.OpenFile(context.Background(), track2, http.MethodGet, nil)
	if err != nil {
		t.Fatal(err)
	}
	res2.Body.Close()
	time.Sleep(50 * time.Millisecond)

	var scrobblesAfter []fakes.Request
	for _, req := range fake.Requests() {
		if req.Path == "/rest/scrobble.view" {
			scrobblesAfter = append(scrobblesAfter, req)
		}
	}
	// Still only Now Playing requests (s-1 now playing, s-2 now playing), no completed scrobbles
	for _, req := range scrobblesAfter {
		if strings.Contains(req.Query, "submission=true") {
			t.Fatalf("unexpected completed scrobble for skipped track: %s", req.Query)
		}
	}
}

func TestSubsonicActivityCallback(t *testing.T) {
	fake := fakes.NewNavidrome(t)
	c := newClient(fake)
	var activities []string
	var mu sync.Mutex
	c.SetOnActivity(func(kind, message string) {
		mu.Lock()
		defer mu.Unlock()
		activities = append(activities, kind+": "+message)
	})

	track := media.Track{ID: "s-1", Title: "So Good", Artist: "Artist", DurationSec: 100, FileRef: StreamPath("s-1")}
	res, err := c.OpenFile(context.Background(), track, http.MethodGet, nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		mu.Lock()
		count := len(activities)
		mu.Unlock()
		if count > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(activities) == 0 {
		t.Fatal("expected activity callback for now playing")
	}
	if !strings.Contains(activities[0], "now_playing") || !strings.Contains(activities[0], "So Good") {
		t.Errorf("unexpected activity: %s", activities[0])
	}
}
