// Package registry runs the configured servers: one adapter, one library
// index and one refresher per enabled server. It follows the store; the
// setup page writes the store and then calls Apply.
package registry

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/jellyfin"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/library"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/plex"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/store"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/subsonic"
)

type Options struct {
	Interval     time.Duration
	Version      string
	Log          *slog.Logger
	PlexTV       string
	HTTP         *http.Client
	ProbeTimeout time.Duration
	// HandoverTimeout caps how long a changed server keeps answering from
	// its old entry while the new one loads its first index.
	HandoverTimeout time.Duration
}

type Entry struct {
	Slug    string
	Kind    store.Kind
	Label   string
	Backend media.Backend
	Library *library.Library
	Log     *slog.Logger
	mu      sync.RWMutex
	config  store.Server
	cancel  context.CancelFunc
	done    chan struct{}
}

// DisplayName is what BitChord shows: "Plex - Home", or "Plex" with no label.
func (e *Entry) DisplayName() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return DisplayName(e.Backend.Name(), e.Label)
}

func DisplayName(kind, label string) string {
	if label == "" {
		return kind
	}
	return kind + " - " + label
}

type Activity struct {
	Seq     uint64
	Time    time.Time
	Slug    string
	Server  string
	Kind    string
	Message string
}

type Status struct {
	Ready     bool
	Tracks    int
	Artists   int
	Albums    int
	Duration  time.Duration
	Refreshed time.Time
	LastError string
}

const (
	maxSyncActivities     = 10
	maxPlaybackActivities = 30
)

type Registry struct {
	opts               Options
	clientID           string
	mu                 sync.RWMutex
	entries            map[string]*Entry
	pending            map[string]*handover
	handovers          sync.WaitGroup
	changed            chan struct{}
	activityMu         sync.RWMutex
	activitySeq        uint64
	syncActivities     []Activity
	playbackActivities []Activity
}

// handover is a restarted server's new entry, waiting for its first refresh
// before it replaces the entry that is still answering.
type handover struct {
	next      *Entry
	cancelled chan struct{}
}

func (h *handover) cancel() {
	close(h.cancelled)
	h.next.stop()
}

// TokenInUse reports whether the running or the starting entry for slug
// sends token, and returns a channel closed at the registry's next change, so
// a caller can wait and ask again without missing one.
func (r *Registry) TokenInUse(slug, token string) (bool, <-chan struct{}) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, running := r.entries[slug]
	pending, starting := r.pending[slug]
	used := running && entry.config.Token == token || starting && pending.next.config.Token == token
	return used, r.changed
}

func (r *Registry) announceChange() {
	close(r.changed)
	r.changed = make(chan struct{})
}

func New(snapshot store.Snapshot, o Options) *Registry {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Interval <= 0 {
		o.Interval = 15 * time.Minute
	}
	if o.ProbeTimeout <= 0 {
		o.ProbeTimeout = 3 * time.Second
	}
	if o.HandoverTimeout <= 0 {
		o.HandoverTimeout = 30 * time.Second
	}
	r := &Registry{opts: o, clientID: snapshot.ClientID, entries: map[string]*Entry{}, pending: map[string]*handover{}, changed: make(chan struct{})}
	r.Apply(snapshot)
	return r
}

func NewBackend(server store.Server, clientID, version string, log *slog.Logger) (media.Backend, error) {
	switch server.Kind {
	case store.Plex:
		return plex.New(plex.Options{BaseURL: server.URL, Token: server.Token, ClientID: clientID, Log: log}), nil
	case store.Jellyfin:
		deviceID := server.DeviceID
		if deviceID == "" {
			deviceID = clientID
		}
		return jellyfin.New(jellyfin.Options{BaseURL: server.URL, APIKey: server.Token, DeviceID: deviceID, Version: version}), nil
	case store.Navidrome:
		username, password := subsonic.SplitCredential(server.Token)
		return subsonic.New(subsonic.Options{BaseURL: server.URL, Username: username, Password: password, ScrobblePercent: server.ScrobblePercent, Log: log}), nil
	default:
		return nil, fmt.Errorf("unknown server kind %q", server.Kind)
	}
}

func (r *Registry) Lookup(slug string) (*Entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.entries[slug]
	return entry, ok
}

func isSyncActivity(kind string) bool {
	return kind == "sync" || strings.HasPrefix(kind, "sync")
}

