// Package setup is the admin page: one password, a cookie session, and forms
// that write the store and then tell the registry to follow it.
package setup

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/registry"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/store"
)

//go:embed templates/*.html
var templateFiles embed.FS

//go:embed static/style.css
var staticFiles embed.FS

const (
	cookieName      = "addon_setup"
	maxFormBytes    = 64 << 10
	loginAttempts   = 5
	lockoutWindow   = time.Minute
	tokenRefTTL     = 10 * time.Minute
	verifyPasswords = 2
)

type Runtime interface {
	Apply(snapshot store.Snapshot)
	Status(slug string) (registry.Status, bool)
	Probe(ctx context.Context, server store.Server) (registry.Probe, error)
	PlexPIN(ctx context.Context) (registry.PIN, error)
	PlexClaim(ctx context.Context, id int) (registry.Account, bool, error)
	PlexServers(ctx context.Context, token string) ([]registry.PlexServer, error)
	JellyfinSignIn(ctx context.Context, serverURL, username, password string) (registry.Account, error)
	JellyfinSignOut(ctx context.Context, serverURL, deviceID, token string) error
	Settled(slug string) <-chan struct{}
}

type Options struct {
	Store    *store.Store
	Registry Runtime
	Version  string
	Log      *slog.Logger
	Now      func() time.Time
}

type app struct {
	Options
	pages      map[string]*template.Template
	logins     *limiter
	signins    *limiter
	refs       *tokenRefs
	verifySem  chan struct{}
	applyMu    sync.Mutex
	ctx        context.Context
	shutdown   context.CancelFunc
	background sync.WaitGroup
}

// Handler is the setup page. Close ends the work a save left running in the
// background and waits for it.
type Handler struct {
	http.Handler
	app *app
}

func (h *Handler) Close() {
	h.app.shutdown()
	h.app.background.Wait()
}

func New(o Options) *Handler {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	a := &app{
		Options:   o,
		pages:     parsePages(),
		logins:    newLimiter(o.Now, loginAttempts, lockoutWindow),
		signins:   newLimiter(o.Now, loginAttempts, lockoutWindow),
		refs:      newTokenRefs(o.Now, tokenRefTTL),
		verifySem: make(chan struct{}, verifyPasswords),
	}
	a.ctx, a.shutdown = context.WithCancel(context.Background())
	return &Handler{Handler: a.handler(), app: a}
}

func parsePages() map[string]*template.Template {
	names, err := fs.Glob(templateFiles, "templates/*.html")
	if err != nil {
		panic(err)
	}
	pages := map[string]*template.Template{}
	for _, name := range names {
		if strings.HasSuffix(name, "/layout.html") {
			continue
		}
		page := strings.TrimSuffix(strings.TrimPrefix(name, "templates/"), ".html")
		pages[page] = template.Must(template.New("layout.html").Funcs(template.FuncMap{
			"since": since,
		}).ParseFS(templateFiles, "templates/layout.html", name))
	}
	return pages
}

func (a *app) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /setup", a.overview)
	mux.HandleFunc("GET /setup/{$}", redirectTo("/setup"))
	mux.HandleFunc("GET /setup/login", redirectTo("/setup"))
	mux.HandleFunc("GET /setup/password", a.passwordForm)
	mux.HandleFunc("POST /setup/password", a.changePassword)
	mux.HandleFunc("POST /setup/logout", a.logout)
	mux.HandleFunc("POST /setup/public-url", a.setPublicURL)
	mux.HandleFunc("POST /setup/secret/regenerate", a.regenerateSecret)
	mux.HandleFunc("GET /setup/servers/new", a.newServerForm)
	mux.HandleFunc("POST /setup/servers", a.createServer)
	mux.HandleFunc("POST /setup/servers/test", a.testServer)
	mux.HandleFunc("GET /setup/servers/{slug}/edit", a.editServerForm)
	mux.HandleFunc("POST /setup/servers/{slug}", a.updateServer)
	mux.HandleFunc("POST /setup/servers/{slug}/delete", a.deleteServer)
	mux.HandleFunc("POST /setup/servers/{slug}/enabled", a.setServerEnabled)
	mux.HandleFunc("POST /setup/plex/pin", a.plexPIN)
	mux.HandleFunc("GET /setup/plex/pin/{id}", a.plexPoll)
	mux.HandleFunc("POST /setup/jellyfin/signin", a.jellyfinSignIn)
	mux.HandleFunc("/setup/", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "not found", http.StatusNotFound) })

	outer := http.NewServeMux()
	outer.Handle("GET /setup/static/", http.StripPrefix("/setup/", http.FileServerFS(staticFiles)))
	outer.Handle("/setup", a.gate(mux))
	outer.Handle("/setup/", a.gate(mux))
	return outer
}

