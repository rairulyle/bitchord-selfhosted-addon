package subsonic

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
)

const (
	maxTrackedPlays = 64
	minListen       = 15 * time.Second
	maxListen       = 240 * time.Second
	unknownDuration = 180
	reportTimeout   = 5 * time.Second
)

type stopper interface{ Stop() bool }

// plays tracks one pending scrobble per track, so a second listener or a
// prefetched next track does not cancel the first. Players close and reopen
// the stream to probe, seek or read a trailing moov atom, so a closed stream
// says nothing about whether the listener stopped; a play is never cancelled.
type plays struct {
	client    *Client
	now       func() time.Time
	afterFunc func(time.Duration, func()) stopper
	mu        sync.Mutex
	byID      map[string]*play
}

type play struct {
	started time.Time
	timer   stopper
	done    bool
}

func newPlays(c *Client) *plays {
	return &plays{
		client:    c,
		now:       time.Now,
		afterFunc: func(d time.Duration, f func()) stopper { return time.AfterFunc(d, f) },
		byID:      map[string]*play{},
	}
}

// played records a GET for a track's file. The first GET starts a play; any
// later GET joins it, whatever its range, until the play is done and a GET
// from the start of the file begins a replay.
func (p *plays) played(track media.Track, rangeHeader string) {
	fromStart := rangeHeader == "" || strings.HasPrefix(rangeHeader, "bytes=0-")
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if current, ok := p.byID[track.ID]; ok {
		if !current.done || !fromStart || now.Sub(current.started) < minListen {
			return
		}
		stop(current)
		delete(p.byID, track.ID)
	}
	p.evictOldest()
	started := &play{started: now, done: p.client.scrobblePercent <= 0}
	p.byID[track.ID] = started
	go p.client.report(track, false)
	if !started.done {
		started.timer = p.afterFunc(p.client.listenFor(track), func() { p.finish(track, started) })
	}
}

func (p *plays) finish(track media.Track, finished *play) {
	p.mu.Lock()
	current, ok := p.byID[track.ID]
	if !ok || current != finished || finished.done {
		p.mu.Unlock()
		return
	}
	finished.done = true
	p.mu.Unlock()
	p.client.report(track, true)
}

func (p *plays) evictOldest() {
	if len(p.byID) < maxTrackedPlays {
		return
	}
	var oldestID string
	var oldest *play
	for id, candidate := range p.byID {
		if oldest == nil || candidate.started.Before(oldest.started) {
			oldestID, oldest = id, candidate
		}
	}
	stop(oldest)
	delete(p.byID, oldestID)
}

func (p *plays) stopAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, pending := range p.byID {
		stop(pending)
		delete(p.byID, id)
	}
}

func stop(pending *play) {
	if pending.timer != nil {
		pending.timer.Stop()
	}
}

// listenFor is how long a track plays before it counts as listened to,
// bounded like Last.fm's rule: at least 15 seconds, at most four minutes.
func (c *Client) listenFor(track media.Track) time.Duration {
	seconds := track.DurationSec
	if seconds <= 0 {
		seconds = unknownDuration
	}
	needed := time.Duration(seconds) * time.Second * time.Duration(c.scrobblePercent) / 100
	return min(max(needed, minListen), maxListen)
}

func (c *Client) report(track media.Track, submission bool) {
	ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
	defer cancel()
	if err := c.Scrobble(ctx, track.ID, submission); err != nil {
		c.log.Debug("navidrome playback report failed", "id", track.ID, "submission", submission, "error", err.Error())
		return
	}
	if c.onActivity == nil {
		return
	}
	label := track.Title
	if track.Artist != "" {
		label += " — " + track.Artist
	}
	if submission {
		c.onActivity("scrobble", fmt.Sprintf("Scrobbled: %s (%d%% played)", label, c.scrobblePercent))
	} else {
		c.onActivity("now_playing", "Now playing: "+label)
	}
}

// Close stops every pending scrobble.
func (c *Client) Close() { c.plays.stopAll() }
