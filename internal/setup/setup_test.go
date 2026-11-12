package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
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

func (a *testApp) multipartForm(path string, fieldName, filename string, content []byte) *httptest.ResponseRecorder {
	return a.multipartFormWithFields(path, fieldName, filename, content, nil)
}

func (a *testApp) multipartFormWithFields(path string, fieldName, filename string, content []byte, extraFields map[string]string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if a.cookie != "" {
		_ = mw.WriteField("csrf", a.csrf())
	}
	for k, v := range extraFields {
		_ = mw.WriteField(k, v)
	}
	fw, err := mw.CreateFormFile(fieldName, filename)
	if err != nil {
		a.t.Fatal(err)
	}
	if _, err := fw.Write(content); err != nil {
		a.t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		a.t.Fatal(err)
	}
	return a.do(http.MethodPost, path, &buf, mw.FormDataContentType())
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
	if first.Header().Get("X-Content-Type-Options") != "nosniff" ||
		first.Header().Get("X-Frame-Options") != "DENY" ||
		first.Header().Get("Referrer-Policy") != "same-origin" {
		t.Fatalf("security headers missing: %+v", first.Header())
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
	if page := a.get("/setup").Body.String(); strings.Contains(page, `id="secret"`) || strings.Contains(page, "Add a server") {
		t.Fatal("the secret or Add a server shows before a public URL is set")
	}
	if err := a.store.SetPublicURL("https://music.example.com"); err != nil {
		t.Fatal(err)
	}
	if page := a.get("/setup").Body.String(); !strings.Contains(page, `id="secret"`) || !strings.Contains(page, "No servers yet") {
		t.Fatalf("overview with a public URL: %s", page)
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
	a.headers.Set("Origin", "https://evil.example")
	if rec := a.form("/setup/public-url", url.Values{"public_url": {"https://music.example.com"}}); rec.Code != http.StatusSeeOther {
		t.Errorf("sec-fetch-site must win over origin, proxies may rewrite the host: %d", rec.Code)
	}
	a.headers.Del("Sec-Fetch-Site")
	if rec := a.form("/setup/public-url", url.Values{"public_url": {"https://music.example.com"}}); rec.Code != http.StatusForbidden {
		t.Errorf("foreign origin without sec-fetch-site: %d", rec.Code)
	}
	a.headers.Set("Origin", "http://example.com")
	if rec := a.form("/setup/public-url", url.Values{"public_url": {"https://music.example.com"}}); rec.Code != http.StatusSeeOther {
		t.Errorf("matching origin without sec-fetch-site: %d", rec.Code)
	}
	a.headers.Del("Origin")
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
	if rec := a.get("/setup"); !strings.Contains(rec.Body.String(), "Server URLs appear once it is set") {
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
	if rec := a.form("/setup/public-url", url.Values{"public_url": {""}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("clear public url: %d", rec.Code)
	}
	if got := a.store.Snapshot().PublicURL; got != "" {
		t.Fatalf("public url after clearing = %q", got)
	}
	if page = a.get("/setup").Body.String(); !strings.Contains(page, "Server URLs appear once it is set") {
		t.Fatal("missing public url banner after clearing")
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
	if !strings.Contains(a.get("/setup").Body.String(), "Sync now") {
		t.Fatal("overview lacks Sync now button")
	}
	if !strings.Contains(a.get("/setup").Body.String(), "Sync Webhook") {
		t.Fatal("overview lacks Sync Webhook")
	}
	if !strings.Contains(a.get("/setup").Body.String(), "••••••••••/sync") {
		t.Fatal("overview lacks masked sync webhook URL")
	}
	if !strings.Contains(a.get("/setup").Body.String(), "Default (1h)") {
		t.Fatal("overview lacks default sync interval value")
	}
	if !strings.Contains(a.get("/setup").Body.String(), "Search tester") {
		t.Fatal("overview lacks Search tester")
	}
	body := a.get("/setup").Body.String()
	if !strings.Contains(body, "Webhook helper &amp; curl command") || !strings.Contains(body, `curl -X POST &quot;https://music.example.com/plex-1/`) {
		t.Fatal("overview lacks webhook helper or curl command")
	}
	if !strings.Contains(body, `curl -X POST "https://music.example.com/plex-1/••••••••••/sync"`) {
		t.Fatal("curl command did not mask secret")
	}
	if !strings.Contains(body, "Recent Activity") || !strings.Contains(body, "Synced 5 tracks") {
		t.Fatalf("overview lacks recent activity: %s", body)
	}
	if actRec := a.get("/setup/activity"); actRec.Code != http.StatusOK || !strings.Contains(actRec.Body.String(), `"ok":true`) || !strings.Contains(actRec.Body.String(), "Synced 5 tracks") {
		t.Fatalf("get activity endpoint failed: %d %s", actRec.Code, actRec.Body.String())
	}
	if searchRec := a.get("/setup/servers/plex-1/test-search?q=First+Song"); searchRec.Code != http.StatusOK || !strings.Contains(searchRec.Body.String(), `"ok":true`) {
		t.Fatalf("test-search failed: %d %s", searchRec.Code, searchRec.Body.String())
	}
	if searchRec := a.get("/setup/servers/plex-1/search?q=First+Song"); searchRec.Code != http.StatusOK || !strings.Contains(searchRec.Body.String(), `"albums"`) {
		t.Fatalf("search endpoint failed: %d %s", searchRec.Code, searchRec.Body.String())
	}
	// Searching an album name ("As You Found Me") returns both the album and its tracks.
	albumSearchRec := a.get("/setup/servers/plex-1/test-search?q=As+You+Found+Me")
	if albumSearchRec.Code != http.StatusOK || !strings.Contains(albumSearchRec.Body.String(), `"name":"As You Found Me"`) || !strings.Contains(albumSearchRec.Body.String(), `"title":"Closer"`) {
		t.Fatalf("test-search album query failed: %d %s", albumSearchRec.Code, albumSearchRec.Body.String())
	}
	if searchRec := a.get("/setup/servers/plex-9/test-search?q=First+Song"); searchRec.Code != http.StatusNotFound {
		t.Fatalf("test-search unknown server: %d", searchRec.Code)
	}
	if rec := a.form("/setup/servers/plex-1/refresh", url.Values{}); rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "synced") {
		t.Fatalf("refresh: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if !strings.Contains(a.get("/setup").Body.String(), "Online") {
		t.Fatal("overview lacks online status indicator")
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
	rec := a.form("/setup/servers/plex-1", url.Values{"label": {"Parents"}, "url": {fake.URL}, "library": {""}, "enabled": {"1"}, "sync_interval": {"30m"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("edit: %d %s", rec.Code, rec.Body.String())
	}
	server, _ = a.store.Snapshot().Server(slug)
	if server.Label != "Parents" || server.Token != fakes.PlexToken || server.Library != "" || server.SyncInterval != "30m" {
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
	if strings.Contains(a.get("/setup").Body.String(), `action="/setup/servers/plex-1/refresh"`) {
		t.Fatal("disabled server still has Sync now button")
	}
	if rec := a.form("/setup/servers/plex-1/refresh", url.Values{}); rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "Sync+failed") {
		t.Fatalf("refresh disabled server: %d %s", rec.Code, rec.Header().Get("Location"))
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
	if rec := a.form("/setup/servers/plex-9/refresh", url.Values{}); rec.Code != http.StatusNotFound {
		t.Fatalf("refresh unknown: %d", rec.Code)
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

func TestASignInForTheOtherKindIsRefusedByName(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	fake := fakes.NewJellyfin(t)
	ref := a.jellyfinSignIn(fake)
	rec, answer := a.json(http.MethodPost, "/setup/servers/test", testRequest{Kind: store.Plex, URL: fake.URL, TokenRef: ref})
	if rec.Code != http.StatusBadRequest || !strings.Contains(answer["error"].(string), "belongs to Jellyfin, not Plex") {
		t.Fatalf("test with the wrong kind: %d %v", rec.Code, answer)
	}
	if rec := a.form("/setup/servers", url.Values{"kind": {"plex"}, "url": {fake.URL}, "token_ref": {ref}, "enabled": {"1"}}); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "belongs to Jellyfin, not Plex") {
		t.Fatalf("save with the wrong kind: %d", rec.Code)
	}
	if rec, answer := a.json(http.MethodPost, "/setup/servers/test", testRequest{Kind: store.Jellyfin, URL: fake.URL, TokenRef: ref}); rec.Code != http.StatusOK {
		t.Fatalf("the mismatch consumed the sign-in: %d %v", rec.Code, answer)
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

func TestNavidromeSignInStoresTheTokenAndNeverThePassword(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	fake := fakes.NewNavidrome(t)
	rec, answer := a.json(http.MethodPost, "/setup/navidrome/signin", navidromeSignInRequest{URL: fake.URL, Username: fakes.NavidromeUser, Password: fakes.NavidromePassword})
	if rec.Code != http.StatusOK || answer["ok"] != true || answer["username"] != fakes.NavidromeUser || answer["ref"] == "" {
		t.Fatalf("sign-in: %d %v", rec.Code, answer)
	}
	if strings.Contains(rec.Body.String(), fakes.NavidromePassword) {
		t.Fatal("the password reached the browser")
	}
	ref := answer["ref"].(string)
	if rec := a.form("/setup/servers", url.Values{"kind": {"navidrome"}, "label": {"NAS"}, "url": {fake.URL}, "token_ref": {ref}, "library": {"Music"}, "enabled": {"1"}, "scrobble_enabled": {"1"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	server, _ := a.store.Snapshot().Server("navidrome-1")
	if server.Token != fakes.NavidromeToken || server.Auth != store.AuthNavidromeSignIn || server.Account != fakes.NavidromeUser || server.Library != fakes.NavidromeMusicFolder || server.ScrobblePercent != 50 {
		t.Fatalf("stored = %+v", server)
	}
	if overviewBody := a.get("/setup").Body.String(); !strings.Contains(overviewBody, "<dt>Scrobbling</dt><dd>Enabled (at 50%)</dd>") {
		t.Fatalf("overview lacks scrobble info: %s", overviewBody)
	}
	moved := a.form("/setup/servers/navidrome-1", url.Values{"label": {"NAS"}, "url": {fakes.NewNavidrome(t).URL}, "enabled": {"1"}})
	if moved.Code != http.StatusBadRequest || !strings.Contains(moved.Body.String(), "sign in again") {
		t.Fatalf("url change on a signed-in server: %d", moved.Code)
	}
	for i := range 5 {
		rec, _ := a.json(http.MethodPost, "/setup/navidrome/signin", navidromeSignInRequest{URL: fake.URL, Username: fakes.NavidromeUser, Password: "nope"})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, rec.Code)
		}
	}
	if rec, _ := a.json(http.MethodPost, "/setup/navidrome/signin", navidromeSignInRequest{URL: fake.URL, Username: fakes.NavidromeUser, Password: fakes.NavidromePassword}); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after five failures: %d", rec.Code)
	}
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

func TestDeletingASignedInJellyfinServerSignsItOut(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	fake := fakes.NewJellyfin(t)
	if rec := a.form("/setup/servers", url.Values{"kind": {"jellyfin"}, "url": {fake.URL}, "token_ref": {a.jellyfinSignIn(fake)}, "enabled": {"1"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
	}
	original, _ := a.store.Snapshot().Server("jellyfin-1")
	if rec := a.form("/setup/servers/jellyfin-1/delete", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete: %d", rec.Code)
	}
	a.setup.app.background.Wait()
	if !loggedOut(fake, original.Token) {
		t.Fatalf("logouts = %+v", logouts(fake))
	}
	if strings.Contains(a.logs.String(), fakes.JellyfinSessionPrefix) {
		t.Fatal("a token reached the logs")
	}
}

func TestDeletingAPastedJellyfinKeySignsNothingOut(t *testing.T) {
	a := newTestApp(t)
	a.signIn()
	fake := fakes.NewJellyfin(t)
	if rec := a.form("/setup/servers", url.Values{"kind": {"jellyfin"}, "url": {fake.URL}, "token": {fakes.JellyfinToken}, "enabled": {"1"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
	}
	if rec := a.form("/setup/servers/jellyfin-1/delete", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete: %d", rec.Code)
	}
	a.setup.app.background.Wait()
	if len(logouts(fake)) != 0 {
		t.Fatal("the pasted API key was signed out")
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

func TestOverHTTPSReadsTheFirstForwardedProto(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/setup", nil)
	r.Header.Set("X-Forwarded-Proto", "https, http")
	if !overHTTPS(r) {
		t.Error("an https first hop was not recognised")
	}
	r.Header.Set("X-Forwarded-Proto", "http, https")
	if overHTTPS(r) {
		t.Error("an http first hop was treated as https")
	}
}

func TestTheFirstPasswordSavesThePublicURLFromTheRequest(t *testing.T) {
	cases := map[string]struct {
		target, proto, want string
	}{
		"https":                 {"https://music.example.com/setup/password", "", "https://music.example.com"},
		"behind a proxy":        {"http://music.example.com/setup/password", "https", "https://music.example.com"},
		"a port is kept":        {"https://music.example.com:8443/setup/password", "", "https://music.example.com:8443"},
		"plain http":            {"http://music.example.com/setup/password", "", ""},
		"localhost":             {"https://localhost/setup/password", "", ""},
		"an ip address":         {"https://192.168.1.10/setup/password", "", ""},
		"an ipv6 address":       {"https://[::1]:8080/setup/password", "", ""},
		"a docker service name": {"http://bitchord-selfhosted-addon:8080/setup/password", "https", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			a := newTestApp(t)
			if tc.proto != "" {
				a.headers.Set("X-Forwarded-Proto", tc.proto)
			}
			if rec := a.form(tc.target, url.Values{"password": {password}}); rec.Code != http.StatusSeeOther {
				t.Fatalf("create password: %d %s", rec.Code, rec.Body.String())
			}
			if got := a.store.Snapshot().PublicURL; got != tc.want {
				t.Fatalf("public url = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTheFirstPasswordKeepsAPublicURLAlreadySet(t *testing.T) {
	a := newTestApp(t)
	if err := a.store.SetPublicURL("https://other.example.com"); err != nil {
		t.Fatal(err)
	}
	a.form("https://music.example.com/setup/password", url.Values{"password": {password}})
	if got := a.store.Snapshot().PublicURL; got != "https://other.example.com" {
		t.Fatalf("public url = %q", got)
	}
}

func TestStaticAssetsCachingAndCompression(t *testing.T) {
	a := newTestApp(t)

	// Normal GET request
	rec := a.get("/setup/static/style.css")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /setup/static/style.css = %d, want 200", rec.Code)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Error("expected ETag header on static asset")
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age") {
		t.Errorf("Cache-Control = %q, want max-age", cc)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/css") {
		t.Errorf("Content-Type = %q, want text/css", ct)
	}

	// Conditional request with matching If-None-Match returns 304
	a.headers.Set("If-None-Match", etag)
	rec304 := a.get("/setup/static/style.css")
	a.headers.Del("If-None-Match")
	if rec304.Code != http.StatusNotModified {
		t.Errorf("GET with matching If-None-Match = %d, want 304", rec304.Code)
	}
	if rec304.Body.Len() > 0 {
		t.Errorf("304 response should have empty body, got %d bytes", rec304.Body.Len())
	}

	// Gzip compressed request
	a.headers.Set("Accept-Encoding", "gzip")
	recGz := a.get("/setup/static/style.css")
	a.headers.Del("Accept-Encoding")
	if recGz.Code != http.StatusOK {
		t.Fatalf("GET with gzip = %d, want 200", recGz.Code)
	}
	if enc := recGz.Header().Get("Content-Encoding"); enc != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", enc)
	}

	// HEAD request
	recHead := a.do(http.MethodHead, "/setup/static/style.css", nil, "")
	if recHead.Code != http.StatusOK {
		t.Errorf("HEAD /setup/static/style.css = %d, want 200", recHead.Code)
	}
	if recHead.Body.Len() > 0 {
		t.Errorf("HEAD body should be empty, got %d bytes", recHead.Body.Len())
	}
	if recHead.Header().Get("Content-Length") == "" {
		t.Error("HEAD expected Content-Length header")
	}

	// Non-existent static asset returns 404
	rec404 := a.get("/setup/static/nonexistent.css")
	if rec404.Code != http.StatusNotFound {
		t.Errorf("GET non-existent asset = %d, want 404", rec404.Code)
	}
}

func TestBackupExport(t *testing.T) {
	a := newTestApp(t)
	a.signIn()

	if err := a.store.SetPublicURL("https://music.example.com"); err != nil {
		t.Fatal(err)
	}
	fake := fakes.NewPlex(t)
	a.addPlex(fake, "Home Plex")

	// Calling backup without password returns Bad Request
	recBad := a.get("/setup/backup")
	if recBad.Code != http.StatusBadRequest {
		t.Fatalf("GET /setup/backup without password = %d, want 400", recBad.Code)
	}

	rec := a.get("/setup/backup?passphrase=backup-test-pass")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /setup/backup = %d, want 200", rec.Code)
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "attachment") || !strings.Contains(cd, "bitchord-addon-backup-encrypted") {
		t.Errorf("Content-Disposition = %q, want attachment with filename", cd)
	}

	snap, err := store.DecryptSnapshot(rec.Body.Bytes(), "backup-test-pass")
	if err != nil {
		t.Fatalf("failed to decrypt backup payload: %v", err)
	}
	if snap.PublicURL != "https://music.example.com" {
		t.Errorf("backup PublicURL = %q, want https://music.example.com", snap.PublicURL)
	}
	if len(snap.Servers) != 1 || snap.Servers[0].Label != "Home Plex" {
		t.Errorf("backup Servers = %+v, want 1 server", snap.Servers)
	}
	if snap.Admin != nil {
		t.Error("backup must omit Admin credentials to prevent hash leakage")
	}
}

func TestBackupRestore(t *testing.T) {
	a := newTestApp(t)
	a.signIn()

	adminBefore := a.store.Snapshot().Admin
	if adminBefore == nil {
		t.Fatal("expected admin credentials after signIn")
	}

	backupData := store.Snapshot{
		PublicURL: "https://restored-music.example.com",
		Admin: &store.Admin{
			PasswordHash: "forged_hash",
			SessionKey:   "forged_session",
		},
		Servers: []store.Server{
			{
				Slug:    "plex-1",
				Kind:    store.Plex,
				Label:   "Restored Plex",
				URL:     "http://localhost:32400",
				Token:   "tok",
				Library: "1",
				Auth:    store.AuthToken,
				Enabled: true,
			},
		},
	}
	raw, _ := json.Marshal(backupData)

	rec := a.multipartForm("/setup/restore", "backup_file", "backup.json", raw)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /setup/restore = %d, want 303: %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "notice=") {
		t.Errorf("redirect location = %q, want notice param", loc)
	}

	snap := a.store.Snapshot()
	if snap.PublicURL != "https://restored-music.example.com" {
		t.Errorf("PublicURL after restore = %q, want https://restored-music.example.com", snap.PublicURL)
	}
	if len(snap.Servers) != 1 || snap.Servers[0].Label != "Restored Plex" {
		t.Errorf("Servers after restore = %+v, want 1 server", snap.Servers)
	}
	if snap.Admin == nil || snap.Admin.PasswordHash != adminBefore.PasswordHash {
		t.Error("Admin credentials must be preserved after restore")
	}

	// Restore invalid JSON returns notice and leaves store intact
	badRec := a.multipartForm("/setup/restore", "backup_file", "backup.json", []byte("invalid json"))
	if badRec.Code != http.StatusSeeOther {
		t.Fatalf("POST invalid json = %d, want 303", badRec.Code)
	}
	if snap := a.store.Snapshot(); snap.PublicURL != "https://restored-music.example.com" {
		t.Errorf("PublicURL mutated after bad restore: %q", snap.PublicURL)
	}
}

func TestEncryptedBackupExportAndRestore(t *testing.T) {
	a := newTestApp(t)
	a.signIn()

	if err := a.store.SetPublicURL("https://secure.example.com"); err != nil {
		t.Fatal(err)
	}
	fake := fakes.NewPlex(t)
	a.addPlex(fake, "Secure Plex")

	// 1. Export with passphrase via POST /setup/backup
	rec := a.form("/setup/backup", url.Values{"passphrase": {"my-backup-password-123"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /setup/backup = %d, want 200", rec.Code)
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "bitchord-addon-backup-encrypted") {
		t.Errorf("Content-Disposition = %q, want encrypted filename", cd)
	}
	encBody := rec.Body.Bytes()
	if !store.IsEncryptedBackup(encBody) {
		t.Fatal("exported body is not marked as encrypted backup")
	}
	if strings.Contains(string(encBody), "Secure Plex") {
		t.Fatal("plaintext server label found in encrypted backup body")
	}

	// 2. Attempt restore without passphrase -> fails with notice
	recFailNoPass := a.multipartForm("/setup/restore", "backup_file", "backup.json", encBody)
	if recFailNoPass.Code != http.StatusSeeOther {
		t.Fatalf("POST restore without pass = %d, want 303", recFailNoPass.Code)
	}
	locNoPass := recFailNoPass.Header().Get("Location")
	if !strings.Contains(locNoPass, "password") {
		t.Errorf("expected password required notice, got location: %s", locNoPass)
	}

	// 3. Attempt restore with wrong passphrase -> fails with notice
	recFailWrongPass := a.multipartFormWithFields("/setup/restore", "backup_file", "backup.json", encBody, map[string]string{
		"passphrase": "wrong-password",
	})
	if recFailWrongPass.Code != http.StatusSeeOther {
		t.Fatalf("POST restore with wrong pass = %d, want 303", recFailWrongPass.Code)
	}
	locWrong := recFailWrongPass.Header().Get("Location")
	if !strings.Contains(locWrong, "incorrect") {
		t.Errorf("expected incorrect password notice, got location: %s", locWrong)
	}

	// Change existing state so we can verify restoration
	if err := a.store.SetPublicURL("https://mutated.example.com"); err != nil {
		t.Fatal(err)
	}

	// 4. Restore with correct passphrase -> succeeds
	recSuccess := a.multipartFormWithFields("/setup/restore", "backup_file", "backup.json", encBody, map[string]string{
		"passphrase": "my-backup-password-123",
	})
	if recSuccess.Code != http.StatusSeeOther {
		t.Fatalf("POST restore with correct pass = %d, want 303", recSuccess.Code)
	}
	locSuccess := recSuccess.Header().Get("Location")
	if !strings.Contains(locSuccess, "restored") {
		t.Errorf("expected success notice, got location: %s", locSuccess)
	}

	snap := a.store.Snapshot()
	if snap.PublicURL != "https://secure.example.com" {
		t.Errorf("restored PublicURL = %q, want https://secure.example.com", snap.PublicURL)
	}
	if len(snap.Servers) != 1 || snap.Servers[0].Label != "Secure Plex" {
		t.Errorf("restored Servers = %+v, want 1 server", snap.Servers)
	}
}

func TestRefreshAllServers(t *testing.T) {
	a := newTestApp(t)
	a.signIn()

	// Sync all with no servers returns notice
	recEmpty := a.form("/setup/sync-all", url.Values{})
	if recEmpty.Code != http.StatusSeeOther || !strings.Contains(recEmpty.Header().Get("Location"), "No+enabled+servers") {
		t.Fatalf("sync-all empty: %d, location: %s", recEmpty.Code, recEmpty.Header().Get("Location"))
	}

	fake1 := fakes.NewPlex(t)
	a.addPlex(fake1, "Server 1")
	fake2 := fakes.NewPlex(t)
	a.addPlex(fake2, "Server 2")

	rec := a.form("/setup/sync-all", url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /setup/sync-all = %d, want 303", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "synced+successfully") {
		t.Errorf("expected success notice in location %q", loc)
	}
}

func TestRecentActivityEndpoint(t *testing.T) {
	a := newTestApp(t)
	a.signIn()

	a.reg.RecordActivity("plex-1", "Plex Server", "sync", "Synced 100 tracks")
	a.reg.RecordActivity("sub-1", "Navidrome", "now_playing", "Now playing: Track A")

	rec := a.get("/setup/activity")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /setup/activity = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Synced 100 tracks") || !strings.Contains(body, "Now playing: Track A") {
		t.Fatalf("expected both activities in /setup/activity response: %s", body)
	}

	recSync := a.get("/setup/activity?filter=sync")
	if recSync.Code != http.StatusOK {
		t.Fatalf("GET /setup/activity?filter=sync = %d", recSync.Code)
	}
	bodySync := recSync.Body.String()
	if !strings.Contains(bodySync, "Synced 100 tracks") || strings.Contains(bodySync, "Now playing: Track A") {
		t.Fatalf("expected only sync in filtered response: %s", bodySync)
	}

	recPlayback := a.get("/setup/activity?filter=playback")
	if recPlayback.Code != http.StatusOK {
		t.Fatalf("GET /setup/activity?filter=playback = %d", recPlayback.Code)
	}
	bodyPlayback := recPlayback.Body.String()
	if strings.Contains(bodyPlayback, "Synced 100 tracks") || !strings.Contains(bodyPlayback, "Now playing: Track A") {
		t.Fatalf("expected only playback in filtered response: %s", bodyPlayback)
	}

	// POST /setup/activity/clear with filter=sync
	clearSyncRec := a.form("/setup/activity/clear", url.Values{"filter": {"sync"}})
	if clearSyncRec.Code != http.StatusSeeOther {
		t.Fatalf("POST /setup/activity/clear = %d, want 303", clearSyncRec.Code)
	}
	if len(a.reg.RecentSyncActivity()) != 0 {
		t.Fatalf("expected sync activities cleared, got %d", len(a.reg.RecentSyncActivity()))
	}
	if len(a.reg.RecentPlaybackActivity()) != 1 {
		t.Fatalf("expected playback activities untouched, got %d", len(a.reg.RecentPlaybackActivity()))
	}

	// POST /setup/activity/clear with filter=all
	clearAllRec := a.form("/setup/activity/clear", url.Values{"filter": {"all"}})
	if clearAllRec.Code != http.StatusSeeOther {
		t.Fatalf("POST /setup/activity/clear = %d, want 303", clearAllRec.Code)
	}
	if len(a.reg.RecentActivity()) != 0 {
		t.Fatalf("expected all activities cleared, got %d", len(a.reg.RecentActivity()))
	}
}
