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
