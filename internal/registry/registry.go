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
	"sync"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/jellyfin"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/library"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/plex"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/store"
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

type Status struct {
	Ready     bool
	Tracks    int
	Refreshed time.Time
	LastError string
}

type Registry struct {
	opts      Options
	clientID  string
	mu        sync.RWMutex
	entries   map[string]*Entry
	pending   map[string]*handover
	handovers sync.WaitGroup
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
	r := &Registry{opts: o, clientID: snapshot.ClientID, entries: map[string]*Entry{}, pending: map[string]*handover{}}
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

func (r *Registry) Status(slug string) (Status, bool) {
	entry, ok := r.Lookup(slug)
	if !ok {
		return Status{}, false
	}
	stats := entry.Library.Stats()
	return Status{Ready: entry.Library.Ready(), Tracks: stats.Tracks, Refreshed: stats.Refreshed, LastError: stats.LastError}, true
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
}

// follow takes the new settings of a server whose connection is unchanged.
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
	return a.Kind == b.Kind && a.URL == b.URL && a.Token == b.Token && a.DeviceID == b.DeviceID && a.Library == b.Library
}

func (r *Registry) start(server store.Server) (*Entry, error) {
	log := r.opts.Log.With("server", server.Slug, "source", string(server.Kind))
	backend, err := NewBackend(server, r.clientID, r.opts.Version, log)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	entry := &Entry{
		Slug: server.Slug, Kind: server.Kind, Label: server.Label, Backend: backend,
		Library: library.NewLibrary(backend, server.Library, r.opts.Interval, log),
		Log:     log, config: server, cancel: cancel, done: make(chan struct{}),
	}
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
