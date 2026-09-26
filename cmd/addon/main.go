package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/config"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/jellyfin"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/library"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/plex"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/server"
)

var version = "dev"

const shutdownGrace = 10 * time.Second

func main() { os.Exit(run(os.Args[1:], os.Getenv, os.Stderr)) }

func run(args []string, getenv func(string) string, stderr io.Writer) int {
	flags := flag.NewFlagSet("addon", flag.ContinueOnError)
	flags.SetOutput(stderr)
	probe := flags.Bool("healthcheck", false, "request /health on the local port, then exit 0 or 1")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *probe {
		port := getenv("PORT")
		if port == "" {
			port = "8080"
		}
		return healthcheck("http://127.0.0.1:" + port + "/health")
	}

	cfg, err := config.Load(getenv)
	if err != nil {
		fmt.Fprintf(stderr, "invalid configuration:\n%v\n", err)
		return 1
	}
	log := newLogger(cfg.LogFormat, cfg.LogLevel, stderr).With("source", string(cfg.Backend))
	log.Info("starting", "version", version, "server", hostOf(cfg.ServerURL), "library", cmp.Or(cfg.Library, "all music"),
		"refresh", cfg.RefreshInterval.String(), "public_url", cfg.PublicURL, "log_level", cfg.LogLevel.String())
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := serve(ctx, cfg, log, func(net.Addr) {}); err != nil {
		log.Error("server stopped", "error", err.Error())
		return 1
	}
	return 0
}

func newLogger(format string, level slog.Level, w io.Writer) *slog.Logger {
	options := &slog.HandlerOptions{Level: level}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(w, options))
	}
	return slog.New(slog.NewTextHandler(w, options))
}

func hostOf(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return parsed.Host
}

func healthcheck(url string) int {
	client := &http.Client{Timeout: 3 * time.Second}
	res, err := client.Get(url)
	if err != nil {
		return 1
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func newBackend(cfg config.Config, log *slog.Logger) media.Backend {
	switch cfg.Backend {
	case config.Jellyfin:
		return jellyfin.New(jellyfin.Options{BaseURL: cfg.ServerURL, APIKey: cfg.ServerToken, Version: version})
	default:
		return plex.New(plex.Options{BaseURL: cfg.ServerURL, Token: cfg.ServerToken, Log: log})
	}
}

func serve(ctx context.Context, cfg config.Config, log *slog.Logger, listening func(net.Addr)) error {
	backend := newBackend(cfg, log)
	lib := library.NewLibrary(backend, cfg.Library, cfg.RefreshInterval, log)
	runCtx, stopRun := context.WithCancel(ctx)
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		lib.Run(runCtx)
	}()
	defer func() {
		stopRun()
		<-runDone
	}()

	listener, err := net.Listen("tcp", ":"+strconv.Itoa(cfg.Port))
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler: server.New(server.Options{
			Secret: cfg.Secret, PublicURL: cfg.PublicURL, AddonName: cfg.AddonName, Version: version,
			Library: lib, Backend: backend, Log: log,
		}),
		// WriteTimeout stays unset: a stream lasts as long as the song.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	failed := make(chan error, 1)
	go func() { failed <- srv.Serve(listener) }()
	log.Info("listening", "addr", listener.Addr().String(), "version", version)
	listening(listener.Addr())

	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
		stopRun()
	}
	log.Info("shutting down")
	grace, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(grace); err != nil {
		srv.Close()
	}
	if err := <-failed; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
