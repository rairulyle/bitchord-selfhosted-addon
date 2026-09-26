package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/fakes"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/registry"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/store"
)

const password = "correct horse battery staple"

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type testApp struct {
	t       *testing.T
	handler http.Handler
	setup   *Handler
	store   *store.Store
	reg     *registry.Registry
	plexTV  *fakes.PlexTV
	logs    *syncBuffer
	log     *slog.Logger
	now     time.Time
	cookie  string
	headers http.Header
}

func newTestApp(t *testing.T) *testApp {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	a := &testApp{t: t, store: s, plexTV: fakes.NewPlexTV(t), logs: logs, log: log, now: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC), headers: http.Header{}}
	a.reg = registry.New(s.Snapshot(), registry.Options{Interval: time.Hour, Version: "1.2.3", Log: log, PlexTV: a.plexTV.URL, ProbeTimeout: time.Second})
	t.Cleanup(a.reg.Stop)
	a.setup = New(Options{Store: s, Registry: a.reg, Version: "1.2.3", Log: log, Now: func() time.Time { return a.now }})
	t.Cleanup(a.setup.Close)
	a.handler = a.setup
	return a
}

func (a *testApp) do(method, path string, body io.Reader, contentType string) *httptest.ResponseRecorder {
	a.t.Helper()
	req := httptest.NewRequest(method, path, body)
	req.RemoteAddr = "10.0.0.7:4444"
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for key, values := range a.headers {
		req.Header[key] = values
	}
	if a.cookie != "" {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: a.cookie})
	}
	rec := httptest.NewRecorder()
	a.handler.ServeHTTP(rec, req)
	if cookie := sessionCookie(rec); cookie != nil {
		a.cookie = cookie.Value
	}
	return rec
}

func sessionCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == cookieName {
			return cookie
		}
	}
	return nil
}

func (a *testApp) get(path string) *httptest.ResponseRecorder {
	return a.do(http.MethodGet, path, nil, "")
}

func (a *testApp) csrf() string {
	return csrfToken(sessionKey(a.store.Snapshot().Admin), a.cookie)
}

func (a *testApp) form(path string, values url.Values) *httptest.ResponseRecorder {
	if a.cookie != "" && values.Get("csrf") == "" {
		values.Set("csrf", a.csrf())
	}
	return a.do(http.MethodPost, path, strings.NewReader(values.Encode()), "application/x-www-form-urlencoded")
}

func (a *testApp) json(method, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	a.t.Helper()
	raw, _ := json.Marshal(body)
	a.headers.Set("X-CSRF-Token", a.csrf())
	defer a.headers.Del("X-CSRF-Token")
	rec := a.do(method, path, bytes.NewReader(raw), "application/json")
	var answer map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		a.t.Fatalf("%s %s: body is not JSON: %s", method, path, rec.Body.String())
	}
	return rec, answer
}

func (a *testApp) signIn() {
	a.t.Helper()
	if rec := a.form("/setup/password", url.Values{"password": {password}}); rec.Code != http.StatusSeeOther || a.cookie == "" {
		a.t.Fatalf("create password: status %d, cookie %q", rec.Code, a.cookie)
	}
}

func (a *testApp) addPlex(fake *fakes.Plex, label string) string {
	a.t.Helper()
	rec := a.form("/setup/servers", url.Values{"kind": {"plex"}, "label": {label}, "url": {fake.URL}, "token": {fake.Token}, "library": {"Music"}, "enabled": {"1"}})
	if rec.Code != http.StatusSeeOther {
		a.t.Fatalf("add server: status %d\n%s", rec.Code, rec.Body.String())
	}
	servers := a.store.Snapshot().Servers
	return servers[len(servers)-1].Slug
}

func plexURLs(t *testing.T, a *testApp) []string {
	t.Helper()
	var out []string
	for _, server := range a.store.Snapshot().Servers {
		out = append(out, server.URL)
	}
	return out
}

// ---- first visit and password

func TestWithoutAnAdminOnlyTheCreateFormExists(t *testing.T) {
	a := newTestApp(t)
	first := a.get("/setup")
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), "Create a password") {
		t.Fatalf("first visit: %d %s", first.Code, first.Body.String())
	}
	for _, path := range []string{"/setup/servers/new", "/setup/login", "/setup/password"} {
		if rec := a.get(path); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/setup" {
			t.Errorf("GET %s: %d -> %q", path, rec.Code, rec.Header().Get("Location"))
		}
	}
	rec := a.form("/setup/servers", url.Values{"kind": {"plex"}, "url": {"http://plex:32400"}, "token": {"x"}})
	if rec.Code != http.StatusSeeOther || len(a.store.Snapshot().Servers) != 0 {
		t.Fatalf("POST without an admin: %d, servers %v", rec.Code, a.store.Snapshot().Servers)
	}
	short := a.form("/setup/password", url.Values{"password": {"short"}})
	if short.Code != http.StatusBadRequest || !strings.Contains(short.Body.String(), "at least 12 characters") || a.store.Snapshot().Admin != nil {
		t.Fatalf("short password: %d", short.Code)
	}
	a.signIn()
	admin := a.store.Snapshot().Admin
	if admin == nil || !VerifyPassword(admin.PasswordHash, password) || admin.SessionKey == "" {
		t.Fatalf("admin = %+v", admin)
	}
	if rec := a.get("/setup"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "No servers yet") {
		t.Fatalf("overview after create: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(a.logs.String(), password) {
		t.Fatal("the password reached the logs")
	}
}