type sessionKeyType struct{}

type session struct {
	key    []byte
	cookie string
}

// gate decides who may reach the mux: with no admin only the create-password
// form exists; with no valid session only the login form; a POST with a
// session must carry the CSRF token and come from this origin.
func (a *app) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
		if r.Method == http.MethodPost && crossSite(r) {
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		}
		snapshot := a.Store.Snapshot()
		if snapshot.Admin == nil {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/setup":
				a.render(w, r, http.StatusOK, "password", passwordView{page: a.page(r, "Create a password"), Create: true})
			case r.Method == http.MethodPost && r.URL.Path == "/setup/password":
				a.createPassword(w, r)
			default:
				http.Redirect(w, r, "/setup", http.StatusSeeOther)
			}
			return
		}
		key := sessionKey(snapshot.Admin)
		cookie, _ := r.Cookie(cookieName)
		if cookie == nil || !sessionValid(key, cookie.Value, a.Now()) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/setup/login":
				a.render(w, r, http.StatusOK, "login", loginView{page: a.page(r, "Sign in")})
			case r.Method == http.MethodPost && r.URL.Path == "/setup/login":
				a.login(w, r, snapshot.Admin)
			default:
				http.Redirect(w, r, "/setup/login", http.StatusSeeOther)
			}
			return
		}
		if r.Method == http.MethodPost && !a.csrfOK(r, key, cookie.Value) {
			http.Error(w, "invalid or missing CSRF token", http.StatusForbidden)
			return
		}
		ctx := context.WithValue(r.Context(), sessionKeyType{}, session{key: key, cookie: cookie.Value})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func crossSite(r *http.Request) bool {
	site := r.Header.Get("Sec-Fetch-Site")
	return site != "" && site != "same-origin" && site != "none"
}

func (a *app) csrfOK(r *http.Request, key []byte, cookie string) bool {
	token := r.Header.Get("X-CSRF-Token")
	if token == "" {
		if err := r.ParseForm(); err != nil {
			return false
		}
		token = r.PostFormValue("csrf")
	}
	return csrfValid(key, cookie, token)
}

func currentSession(r *http.Request) session {
	s, _ := r.Context().Value(sessionKeyType{}).(session)
	return s
}

func (a *app) setSessionCookie(w http.ResponseWriter, r *http.Request, admin *store.Admin) {
	value := issueSession(sessionKey(admin), a.Now().Add(SessionLifetime))
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: value, Path: "/setup", MaxAge: int(SessionLifetime.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: overHTTPS(r),
	})
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/setup", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: overHTTPS(r)})
}

func overHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// clientAddress is the first hop of X-Forwarded-For when a proxy sets it,
// else the peer address. It is for logging only: it is client-supplied and
// must never key a rate limit.
func clientAddress(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		first, _, _ := strings.Cut(forwarded, ",")
		return strings.TrimSpace(first)
	}
	return peerAddress(r)
}

// peerAddress is the TCP connection's address, which a client cannot spoof.
func peerAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func redirectTo(target string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target, http.StatusSeeOther) }
}

func since(at time.Time, now time.Time) string {
	if at.IsZero() {
		return "never"
	}
	d := now.Sub(at)
	hours, minutes := int(d/time.Hour), int(d%time.Hour/time.Minute)
	switch {
	case d < time.Minute:
		return "just now"
	case hours == 0:
		return fmt.Sprintf("%dm ago", minutes)
	case minutes == 0:
		return fmt.Sprintf("%dh ago", hours)
	default:
		return fmt.Sprintf("%dh%dm ago", hours, minutes)
	}
}
