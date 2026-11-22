package library

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
)

const maxBackoff = time.Minute

type Source interface {
	AllTracks(ctx context.Context, library string) ([]media.Track, error)
}

type Stats struct {
	Tracks    int
	Artists   int
	Albums    int
	Duration  time.Duration
	Refreshed time.Time
	LastError string
}

type Library struct {
	source     Source
	filter     string
	interval   time.Duration
	log        *slog.Logger
	onActivity func(kind, message string)
	index      atomic.Pointer[Index]
	sleep      func(context.Context, time.Duration) error
	refreshSem chan struct{}
	mu         sync.Mutex
	stats      Stats
	tried      chan struct{}
	tryOnce    sync.Once
}

func NewLibrary(source Source, filter string, interval time.Duration, log *slog.Logger) *Library {
	return &Library{
		source:     source,
		filter:     filter,
		interval:   interval,
		log:        log,
		sleep:      sleep,
		refreshSem: make(chan struct{}, 1),
		tried:      make(chan struct{}),
	}
}

func (l *Library) SetOnActivity(fn func(kind, message string)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.onActivity = fn
}

// Tried is closed once the first refresh has finished, whether it loaded an
// index or failed.
func (l *Library) Tried() <-chan struct{} { return l.tried }

func (l *Library) Ready() bool { return l.index.Load() != nil }

func (l *Library) Stats() Stats {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stats
}

func (l *Library) note(update func(*Stats)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	update(&l.stats)
}

func (l *Library) Search(query string, limit int) []media.Track { return l.Find(query, limit).Tracks }

func (l *Library) Find(query string, limit int) Result {
	ix := l.index.Load()
	if ix == nil {
		return Result{}
	}
	return ix.Find(query, limit)
}

func (l *Library) Get(id string) (media.Track, bool) {
	ix := l.index.Load()
	if ix == nil {
		return media.Track{}, false
	}
	return ix.Get(id)
}

func (l *Library) Refresh(ctx context.Context) error {
	select {
	case l.refreshSem <- struct{}{}:
		defer func() { <-l.refreshSem }()
	case <-ctx.Done():
		return ctx.Err()
	}
	defer l.tryOnce.Do(func() { close(l.tried) })
	started := time.Now()
	all, err := l.source.AllTracks(ctx, l.filter)
	took := time.Since(started)
	if err != nil {
		l.note(func(s *Stats) { s.LastError = err.Error(); s.Duration = took })
		l.mu.Lock()
		act := l.onActivity
		l.mu.Unlock()
		if act != nil {
			act("sync_error", "Sync failed: "+err.Error())
		}
		return err
	}
	tracks := make([]media.Track, 0, len(all))
	artistsMap := make(map[string]struct{})
	albumsMap := make(map[string]struct{})
	for _, track := range all {
		if track.Playable() {
			tracks = append(tracks, track)
			if track.Artist != "" {
				artistsMap[track.Artist] = struct{}{}
			}
			if track.Album != "" {
				albumsMap[track.Album] = struct{}{}
			}
		}
	}
	next := NewIndex(tracks)
	previous := l.index.Swap(next)
	stats := Stats{
		Tracks:    len(tracks),
		Artists:   len(artistsMap),
		Albums:    len(albumsMap),
		Duration:  took,
		Refreshed: time.Now(),
	}
	l.note(func(s *Stats) { *s = stats })
	removed := 0
	if previous != nil {
		removed = previous.missingFrom(next)
	}
	l.log.Info("library indexed", "tracks", len(tracks), "added", next.missingFrom(previous), "removed", removed,
		"skipped", len(all)-len(tracks), "artists", len(artistsMap), "albums", len(albumsMap), "took", took.String())
	l.mu.Lock()
	act := l.onActivity
	l.mu.Unlock()
	if act != nil {
		var tookStr string
		if took < time.Second {
			tookStr = fmt.Sprintf("%dms", took.Milliseconds())
		} else {
			tookStr = fmt.Sprintf("%.1fs", took.Seconds())
		}
		msg := fmt.Sprintf("Synced %d tracks · %d artists · %d albums (took %s)", len(tracks), len(artistsMap), len(albumsMap), tookStr)
		act("sync", msg)
	}
	return nil
}

func (l *Library) Run(ctx context.Context) {
	backoff := time.Second
	for !l.Ready() {
		err := l.Refresh(ctx)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return
		}
		l.log.Error("first library load failed, retrying", "error", err.Error(), "in", backoff.String())
		if l.sleep(ctx, backoff) != nil {
			return
		}
		backoff = min(backoff*2, maxBackoff)
	}
	if l.interval <= 0 {
		<-ctx.Done()
		return
	}
	for l.sleep(ctx, l.interval) == nil {
		if err := l.Refresh(ctx); err != nil && ctx.Err() == nil {
			l.log.Error("library refresh failed, keeping the previous index", "error", err.Error())
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
