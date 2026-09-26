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
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/jellyfintest"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/plextest"
)

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

func plexConfig(fake *plextest.Fake) config.Config {
	return config.Config{
		Backend: config.Plex, ServerURL: fake.URL, ServerToken: plextest.Token, Secret: "abcdefghijklmnop",
		PublicURL: "https://music.example.com", RefreshInterval: time.Hour, Port: 0,
	}
}

func jellyfinConfig(fake *jellyfintest.Fake) config.Config {
	return config.Config{
		Backend: config.Jellyfin, ServerURL: fake.URL, ServerToken: jellyfintest.Token, Secret: "abcdefghijklmnop",
		PublicURL: "https://music.example.com", RefreshInterval: time.Hour, Port: 0,
	}
}

// start runs serve in the background and returns the base URL once it listens.
func start(ctx context.Context, t *testing.T, cfg config.Config, log *slog.Logger) (string, <-chan error) {
	t.Helper()
	addrs := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, log, func(addr net.Addr) { addrs <- addr }) }()
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

func TestRunReportsEveryConfigurationProblem(t *testing.T) {
	var stderr bytes.Buffer
	code := run(nil, func(string) string { return "" }, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d", code)
	}
	for _, key := range []string{"PLEX_URL", "PLEX_TOKEN", "JELLYFIN_URL", "JELLYFIN_API_KEY", "ADDON_SECRET", "PUBLIC_URL"} {
		if !strings.Contains(stderr.String(), key) {
			t.Errorf("stderr lacks %s: %s", key, stderr.String())
		}
	}
}

func TestServeAnswersOverHTTPAndShutsDownOnCancel(t *testing.T) {
	cases := map[string]struct {
		cfg    config.Config
		wantID string
	}{
		"plex":     {plexConfig(plextest.New(t)), `"id":"101"`},
		"jellyfin": {jellyfinConfig(jellyfintest.New(t)), `"id":"f101"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			logs := &syncBuffer{}
			log := newLogger("text", slog.LevelInfo, logs).With("source", string(tc.cfg.Backend))
			base, done := start(ctx, t, tc.cfg, log)
			waitHealthy(t, base)
			res, err := http.Get(base + "/abcdefghijklmnop/search?q=new+religion")
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode != http.StatusOK || !strings.Contains(string(body), tc.wantID) {
				t.Fatalf("status %d, body %s", res.StatusCode, body)
			}
			manifest, err := http.Get(base + "/abcdefghijklmnop/manifest.json")
			if err != nil {
				t.Fatal(err)
			}
			body, _ = io.ReadAll(manifest.Body)
			manifest.Body.Close()
			if !strings.Contains(string(body), `"id":"app.bitchord-selfhosted-addon.`+name+`"`) {
				t.Fatalf("manifest = %s", body)
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
			for _, want := range []string{"msg=listening", "msg=\"library indexed\"", "source=" + name} {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("logs lack %s:\n%s", want, logs.String())
				}
			}
		})
	}
}

func TestServeStopsTheRefreshLoopBeforeShuttingDown(t *testing.T) {
	fake := plextest.New(t)
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
	base, done := start(ctx, t, plexConfig(fake), slog.New(slog.NewTextHandler(io.Discard, nil)))

	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		res, err := http.Get(base + "/abcdefghijklmnop/file/102")
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
	fake := plextest.New(t)
	taken, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	port := taken.Addr().(*net.TCPAddr).Port

	cfg := plexConfig(fake)
	cfg.Port = port
	done := make(chan error, 1)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() { done <- serve(context.Background(), cfg, log, func(net.Addr) {}) }()

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
