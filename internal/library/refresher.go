package library

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/plex"
)

const maxBackoff = time.Minute

type Source interface {
	AllTracks(ctx context.Context, section string) ([]plex.Track, error)
}

type Library struct {
	source   Source
	section  string
	interval time.Duration
	log      *slog.Logger
	index    atomic.Pointer[Index]
	sleep    func(context.Context, time.Duration) error
}

func NewLibrary(source Source, section string, interval time.Duration, log *slog.Logger) *Library {
	return &Library{source: source, section: section, interval: interval, log: log, sleep: sleep}
}

func (l *Library) Ready() bool { return l.index.Load() != nil }

func (l *Library) Search(query string, limit int) []Track {
	ix := l.index.Load()
	if ix == nil {
		return nil
	}
	return ix.Search(query, limit)
}

func (l *Library) Get(id string) (Track, bool) {
	ix := l.index.Load()
	if ix == nil {
		return Track{}, false
	}
	return ix.Get(id)
}

func (l *Library) Refresh(ctx context.Context) error {
	started := time.Now()
	raw, err := l.source.AllTracks(ctx, l.section)
	if err != nil {
		return err
	}
	tracks := make([]Track, 0, len(raw))
	for _, item := range raw {
		if track, ok := FromPlex(item); ok {
			tracks = append(tracks, track)
		}
	}
	l.index.Store(NewIndex(tracks))
	l.log.Info("library indexed", "tracks", len(tracks), "skipped", len(raw)-len(tracks), "took", time.Since(started).String())
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

func FromPlex(item plex.Track) (Track, bool) {
	media, part, ok := item.FirstPart()
	if !ok || item.RatingKey == "" {
		return Track{}, false
	}
	container := media.Container
	if container == "" {
		container = part.Container
	}
	artist := item.OriginalTitle
	if artist == "" {
		artist = item.GrandparentTitle
	}
	thumb := item.Thumb
	if thumb == "" {
		thumb = item.ParentThumb
	}
	return Track{
		ID:          item.RatingKey,
		Title:       item.Title,
		Artist:      artist,
		AlbumArtist: item.GrandparentTitle,
		Album:       item.ParentTitle,
		DurationSec: (item.Duration + 500) / 1000,
		Codec:       plex.AudioFormat(media.AudioCodec, container),
		Container:   container,
		BitrateKbps: media.Bitrate,
		PartKey:     part.Key,
		Thumb:       thumb,
	}, true
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
