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
	"testing"
	"time"

	"github.com/rairulyle/eclipse-plex-addon/internal/config"
	"github.com/rairulyle/eclipse-plex-addon/internal/plextest"
)

func TestHealthcheckExitCodes(t *testing.T) {
	status := http.StatusOK
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
	if got := healthcheck(probe.URL + "/healthz"); got != 0 {
		t.Errorf("200 gave exit code %d", got)
	}
	status = http.StatusServiceUnavailable
	if got := healthcheck(probe.URL + "/healthz"); got != 1 {
		t.Errorf("503 gave exit code %d", got)
	}
	probe.Close()
	if got := healthcheck(probe.URL + "/healthz"); got != 1 {
		t.Errorf("a dead server gave exit code %d", got)
	}
}

func TestRunReportsEveryConfigurationProblem(t *testing.T) {
	var stderr bytes.Buffer
	code := run(nil, func(string) string { return "" }, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d", code)
	}
	for _, key := range []string{"PLEX_URL", "PLEX_TOKEN", "ADDON_SECRET", "PUBLIC_URL"} {
		if !strings.Contains(stderr.String(), key) {
			t.Errorf("stderr lacks %s: %s", key, stderr.String())
		}
	}
}

func TestServeAnswersOverHTTPAndShutsDownOnCancel(t *testing.T) {
	fake := plextest.New(t)
	cfg := config.Config{
		PlexURL: fake.URL, PlexToken: plextest.Token, Secret: "abcdefghijklmnop",
		PublicURL: "https://music.example.com", AddonName: "Plex", RefreshInterval: time.Hour, Port: 0,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addrs := make(chan net.Addr, 1)
	done := make(chan error, 1)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() { done <- serve(ctx, cfg, log, func(addr net.Addr) { addrs <- addr }) }()

	var base string
	select {
	case addr := <-addrs:
		base = "http://127.0.0.1:" + strconv.Itoa(addr.(*net.TCPAddr).Port)
	case err := <-done:
		t.Fatalf("serve returned early: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for healthcheck(base+"/healthz") != 0 {
		if time.Now().After(deadline) {
			t.Fatal("never became healthy")
		}
		time.Sleep(10 * time.Millisecond)
	}
	res, err := http.Get(base + "/abcdefghijklmnop/search?q=paniyon+sa")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), `"id":"101"`) {
		t.Fatalf("status %d, body %s", res.StatusCode, body)
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

	cfg := config.Config{
		PlexURL: fake.URL, PlexToken: plextest.Token, Secret: "abcdefghijklmnop",
		PublicURL: "https://music.example.com", AddonName: "Plex", RefreshInterval: time.Hour, Port: 0,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addrs := make(chan net.Addr, 1)
	done := make(chan error, 1)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() { done <- serve(ctx, cfg, log, func(addr net.Addr) { addrs <- addr }) }()

	var base string
	select {
	case addr := <-addrs:
		base = "http://127.0.0.1:" + strconv.Itoa(addr.(*net.TCPAddr).Port)
	case err := <-done:
		t.Fatalf("serve returned early: %v", err)
	}

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

	cfg := config.Config{
		PlexURL: fake.URL, PlexToken: plextest.Token, Secret: "abcdefghijklmnop",
		PublicURL: "https://music.example.com", AddonName: "Plex", RefreshInterval: time.Hour, Port: port,
	}
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
