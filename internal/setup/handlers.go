package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/registry"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/store"
)

const probeTimeout = 15 * time.Second

func (a *app) render(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := a.pages[name].ExecuteTemplate(w, "layout.html", data); err != nil {
		a.Log.Error("template failed", "page", name, "error", err.Error())
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func jsonError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": message})
}

func readJSON(r *http.Request, into any) error {
	return json.NewDecoder(r.Body).Decode(into)
}

// apply takes the snapshot inside the lock, so a write that saved earlier can
// never apply after one that saved later.
func (a *app) apply() {
	a.applyMu.Lock()
	defer a.applyMu.Unlock()
	a.Registry.Apply(a.Store.Snapshot())
}

func redirectNotice(w http.ResponseWriter, r *http.Request, notice string) {
	http.Redirect(w, r, "/setup?notice="+url.QueryEscape(notice), http.StatusSeeOther)
}

// ---- password and session

func (a *app) createPassword(w http.ResponseWriter, r *http.Request) {
	password := r.PostFormValue("password")
	if utf8.RuneCountInString(password) < MinPasswordLength {
		view := passwordView{page: a.page(r, "Create a password"), Create: true}
		view.Error = fmt.Sprintf("the password must be at least %d characters", MinPasswordLength)
		a.render(w, r, http.StatusBadRequest, "password", view)
		return
	}
	admin := &store.Admin{PasswordHash: HashPassword(password), SessionKey: NewSessionKey()}
	if err := a.Store.SetAdmin(admin); err != nil {
		a.fail(w, r, err)
		return
	}
	a.Log.Info("admin password created", "from", clientAddress(r))
	a.setSessionCookie(w, r, admin)
	redirectNotice(w, r, "Password set. Add your public URL and a server.")
}

func (a *app) login(w http.ResponseWriter, r *http.Request, admin *store.Admin) {
	peer := peerAddress(r)
	if !a.logins.attempt(peer) {
		view := loginView{page: a.page(r, "Sign in")}
		view.Error = "too many failed attempts, try again in a minute"
		a.render(w, r, http.StatusTooManyRequests, "login", view)
		return
	}
	if !a.verifyPassword(r.Context(), admin.PasswordHash, r.PostFormValue("password")) {
		a.Log.Warn("setup login failed", "from", clientAddress(r))
		view := loginView{page: a.page(r, "Sign in")}
		view.Error = "wrong password"
		a.render(w, r, http.StatusUnauthorized, "login", view)
		return
	}
	a.logins.reset(peer)
	a.setSessionCookie(w, r, admin)
	http.Redirect(w, r, "/setup", http.StatusSeeOther)
}

// verifyPassword caps concurrent argon2id verifications, each of which
// allocates a fixed amount of memory, so a burst of requests cannot exhaust
// it. It gives up if ctx ends first.
func (a *app) verifyPassword(ctx context.Context, hash, password string) bool {
	select {
	case a.verifySem <- struct{}{}:
	case <-ctx.Done():
		return false
	}
	defer func() { <-a.verifySem }()
	return VerifyPassword(hash, password)
}

func (a *app) logout(w http.ResponseWriter, r *http.Request) {
	clearSessionCookie(w, r)
	http.Redirect(w, r, "/setup/login", http.StatusSeeOther)
}

func (a *app) passwordForm(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, http.StatusOK, "password", passwordView{page: a.page(r, "Change the password")})
}

// changePassword rotates the session key too, which signs every browser out,
// then signs this one back in.
func (a *app) changePassword(w http.ResponseWriter, r *http.Request) {
	snapshot := a.Store.Snapshot()
	view := passwordView{page: a.page(r, "Change the password")}
	switch password := r.PostFormValue("password"); {
	case !a.verifyPassword(r.Context(), snapshot.Admin.PasswordHash, r.PostFormValue("current")):
		view.Error = "the current password is wrong"
	case utf8.RuneCountInString(password) < MinPasswordLength:
		view.Error = fmt.Sprintf("the new password must be at least %d characters", MinPasswordLength)
	}
	if view.Error != "" {
		a.render(w, r, http.StatusBadRequest, "password", view)
		return
	}
	admin := &store.Admin{PasswordHash: HashPassword(r.PostFormValue("password")), SessionKey: NewSessionKey()}
	if err := a.Store.SetAdmin(admin); err != nil {
		a.fail(w, r, err)
		return
	}
	a.Log.Info("admin password changed", "from", clientAddress(r))
	a.setSessionCookie(w, r, admin)
	redirectNotice(w, r, "Password changed. Other browsers have been signed out.")
}

