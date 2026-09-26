package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/config"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/fakes"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/store"
)

const testSecret = "abcdefghijklmnop"

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

func testConfig() config.Config {
	return config.Config{RefreshInterval: time.Hour, Port: 0, LogLevel: slog.LevelInfo, LogFormat: "text"}
}

// newStore opens a store in a temp dir with the given servers, a known
// secret and a public URL, the way the setup page would have left it.
func newStore(t *testing.T, servers ...store.Server) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	err = st.Update(func(s *store.Snapshot) error {
		s.PublicURL, s.Secret = "https://music.example.com", testSecret
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, server := range servers {
		if _, err := st.AddServer(server); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func plexServer(fake *fakes.Plex) store.Server {
	return store.Server{Kind: store.Plex, Label: "Home", URL: fake.URL, Token: fakes.PlexToken, Auth: store.AuthToken, Library: "Music", Enabled: true}
}

func jellyfinServer(fake *fakes.Jellyfin) store.Server {
	return store.Server{Kind: store.Jellyfin, URL: fake.URL, Token: fakes.JellyfinToken, Auth: store.AuthToken, Enabled: true}
}

// start runs serve in the background and returns the base URL once it listens.
func start(ctx context.Context, t *testing.T, cfg config.Config, st *store.Store, log *slog.Logger) (string, <-chan error) {
	t.Helper()
	addrs := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, st, log, func(addr net.Addr) { addrs <- addr }) }()
	select {
	case addr := <-addrs:
		return "http://127.0.0.1:" + strconv.Itoa(addr.(*net.TCPAddr).Port), done
	case err := <-done:
		t.Fatalf("serve returned early: %v", err)
		return "", done
	}
}

func waitHealthy(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for healthcheck(base+"/health") != 0 {
		if time.Now().After(deadline) {
			t.Fatal("never became healthy")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	return res.StatusCode, string(body)
}

func TestHealthcheckExitCodes(t *testing.T) {
	status := http.StatusOK
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
	if got := healthcheck(probe.URL + "/health"); got != 0 {
		t.Errorf("200 gave exit code %d", got)
	}
	status = http.StatusServiceUnavailable
	if got := healthcheck(probe.URL + "/health"); got != 1 {
		t.Errorf("503 gave exit code %d", got)
	}
	probe.Close()
	if got := healthcheck(probe.URL + "/health"); got != 1 {
		t.Errorf("a dead server gave exit code %d", got)
	}
}

func TestRunReportsABadProcessSetting(t *testing.T) {
	var stderr bytes.Buffer
	code := run(nil, func(key string) string { return map[string]string{"PORT": "x", "LOG_LEVEL": "loud"}[key] }, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "PORT") || !strings.Contains(stderr.String(), "LOG_LEVEL") {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
}

func TestRemovedVariablesAreWarnedAboutByName(t *testing.T) {
	var logs bytes.Buffer
	log := newLogger("text", slog.LevelInfo, &logs)
	warnRemoved(log, config.Removed(func(key string) string {
		return map[string]string{"PLEX_URL": "http://plex:32400", "ADDON_SECRET": "s3cr3t-s3cr3t-s3cr3t"}[key]
	}))
	for _, want := range []string{"level=WARN", "PLEX_URL is no longer read", "ADDON_SECRET is no longer read", "setup page"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs lack %s:\n%s", want, logs.String())
		}
	}
	if strings.Contains(logs.String(), "s3cr3t") {
		t.Error("a removed variable's value was logged")
	}
}

func TestAnEmptyStoreServesTheSetupPageAndIsHealthy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logs := &syncBuffer{}
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base, done := start(ctx, t, testConfig(), st, newLogger("text", slog.LevelInfo, logs))
	waitHealthy(t, base)
	if status, body := get(t, base+"/setup"); status != http.StatusOK || !strings.Contains(body, "Create a password") {
		t.Fatalf("setup page: %d %s", status, body)
	}
	if status, _ := get(t, base+"/setup/static/style.css"); status != http.StatusOK {
		t.Fatalf("stylesheet: %d", status)
	}
	if status, body := get(t, base+"/plex-1/"+st.Snapshot().Secret+"/manifest.json"); status != http.StatusNotFound || body != "" {
		t.Fatalf("unknown server: %d %q", status, body)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"msg=starting", "servers=0", "has no password yet"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs lack %s:\n%s", want, logs.String())
		}
	}
}

func TestServeAnswersEveryServerAndShutsDownOnCancel(t *testing.T) {
	plexFake, jellyfinFake := fakes.NewPlex(t), fakes.NewJellyfin(t)
	st := newStore(t, plexServer(plexFake), jellyfinServer(jellyfinFake))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logs := &syncBuffer{}
	base, done := start(ctx, t, testConfig(), st, newLogger("text", slog.LevelInfo, logs))
	waitHealthy(t, base)
	cases := map[string]string{"plex-1": `"id":"101"`, "jellyfin-1": `"id":"f101"`}
	for slug, wantID := range cases {
		if status, body := get(t, base+"/"+slug+"/"+testSecret+"/search?q=new+religion"); status != http.StatusOK || !strings.Contains(body, wantID) {
			t.Fatalf("%s search: %d %s", slug, status, body)
		}
		if _, body := get(t, base+"/"+slug+"/"+testSecret+"/manifest.json"); !strings.Contains(body, `"id":"app.bitchord-selfhosted-addon.`+slug+`"`) {
			t.Fatalf("%s manifest = %s", slug, body)
		}
	}
	if _, body := get(t, base+"/plex-1/"+testSecret+"/manifest.json"); !strings.Contains(body, `"name":"Plex - Home"`) {
		t.Fatalf("plex manifest name: %s", body)
	}
	if status, _ := get(t, base+"/setup"); status != http.StatusOK {
		t.Fatalf("setup page alongside the servers: %d", status)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after cancel")
	}
	for _, want := range []string{"msg=listening", "msg=\"library indexed\"", "server=plex-1", "server=jellyfin-1", "servers=2"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs lack %s:\n%s", want, logs.String())
		}
	}
	if strings.Contains(logs.String(), fakes.PlexToken) || strings.Contains(logs.String(), testSecret) {
		t.Error("a credential reached the logs")
	}
}

