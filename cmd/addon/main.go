package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/config"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/registry"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/server"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/setup"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/store"
)

var version = "dev"

const (
	dataDir       = "/data"
	shutdownGrace = 10 * time.Second
)

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
	log := newLogger(cfg.LogFormat, cfg.LogLevel, stderr)
	st, err := store.Open(dataDir)
	if err != nil {
		log.Error("cannot open the data file", "error", err.Error())
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := serve(ctx, cfg, st, log, config.Removed(getenv), func(net.Addr) {}); err != nil {
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

func warnRemoved(log *slog.Logger, names []string) {
	for _, name := range names {
		log.Warn(name+" is no longer read; servers, the public URL and the secret are set on the setup page", "variable", name)
	}
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

func serve(ctx context.Context, cfg config.Config, st *store.Store, log *slog.Logger, removed []string, listening func(net.Addr)) error {
	snapshot := st.Snapshot()
	log.Info("starting", "version", version, "data", st.Path(), "servers", len(snapshot.Servers),
		"public_url", snapshot.PublicURL, "refresh", cfg.RefreshInterval.String(), "log_level", cfg.LogLevel.String())
	warnRemoved(log, removed)
	if snapshot.Admin == nil {
		log.Warn("the setup page has no password yet; open /setup on your public URL and set one before anyone else does")
	}
	reg := registry.New(snapshot, registry.Options{Interval: cfg.RefreshInterval, Version: version, Log: log})
	defer reg.Stop()

	listener, err := net.Listen("tcp", ":"+strconv.Itoa(cfg.Port))
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	admin := setup.New(setup.Options{Store: st, Registry: reg, Version: version, Log: log})
	mux.Handle("/setup", admin)
	mux.Handle("/setup/", admin)
	mux.Handle("/", server.New(server.Options{
		Site: func() server.Site {
			current := st.Snapshot()
			return server.Site{PublicURL: current.PublicURL, Secret: current.Secret}
		},
		Registry: reg, Version: version, Log: log,
	}))
	srv := &http.Server{
		Handler: mux,
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
		reg.Stop()
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