// ---- overview, public URL, secret

func (a *app) overview(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, http.StatusOK, "overview", a.overviewView(r, a.Store.Snapshot()))
}

func (a *app) setPublicURL(w http.ResponseWriter, r *http.Request) {
	value := strings.TrimRight(strings.TrimSpace(r.PostFormValue("public_url")), "/")
	if err := store.ValidatePublicURL(value); err != nil {
		view := a.overviewView(r, a.Store.Snapshot())
		view.Error = err.Error()
		view.PublicURL = value
		a.render(w, r, http.StatusBadRequest, "overview", view)
		return
	}
	if err := a.Store.SetPublicURL(value); err != nil {
		a.fail(w, r, err)
		return
	}
	a.Log.Info("public url set", "public_url", value)
	redirectNotice(w, r, "Public URL saved.")
}

func (a *app) regenerateSecret(w http.ResponseWriter, r *http.Request) {
	if _, err := a.Store.RegenerateSecret(); err != nil {
		a.fail(w, r, err)
		return
	}
	a.Log.Warn("secret regenerated, every server URL has changed")
	redirectNotice(w, r, "Secret regenerated. Every server URL has changed; update your clients.")
}

// ---- server forms

func (a *app) newServerForm(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, http.StatusOK, "server", a.formView(r, "Add a server"))
}