func TestServeStopsTheRefreshLoopBeforeShuttingDown(t *testing.T) {
	fake := fakes.NewPlex(t)
	sectionsBlocked := make(chan struct{})
	refreshCancelledAt := make(chan time.Time, 1)
	fake.Extra["/library/sections"] = func(w http.ResponseWriter, r *http.Request) {
		close(sectionsBlocked)
		<-r.Context().Done()
		refreshCancelledAt <- time.Now()
	}
	streamStarted := make(chan struct{})
	fake.Extra["/library/parts/102/1/file.mp3"] = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		close(streamStarted)
		for range 5 {
			w.Write([]byte("slow!"))
			w.(http.Flusher).Flush()
			time.Sleep(50 * time.Millisecond)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base, done := start(ctx, t, testConfig(), newStore(t, plexServer(fake)), slog.New(slog.NewTextHandler(io.Discard, nil)))

	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		res, err := http.Get(base + "/plex-1/" + testSecret + "/file/102")
		if err != nil {
			return
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}()

	select {
	case <-sectionsBlocked:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh never reached plex")
	}
	select {
	case <-streamStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("stream never started")
	}

	cancelledAt := time.Now()
	cancel()

	var refreshCancelled time.Time
	select {
	case refreshCancelled = <-refreshCancelledAt:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh loop never stopped")
	}
	if delay := refreshCancelled.Sub(cancelledAt); delay > 100*time.Millisecond {
		t.Fatalf("refresh loop stopped %s after cancel, want it stopped before shutdown began", delay)
	}

	select {
	case <-streamDone:
	case <-time.After(5 * time.Second):
		t.Fatal("stream never finished")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after cancel")
	}
}

func TestServeStopsTheRefreshLoopWhenThePortIsTaken(t *testing.T) {
	fake := fakes.NewPlex(t)
	taken, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	cfg := testConfig()
	cfg.Port = taken.Addr().(*net.TCPAddr).Port
	done := make(chan error, 1)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() {
		done <- serve(context.Background(), cfg, newStore(t, plexServer(fake)), log, func(net.Addr) {})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("serve did not error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return")
	}
}

func TestNewLoggerFormats(t *testing.T) {
	var text, structured bytes.Buffer
	newLogger("text", slog.LevelInfo, &text).Info("hello", "n", 1)
	newLogger("json", slog.LevelInfo, &structured).Info("hello", "n", 1)
	if got := text.String(); !strings.Contains(got, "msg=hello") || !strings.Contains(got, "n=1") {
		t.Errorf("text = %s", got)
	}
	if got := structured.String(); !strings.Contains(got, `"msg":"hello"`) {
		t.Errorf("json = %s", got)
	}
	newLogger("text", slog.LevelWarn, &text).Info("quiet")
	if strings.Contains(text.String(), "quiet") {
		t.Error("level not applied")
	}
}
