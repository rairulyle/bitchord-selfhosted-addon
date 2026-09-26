package library

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type scriptedSource struct {
	mu      sync.Mutex
	answers []func() ([]media.Track, error)
	calls   int
	filter  string
}

func (s *scriptedSource) AllTracks(_ context.Context, filter string) ([]media.Track, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.filter = filter
	answer := s.answers[min(s.calls, len(s.answers)-1)]
	s.calls++
	return answer()
}

func track(id, title string) media.Track {
	return media.Track{ID: id, Title: title, Artist: "Album Artist", AlbumArtist: "Album Artist", Album: "Album",
		DurationSec: 250, Codec: "wav", Container: "wav", BitrateKbps: 1411, FileRef: "/parts/" + id, ArtRef: "/album/thumb"}
}

func ok(tracks ...media.Track) func() ([]media.Track, error) {
	return func() ([]media.Track, error) { return tracks, nil }
}

func fails(message string) func() ([]media.Track, error) {
	return func() ([]media.Track, error) { return nil, errors.New(message) }
}

func TestLibraryIsEmptyAndNotReadyBeforeTheFirstLoad(t *testing.T) {
	lib := NewLibrary(&scriptedSource{}, "", time.Minute, quiet)
	if lib.Ready() {
		t.Error("Ready before any load")
	}
	if got := lib.Search("song", 50); len(got) != 0 {
		t.Errorf("Search = %v", got)
	}
	if _, found := lib.Get("1"); found {
		t.Error("Get found a track in an empty library")
	}
}

func TestRefreshSwapsTheIndexAndSkipsUnplayableTracks(t *testing.T) {
	source := &scriptedSource{answers: []func() ([]media.Track, error){
		ok(track("1", "First Song"), media.Track{ID: "2", Title: "Broken"}),
		ok(track("3", "Second Song")),
	}}
	lib := NewLibrary(source, "Music", time.Minute, quiet)
	if err := lib.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if source.filter != "Music" {
		t.Errorf("library filter passed = %q", source.filter)
	}
	if !lib.Ready() || len(lib.Search("first song", 50)) != 1 {
		t.Fatal("first index not live")
	}
	if _, found := lib.Get("2"); found {
		t.Error("unplayable track was indexed")
	}
	if err := lib.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(lib.Search("first song", 50)) != 0 || len(lib.Search("second song", 50)) != 1 {
		t.Error("index was not replaced")
	}
}

func TestFailedRefreshKeepsThePreviousIndex(t *testing.T) {
	source := &scriptedSource{answers: []func() ([]media.Track, error){ok(track("1", "First Song")), fails("server down")}}
	lib := NewLibrary(source, "", time.Minute, quiet)
	if err := lib.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := lib.Refresh(context.Background()); err == nil {
		t.Fatal("expected the second refresh to fail")
	}
	if len(lib.Search("first song", 50)) != 1 {
		t.Error("previous index was lost")
	}
}

func TestRunRetriesTheFirstLoadWithCappedBackoffThenRefreshesOnTheInterval(t *testing.T) {
	answers := make([]func() ([]media.Track, error), 0, 10)
	for range 8 {
		answers = append(answers, fails("server not up yet"))
	}
	answers = append(answers, ok(track("1", "First Song")), ok(track("2", "Second Song")))
	source := &scriptedSource{answers: answers}
	lib := NewLibrary(source, "", 15*time.Minute, quiet)

	ctx, cancel := context.WithCancel(context.Background())
	var slept []time.Duration
	lib.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		if len(slept) == 10 {
			cancel()
			return context.Canceled
		}
		return nil
	}
	lib.Run(ctx)

	want := []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second,
		time.Minute, time.Minute, 15 * time.Minute, 15 * time.Minute,
	}
	if len(slept) != len(want) {
		t.Fatalf("slept %v", slept)
	}
	for i := range want {
		if slept[i] != want[i] {
			t.Fatalf("slept %v, want %v", slept, want)
		}
	}
	if len(lib.Search("second song", 50)) != 1 {
		t.Error("the interval refresh did not run")
	}
}

func TestRunStopsWhenTheContextIsCancelled(t *testing.T) {
	lib := NewLibrary(&scriptedSource{answers: []func() ([]media.Track, error){ok(track("1", "Song"))}}, "", time.Hour, quiet)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		lib.Run(ctx)
		close(done)
	}()
	for !lib.Ready() {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestSearchIsSafeDuringASwap(t *testing.T) {
	source := &scriptedSource{answers: []func() ([]media.Track, error){ok(track("1", "First Song"))}}
	lib := NewLibrary(source, "", time.Minute, quiet)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 200 {
				lib.Search("first song", 50)
				lib.Get("1")
			}
		})
	}
	for range 50 {
		if err := lib.Refresh(context.Background()); err != nil {
			t.Error(err)
		}
	}
	wg.Wait()
}

func TestRefreshLogsWhatChanged(t *testing.T) {
	var logs bytes.Buffer
	source := &scriptedSource{answers: []func() ([]media.Track, error){
		ok(track("1", "First Song"), track("2", "Second Song"), media.Track{ID: "9", Title: "Broken"}),
		ok(track("2", "Second Song"), track("3", "Third Song"), track("4", "Fourth Song")),
	}}
	lib := NewLibrary(source, "", time.Minute, slog.New(slog.NewJSONHandler(&logs, nil)))
	for range 2 {
		if err := lib.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("logs = %s", logs.String())
	}
	for i, want := range []string{`"tracks":2,"added":2,"removed":0,"skipped":1`, `"tracks":3,"added":2,"removed":1,"skipped":0`} {
		if !strings.Contains(lines[i], want) {
			t.Errorf("line %d lacks %s: %s", i, want, lines[i])
		}
	}
}
