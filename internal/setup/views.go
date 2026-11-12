package setup

import (
	"cmp"
	"fmt"
	"net/http"
	"net/url"
	"time"

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
	p := page{
		Title:   title,
		Version: a.Version,
		Notice:  r.URL.Query().Get("notice"),
		Error:   r.URL.Query().Get("error"),
	}
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

var kindOptions = []kindOption{
	{store.Plex, "Plex"},
	{store.Jellyfin, "Jellyfin"},
	{store.Navidrome, "Navidrome / Subsonic"},
}

func kindName(kind store.Kind) string {
	switch kind {
	case store.Plex:
		return "Plex"
	case store.Jellyfin:
		return "Jellyfin"
	case store.Navidrome:
		return "Navidrome"
	default:
		return string(kind)
	}
}

type serverCard struct {
	Slug          string
	Name          string
	Kind          string
	Host          string
	ServerURL     string
	Library       string
	URL           string
	MaskedURL     string
	SyncURL       string
	MaskedSyncURL string
	SyncInterval  string
	Account       string
	Auth          store.Auth
	Enabled       bool
	Running       bool
	Status        registry.Status
	ScanDuration  string
	Checked       string
	Online        bool
	ScrobbleInfo  string
	Problems      string
}

type ActivityView struct {
	TimeAgo string
	Slug    string
	Server  string
	Kind    string
	Message string
}

type overviewView struct {
	page
	PublicURL  string
	Secret     string
	Servers    []serverCard
	Activities []ActivityView
}

func (a *app) overviewView(r *http.Request, snapshot store.Snapshot) overviewView {
	view := overviewView{page: a.page(r, "Servers"), PublicURL: snapshot.PublicURL, Secret: snapshot.Secret}
	now := a.Now()
	for _, server := range snapshot.Ordered() {
		card := serverCard{
			Slug: server.Slug, Name: registry.DisplayName(kindName(server.Kind), server.Label), Kind: kindName(server.Kind),
			Host: hostOf(server.URL), ServerURL: server.URL, Library: cmp.Or(server.LibraryName, server.Library), Account: server.Account, Auth: server.Auth, Enabled: server.Enabled,
		}
		if card.Library == "" {
			card.Library = "All music"
		}
		if snapshot.PublicURL != "" {
			card.URL = snapshot.PublicURL + "/" + server.Slug + "/" + snapshot.Secret
			card.MaskedURL = snapshot.PublicURL + "/" + server.Slug + "/••••••••••"
			card.SyncURL = snapshot.PublicURL + "/" + server.Slug + "/" + snapshot.Secret + "/sync"
			card.MaskedSyncURL = snapshot.PublicURL + "/" + server.Slug + "/••••••••••/sync"
		}
		card.SyncInterval = formatInterval(server.SyncInterval, a.Registry.DefaultInterval())
		if server.Kind == store.Navidrome {
			if server.ScrobblePercent > 0 {
				card.ScrobbleInfo = fmt.Sprintf("Enabled (at %d%%)", server.ScrobblePercent)
			} else {
				card.ScrobbleInfo = "Disabled"
			}
		}
		if status, ok := a.Registry.Status(server.Slug); ok {
			card.Running, card.Status, card.Checked = true, status, since(status.Refreshed, now)
			if status.Duration > 0 {
				card.ScanDuration = formatDurationMs(status.Duration)
			}
			card.Online = card.Running && status.Ready && status.LastError == ""
		}
		view.Servers = append(view.Servers, card)
	}
	for _, act := range a.Registry.RecentActivity() {
		view.Activities = append(view.Activities, ActivityView{
			TimeAgo: since(act.Time, now),
			Slug:    act.Slug,
			Server:  act.Server,
			Kind:    act.Kind,
			Message: act.Message,
		})
	}
	return view
}

func formatInterval(raw string, defaultInterval time.Duration) string {
	defStr := formatDuration(defaultInterval)
	switch raw {
	case "":
		return fmt.Sprintf("Default (%s)", defStr)
	case "manual", "0":
		return "Manual only"
	default:
		return "Every " + raw
	}
}

func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "15m"
	}
	hours, minutes := int(d/time.Hour), int(d%time.Hour/time.Minute)
	switch {
	case hours == 0 && minutes > 0:
		return fmt.Sprintf("%dm", minutes)
	case minutes == 0 && hours > 0:
		return fmt.Sprintf("%dh", hours)
	case hours > 0 && minutes > 0:
		return fmt.Sprintf("%dh%dm", hours, minutes)
	default:
		return d.String()
	}
}

func formatDurationMs(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

type serverFormView struct {
	page
	Editing         bool
	Slug            string
	Kind            store.Kind
	KindName        string
	Label           string
	URL             string
	Library         string
	LibraryName     string
	SyncInterval    string
	DefaultInterval string
	Account         string
	Auth            store.Auth
	Enabled         bool
	ScrobblePercent int
	Libraries       []media.Library
	Kinds           []kindOption
	Action          string
}

func (a *app) formView(r *http.Request, title string) serverFormView {
	defInterval := formatDuration(a.Registry.DefaultInterval())
	return serverFormView{page: a.page(r, title), Kind: store.Plex, KindName: "Plex", Enabled: true, ScrobblePercent: -1, DefaultInterval: defInterval, Kinds: kindOptions, Action: "/setup/servers", Auth: store.AuthToken}
}

func hostOf(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return parsed.Host
}
