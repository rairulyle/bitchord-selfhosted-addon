// Package store owns the addon's one state file. Every write goes through
// Update, which validates, saves atomically and only then changes the copy
// in memory, so a failed save leaves both the file and the process as they
// were.
package store

import (
	"cmp"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const FileName = "addon.json"

type Kind string

const (
	Plex      Kind = "plex"
	Jellyfin  Kind = "jellyfin"
	Navidrome Kind = "navidrome"
)

var Kinds = []Kind{Plex, Jellyfin, Navidrome}

type Auth string

const (
	AuthToken           Auth = "token"
	AuthPlexSignIn      Auth = "plex-signin"
	AuthJellyfinSignIn  Auth = "jellyfin-signin"
	AuthNavidromeSignIn Auth = "navidrome-signin"
)

type Server struct {
	Slug            string    `json:"slug"`
	Label           string    `json:"label"`
	Kind            Kind      `json:"kind"`
	URL             string    `json:"url"`
	Token           string    `json:"token"`
	Auth            Auth      `json:"auth"`
	Account         string    `json:"account"`
	DeviceID        string    `json:"device_id,omitempty"`
	Library         string    `json:"library"`
	LibraryName     string    `json:"library_name,omitempty"`
	SyncInterval    string    `json:"sync_interval,omitempty"`
	ScrobblePercent int       `json:"scrobble_percent,omitempty"`
	Enabled         bool      `json:"enabled"`
	Created         time.Time `json:"created"`
}

type Admin struct {
	PasswordHash string `json:"password_hash"`
	SessionKey   string `json:"session_key"`
}

const (
	AppAuthDisabled = "disabled"
	AppAuthLog      = "log"
	AppAuthEnforce  = "enforce"
)

// AppAuth is the BitChord app authentication set on the setup page. An empty
// field means its default: disabled, no extra hosts, the default issuer.
type AppAuth struct {
	Mode   string   `json:"mode,omitempty"`
	Hosts  []string `json:"hosts,omitempty"`
	Issuer string   `json:"issuer,omitempty"`
}

func (a AppAuth) EffectiveMode() string {
	return cmp.Or(a.Mode, AppAuthDisabled)
}

type Snapshot struct {
	PublicURL string `json:"public_url"`
	Secret    string `json:"secret"`
	ClientID  string `json:"client_id"`
	// LinkKey signs the file links handed to BitChord. It never leaves this
	// server: a backup omits it and a restore makes a new one.
	LinkKey string   `json:"link_key,omitempty"`
	AppAuth AppAuth  `json:"app_auth,omitzero"`
	Admin   *Admin   `json:"admin,omitempty"`
	Servers []Server `json:"servers"`
}

func (s Snapshot) Server(slug string) (Server, bool) {
	for _, server := range s.Servers {
		if server.Slug == slug {
			return server, true
		}
	}
	return Server{}, false
}

func (s Snapshot) clone() Snapshot {
	out := s
	out.Servers = slices.Clone(s.Servers)
	out.AppAuth.Hosts = slices.Clone(s.AppAuth.Hosts)
	if s.Admin != nil {
		admin := *s.Admin
		out.Admin = &admin
	}
	return out
}

// Store serializes writers on mu; readers load the last saved snapshot
// without a lock, so a save's fsync never stalls them.
type Store struct {
	path    string
	mu      sync.Mutex
	current atomic.Pointer[Snapshot]
}

// Open reads dir/addon.json, or creates it with a fresh secret and client id
// when it does not exist. A file that does not parse or validate is an error
// naming it.
func Open(dir string) (*Store, error) {
	s := &Store{path: filepath.Join(dir, FileName)}
	raw, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		data := Snapshot{Secret: NewSecret(), ClientID: newClientID(), LinkKey: NewLinkKey(), Servers: []Server{}}
		if err := s.save(data); err != nil {
			return nil, err
		}
		s.current.Store(&data)
		return s, nil
	case err != nil:
		return nil, err
	}
	var data Snapshot
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}
	if err := Validate(data); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}
	if data.Servers == nil {
		data.Servers = []Server{}
	}
	if data.LinkKey == "" {
		data.LinkKey = NewLinkKey()
		if err := s.save(data); err != nil {
			return nil, err
		}
	}
	s.current.Store(&data)
	return s, nil
}

func (s *Store) Path() string { return s.path }

func (s *Store) Snapshot() Snapshot {
	return s.current.Load().clone()
}

