// Package store owns the addon's one state file. Every write goes through
// Update, which validates, saves atomically and only then changes the copy
// in memory, so a failed save leaves both the file and the process as they
// were.
package store

import (
	"crypto/rand"
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
	"time"
)

const FileName = "addon.json"

type Kind string

const (
	Plex     Kind = "plex"
	Jellyfin Kind = "jellyfin"
)

var Kinds = []Kind{Plex, Jellyfin}

type Auth string

const (
	AuthToken          Auth = "token"
	AuthPlexSignIn     Auth = "plex-signin"
	AuthJellyfinSignIn Auth = "jellyfin-signin"
)

type Server struct {
	Slug     string    `json:"slug"`
	Label    string    `json:"label"`
	Kind     Kind      `json:"kind"`
	URL      string    `json:"url"`
	Token    string    `json:"token"`
	Auth     Auth      `json:"auth"`
	Account  string    `json:"account"`
	DeviceID string    `json:"device_id,omitempty"`
	Library  string    `json:"library"`
	Enabled  bool      `json:"enabled"`
	Created  time.Time `json:"created"`
}

type Admin struct {
	PasswordHash string `json:"password_hash"`
	SessionKey   string `json:"session_key"`
}

type Snapshot struct {
	PublicURL string   `json:"public_url"`
	Secret    string   `json:"secret"`
	ClientID  string   `json:"client_id"`
	Admin     *Admin   `json:"admin,omitempty"`
	Servers   []Server `json:"servers"`
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
	if s.Admin != nil {
		admin := *s.Admin
		out.Admin = &admin
	}
	return out
}

type Store struct {
	path string
	mu   sync.Mutex
	data Snapshot
}

// Open reads dir/addon.json, or creates it with a fresh secret and client id
// when it does not exist. A file that does not parse or validate is an error
// naming it.
func Open(dir string) (*Store, error) {
	s := &Store{path: filepath.Join(dir, FileName)}
	raw, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		s.data = Snapshot{Secret: NewSecret(), ClientID: newClientID(), Servers: []Server{}}
		if err := s.save(s.data); err != nil {
			return nil, err
		}
		return s, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(raw, &s.data); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}
	if err := Validate(s.data); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}
	if s.data.Servers == nil {
		s.data.Servers = []Server{}
	}
	return s, nil
}

func (s *Store) Path() string { return s.path }

func (s *Store) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.clone()
}

// Update applies change to a copy, validates it, saves it, and only then
// makes it the current state.
func (s *Store) Update(change func(*Snapshot) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.data.clone()
	if err := change(&next); err != nil {
		return err
	}
	if err := Validate(next); err != nil {
		return err
	}
	if err := s.save(next); err != nil {
		return err
	}
	s.data = next
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

func (s *Store) SetAdmin(admin *Admin) error {
	return s.Update(func(data *Snapshot) error {
		data.Admin = admin
		return nil
	})
}

var ErrNoServer = errors.New("no such server")

var slugPattern = regexp.MustCompile(`^(plex|jellyfin)-[1-9][0-9]*$`)

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

func newClientID() string {
	raw := make([]byte, 16)
	rand.Read(raw)
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}