func TestLoginLockoutAndCookieFlags(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	a.cookie = ""
	if rec := a.get("/setup"); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/setup/login" {
		t.Fatalf("no session: %d -> %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := a.form("/setup/servers/plex-1/delete", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("POST without a session: %d", rec.Code)
	}
	for i := range 5 {
		if rec := a.form("/setup/login", url.Values{"password": {"wrong"}}); rec.Code != http.StatusUnauthorized || a.cookie != "" {
			t.Fatalf("attempt %d: %d, cookie %q", i, rec.Code, a.cookie)
		}
	}
	if rec := a.form("/setup/login", url.Values{"password": {password}}); rec.Code != http.StatusTooManyRequests || a.cookie != "" {
		t.Fatalf("locked out login: %d", rec.Code)
	}
	a.now = a.now.Add(61 * time.Second)
	rec := a.form("/setup/login", url.Values{"password": {password}})
	if rec.Code != http.StatusSeeOther || a.cookie == "" {
		t.Fatalf("login after lockout: %d", rec.Code)
	}
	cookie := sessionCookie(rec)
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/setup" || cookie.Secure {
		t.Fatalf("cookie over http = %+v", cookie)
	}
	a.cookie = ""
	a.headers.Set("X-Forwarded-Proto", "https")
	secure := sessionCookie(a.form("/setup/login", url.Values{"password": {password}}))
	if secure == nil || !secure.Secure {
		t.Fatalf("cookie over https = %+v", secure)
	}
	a.headers.Del("X-Forwarded-Proto")
	if logs := a.logs.String(); !strings.Contains(logs, `"msg":"setup login failed","from":"10.0.0.7"`) || strings.Contains(logs, "wrong") {
		t.Fatalf("logs = %s", logs)
	}
	a.now = a.now.Add(SessionLifetime)
	if rec := a.get("/setup"); rec.Code != http.StatusSeeOther {
		t.Fatalf("expired session still valid: %d", rec.Code)
	}
}

func TestCSRFAndCrossSitePostsAreRefused(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	attempts := map[string]string{"missing": "public_url=https%3A%2F%2Fmusic.example.com", "wrong": "public_url=https%3A%2F%2Fmusic.example.com&csrf=nope"}
	for name, body := range attempts {
		rec := a.do(http.MethodPost, "/setup/public-url", strings.NewReader(body), "application/x-www-form-urlencoded")
		if rec.Code != http.StatusForbidden || a.store.Snapshot().PublicURL != "" {
			t.Errorf("%s csrf: %d, public url %q", name, rec.Code, a.store.Snapshot().PublicURL)
		}
	}
	a.headers.Set("Sec-Fetch-Site", "cross-site")
	if rec := a.form("/setup/public-url", url.Values{"public_url": {"https://music.example.com"}}); rec.Code != http.StatusForbidden {
		t.Errorf("cross-site: %d", rec.Code)
	}
	a.headers.Set("Sec-Fetch-Site", "same-origin")
	if rec := a.form("/setup/public-url", url.Values{"public_url": {"https://music.example.com"}}); rec.Code != http.StatusSeeOther {
		t.Errorf("same-origin: %d", rec.Code)
	}
}