func (r *Registry) RecordActivity(slug, server, kind, message string) {
	r.activityMu.Lock()
	defer r.activityMu.Unlock()
	r.activitySeq++
	act := Activity{
		Seq:     r.activitySeq,
		Time:    time.Now(),
		Slug:    slug,
		Server:  server,
		Kind:    kind,
		Message: message,
	}
	if isSyncActivity(kind) {
		r.syncActivities = append([]Activity{act}, r.syncActivities...)
		if len(r.syncActivities) > maxSyncActivities {
			r.syncActivities = r.syncActivities[:maxSyncActivities]
		}
	} else {
		r.playbackActivities = append([]Activity{act}, r.playbackActivities...)
		if len(r.playbackActivities) > maxPlaybackActivities {
			r.playbackActivities = r.playbackActivities[:maxPlaybackActivities]
		}
	}
}

func (r *Registry) RecentActivity() []Activity {
	r.activityMu.RLock()
	defer r.activityMu.RUnlock()

	res := make([]Activity, 0, len(r.syncActivities)+len(r.playbackActivities))
	i, j := 0, 0
	for i < len(r.syncActivities) && j < len(r.playbackActivities) {
		sAct := r.syncActivities[i]
		pAct := r.playbackActivities[j]
		if sAct.Time.After(pAct.Time) || (sAct.Time.Equal(pAct.Time) && sAct.Seq > pAct.Seq) {
			res = append(res, sAct)
			i++
		} else {
			res = append(res, pAct)
			j++
		}
	}
	for i < len(r.syncActivities) {
		res = append(res, r.syncActivities[i])
		i++
	}
	for j < len(r.playbackActivities) {
		res = append(res, r.playbackActivities[j])
		j++
	}
	return res
}

func (r *Registry) RecentSyncActivity() []Activity {
	r.activityMu.RLock()
	defer r.activityMu.RUnlock()
	res := make([]Activity, len(r.syncActivities))
	copy(res, r.syncActivities)
	return res
}

func (r *Registry) RecentPlaybackActivity() []Activity {
	r.activityMu.RLock()
	defer r.activityMu.RUnlock()
	res := make([]Activity, len(r.playbackActivities))
	copy(res, r.playbackActivities)
	return res
}

func (r *Registry) ClearActivity(filter string) {
	r.activityMu.Lock()
	defer r.activityMu.Unlock()
	switch filter {
	case "sync":
		r.syncActivities = nil
	case "playback":
		r.playbackActivities = nil
	default:
		r.syncActivities = nil
		r.playbackActivities = nil
	}
}

func (r *Registry) Status(slug string) (Status, bool) {
	entry, ok := r.Lookup(slug)
	if !ok {
		return Status{}, false
	}
	stats := entry.Library.Stats()
	return Status{
		Ready:     entry.Library.Ready(),
		Tracks:    stats.Tracks,
		Artists:   stats.Artists,
		Albums:    stats.Albums,
		Duration:  stats.Duration,
		Refreshed: stats.Refreshed,
		LastError: stats.LastError,
	}, true
}

// Refresh triggers an immediate library sync for the server named by slug.
func (r *Registry) Refresh(ctx context.Context, slug string) error {
	entry, ok := r.Lookup(slug)
	if !ok {
		return fmt.Errorf("server %q is not running", slug)
	}
	return entry.Library.Refresh(ctx)
}

type SearchResult struct {
	Strict   int                   `json:"strict"`
	Fallback int                   `json:"fallback"`
	Tokens   []string              `json:"tokens"`
	Tracks   []media.Track         `json:"tracks"`
	Albums   []library.AlbumMatch  `json:"albums"`
	Artists  []library.ArtistMatch `json:"artists"`
}

// Search queries a server's in-memory index directly for testing and diagnostics.
func (r *Registry) Search(slug, query string, limit int) (SearchResult, bool) {
	entry, ok := r.Lookup(slug)
	if !ok {
		return SearchResult{}, false
	}
	tokens := library.Tokens(query)
	res := entry.Library.Find(query, limit)
	return SearchResult{
		Strict:   res.Strict,
		Fallback: res.Fallback,
		Tokens:   tokens,
		Tracks:   res.Tracks,
		Albums:   res.Albums,
		Artists:  res.Artists,
	}, true
}

// DefaultInterval reports the registry's configured default sync interval.
func (r *Registry) DefaultInterval() time.Duration {
	return r.opts.Interval
}

// Healthy is true once every running server has loaded an index, and true
// with no servers at all, so an unconfigured container passes its healthcheck.
func (r *Registry) Healthy() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, entry := range r.entries {
		if !entry.Library.Ready() {
			return false
		}
	}
	return true
}

