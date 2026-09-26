package library

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
)

const maxBackoff = time.Minute

type Source interface {
	AllTracks(ctx context.Context, library string) ([]media.Track, error)
}

type Library struct {
	source   Source
	filter   string
	interval time.Duration
	log      *slog.Logger
	index    atomic.Pointer[Index]
	sleep    func(context.Context, time.Duration) error
}

func NewLibrary(source Source, filter string, interval time.Duration, log *slog.Logger) *Library {
	return &Library{source: source, filter: filter, interval: interval, log: log, sleep: sleep}
}

func (l *Library) Ready() bool { return l.index.Load() != nil }

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
	started := time.Now()
	all, err := l.source.AllTracks(ctx, l.filter)
	if err != nil {
		return err
	}
	tracks := make([]media.Track, 0, len(all))
	for _, track := range all {
		if track.Playable() {
			tracks = append(tracks, track)
		}
	}
	next := NewIndex(tracks)
	previous := l.index.Swap(next)
	removed := 0
	if previous != nil {
		removed = previous.missingFrom(next)
	}
	l.log.Info("library indexed", "tracks", len(tracks), "added", next.missingFrom(previous), "removed", removed,
		"skipped", len(all)-len(tracks), "took", time.Since(started).String())
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
