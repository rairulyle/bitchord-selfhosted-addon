package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/library"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/registry"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/store"
)

const (
	probeTimeout   = 15 * time.Second
	signOutTimeout = 3 * time.Second
	signOutWait    = time.Minute
)

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

func redirectError(w http.ResponseWriter, r *http.Request, err string) {
	http.Redirect(w, r, "/setup?error="+url.QueryEscape(err), http.StatusSeeOther)
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
	if origin, ok := publicOrigin(r); ok && a.Store.Snapshot().PublicURL == "" {
		if err := a.Store.SetPublicURL(origin); err != nil {
			a.fail(w, r, err)
			return
		}
		a.Log.Info("public url set", "public_url", origin)
		redirectNotice(w, r, "Password set. The public URL is "+origin+"; change it below if that is wrong. Now add a server.")
		return
	}
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
	if value != "" {
		if err := store.ValidatePublicURL(value); err != nil {
			view := a.overviewView(r, a.Store.Snapshot())
			view.Error = err.Error()
			view.PublicURL = value
			a.render(w, r, http.StatusBadRequest, "overview", view)
			return
		}
	}
	if err := a.Store.SetPublicURL(value); err != nil {
		a.fail(w, r, err)
		return
	}
	if value == "" {
		a.Log.Info("public url cleared")
		redirectNotice(w, r, "Public URL cleared. Server URLs cannot be shown until it is set again.")
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

func (a *app) exportBackup(w http.ResponseWriter, r *http.Request) {
	passphrase := r.FormValue("passphrase")
	if passphrase == "" && r.Method == http.MethodGet {
		passphrase = r.URL.Query().Get("passphrase")
	}
	if passphrase == "" {
		http.Error(w, "password is required to encrypt backup", http.StatusBadRequest)
		return
	}

	snap := a.Store.Snapshot()
	snap.Admin = nil
	raw, err := store.EncryptSnapshot(snap, passphrase)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	filename := fmt.Sprintf("bitchord-addon-backup-encrypted-%s.json", a.Now().Format("2006-01-02-150405"))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(append(raw, '\n'))
}

func (a *app) restoreBackup(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var raw []byte
	var passphrase string

	contentType := r.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "multipart/form-data") {
		if r.MultipartForm == nil {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				redirectError(w, r, "Restore failed: invalid upload format or file too large.")
				return
			}
		}
		passphrase = r.FormValue("passphrase")
		file, _, err := r.FormFile("backup_file")
		if err != nil {
			redirectError(w, r, "Restore failed: no backup file provided.")
			return
		}
		defer file.Close()
		buf, err := io.ReadAll(file)
		if err != nil {
			redirectError(w, r, "Restore failed: unable to read uploaded file.")
			return
		}
		raw = buf
	} else {
		passphrase = r.FormValue("passphrase")
		buf, err := io.ReadAll(r.Body)
		if err != nil {
			redirectError(w, r, "Restore failed: unable to read request body.")
			return
		}
		raw = buf
	}

	snap, err := store.DecryptSnapshot(raw, passphrase)
	if err != nil {
		redirectError(w, r, fmt.Sprintf("Restore failed: %s", err.Error()))
		return
	}

	if err := a.Store.Restore(snap); err != nil {
		redirectError(w, r, fmt.Sprintf("Restore failed: %s", err.Error()))
		return
	}

	a.apply()
	count := len(snap.Servers)
	serverWord := "server"
	if count != 1 {
		serverWord = "servers"
	}
	redirectNotice(w, r, fmt.Sprintf("Configuration restored successfully (%d %s configured).", count, serverWord))
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
	view.Label, view.URL, view.Library, view.LibraryName, view.Account, view.Auth, view.Enabled = server.Label, server.URL, server.Library, server.LibraryName, server.Account, server.Auth, server.Enabled
	view.ScrobblePercent = server.ScrobblePercent
	view.SyncInterval = server.SyncInterval
	view.Action = "/setup/servers/" + server.Slug
	a.render(w, r, http.StatusOK, "server", view)
}

// submitted is what the form posts, before any of it has been checked.
type submitted struct {
	kind            store.Kind
	label           string
	url             string
	token           string
	tokenRef        string
	library         string
	enabled         bool
	scrobblePercent int
	syncInterval    string
}