// Apply makes the running set match the snapshot. A label change is applied
// in place. A server whose connection settings changed gets a new entry, and
// its old entry keeps answering until the new one has tried its first
// refresh, so Apply can return before the swap.
func (r *Registry) Apply(snapshot store.Snapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer r.announceChange()
	r.clientID = snapshot.ClientID
	wanted := map[string]bool{}
	for _, server := range snapshot.Servers {
		if !server.Enabled {
			continue
		}
		wanted[server.Slug] = true
		if pending, ok := r.pending[server.Slug]; ok {
			if sameConnection(pending.next.config, server) {
				pending.next.follow(server)
				r.entries[server.Slug].setLabel(server.Label)
				continue
			}
			pending.cancel()
			delete(r.pending, server.Slug)
		}
		existing, running := r.entries[server.Slug]
		if running && sameConnection(existing.config, server) {
			existing.follow(server)
			continue
		}
		entry, err := r.start(server)
		if err != nil {
			r.opts.Log.Error("server not started", "server", server.Slug, "error", err.Error())
			continue
		}
		if !running || !existing.Library.Ready() {
			if running {
				existing.stop()
			}
			r.entries[server.Slug] = entry
			continue
		}
		existing.setLabel(server.Label)
		pending := &handover{next: entry, cancelled: make(chan struct{})}
		r.pending[server.Slug] = pending
		r.handovers.Add(1)
		go r.handOver(server.Slug, pending)
	}
	for slug, pending := range r.pending {
		if !wanted[slug] {
			pending.cancel()
			delete(r.pending, slug)
		}
	}
	for slug, entry := range r.entries {
		if !wanted[slug] {
			entry.stop()
			delete(r.entries, slug)
		}
	}
}

func (r *Registry) handOver(slug string, pending *handover) {
	defer r.handovers.Done()
	timer := time.NewTimer(r.opts.HandoverTimeout)
	defer timer.Stop()
	select {
	case <-pending.next.Library.Tried():
	case <-timer.C:
	case <-pending.cancelled:
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending[slug] != pending {
		return
	}
	delete(r.pending, slug)
	r.entries[slug].stop()
	r.entries[slug] = pending.next
	r.announceChange()
}

func (e *Entry) follow(server store.Server) {
	e.setLabel(server.Label)
	e.config = server
}

func (e *Entry) setLabel(label string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Label = label
}

func sameConnection(a, b store.Server) bool {
	return a.Kind == b.Kind && a.URL == b.URL && a.Token == b.Token && a.DeviceID == b.DeviceID && a.Library == b.Library && a.SyncInterval == b.SyncInterval
}

func (r *Registry) serverInterval(server store.Server) time.Duration {
	if server.SyncInterval == "manual" || server.SyncInterval == "0" {
		return 0
	}
	if server.SyncInterval != "" {
		if d, err := time.ParseDuration(server.SyncInterval); err == nil && d > 0 {
			return d
		}
	}
	return r.opts.Interval
}

func (r *Registry) start(server store.Server) (*Entry, error) {
	log := r.opts.Log.With("server", server.Slug, "source", string(server.Kind))
	backend, err := NewBackend(server, r.clientID, r.opts.Version, log)
	if err != nil {
		return nil, err
	}
	slug := server.Slug
	name := DisplayName(backend.Name(), server.Label)
	if sub, ok := backend.(interface{ SetOnActivity(func(string, string)) }); ok {
		sub.SetOnActivity(func(kind, message string) {
			r.RecordActivity(slug, name, kind, message)
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	entry := &Entry{
		Slug: server.Slug, Kind: server.Kind, Label: server.Label, Backend: backend,
		Library: library.NewLibrary(backend, server.Library, r.serverInterval(server), log),
		Log:     log, config: server, cancel: cancel, done: make(chan struct{}),
	}
	entry.Library.SetOnActivity(func(kind, message string) {
		r.RecordActivity(slug, name, kind, message)
	})
	go func() {
		defer close(entry.done)
		entry.Library.Run(ctx)
	}()
	log.Info("server started", "host", hostOf(server.URL), "library", libraryOrAll(server.Library))
	return entry, nil
}

func (e *Entry) stop() {
	e.cancel()
	<-e.done
	e.Log.Info("server stopped")
}

func (r *Registry) Stop() {
	r.mu.Lock()
	for slug, pending := range r.pending {
		pending.cancel()
		delete(r.pending, slug)
	}
	for slug, entry := range r.entries {
		entry.stop()
		delete(r.entries, slug)
	}
	r.announceChange()
	r.mu.Unlock()
	r.handovers.Wait()
}

func hostOf(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return parsed.Host
}

func libraryOrAll(filter string) string {
	if filter == "" {
		return "all music"
	}
	return filter
}