// Update applies change to a copy, validates it, saves it, and only then
// makes it the current state.
func (s *Store) Update(change func(*Snapshot) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.current.Load().clone()
	if err := change(&next); err != nil {
		return err
	}
	if err := Validate(next); err != nil {
		return err
	}
	if err := s.save(next); err != nil {
		return err
	}
	s.current.Store(&next)
	return nil
}

func (s *Store) save(data Snapshot) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), FileName+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

// AddServer assigns the lowest free <kind>-<n> slug, stamps the creation
// time, and stores the server.
func (s *Store) AddServer(server Server) (Server, error) {
	err := s.Update(func(data *Snapshot) error {
		server.Slug = nextSlug(server.Kind, data.Servers)
		server.Created = time.Now().UTC().Truncate(time.Second)
		data.Servers = append(data.Servers, server)
		return nil
	})
	return server, err
}

func (s *Store) UpdateServer(slug string, change func(*Server) error) error {
	return s.Update(func(data *Snapshot) error {
		i := slices.IndexFunc(data.Servers, func(server Server) bool { return server.Slug == slug })
		if i < 0 {
			return ErrNoServer
		}
		server := data.Servers[i]
		if err := change(&server); err != nil {
			return err
		}
		server.Slug, server.Kind, server.Created = data.Servers[i].Slug, data.Servers[i].Kind, data.Servers[i].Created
		data.Servers[i] = server
		return nil
	})
}

func (s *Store) RemoveServer(slug string) error {
	return s.Update(func(data *Snapshot) error {
		before := len(data.Servers)
		data.Servers = slices.DeleteFunc(data.Servers, func(server Server) bool { return server.Slug == slug })
		if len(data.Servers) == before {
			return ErrNoServer
		}
		return nil
	})
}

func (s *Store) SetPublicURL(publicURL string) error {
	return s.Update(func(data *Snapshot) error {
		data.PublicURL = publicURL
		return nil
	})
}

func (s *Store) RegenerateSecret() (string, error) {
	secret := NewSecret()
	return secret, s.Update(func(data *Snapshot) error {
		data.Secret = secret
		return nil
	})
}

func (s *Store) SetAppAuth(settings AppAuth) error {
	return s.Update(func(data *Snapshot) error {
		data.AppAuth = settings
		return nil
	})
}

func (s *Store) SetAdmin(admin *Admin) error {
	return s.Update(func(data *Snapshot) error {
		data.Admin = admin
		return nil
	})
}

// Restore replaces the configuration with snap through the same validation as
// any save. The current admin is always kept, so a backup can never change the
// password or sign anyone in; a backup without a secret or client id keeps the
// current ones. The link key is always new, so a backup never carries one.
func (s *Store) Restore(snap Snapshot) error {
	return s.Update(func(data *Snapshot) error {
		restored := snap.clone()
		restored.Admin = data.Admin
		restored.Secret = cmp.Or(restored.Secret, data.Secret)
		restored.ClientID = cmp.Or(restored.ClientID, data.ClientID)
		restored.LinkKey = NewLinkKey()
		if restored.Servers == nil {
			restored.Servers = []Server{}
		}
		if err := Validate(restored); err != nil {
			return fmt.Errorf("the backup is not a valid configuration: %w", err)
		}
		*data = restored
		return nil
	})
}

var ErrNoServer = errors.New("no such server")

var slugPattern = regexp.MustCompile(`^(plex|jellyfin|navidrome)-[1-9][0-9]*$`)

func nextSlug(kind Kind, servers []Server) string {
	taken := map[string]bool{}
	for _, server := range servers {
		taken[server.Slug] = true
	}
	for n := 1; ; n++ {
		slug := string(kind) + "-" + strconv.Itoa(n)
		if !taken[slug] {
			return slug
		}
	}
}

// Ordered returns the servers by creation time, then slug, for a stable page.
func (s Snapshot) Ordered() []Server {
	out := slices.Clone(s.Servers)
	sort.SliceStable(out, func(a, b int) bool {
		if !out[a].Created.Equal(out[b].Created) {
			return out[a].Created.Before(out[b].Created)
		}
		return out[a].Slug < out[b].Slug
	})
	return out
}

const secretAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

func NewSecret() string {
	raw := make([]byte, 32)
	rand.Read(raw)
	for i, b := range raw {
		raw[i] = secretAlphabet[int(b)%len(secretAlphabet)]
	}
	return string(raw)
}

func NewLinkKey() string {
	raw := make([]byte, 32)
	rand.Read(raw)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func newClientID() string {
	raw := make([]byte, 16)
	rand.Read(raw)
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}
