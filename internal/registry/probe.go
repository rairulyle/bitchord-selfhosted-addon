package registry

import (
	"context"
	"net/url"
	"sync"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/jellyfin"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/plex"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/store"
)

type Probe struct {
	Version   string
	Libraries []media.Library
}

// Probe checks that a server answers with the given credentials and lists
// its music libraries. Libraries is asked first because Plex answers
// /identity without a token, so a bad token would otherwise pass.
func (r *Registry) Probe(ctx context.Context, kind store.Kind, serverURL, token string) (Probe, error) {
	backend, err := NewBackend(store.Server{Kind: kind, URL: serverURL, Token: token}, r.clientIDNow(), r.opts.Version, r.opts.Log)
	if err != nil {
		return Probe{}, err
	}
	libraries, err := backend.Libraries(ctx)
	if err != nil {
		return Probe{}, err
	}
	version, _ := backend.Version(ctx)
	return Probe{Version: version, Libraries: libraries}, nil
}

func (r *Registry) clientIDNow() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.clientID
}

func (r *Registry) PlexSignIn() *plex.SignIn {
	return &plex.SignIn{ClientID: r.clientIDNow(), Version: r.opts.Version, PlexTV: r.opts.PlexTV, HTTP: r.opts.HTTP}
}

func (r *Registry) JellyfinSignIn(ctx context.Context, serverURL, username, password string) (jellyfin.Session, error) {
	return jellyfin.SignIn(ctx, r.opts.HTTP, serverURL, r.clientIDNow(), r.opts.Version, username, password)
}

type PlexServer struct {
	Name      string
	URL       string
	Reachable bool
}

// PlexServers lists the account's servers and tries each one's addresses
// from here, all at once, so the whole call takes about one probe timeout.
// A server is reported on the first of its addresses, in plex.tv's local-first
// order, that answers /identity.
func (r *Registry) PlexServers(ctx context.Context, token string) ([]PlexServer, error) {
	resources, err := r.PlexSignIn().Servers(ctx, token)
	if err != nil {
		return nil, err
	}
	answered := make([][]bool, len(resources))
	var wg sync.WaitGroup
	for i, resource := range resources {
		answered[i] = make([]bool, len(resource.Connections))
		for j, address := range resource.Connections {
			wg.Add(1)
			go func() {
				defer wg.Done()
				answered[i][j] = r.reachable(ctx, address, token)
			}()
		}
	}
	wg.Wait()
	out := make([]PlexServer, len(resources))
	for i, resource := range resources {
		out[i] = PlexServer{Name: resource.Name}
		if len(resource.Connections) > 0 {
			out[i].URL = resource.Connections[0]
		}
		for j, address := range resource.Connections {
			if answered[i][j] {
				out[i].URL, out[i].Reachable = address, true
				break
			}
		}
	}
	return out, nil
}

func (r *Registry) reachable(ctx context.Context, address, token string) bool {
	if _, err := url.Parse(address); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, r.opts.ProbeTimeout)
	defer cancel()
	client := plex.New(plex.Options{BaseURL: address, Token: token, ClientID: r.clientIDNow(), HeaderTimeout: r.opts.ProbeTimeout, CallTimeout: r.opts.ProbeTimeout})
	_, err := client.Version(ctx)
	return err == nil
}
