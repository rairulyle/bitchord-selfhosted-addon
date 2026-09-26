package registry

import (
	"context"
	"crypto/rand"
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

// Probe checks that a server answers with its credentials and lists its
// music libraries. Libraries is asked first because Plex answers /identity
// without a token, so a bad token would otherwise pass.
func (r *Registry) Probe(ctx context.Context, server store.Server) (Probe, error) {
	backend, err := NewBackend(server, r.clientIDNow(), r.opts.Version, r.opts.Log)
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

func (r *Registry) plexSignIn() *plex.SignIn {
	return &plex.SignIn{ClientID: r.clientIDNow(), Version: r.opts.Version, PlexTV: r.opts.PlexTV, HTTP: r.opts.HTTP}
}

// PIN and Account are what the setup page needs from a sign-in; the adapter
// types stay inside this package.
type PIN struct {
	ID      int
	AuthURL string
}

type Account struct {
	Token    string
	Username string
	DeviceID string
}

func (r *Registry) PlexPIN(ctx context.Context) (PIN, error) {
	pin, err := r.plexSignIn().NewPIN(ctx)
	if err != nil {
		return PIN{}, err
	}
	return PIN{ID: pin.ID, AuthURL: pin.AuthURL}, nil
}

// PlexClaim reports false until the user has signed in on app.plex.tv.
func (r *Registry) PlexClaim(ctx context.Context, id int) (Account, bool, error) {
	account, ok, err := r.plexSignIn().Claim(ctx, id)
	if err != nil || !ok {
		return Account{}, false, err
	}
	return Account{Token: account.Token, Username: account.Username}, true, nil
}

// JellyfinSignIn signs in as a new device each time: Jellyfin ends the other
// sessions of a device when it signs in again, so two servers added with the
// same account must not share one.
func (r *Registry) JellyfinSignIn(ctx context.Context, serverURL, username, password string) (Account, error) {
	deviceID := rand.Text()
	session, err := jellyfin.SignIn(ctx, r.opts.HTTP, serverURL, deviceID, r.opts.Version, username, password)
	if err != nil {
		return Account{}, err
	}
	return Account{Token: session.Token, Username: session.Username, DeviceID: deviceID}, nil
}

// JellyfinSignOut revokes a signed-in server's old token.
func (r *Registry) JellyfinSignOut(ctx context.Context, serverURL, deviceID, token string) error {
	if deviceID == "" {
		deviceID = r.clientIDNow()
	}
	return jellyfin.SignOut(ctx, r.opts.HTTP, serverURL, r.opts.Version, deviceID, token)
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
	resources, err := r.plexSignIn().Servers(ctx, token)
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