func readSubmitted(r *http.Request) submitted {
	scrobblePercent := -1
	if r.PostFormValue("scrobble_enabled") != "" {
		scrobblePercent = 50
		if raw := r.PostFormValue("scrobble_percent"); raw != "" {
			if p, err := strconv.Atoi(raw); err == nil && p >= 10 && p <= 90 {
				scrobblePercent = p
			}
		}
	}
	return submitted{
		kind:            store.Kind(r.PostFormValue("kind")),
		label:           strings.TrimSpace(r.PostFormValue("label")),
		url:             strings.TrimRight(strings.TrimSpace(r.PostFormValue("url")), "/"),
		token:           strings.TrimSpace(r.PostFormValue("token")),
		tokenRef:        r.PostFormValue("token_ref"),
		library:         r.PostFormValue("library"),
		enabled:         r.PostFormValue("enabled") != "",
		scrobblePercent: scrobblePercent,
		syncInterval:    strings.TrimSpace(r.PostFormValue("sync_interval")),
	}
}

func (a *app) createServer(w http.ResponseWriter, r *http.Request) {
	form := readSubmitted(r)
	view := a.formView(r, "Add a server")
	server, giveBack, err := a.resolveServer(r.Context(), form, store.Server{Kind: form.kind})
	if err != nil {
		a.renderServerError(w, r, view, form, err)
		return
	}
	added, err := a.Store.AddServer(server)
	if err != nil {
		giveBack()
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
	if form.library == existing.Library {
		view.LibraryName = existing.LibraryName
	}
	server, giveBack, err := a.resolveServer(r.Context(), form, existing)
	if err != nil {
		a.renderServerError(w, r, view, form, err)
		return
	}
	err = a.Store.UpdateServer(slug, func(current *store.Server) error {
		*current = server
		return nil
	})
	if err != nil {
		giveBack()
		a.renderServerError(w, r, view, form, err)
		return
	}
	a.apply()
	a.Log.Info("server updated", "server", slug, "source", string(server.Kind), "host", hostOf(server.URL), "auth", string(server.Auth))
	a.signOutReplaced(existing, server)
	redirectNotice(w, r, registry.DisplayName(kindName(server.Kind), server.Label)+" saved.")
}

// resolveServer turns a submitted form into a validated server: it settles
// which credential applies, probes the server with it, and checks the
// library against what the server listed. A sign-in reference is taken at
// once, so two saves cannot both use it, and given back if anything fails;
// giveBack does the same for a failure after it returns.
func (a *app) resolveServer(ctx context.Context, form submitted, existing store.Server) (server store.Server, giveBack func(), err error) {
	giveBack = func() {}
	defer func() {
		if err != nil {
			giveBack()
		}
	}()
	server = existing
	server.Label, server.URL, server.Enabled = form.label, form.url, form.enabled
	server.SyncInterval = form.syncInterval
	if form.kind == store.Navidrome {
		server.ScrobblePercent = form.scrobblePercent
	}
	if err = store.ValidateServerURL(server.URL); err != nil {
		return server, giveBack, err
	}
	if utf8.RuneCountInString(server.Label) > store.LabelMaxLength {
		return server, giveBack, fmt.Errorf("the label must be at most %d characters", store.LabelMaxLength)
	}
	switch {
	case form.tokenRef != "":
		pending, ok := a.refs.take(form.tokenRef)
		if !ok {
			return server, giveBack, errors.New("the sign-in has expired, sign in again")
		}
		giveBack = func() { a.refs.restore(form.tokenRef, pending) }
		if pending.kind != server.Kind {
			return server, giveBack, fmt.Errorf("the sign-in belongs to %s, not %s; sign in again", kindName(pending.kind), kindName(server.Kind))
		}
		server.Token, server.Account, server.DeviceID = pending.token, pending.account, pending.deviceID
		server.Auth = map[store.Kind]store.Auth{store.Plex: store.AuthPlexSignIn, store.Jellyfin: store.AuthJellyfinSignIn, store.Navidrome: store.AuthNavidromeSignIn}[server.Kind]
	case form.token != "":
		server.Token, server.Account, server.DeviceID, server.Auth = form.token, "", "", store.AuthToken
	case existing.Token == "":
		return server, giveBack, errors.New("sign in or paste a token")
	case existing.Auth == store.AuthJellyfinSignIn && existing.URL != server.URL:
		return server, giveBack, errors.New("the Jellyfin sign-in belongs to the old address, sign in again")
	case existing.Auth == store.AuthNavidromeSignIn && existing.URL != server.URL:
		return server, giveBack, errors.New("the Navidrome sign-in belongs to the old address, sign in again")
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	probe, err := a.Registry.Probe(ctx, server)
	if err != nil {
		return server, giveBack, describe(err)
	}
	server.Library, server.LibraryName = "", ""
	if form.library != "" {
		match := false
		for _, library := range probe.Libraries {
			if form.library == library.ID || form.library == library.Name {
				server.Library, server.LibraryName, match = library.ID, library.Name, true
			}
		}
		if !match {
			return server, giveBack, errors.New("the server has no music library with that name")
		}
	}
	return server, giveBack, nil
}

func (a *app) renderServerError(w http.ResponseWriter, r *http.Request, view serverFormView, form submitted, err error) {
	view.Error = err.Error()
	view.Kind, view.KindName, view.Label, view.URL, view.Library, view.Enabled = form.kind, kindName(form.kind), form.label, form.url, form.library, form.enabled
	view.ScrobblePercent = form.scrobblePercent
	view.SyncInterval = form.syncInterval
	a.render(w, r, http.StatusBadRequest, "server", view)
}

func (a *app) deleteServer(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	existing, _ := a.Store.Snapshot().Server(slug)
	err := a.Store.RemoveServer(slug)
	switch {
	case errors.Is(err, store.ErrNoServer):
		http.NotFound(w, r)
		return
	case err != nil:
		a.fail(w, r, err)
		return
	}
	a.apply()
	a.Log.Info("server removed", "server", slug)
	a.signOutReplaced(existing, store.Server{})
	redirectNotice(w, r, slug+" removed. Its URL no longer answers.")
}

func (a *app) setServerEnabled(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	enabled := r.PostFormValue("enabled") == "true"
	err := a.Store.UpdateServer(slug, func(server *store.Server) error {
		server.Enabled = enabled
		return nil
	})
	switch {
	case errors.Is(err, store.ErrNoServer):
		http.NotFound(w, r)
		return
	case err != nil:
		a.fail(w, r, err)
		return
	}
	a.apply()
	a.Log.Info("server enabled changed", "server", slug, "enabled", enabled)
	redirectNotice(w, r, slug+" "+map[bool]string{true: "enabled", false: "disabled"}[enabled]+".")
}

func (a *app) refreshServer(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	server, ok := a.Store.Snapshot().Server(slug)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	name := registry.DisplayName(kindName(server.Kind), server.Label)
	if err := a.Registry.Refresh(ctx, slug); err != nil {
		redirectNotice(w, r, fmt.Sprintf("Sync failed for %s: %s", name, err.Error()))
		return
	}
	stats, hasStats := a.Registry.Status(slug)
	if hasStats && stats.Ready {
		tookStr := formatDurationMs(stats.Duration)
		if tookStr != "" {
			tookStr = " in " + tookStr
		}
		redirectNotice(w, r, fmt.Sprintf("%s synced%s (%d tracks · %d artists · %d albums).", name, tookStr, stats.Tracks, stats.Artists, stats.Albums))
		return
	}
	redirectNotice(w, r, name+" synced.")
}

func (a *app) refreshAllServers(w http.ResponseWriter, r *http.Request) {
	snapshot := a.Store.Snapshot()
	var servers []store.Server
	for _, s := range snapshot.Servers {
		if s.Enabled {
			servers = append(servers, s)
		}
	}
	if len(servers) == 0 {
		redirectNotice(w, r, "No enabled servers to sync.")
		return
	}

	a.Log.Info("syncing all servers", "count", len(servers))
	ctx, cancel := context.WithTimeout(r.Context(), a.Registry.DefaultInterval())
	defer cancel()

	type result struct {
		name string
		err  error
	}
	resCh := make(chan result, len(servers))
	var wg sync.WaitGroup
	for _, s := range servers {
		wg.Add(1)
		go func(srv store.Server) {
			defer wg.Done()
			name := registry.DisplayName(kindName(srv.Kind), srv.Label)
			err := a.Registry.Refresh(ctx, srv.Slug)
			resCh <- result{name: name, err: err}
		}(s)
	}
	wg.Wait()
	close(resCh)

	var errs []string
	synced := 0
	for res := range resCh {
		if res.err != nil {
			a.Log.Error("sync failed during sync-all", "server", res.name, "error", res.err.Error())
			errs = append(errs, fmt.Sprintf("%s: %s", res.name, res.err.Error()))
		} else {
			synced++
		}
	}

	if len(errs) > 0 {
		redirectNotice(w, r, fmt.Sprintf("Synced %d of %d servers (errors: %s)", synced, len(servers), strings.Join(errs, "; ")))
		return
	}
	serverWord := "server"
	if synced != 1 {
		serverWord = "servers"
	}
	redirectNotice(w, r, fmt.Sprintf("All %d %s synced successfully.", synced, serverWord))
}

func (a *app) testSearch(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	query := r.URL.Query().Get("q")
	res, ok := a.Registry.Search(slug, query, 50)
	if !ok {
		jsonError(w, http.StatusNotFound, "server is not running or ready")
		return
	}
	type matchTrack struct {
		ID          string `json:"id"`
		Title       string `json:"title"`
		Artist      string `json:"artist"`
		Album       string `json:"album"`
		DurationSec int    `json:"duration"`
		Codec       string `json:"codec"`
		BitDepth    int    `json:"bitDepth,omitempty"`
		SampleRate  int    `json:"sampleRate,omitempty"`
		Lossless    bool   `json:"lossless"`
		ArtworkURL  string `json:"artworkURL,omitempty"`
	}
	type matchAlbum struct {
		Name       string `json:"name"`
		Artist     string `json:"artist"`
		ArtworkURL string `json:"artworkURL,omitempty"`
		TrackCount int    `json:"trackCount"`
		Year       int    `json:"year,omitempty"`
	}
	type matchArtist struct {
		Name       string `json:"name"`
		ArtworkURL string `json:"artworkURL,omitempty"`
		TrackCount int    `json:"trackCount"`
	}

	snapshot := a.Store.Snapshot()
	base := ""
	if snapshot.PublicURL != "" && snapshot.Secret != "" {
		base = snapshot.PublicURL + "/" + slug + "/" + snapshot.Secret
	}

	matches := make([]matchTrack, len(res.Tracks))
	for i, t := range res.Tracks {
		art := ""
		if t.ArtRef != "" && base != "" {
			art = base + "/art/" + t.ID
		}
		matches[i] = matchTrack{
			ID:          t.ID,
			Title:       t.Title,
			Artist:      library.FormatArtists(t.Artist),
			Album:       t.Album,
			DurationSec: t.DurationSec,
			Codec:       t.Codec,
			BitDepth:    t.BitDepth,
			SampleRate:  t.SampleRate,
			Lossless:    media.Lossless(t.Codec),
			ArtworkURL:  art,
		}
	}

	albums := make([]matchAlbum, len(res.Albums))
	for i, alb := range res.Albums {
		art := ""
		if alb.ArtRef != "" && alb.SampleID != "" && base != "" {
			art = base + "/art/" + alb.SampleID
		}
		albums[i] = matchAlbum{
			Name:       alb.Name,
			Artist:     library.FormatArtists(alb.Artist),
			ArtworkURL: art,
			TrackCount: alb.TrackCount,
			Year:       alb.Year,
		}
	}

	artists := make([]matchArtist, len(res.Artists))
	for i, art := range res.Artists {
		artURL := ""
		if art.ArtRef != "" && art.SampleID != "" && base != "" {
			artURL = base + "/art/" + art.SampleID
		}
		artists[i] = matchArtist{
			Name:       art.Name,
			ArtworkURL: artURL,
			TrackCount: art.TrackCount,
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"query":    query,
		"tokens":   res.Tokens,
		"strict":   res.Strict,
		"fallback": res.Fallback,
		"total":    res.Strict + res.Fallback,
		"tracks":   matches,
		"albums":   albums,
		"artists":  artists,
	})
}

func (a *app) recentActivity(w http.ResponseWriter, r *http.Request) {
	now := a.Now()
	filter := r.URL.Query().Get("filter")
	raw := a.Registry.RecentActivity()
	items := make([]ActivityView, 0, len(raw))
	for _, act := range raw {
		if filter == "sync" && act.Kind != "sync" && act.Kind != "sync_error" {
			continue
		}
		if filter == "playback" && act.Kind != "now_playing" && act.Kind != "scrobble" && act.Kind != "playback" {
			continue
		}
		items = append(items, ActivityView{
			TimeAgo: since(act.Time, now),
			Slug:    act.Slug,
			Server:  act.Server,
			Kind:    act.Kind,
			Message: act.Message,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"activities": items,
	})
}

func (a *app) clearActivity(w http.ResponseWriter, r *http.Request) {
	filter := r.FormValue("filter")
	a.Registry.ClearActivity(filter)
	if strings.Contains(r.Header.Get("Accept"), "application/json") || r.Header.Get("X-Requested-With") == "XMLHttpRequest" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	redirectNotice(w, r, "Activity log cleared.")
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
		switch {
		case !ok:
			jsonError(w, http.StatusBadRequest, "the sign-in has expired, sign in again")
			return
		case pending.kind != req.Kind:
			jsonError(w, http.StatusBadRequest, fmt.Sprintf("the sign-in belongs to %s, not %s; sign in again", kindName(pending.kind), kindName(req.Kind)))
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
		a.signins.forgive(req.URL)
		jsonError(w, http.StatusBadGateway, describe(err).Error())
		return
	}
	a.signins.reset(req.URL)
	ref := a.refs.put(store.Jellyfin, account.Token, account.Username, account.DeviceID)
	a.Log.Info("jellyfin sign-in completed", "host", hostOf(req.URL), "username", account.Username)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "username": account.Username, "ref": ref})
}

type navidromeSignInRequest struct {
	URL      string `json:"url"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// navidromeSignIn verifies a Navidrome username and password against its
// Subsonic API and remembers the credential for the "Paste a token/sign in"
// tab. Navidrome offers no separate API token, so the pair is stored as the
// server's token once the form is saved.
func (a *app) navidromeSignIn(w http.ResponseWriter, r *http.Request) {
	var req navidromeSignInRequest
	if err := readJSON(r, &req); err != nil {
		jsonError(w, http.StatusBadRequest, "malformed request")
		return
	}
	req.URL = strings.TrimRight(strings.TrimSpace(req.URL), "/")
	if err := store.ValidateServerURL(req.URL); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || req.Password == "" {
		jsonError(w, http.StatusBadRequest, "a username and password are required")
		return
	}
	if !a.signins.attempt(req.URL) {
		jsonError(w, http.StatusTooManyRequests, "too many failed sign-ins for this server, try again in a minute")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	account, err := a.Registry.NavidromeSignIn(ctx, req.URL, req.Username, req.Password)
	switch {
	case errors.Is(err, media.ErrUnauthorized):
		a.Log.Warn("navidrome sign-in rejected", "host", hostOf(req.URL), "username", req.Username)
		jsonError(w, http.StatusUnauthorized, "navidrome rejected the username or password")
		return
	case err != nil:
		a.signins.forgive(req.URL)
		jsonError(w, http.StatusBadGateway, describe(err).Error())
		return
	}
	a.signins.reset(req.URL)
	ref := a.refs.put(store.Navidrome, account.Token, account.Username, "")
	a.Log.Info("navidrome sign-in completed", "host", hostOf(req.URL), "username", account.Username)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "username": account.Username, "ref": ref})
}

// signOutReplaced revokes the token a Jellyfin sign-in replaced, or the token
// of a removed server, so the old session does not linger in Jellyfin. A
// pasted API key is left alone: the
// user made it by hand. It waits in the background until no running or
// starting entry for the server sends the old token any more, and leaves the
// session alone if one still does when the wait ends. It never fails the
// save.
func (a *app) signOutReplaced(old, saved store.Server) {
	if old.Kind != store.Jellyfin || old.Auth != store.AuthJellyfinSignIn || old.Token == saved.Token {
		return
	}
	if !a.goBackground(func() { a.signOutWhenUnused(old) }) {
		a.Log.Info("old jellyfin session left signed in", "server", old.Slug, "host", hostOf(old.URL))
	}
}

func (a *app) signOutWhenUnused(old store.Server) {
	timer := time.NewTimer(a.SignOutWait)
	defer timer.Stop()
	used, changed := a.Registry.TokenInUse(old.Slug, old.Token)
	for waiting := true; used && waiting; used, changed = a.Registry.TokenInUse(old.Slug, old.Token) {
		select {
		case <-changed:
		case <-timer.C:
			waiting = false
		case <-a.ctx.Done():
			waiting = false
		}
	}
	if used {
		a.Log.Info("old jellyfin session left signed in", "server", old.Slug, "host", hostOf(old.URL))
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(a.ctx), signOutTimeout)
	defer cancel()
	if err := a.Registry.JellyfinSignOut(ctx, old.URL, old.DeviceID, old.Token); err != nil {
		a.Log.Warn("old jellyfin session not signed out", "server", old.Slug, "host", hostOf(old.URL))
	}
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