func TestChangePasswordSignsOtherBrowsersOut(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	other := a.cookie
	if rec := a.form("/setup/password", url.Values{"current": {"wrong"}, "password": {"another long password"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong current password: %d", rec.Code)
	}
	if rec := a.form("/setup/password", url.Values{"current": {password}, "password": {"another long password"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("change: %d %s", rec.Code, rec.Body.String())
	}
	if rec := a.get("/setup"); rec.Code != http.StatusOK {
		t.Fatalf("new session: %d", rec.Code)
	}
	a.cookie = other
	if rec := a.get("/setup"); rec.Code != http.StatusSeeOther {
		t.Fatalf("old session still valid: %d", rec.Code)
	}
	a.cookie = ""
	if rec := a.form("/setup/login", url.Values{"password": {"another long password"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("login with the new password: %d", rec.Code)
	}
	if rec := a.form("/setup/logout", url.Values{}); rec.Code != http.StatusSeeOther || sessionCookie(rec).MaxAge != -1 {
		t.Fatalf("logout: %d", rec.Code)
	}
}

// ---- public url, secret, servers

func TestPublicURLAndSecret(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	if rec := a.get("/setup"); !strings.Contains(rec.Body.String(), "Set the HTTPS address") {
		t.Fatal("missing public url banner")
	}
	if rec := a.form("/setup/public-url", url.Values{"public_url": {"http://music.example.com"}}); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "https://") {
		t.Fatalf("http public url: %d", rec.Code)
	}
	if rec := a.form("/setup/public-url", url.Values{"public_url": {"https://music.example.com/"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("public url: %d", rec.Code)
	}
	fake := fakes.NewPlex(t)
	slug := a.addPlex(fake, "Home")
	before := a.store.Snapshot().Secret
	page := a.get("/setup").Body.String()
	if !strings.Contains(page, "https://music.example.com/"+slug+"/"+before) || !strings.Contains(page, "Plex - Home") {
		t.Fatalf("overview lacks the server url:\n%s", page)
	}
	if rec := a.form("/setup/secret/regenerate", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("regenerate: %d", rec.Code)
	}
	after := a.store.Snapshot().Secret
	page = a.get("/setup").Body.String()
	if after == before || strings.Contains(page, before) || !strings.Contains(page, "https://music.example.com/"+slug+"/"+after) {
		t.Fatalf("secret not rotated on the page")
	}
}

func TestServerLifecycleThroughTheForms(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	a.form("/setup/public-url", url.Values{"public_url": {"https://music.example.com"}})
	fake := fakes.NewPlex(t)
	if rec := a.get("/setup/servers/new"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Sign in with Plex") {
		t.Fatalf("new form: %d", rec.Code)
	}
	slug := a.addPlex(fake, "Home")
	server, _ := a.store.Snapshot().Server(slug)
	if slug != "plex-1" || server.Token != fakes.PlexToken || server.Auth != store.AuthToken || server.Library != "3" || server.LibraryName != "Music" || !server.Enabled {
		t.Fatalf("stored = %+v", server)
	}
	deadline := time.Now().Add(5 * time.Second)
	for status, ok := a.reg.Status(slug); !ok || !status.Ready; status, ok = a.reg.Status(slug) {
		if time.Now().After(deadline) {
			t.Fatal("registry never indexed the added server")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rec := a.get("/setup"); !strings.Contains(rec.Body.String(), "5 tracks") || !strings.Contains(rec.Body.String(), "<dt>Library</dt><dd>Music</dd>") {
		t.Fatalf("overview lacks the track count or the library name:\n%s", rec.Body.String())
	}

	edit := a.get("/setup/servers/plex-1/edit")
	if edit.Code != http.StatusOK || !strings.Contains(edit.Body.String(), `value="Home"`) || !strings.Contains(edit.Body.String(), "leave empty to keep") || !strings.Contains(edit.Body.String(), `<option value="3" selected>Music</option>`) {
		t.Fatalf("edit form: %d %s", edit.Code, edit.Body.String())
	}
	a.store.UpdateServer(slug, func(server *store.Server) error {
		server.LibraryName = ""
		return nil
	})
	if !strings.Contains(a.get("/setup").Body.String(), "<dt>Library</dt><dd>3</dd>") || !strings.Contains(a.get("/setup/servers/plex-1/edit").Body.String(), `<option value="3" selected>3</option>`) {
		t.Fatal("a server saved without a library name does not fall back to its id")
	}
	rec := a.form("/setup/servers/plex-1", url.Values{"label": {"Parents"}, "url": {fake.URL}, "library": {""}, "enabled": {"1"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("edit: %d %s", rec.Code, rec.Body.String())
	}
	server, _ = a.store.Snapshot().Server(slug)
	if server.Label != "Parents" || server.Token != fakes.PlexToken || server.Library != "" {
		t.Fatalf("after edit = %+v", server)
	}
	if entry, _ := a.reg.Lookup(slug); entry.DisplayName() != "Plex - Parents" {
		t.Fatalf("registry name = %q", entry.DisplayName())
	}

	if rec := a.form("/setup/servers/plex-1", url.Values{"label": {"x"}, "url": {fake.URL}, "library": {"Movies"}, "enabled": {"1"}}); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "no music library") {
		t.Fatalf("unknown library: %d", rec.Code)
	}
	if rec := a.form("/setup/servers/plex-1", url.Values{"label": {"x"}, "url": {fake.URL}, "token": {"wrong"}, "enabled": {"1"}}); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "rejected the credentials") {
		t.Fatalf("wrong token: %d %s", rec.Code, rec.Body.String())
	}
	if server, _ := a.store.Snapshot().Server(slug); server.Token != fakes.PlexToken {
		t.Fatal("a failed save changed the token")
	}

	if rec := a.form("/setup/servers/plex-1/enabled", url.Values{"enabled": {"false"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("disable: %d", rec.Code)
	}
	if _, ok := a.reg.Lookup(slug); ok {
		t.Fatal("disabled server still running")
	}
	if server, _ := a.store.Snapshot().Server(slug); server.Enabled {
		t.Fatal("disabled flag not stored")
	}
	second := a.addPlex(fakes.NewPlex(t), "")
	if second != "plex-2" {
		t.Fatalf("second slug = %q", second)
	}
	if rec := a.form("/setup/servers/plex-1/delete", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete: %d", rec.Code)
	}
	if _, ok := a.store.Snapshot().Server("plex-1"); ok {
		t.Fatal("deleted server still stored")
	}
	if rec := a.form("/setup/servers/plex-9/delete", url.Values{}); rec.Code != http.StatusNotFound {
		t.Fatalf("delete unknown: %d", rec.Code)
	}
	if rec := a.form("/setup/servers/plex-9/enabled", url.Values{"enabled": {"false"}}); rec.Code != http.StatusNotFound {
		t.Fatalf("disable unknown: %d", rec.Code)
	}
	if rec := a.get("/setup/servers/plex-9/edit"); rec.Code != http.StatusNotFound {
		t.Fatalf("edit unknown: %d", rec.Code)
	}
	if rec := a.form("/setup/servers", url.Values{"kind": {"plex"}, "url": {fake.URL}, "enabled": {"1"}}); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "sign in or paste a token") {
		t.Fatalf("no credential: %d", rec.Code)
	}
	if strings.Contains(a.logs.String(), fakes.PlexToken) {
		t.Fatal("a token reached the logs")
	}
}

func TestTestConnectionEndpoint(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	fake := fakes.NewPlex(t)
	rec, answer := a.json(http.MethodPost, "/setup/servers/test", testRequest{Kind: store.Plex, URL: fake.URL, Token: fakes.PlexToken})
	if rec.Code != http.StatusOK || answer["ok"] != true || answer["version"] != "1.42.0.9999" {
		t.Fatalf("test: %d %v", rec.Code, answer)
	}
	if libraries := answer["libraries"].([]any); len(libraries) != 2 || libraries[0].(map[string]any)["name"] != "Music" {
		t.Fatalf("libraries = %v", answer["libraries"])
	}
	rec, answer = a.json(http.MethodPost, "/setup/servers/test", testRequest{Kind: store.Plex, URL: fake.URL, Token: "wrong"})
	if rec.Code != http.StatusBadGateway || answer["ok"] != false || !strings.Contains(answer["error"].(string), "rejected the credentials") || strings.Contains(rec.Body.String(), "wrong") {
		t.Fatalf("wrong token: %d %s", rec.Code, rec.Body.String())
	}
	rec, answer = a.json(http.MethodPost, "/setup/servers/test", testRequest{Kind: store.Plex, URL: fakes.DeadURL(t), Token: "x"})
	if message, _ := answer["error"].(string); rec.Code != http.StatusBadGateway || answer["ok"] != false || !strings.HasPrefix(message, "plex:") || strings.Contains(message, "rejected the credentials") {
		t.Fatalf("dead server: %d %v", rec.Code, answer)
	}
	slug := a.addPlex(fake, "Home")
	rec, answer = a.json(http.MethodPost, "/setup/servers/test", testRequest{Kind: store.Plex, URL: fake.URL, Slug: slug})
	if rec.Code != http.StatusOK || answer["ok"] != true {
		t.Fatalf("test with the stored token: %d %v", rec.Code, answer)
	}
	rec, _ = a.json(http.MethodPost, "/setup/servers/test", testRequest{Kind: store.Plex, URL: fake.URL})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("test without a credential: %d", rec.Code)
	}
	a.headers.Set("X-CSRF-Token", "bad")
	if rec := a.do(http.MethodPost, "/setup/servers/test", strings.NewReader("{}"), "application/json"); rec.Code != http.StatusForbidden {
		t.Fatalf("json post with a bad csrf header: %d", rec.Code)
	}
}

func TestPlexSignInEndToEnd(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	home := fakes.NewPlex(t)
	home.Token = fakes.PlexTVToken
	a.plexTV.Resources = fakes.PlexTVResources(home.URL, fakes.DeadURL(t))

	rec, pin := a.json(http.MethodPost, "/setup/plex/pin", map[string]any{})
	if rec.Code != http.StatusOK || pin["ok"] != true || pin["id"] != float64(1) || !strings.HasPrefix(pin["auth_url"].(string), "https://app.plex.tv/auth#?") {
		t.Fatalf("pin: %d %v", rec.Code, pin)
	}
	rec, poll := a.json(http.MethodGet, "/setup/plex/pin/1", nil)
	if rec.Code != http.StatusOK || poll["claimed"] != false {
		t.Fatalf("poll before sign-in: %d %v", rec.Code, poll)
	}
	a.plexTV.Claim(1)
	rec, poll = a.json(http.MethodGet, "/setup/plex/pin/1", nil)
	if rec.Code != http.StatusOK || poll["claimed"] != true || poll["username"] != fakes.PlexTVUsername || poll["ref"] == "" {
		t.Fatalf("poll after sign-in: %d %v", rec.Code, poll)
	}
	if strings.Contains(rec.Body.String(), fakes.PlexTVToken) {
		t.Fatal("the account token reached the browser")
	}
	servers := poll["servers"].([]any)
	first := servers[0].(map[string]any)
	if len(servers) != 2 || first["name"] != "Home" || first["url"] != home.URL || first["reachable"] != true || servers[1].(map[string]any)["reachable"] != false {
		t.Fatalf("servers = %v", servers)
	}
	ref := poll["ref"].(string)

	rec, answer := a.json(http.MethodPost, "/setup/servers/test", testRequest{Kind: store.Plex, URL: home.URL, TokenRef: ref})
	if rec.Code != http.StatusOK || answer["ok"] != true {
		t.Fatalf("test with the reference: %d %v", rec.Code, answer)
	}
	rec, answer = a.json(http.MethodPost, "/setup/servers/test", testRequest{Kind: store.Plex, URL: home.URL, TokenRef: ref, Token: "stale-pasted-token"})
	if rec.Code != http.StatusOK || answer["ok"] != true {
		t.Fatalf("test with a reference and a pasted token must use the reference, as a save does: %d %v", rec.Code, answer)
	}
	if rec := a.form("/setup/servers", url.Values{"kind": {"plex"}, "label": {"Home"}, "url": {home.URL}, "token_ref": {ref}, "enabled": {"1"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	server, _ := a.store.Snapshot().Server("plex-1")
	if server.Token != fakes.PlexTVToken || server.Auth != store.AuthPlexSignIn || server.Account != fakes.PlexTVUsername {
		t.Fatalf("stored = %+v", server)
	}
	if rec := a.form("/setup/servers", url.Values{"kind": {"plex"}, "url": {home.URL}, "token_ref": {ref}, "enabled": {"1"}}); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "sign-in has expired") {
		t.Fatalf("reused reference: %d", rec.Code)
	}
	if rec := a.get("/setup/servers/plex-1/edit"); !strings.Contains(rec.Body.String(), "Signed in as lyle") {
		t.Fatal("edit form does not show the account")
	}
	if rec := a.get("/setup"); !strings.Contains(rec.Body.String(), "signed in as lyle") {
		t.Fatal("overview does not show the account")
	}
	if rec, _ := a.json(http.MethodGet, "/setup/plex/pin/99", nil); rec.Code != http.StatusGone {
		t.Fatalf("unknown pin: %d", rec.Code)
	}
	if strings.Contains(a.logs.String(), fakes.PlexTVToken) {
		t.Fatal("the account token reached the logs")
	}
}

func TestPlexSignInWhenPlexTVIsUnreachable(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	a.reg = registry.New(a.store.Snapshot(), registry.Options{Interval: time.Hour, Log: slog.New(slog.DiscardHandler), PlexTV: fakes.DeadURL(t)})
	t.Cleanup(a.reg.Stop)
	a.handler = New(Options{Store: a.store, Registry: a.reg, Now: func() time.Time { return a.now }})
	rec, answer := a.json(http.MethodPost, "/setup/plex/pin", map[string]any{})
	if rec.Code != http.StatusBadGateway || !strings.Contains(answer["error"].(string), "paste a token") {
		t.Fatalf("pin: %d %v", rec.Code, answer)
	}
}

func TestJellyfinSignInStoresTheTokenAndNeverThePassword(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	fake := fakes.NewJellyfin(t)
	rec, answer := a.json(http.MethodPost, "/setup/jellyfin/signin", jellyfinSignInRequest{URL: fake.URL, Username: fakes.JellyfinUser, Password: fakes.JellyfinPassword})
	if rec.Code != http.StatusOK || answer["ok"] != true || answer["username"] != fakes.JellyfinUser || answer["ref"] == "" {
		t.Fatalf("sign-in: %d %v", rec.Code, answer)
	}
	if strings.Contains(rec.Body.String(), fakes.JellyfinSessionPrefix) {
		t.Fatal("the access token reached the browser")
	}
	ref := answer["ref"].(string)
	if rec := a.form("/setup/servers", url.Values{"kind": {"jellyfin"}, "label": {"NAS"}, "url": {fake.URL}, "token_ref": {ref}, "library": {"Music"}, "enabled": {"1"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	server, _ := a.store.Snapshot().Server("jellyfin-1")
	if !strings.HasPrefix(server.Token, fakes.JellyfinSessionPrefix) || server.Auth != store.AuthJellyfinSignIn || server.Account != fakes.JellyfinUser || server.Library != fakes.MusicFolder {
		t.Fatalf("stored = %+v", server)
	}
	moved := a.form("/setup/servers/jellyfin-1", url.Values{"label": {"NAS"}, "url": {fakes.NewJellyfin(t).URL}, "enabled": {"1"}})
	if moved.Code != http.StatusBadRequest || !strings.Contains(moved.Body.String(), "sign in again") {
		t.Fatalf("url change on a signed-in server: %d", moved.Code)
	}
	for i := range 5 {
		rec, _ := a.json(http.MethodPost, "/setup/jellyfin/signin", jellyfinSignInRequest{URL: fake.URL, Username: fakes.JellyfinUser, Password: "nope"})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, rec.Code)
		}
	}
	if rec, _ := a.json(http.MethodPost, "/setup/jellyfin/signin", jellyfinSignInRequest{URL: fake.URL, Username: fakes.JellyfinUser, Password: fakes.JellyfinPassword}); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after five failures: %d", rec.Code)
	}
	if logs := a.logs.String(); strings.Contains(logs, fakes.JellyfinPassword) || strings.Contains(logs, "nope") || strings.Contains(logs, fakes.JellyfinSessionPrefix) {
		t.Fatalf("logs carry a credential:\n%s", logs)
	}
	_ = plexURLs
}

func TestConcurrentLoginAttemptsAreLimitedAtomically(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	a.cookie = ""

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/setup/login", strings.NewReader(url.Values{"password": {"wrong"}}.Encode()))
			req.RemoteAddr = "10.0.0.9:5555"
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			a.handler.ServeHTTP(httptest.NewRecorder(), req)
		}()
	}
	wg.Wait()

	req := httptest.NewRequest(http.MethodPost, "/setup/login", strings.NewReader(url.Values{"password": {password}}.Encode()))
	req.RemoteAddr = "10.0.0.9:5555"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	a.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after 20 concurrent attempts: %d", rec.Code)
	}
	if failures := strings.Count(a.logs.String(), `"msg":"setup login failed"`); failures > 5 {
		t.Fatalf("VerifyPassword ran %d times, want at most 5", failures)
	}
}

func TestLoginLockoutIgnoresXForwardedFor(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	a.cookie = ""
	for i := range 6 {
		a.headers.Set("X-Forwarded-For", fmt.Sprintf("203.0.113.%d", i))
		rec := a.form("/setup/login", url.Values{"password": {"wrong"}})
		switch {
		case i < 5 && rec.Code != http.StatusUnauthorized:
			t.Fatalf("attempt %d: %d", i, rec.Code)
		case i == 5 && rec.Code != http.StatusTooManyRequests:
			t.Fatalf("sixth attempt with rotated X-Forwarded-For: %d", rec.Code)
		}
	}
	a.headers.Del("X-Forwarded-For")
}

func TestStaticAndUnknownSetupPaths(t *testing.T) {
	a := newTestApp(t)
	if rec := a.get("/setup/static/style.css"); rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "text/css") {
		t.Fatalf("stylesheet: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	a.signIn()
	if rec := a.get("/setup/nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown path: %d", rec.Code)
	}
	if rec := a.get("/setup/"); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/setup" {
		t.Fatalf("trailing slash: %d", rec.Code)
	}
	if rec := a.get("/setup"); rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("pages must not be cached")
	}
}

// applyRecorder records every snapshot handed to Apply. Its first Apply holds
// until the store has moved past the snapshot it was given, so a second write
// lands while the first is still applying.
type applyRecorder struct {
	Runtime
	store   *store.Store
	entered chan struct{}
	mu      sync.Mutex
	applied []store.Snapshot
}

func (r *applyRecorder) Apply(snapshot store.Snapshot) {
	r.mu.Lock()
	entered := r.entered
	r.entered = nil
	r.mu.Unlock()
	if entered != nil {
		close(entered)
		deadline := time.Now().Add(2 * time.Second)
		for reflect.DeepEqual(r.store.Snapshot(), snapshot) && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.applied = append(r.applied, snapshot)
}

func (r *applyRecorder) last() store.Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.applied[len(r.applied)-1]
}

func TestConcurrentWritesApplyTheLatestSnapshotLast(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	for range 2 {
		if _, err := a.store.AddServer(store.Server{Kind: store.Plex, URL: "http://plex:32400", Token: "t", Auth: store.AuthToken, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	recorder := &applyRecorder{Runtime: a.reg, store: a.store, entered: make(chan struct{})}
	a.handler = New(Options{Store: a.store, Registry: recorder, Now: func() time.Time { return a.now }})
	disable := func(slug string) *http.Request {
		body := url.Values{"enabled": {"false"}, "csrf": {a.csrf()}}.Encode()
		req := httptest.NewRequest(http.MethodPost, "/setup/servers/"+slug+"/enabled", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: cookieName, Value: a.cookie})
		return req
	}
	first, second := disable("plex-1"), disable("plex-2")
	entered := recorder.entered
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		a.handler.ServeHTTP(httptest.NewRecorder(), first)
	}()
	<-entered
	go func() {
		defer wg.Done()
		a.handler.ServeHTTP(httptest.NewRecorder(), second)
	}()
	wg.Wait()
	if got, want := recorder.last(), a.store.Snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("last applied servers = %+v, store servers = %+v", got.Servers, want.Servers)
	}
}

func TestAFailedSaveKeepsTheSignIn(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	fake := fakes.NewJellyfin(t)
	rec, answer := a.json(http.MethodPost, "/setup/jellyfin/signin", jellyfinSignInRequest{URL: fake.URL, Username: fakes.JellyfinUser, Password: fakes.JellyfinPassword})
	if rec.Code != http.StatusOK {
		t.Fatalf("sign-in: %d %v", rec.Code, answer)
	}
	ref := answer["ref"].(string)
	if rec := a.form("/setup/servers", url.Values{"kind": {"jellyfin"}, "url": {fake.URL}, "token_ref": {ref}, "library": {"Movies"}, "enabled": {"1"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("save with an unknown library: %d", rec.Code)
	}
	if rec := a.form("/setup/servers", url.Values{"kind": {"jellyfin"}, "url": {fake.URL}, "token_ref": {ref}, "library": {"Music"}, "enabled": {"1"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("save after fixing the form: %d %s", rec.Code, rec.Body.String())
	}
	if rec := a.form("/setup/servers", url.Values{"kind": {"jellyfin"}, "url": {fake.URL}, "token_ref": {ref}, "enabled": {"1"}}); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "sign-in has expired") {
		t.Fatalf("a saved sign-in was reused: %d", rec.Code)
	}
}

func TestASignInSavesOnlyOnceUnderConcurrentSaves(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	fake := fakes.NewJellyfin(t)
	ref := a.jellyfinSignIn(fake)
	start := make(chan struct{})
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			codes[i] = a.form("/setup/servers", url.Values{"kind": {"jellyfin"}, "url": {fake.URL}, "token_ref": {ref}, "enabled": {"1"}}).Code
		}()
	}
	close(start)
	wg.Wait()
	slices.Sort(codes)
	if codes[0] != http.StatusSeeOther || codes[1] != http.StatusBadRequest || len(a.store.Snapshot().Servers) != 1 {
		t.Fatalf("codes = %v, servers = %d", codes, len(a.store.Snapshot().Servers))
	}
}

func TestJellyfinSignInLimiterCountsOnlyRejectedCredentials(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	fake := fakes.NewJellyfin(t)
	fake.FailWith(http.StatusServiceUnavailable)
	for i := range 6 {
		if rec, _ := a.json(http.MethodPost, "/setup/jellyfin/signin", jellyfinSignInRequest{URL: fake.URL, Username: fakes.JellyfinUser, Password: fakes.JellyfinPassword}); rec.Code != http.StatusBadGateway {
			t.Fatalf("upstream failure %d: %d", i, rec.Code)
		}
	}
	fake.FailWith(0)
	for i := range 4 {
		if rec, _ := a.json(http.MethodPost, "/setup/jellyfin/signin", jellyfinSignInRequest{URL: fake.URL, Username: fakes.JellyfinUser, Password: "nope"}); rec.Code != http.StatusUnauthorized {
			t.Fatalf("rejected %d: %d", i, rec.Code)
		}
	}
	if rec, _ := a.json(http.MethodPost, "/setup/jellyfin/signin", jellyfinSignInRequest{URL: fake.URL, Username: fakes.JellyfinUser, Password: fakes.JellyfinPassword}); rec.Code != http.StatusOK {
		t.Fatalf("sign-in after upstream failures and four rejections: %d", rec.Code)
	}
}

func logouts(fake *fakes.Jellyfin) []fakes.Request {
	return slices.DeleteFunc(fake.Requests(), func(request fakes.Request) bool { return request.Path != "/Sessions/Logout" })
}

func (a *testApp) jellyfinSignIn(fake *fakes.Jellyfin) string {
	a.t.Helper()
	rec, answer := a.json(http.MethodPost, "/setup/jellyfin/signin", jellyfinSignInRequest{URL: fake.URL, Username: fakes.JellyfinUser, Password: fakes.JellyfinPassword})
	if rec.Code != http.StatusOK {
		a.t.Fatalf("sign-in: %d %v", rec.Code, answer)
	}
	return answer["ref"].(string)
}

func TestJellyfinReSignInSignsTheOldSessionOutOnlyOnceSaved(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	fake := fakes.NewJellyfin(t)
	if rec := a.form("/setup/servers", url.Values{"kind": {"jellyfin"}, "url": {fake.URL}, "token_ref": {a.jellyfinSignIn(fake)}, "enabled": {"1"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
	}
	original, _ := a.store.Snapshot().Server("jellyfin-1")
	works := func(token string) bool {
		t.Helper()
		request := testRequest{Kind: store.Jellyfin, URL: fake.URL, Token: token, Slug: "jellyfin-1"}
		rec, _ := a.json(http.MethodPost, "/setup/servers/test", request)
		return rec.Code == http.StatusOK
	}

	a.jellyfinSignIn(fake)
	if rec := a.form("/setup/servers/jellyfin-1", url.Values{"label": {"Renamed"}, "url": {fake.URL}, "enabled": {"1"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("label edit: %d %s", rec.Code, rec.Body.String())
	}
	if len(logouts(fake)) != 0 || !works("") {
		t.Fatal("an abandoned re-sign-in or a label edit signed the running server out")
	}

	if rec := a.form("/setup/servers/jellyfin-1", url.Values{"url": {fake.URL}, "token_ref": {a.jellyfinSignIn(fake)}, "enabled": {"1"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	a.setup.app.background.Wait()
	saved, _ := a.store.Snapshot().Server("jellyfin-1")
	if saved.Token == original.Token || saved.DeviceID == original.DeviceID || saved.DeviceID == "" {
		t.Fatalf("after the save = %+v, before = %+v", saved, original)
	}
	sent := logouts(fake)
	if len(sent) != 1 || !strings.Contains(sent[0].Header.Get("Authorization"), `DeviceId="`+original.DeviceID+`"`) || !strings.Contains(sent[0].Header.Get("Authorization"), `Token="`+original.Token+`"`) {
		t.Fatalf("logouts = %+v", sent)
	}
	if works(original.Token) || !works("") {
		t.Fatal("the old token still works, or the new one does not")
	}
	if strings.Contains(a.logs.String(), fakes.JellyfinSessionPrefix) {
		t.Fatal("a token reached the logs")
	}
}

// replaceJellyfinSignIn adds a signed-in Jellyfin server, holds the first
// refresh of whatever entry starts next, and saves a new sign-in for it.
func replaceJellyfinSignIn(t *testing.T, a *testApp, fake *fakes.Jellyfin, release <-chan struct{}) (original store.Server, old *registry.Entry) {
	t.Helper()
	original, old = addHeldJellyfin(t, a, fake, release)
	a.resignJellyfin(fake)
	return original, old
}

func addHeldJellyfin(t *testing.T, a *testApp, fake *fakes.Jellyfin, release <-chan struct{}) (original store.Server, old *registry.Entry) {
	t.Helper()
	if rec := a.form("/setup/servers", url.Values{"kind": {"jellyfin"}, "url": {fake.URL}, "token_ref": {a.jellyfinSignIn(fake)}, "enabled": {"1"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
	}
	original, _ = a.store.Snapshot().Server("jellyfin-1")
	deadline := time.Now().Add(5 * time.Second)
	for !a.reg.Healthy() {
		if time.Now().After(deadline) {
			t.Fatal("registry never indexed the server")
		}
		time.Sleep(10 * time.Millisecond)
	}
	old, _ = a.reg.Lookup("jellyfin-1")
	fake.Extra["/Items"] = func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
			w.WriteHeader(http.StatusServiceUnavailable)
		case <-r.Context().Done():
		}
	}
	return original, old
}

func (a *testApp) resignJellyfin(fake *fakes.Jellyfin) {
	a.t.Helper()
	if rec := a.form("/setup/servers/jellyfin-1", url.Values{"url": {fake.URL}, "token_ref": {a.jellyfinSignIn(fake)}, "enabled": {"1"}}); rec.Code != http.StatusSeeOther {
		a.t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
}

func loggedOut(fake *fakes.Jellyfin, token string) bool {
	return slices.ContainsFunc(logouts(fake), func(request fakes.Request) bool {
		return strings.Contains(request.Header.Get("Authorization"), `Token="`+token+`"`)
	})
}

func playsThrough(t *testing.T, entry *registry.Entry) {
	t.Helper()
	track, ok := entry.Library.Get("f101")
	if !ok {
		t.Fatal("the old index lost its track")
	}
	res, err := entry.Backend.OpenFile(context.Background(), track, http.MethodGet, http.Header{})
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("audio through the old entry: %v %v", res, err)
	}
	res.Body.Close()
}

func TestASecondSaveDuringAHandoverKeepsTheOldTokenUntilItIsUnused(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	fake := fakes.NewJellyfin(t)
	release := make(chan struct{})
	original, old := replaceJellyfinSignIn(t, a, fake, release)
	first, _ := a.store.Snapshot().Server("jellyfin-1")
	a.resignJellyfin(fake)
	second, _ := a.store.Snapshot().Server("jellyfin-1")

	deadline := time.Now().Add(5 * time.Second)
	for !loggedOut(fake, first.Token) {
		if time.Now().After(deadline) {
			t.Fatal("the cancelled sign-in was never signed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if loggedOut(fake, original.Token) {
		t.Fatal("the running entry's token was signed out while it still serves")
	}
	if entry, _ := a.reg.Lookup("jellyfin-1"); entry != old {
		t.Fatal("the old entry stopped serving while both new ones were held")
	}
	playsThrough(t, old)

	close(release)
	a.setup.app.background.Wait()
	if !loggedOut(fake, original.Token) || loggedOut(fake, second.Token) {
		t.Fatalf("logouts after the swap = %+v", logouts(fake))
	}
}

type stuckRuntime struct {
	Runtime
}

func (stuckRuntime) Apply(store.Snapshot) {}

func TestAFailedRestartKeepsTheOldToken(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	fake := fakes.NewJellyfin(t)
	original, old := addHeldJellyfin(t, a, fake, nil)
	a.setup = New(Options{Store: a.store, Registry: stuckRuntime{a.reg}, Log: a.log, Now: func() time.Time { return a.now }, SignOutWait: 50 * time.Millisecond})
	t.Cleanup(a.setup.Close)
	a.handler = a.setup
	a.resignJellyfin(fake)
	a.setup.app.background.Wait()
	if loggedOut(fake, original.Token) {
		t.Fatal("signed out the token the running entry still uses")
	}
	playsThrough(t, old)
	if logs := a.logs.String(); !strings.Contains(logs, "old jellyfin session left signed in") || strings.Contains(logs, fakes.JellyfinSessionPrefix) {
		t.Fatalf("logs:\n%s", logs)
	}
}

func TestASaveAfterCloseStartsNoSignOut(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	fake := fakes.NewJellyfin(t)
	original, _ := addHeldJellyfin(t, a, fake, nil)
	a.setup.Close()
	a.resignJellyfin(fake)
	a.reg.Stop()
	a.setup.app.background.Wait()
	if loggedOut(fake, original.Token) || !strings.Contains(a.logs.String(), "old jellyfin session left signed in") {
		t.Fatal("a save after Close started a sign-out")
	}
}

func TestJellyfinSignOutWaitsForTheHandover(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	fake := fakes.NewJellyfin(t)
	release := make(chan struct{})
	original, old := replaceJellyfinSignIn(t, a, fake, release)

	if len(logouts(fake)) != 0 {
		t.Fatal("the old session was signed out before the new entry took over")
	}
	if entry, _ := a.reg.Lookup("jellyfin-1"); entry != old {
		t.Fatal("the new entry took over while its first refresh was held")
	}
	playsThrough(t, old)

	close(release)
	a.setup.app.background.Wait()
	sent := logouts(fake)
	if len(sent) != 1 || !strings.Contains(sent[0].Header.Get("Authorization"), `Token="`+original.Token+`"`) {
		t.Fatalf("logouts = %+v", sent)
	}
	if entry, _ := a.reg.Lookup("jellyfin-1"); entry == old {
		t.Fatal("signed out before the swap")
	}
}

func TestShutdownStillSignsOutAReplacedSession(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	fake := fakes.NewJellyfin(t)
	release := make(chan struct{})
	defer close(release)
	original, _ := replaceJellyfinSignIn(t, a, fake, release)
	a.reg.Stop()
	a.setup.Close()
	if sent := logouts(fake); len(sent) != 1 || !strings.Contains(sent[0].Header.Get("Authorization"), `Token="`+original.Token+`"`) {
		t.Fatalf("logouts after shutdown = %+v", sent)
	}
}

func TestAPastedJellyfinKeyIsNotSignedOut(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	fake := fakes.NewJellyfin(t)
	if rec := a.form("/setup/servers", url.Values{"kind": {"jellyfin"}, "url": {fake.URL}, "token": {fakes.JellyfinToken}, "enabled": {"1"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
	}
	if rec := a.form("/setup/servers/jellyfin-1", url.Values{"url": {fake.URL}, "token_ref": {a.jellyfinSignIn(fake)}, "enabled": {"1"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	if len(logouts(fake)) != 0 {
		t.Fatal("the pasted API key was signed out")
	}
	if rec, _ := a.json(http.MethodPost, "/setup/servers/test", testRequest{Kind: store.Jellyfin, URL: fake.URL, Token: fakes.JellyfinToken}); rec.Code != http.StatusOK {
		t.Fatalf("pasted key after the edit: %d", rec.Code)
	}
}

func TestTwoJellyfinSignInsKeepTheirOwnDeviceIDs(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	fake := fakes.NewJellyfin(t)
	for _, label := range []string{"Music", "Audiobooks"} {
		rec, answer := a.json(http.MethodPost, "/setup/jellyfin/signin", jellyfinSignInRequest{URL: fake.URL, Username: fakes.JellyfinUser, Password: fakes.JellyfinPassword})
		if rec.Code != http.StatusOK {
			t.Fatalf("sign-in: %d %v", rec.Code, answer)
		}
		if rec := a.form("/setup/servers", url.Values{"kind": {"jellyfin"}, "label": {label}, "url": {fake.URL}, "token_ref": {answer["ref"].(string)}, "library": {label}, "enabled": {"1"}}); rec.Code != http.StatusSeeOther {
			t.Fatalf("save %s: %d %s", label, rec.Code, rec.Body.String())
		}
	}
	snapshot := a.store.Snapshot()
	first, _ := snapshot.Server("jellyfin-1")
	second, _ := snapshot.Server("jellyfin-2")
	if first.DeviceID == "" || second.DeviceID == "" || first.DeviceID == second.DeviceID || first.DeviceID == snapshot.ClientID {
		t.Fatalf("device ids = %q, %q (client id %q)", first.DeviceID, second.DeviceID, snapshot.ClientID)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !a.reg.Healthy() {
		if time.Now().After(deadline) {
			t.Fatal("registry never indexed both servers")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, server := range []store.Server{first, second} {
		sent := false
		for _, request := range fake.Requests() {
			if request.Path == "/Items" && strings.Contains(request.Query, "ParentId="+server.Library) && strings.Contains(request.Header.Get("Authorization"), `DeviceId="`+server.DeviceID+`"`) {
				sent = true
			}
		}
		if !sent {
			t.Errorf("%s never sent its own DeviceId %q", server.Slug, server.DeviceID)
		}
	}
	if rec := a.form("/setup/servers/jellyfin-1", url.Values{"label": {"Renamed"}, "url": {fake.URL}, "library": {"Music"}, "enabled": {"1"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("edit: %d %s", rec.Code, rec.Body.String())
	}
	if edited, _ := a.store.Snapshot().Server("jellyfin-1"); edited.DeviceID != first.DeviceID {
		t.Fatalf("edit changed the device id from %q to %q", first.DeviceID, edited.DeviceID)
	}
	for _, path := range []string{"/setup", "/setup/servers/jellyfin-1/edit"} {
		if body := a.get(path).Body.String(); strings.Contains(body, first.DeviceID) || strings.Contains(body, second.DeviceID) {
			t.Errorf("%s shows a device id", path)
		}
	}
}

func TestSignedInLoginPageRedirectsToTheOverview(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	if rec := a.get("/setup/login"); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/setup" {
		t.Fatalf("GET /setup/login signed in: %d -> %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestSinceDropsZeroUnits(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	cases := map[time.Duration]string{
		30 * time.Second:                            "just now",
		5 * time.Minute:                             "5m ago",
		5*time.Minute + 40*time.Second:              "5m ago",
		2 * time.Hour:                               "2h ago",
		2*time.Hour + 5*time.Minute:                 "2h5m ago",
		2*time.Hour + 5*time.Minute + 9*time.Second: "2h5m ago",
	}
	for ago, want := range cases {
		if got := since(now.Add(-ago), now); got != want {
			t.Errorf("since(%s) = %q, want %q", ago, got, want)
		}
	}
	if got := since(time.Time{}, now); got != "never" {
		t.Errorf("zero time = %q", got)
	}
}
