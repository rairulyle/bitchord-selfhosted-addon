package setup

import (
	"cmp"
	"net/http"
	"net/url"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/registry"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/store"
)

type page struct {
	Title    string
	Version  string
	CSRF     string
	SignedIn bool
	Notice   string
	Error    string
}

func (a *app) page(r *http.Request, title string) page {
	p := page{Title: title, Version: a.Version, Notice: r.URL.Query().Get("notice")}
	if s := currentSession(r); s.cookie != "" {
		p.SignedIn = true
		p.CSRF = csrfToken(s.key, s.cookie)
	}
	return p
}

type passwordView struct {
	page
	Create bool
}

type loginView struct {
	page
}

type kindOption struct {
	Kind store.Kind
	Name string
}

var kindOptions = []kindOption{{store.Plex, "Plex"}, {store.Jellyfin, "Jellyfin"}}

func kindName(kind store.Kind) string {
	for _, option := range kindOptions {
		if option.Kind == kind {
			return option.Name
		}
	}
	return string(kind)
}

type serverCard struct {
	Slug     string
	Name     string
	Kind     string
	Host     string
	Library  string
	URL      string
	Account  string
	Auth     store.Auth
	Enabled  bool
	Running  bool
	Status   registry.Status
	Checked  string
	Problems string
}

type overviewView struct {
	page
	PublicURL string
	Secret    string
	Servers   []serverCard
}

func (a *app) overviewView(r *http.Request, snapshot store.Snapshot) overviewView {
	view := overviewView{page: a.page(r, "Servers"), PublicURL: snapshot.PublicURL, Secret: snapshot.Secret}
	now := a.Now()
	for _, server := range snapshot.Ordered() {
		card := serverCard{
			Slug: server.Slug, Name: registry.DisplayName(kindName(server.Kind), server.Label), Kind: kindName(server.Kind),
			Host: hostOf(server.URL), Library: cmp.Or(server.LibraryName, server.Library), Account: server.Account, Auth: server.Auth, Enabled: server.Enabled,
		}
		if card.Library == "" {
			card.Library = "all music"
		}
		if snapshot.PublicURL != "" {
			card.URL = snapshot.PublicURL + "/" + server.Slug + "/" + snapshot.Secret
		}
		if status, ok := a.Registry.Status(server.Slug); ok {
			card.Running, card.Status, card.Checked = true, status, since(status.Refreshed, now)
		}
		view.Servers = append(view.Servers, card)
	}
	return view
}

type serverFormView struct {
	page
	Editing     bool
	Slug        string
	Kind        store.Kind
	KindName    string
	Label       string
	URL         string
	Library     string
	LibraryName string
	Account     string
	Auth        store.Auth
	Enabled     bool
	Libraries   []media.Library
	Kinds       []kindOption
	Action      string
}

func (a *app) formView(r *http.Request, title string) serverFormView {
	return serverFormView{page: a.page(r, title), Kind: store.Plex, KindName: "Plex", Enabled: true, Kinds: kindOptions, Action: "/setup/servers", Auth: store.AuthToken}
}

func hostOf(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return parsed.Host
}
