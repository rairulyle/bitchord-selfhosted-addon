package plex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
)

const (
	Product       = "BitChord Selfhosted Addon"
	DefaultPlexTV = "https://plex.tv"
)

// SignIn drives the plex.tv PIN flow. Plex ties the resulting token to the
// client identifier that asked for the PIN, so ClientID must stay stable.
type SignIn struct {
	ClientID string
	Version  string
	PlexTV   string
	HTTP     *http.Client
}

type PIN struct {
	ID      int
	Code    string
	AuthURL string
}

type Account struct {
	Token    string
	Username string
}

type Resource struct {
	Name        string
	Connections []string
}

func (s *SignIn) NewPIN(ctx context.Context) (PIN, error) {
	var answer struct {
		ID   int    `json:"id"`
		Code string `json:"code"`
	}
	if err := s.call(ctx, http.MethodPost, "/api/v2/pins?strong=true", "", &answer); err != nil {
		return PIN{}, err
	}
	query := url.Values{
		"clientID":                 {s.ClientID},
		"code":                     {answer.Code},
		"context[device][product]": {Product},
	}
	return PIN{ID: answer.ID, Code: answer.Code, AuthURL: "https://app.plex.tv/auth#?" + query.Encode()}, nil
}

// Claim reports false until the user has signed in on app.plex.tv.
func (s *SignIn) Claim(ctx context.Context, id int) (Account, bool, error) {
	var answer struct {
		AuthToken string `json:"authToken"`
	}
	if err := s.call(ctx, http.MethodGet, "/api/v2/pins/"+strconv.Itoa(id), "", &answer); err != nil {
		return Account{}, false, err
	}
	if answer.AuthToken == "" {
		return Account{}, false, nil
	}
	var user struct {
		Username string `json:"username"`
	}
	if err := s.call(ctx, http.MethodGet, "/api/v2/user", answer.AuthToken, &user); err != nil {
		return Account{}, false, err
	}
	return Account{Token: answer.AuthToken, Username: user.Username}, true, nil
}

// Servers lists the account's servers with local addresses first, then
// direct remote ones, then relays.
func (s *SignIn) Servers(ctx context.Context, token string) ([]Resource, error) {
	var answer []struct {
		Name        string `json:"name"`
		Provides    string `json:"provides"`
		Connections []struct {
			URI   string `json:"uri"`
			Local bool   `json:"local"`
			Relay bool   `json:"relay"`
		} `json:"connections"`
	}
	if err := s.call(ctx, http.MethodGet, "/api/v2/resources?includeHttps=1&includeRelay=1", token, &answer); err != nil {
		return nil, err
	}
	var out []Resource
	for _, resource := range answer {
		if resource.Provides != "server" {
			continue
		}
		connections := resource.Connections
		sort.SliceStable(connections, func(a, b int) bool {
			return rank(connections[a].Local, connections[a].Relay) < rank(connections[b].Local, connections[b].Relay)
		})
		uris := make([]string, len(connections))
		for i, connection := range connections {
			uris[i] = connection.URI
		}
		out = append(out, Resource{Name: resource.Name, Connections: uris})
	}
	return out, nil
}

func rank(local, relay bool) int {
	switch {
	case local:
		return 0
	case relay:
		return 2
	}
	return 1
}

func (s *SignIn) call(ctx context.Context, method, path, token string, into any) error {
	base := s.PlexTV
	if base == "" {
		base = DefaultPlexTV
	}
	client := s.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Client-Identifier", s.ClientID)
	req.Header.Set("X-Plex-Product", Product)
	req.Header.Set("X-Plex-Version", s.Version)
	if token != "" {
		req.Header.Set("X-Plex-Token", token)
	}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("plex.tv: %w", err)
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("plex.tv rejected the token: %w", media.ErrUnauthorized)
	case res.StatusCode == http.StatusNotFound:
		return fmt.Errorf("plex.tv: %w", media.ErrNotFound)
	case res.StatusCode < 200 || res.StatusCode > 299:
		return fmt.Errorf("plex.tv: %w", media.StatusError(res.StatusCode))
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(into); err != nil {
		return fmt.Errorf("plex.tv: malformed answer: %w", err)
	}
	return nil
}