func (a *app) editServerForm(w http.ResponseWriter, r *http.Request) {
	server, ok := a.Store.Snapshot().Server(r.PathValue("slug"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	view := a.formView(r, "Edit "+registry.DisplayName(kindName(server.Kind), server.Label))
	view.Editing, view.Slug, view.Kind, view.KindName = true, server.Slug, server.Kind, kindName(server.Kind)
	view.Label, view.URL, view.Library, view.Account, view.Auth, view.Enabled = server.Label, server.URL, server.Library, server.Account, server.Auth, server.Enabled
	view.Action = "/setup/servers/" + server.Slug
	a.render(w, r, http.StatusOK, "server", view)
}

// submitted is what the form posts, before any of it has been checked.
type submitted struct {
	kind     store.Kind
	label    string
	url      string
	token    string
	tokenRef string
	library  string
	enabled  bool
}

func readSubmitted(r *http.Request) submitted {
	return submitted{
		kind:     store.Kind(r.PostFormValue("kind")),
		label:    strings.TrimSpace(r.PostFormValue("label")),
		url:      strings.TrimRight(strings.TrimSpace(r.PostFormValue("url")), "/"),
		token:    strings.TrimSpace(r.PostFormValue("token")),
		tokenRef: r.PostFormValue("token_ref"),
		library:  r.PostFormValue("library"),
		enabled:  r.PostFormValue("enabled") != "",
	}
}

func (a *app) createServer(w http.ResponseWriter, r *http.Request) {
	form := readSubmitted(r)
	view := a.formView(r, "Add a server")
	server, err := a.resolveServer(r.Context(), form, store.Server{Kind: form.kind})
	if err != nil {
		a.renderServerError(w, r, view, form, err)
		return
	}
	added, err := a.Store.AddServer(server)
	if err != nil {
		a.renderServerError(w, r, view, form, err)
		return
	}
	a.apply()
	a.Log.Info("server added", "server", added.Slug, "source", string(added.Kind), "host", hostOf(added.URL), "auth", string(added.Auth))
	redirectNotice(w, r, registry.DisplayName(kindName(added.Kind), added.Label)+" added. Copy its URL into BitChord.")
}

func (a *app) updateServer(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	existing, ok := a.Store.Snapshot().Server(slug)
	if !ok {
		http.NotFound(w, r)
		return
	}
	form := readSubmitted(r)
	form.kind = existing.Kind
	view := a.formView(r, "Edit "+registry.DisplayName(kindName(existing.Kind), existing.Label))
	view.Editing, view.Slug, view.Action, view.Account = true, slug, "/setup/servers/"+slug, existing.Account
	server, err := a.resolveServer(r.Context(), form, existing)
	if err != nil {
		a.renderServerError(w, r, view, form, err)
		return
	}
	err = a.Store.UpdateServer(slug, func(current *store.Server) error {
		*current = server
		return nil
	})
	if err != nil {
		a.renderServerError(w, r, view, form, err)
		return
	}
	a.apply()
	a.Log.Info("server updated", "server", slug, "source", string(server.Kind), "host", hostOf(server.URL), "auth", string(server.Auth))
	redirectNotice(w, r, registry.DisplayName(kindName(server.Kind), server.Label)+" saved.")
}

// resolveServer turns a submitted form into a validated server: it settles
// which credential applies, probes the server with it, and checks the
// library against what the server listed.
func (a *app) resolveServer(ctx context.Context, form submitted, existing store.Server) (store.Server, error) {
	server := existing
	server.Label, server.URL, server.Enabled = form.label, form.url, form.enabled
	if err := store.ValidateServerURL(server.URL); err != nil {
		return server, err
	}
	if utf8.RuneCountInString(server.Label) > store.LabelMaxLength {
		return server, fmt.Errorf("the label must be at most %d characters", store.LabelMaxLength)
	}
	switch {
	case form.tokenRef != "":
		pending, ok := a.refs.take(form.tokenRef)
		if !ok || pending.kind != server.Kind {
			return server, errors.New("the sign-in has expired, sign in again")
		}
		server.Token, server.Account, server.DeviceID = pending.token, pending.account, pending.deviceID
		server.Auth = map[store.Kind]store.Auth{store.Plex: store.AuthPlexSignIn, store.Jellyfin: store.AuthJellyfinSignIn}[server.Kind]
	case form.token != "":
		server.Token, server.Account, server.DeviceID, server.Auth = form.token, "", "", store.AuthToken
	case existing.Token == "":
		return server, errors.New("sign in or paste a token")
	case existing.Auth == store.AuthJellyfinSignIn && existing.URL != server.URL:
		return server, errors.New("the Jellyfin sign-in belongs to the old address, sign in again")
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	probe, err := a.Registry.Probe(ctx, server)
	if err != nil {
		return server, describe(err)
	}
	server.Library = ""
	if form.library != "" {
		match := false
		for _, library := range probe.Libraries {
			if form.library == library.ID || form.library == library.Name {
				server.Library, match = library.ID, true
			}
		}
		if !match {
			return server, errors.New("the server has no music library with that name")
		}
	}
	return server, nil
}

func (a *app) renderServerError(w http.ResponseWriter, r *http.Request, view serverFormView, form submitted, err error) {
	view.Error = err.Error()
	view.Kind, view.KindName, view.Label, view.URL, view.Library, view.Enabled = form.kind, kindName(form.kind), form.label, form.url, form.library, form.enabled
	a.render(w, r, http.StatusBadRequest, "server", view)
}

func (a *app) deleteServer(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if err := a.Store.RemoveServer(slug); err != nil {
		a.fail(w, r, err)
		return
	}
	a.apply()
	a.Log.Info("server removed", "server", slug)
	redirectNotice(w, r, slug+" removed. Its URL no longer answers.")
}

func (a *app) setServerEnabled(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	enabled := r.PostFormValue("enabled") == "true"
	err := a.Store.UpdateServer(slug, func(server *store.Server) error {
		server.Enabled = enabled
		return nil
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.apply()
	a.Log.Info("server enabled changed", "server", slug, "enabled", enabled)
	redirectNotice(w, r, slug+" "+map[bool]string{true: "enabled", false: "disabled"}[enabled]+".")
}

// ---- JSON endpoints used by the form's buttons

type testRequest struct {
	Kind     store.Kind `json:"kind"`
	URL      string     `json:"url"`
	Token    string     `json:"token"`
	TokenRef string     `json:"token_ref"`
	Slug     string     `json:"slug"`
}

// testServer probes with the referenced sign-in, the pasted token, or the
// stored token of the server being edited, in that order, as a save would.
func (a *app) testServer(w http.ResponseWriter, r *http.Request) {
	var req testRequest
	if err := readJSON(r, &req); err != nil {
		jsonError(w, http.StatusBadRequest, "malformed request")
		return
	}
	req.URL = strings.TrimRight(strings.TrimSpace(req.URL), "/")
	if err := store.ValidateServerURL(req.URL); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	server := store.Server{Kind: req.Kind, URL: req.URL, Token: strings.TrimSpace(req.Token)}
	switch {
	case req.TokenRef != "":
		pending, ok := a.refs.peek(req.TokenRef)
		if !ok || pending.kind != req.Kind {
			jsonError(w, http.StatusBadRequest, "the sign-in has expired, sign in again")
			return
		}
		server.Token, server.DeviceID = pending.token, pending.deviceID
	case server.Token != "":
	case req.Slug != "":
		existing, ok := a.Store.Snapshot().Server(req.Slug)
		if !ok {
			jsonError(w, http.StatusBadRequest, "unknown server")
			return
		}
		server.Kind, server.Token, server.DeviceID = existing.Kind, existing.Token, existing.DeviceID
	default:
		jsonError(w, http.StatusBadRequest, "sign in or paste a token first")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	probe, err := a.Registry.Probe(ctx, server)
	if err != nil {
		jsonError(w, http.StatusBadGateway, describe(err).Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": probe.Version, "libraries": libraryList(probe.Libraries)})
}

func libraryList(libraries []media.Library) []map[string]string {
	out := make([]map[string]string, len(libraries))
	for i, library := range libraries {
		out[i] = map[string]string{"id": library.ID, "name": library.Name}
	}
	return out
}

func (a *app) plexPIN(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	pin, err := a.Registry.PlexPIN(ctx)
	if err != nil {
		jsonError(w, http.StatusBadGateway, "cannot reach plex.tv from the addon; paste a token instead")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": pin.ID, "auth_url": pin.AuthURL})
}

// plexPoll answers claimed=false until the user signs in on app.plex.tv. The
// token never reaches the browser: the answer carries a one-time reference.
func (a *app) plexPoll(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "malformed pin id")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	account, claimed, err := a.Registry.PlexClaim(ctx, id)
	switch {
	case errors.Is(err, media.ErrNotFound):
		jsonError(w, http.StatusGone, "the sign-in expired, start over")
		return
	case err != nil:
		jsonError(w, http.StatusBadGateway, "cannot reach plex.tv from the addon; paste a token instead")
		return
	case !claimed:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "claimed": false})
		return
	}
	servers, err := a.Registry.PlexServers(ctx, account.Token)
	if err != nil {
		jsonError(w, http.StatusBadGateway, "signed in, but the server list could not be read: "+describe(err).Error())
		return
	}
	list := make([]map[string]any, len(servers))
	for i, server := range servers {
		list[i] = map[string]any{"name": server.Name, "url": server.URL, "reachable": server.Reachable}
	}
	ref := a.refs.put(store.Plex, account.Token, account.Username, "")
	a.Log.Info("plex sign-in completed", "account", account.Username)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "claimed": true, "username": account.Username, "ref": ref, "servers": list})
}

type jellyfinSignInRequest struct {
	URL      string `json:"url"`
	Username string `json:"username"`
	Password string `json:"password"`
}

func (a *app) jellyfinSignIn(w http.ResponseWriter, r *http.Request) {
	var req jellyfinSignInRequest
	if err := readJSON(r, &req); err != nil {
		jsonError(w, http.StatusBadRequest, "malformed request")
		return
	}
	req.URL = strings.TrimRight(strings.TrimSpace(req.URL), "/")
	if err := store.ValidateServerURL(req.URL); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !a.signins.attempt(req.URL) {
		jsonError(w, http.StatusTooManyRequests, "too many failed sign-ins for this server, try again in a minute")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	account, err := a.Registry.JellyfinSignIn(ctx, req.URL, req.Username, req.Password)
	switch {
	case errors.Is(err, media.ErrUnauthorized):
		a.Log.Warn("jellyfin sign-in rejected", "host", hostOf(req.URL), "username", req.Username)
		jsonError(w, http.StatusUnauthorized, "jellyfin rejected the sign-in")
		return
	case err != nil:
		jsonError(w, http.StatusBadGateway, describe(err).Error())
		return
	}
	a.signins.reset(req.URL)
	ref := a.refs.put(store.Jellyfin, account.Token, account.Username, account.DeviceID)
	a.Log.Info("jellyfin sign-in completed", "host", hostOf(req.URL), "username", account.Username)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "username": account.Username, "ref": ref})
}

// describe turns an adapter error into a sentence for the page.
func describe(err error) error {
	switch {
	case errors.Is(err, media.ErrUnauthorized):
		return errors.New("the server rejected the credentials")
	case errors.Is(err, context.DeadlineExceeded):
		return errors.New("the server did not answer in time")
	}
	return err
}

func (a *app) fail(w http.ResponseWriter, r *http.Request, err error) {
	a.Log.Error("setup action failed", "path", r.URL.Path, "error", err.Error())
	view := a.overviewView(r, a.Store.Snapshot())
	view.Error = err.Error()
	a.render(w, r, http.StatusInternalServerError, "overview", view)
}
