package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Backend string

const (
	Plex     Backend = "plex"
	Jellyfin Backend = "jellyfin"
)

type Config struct {
	Backend         Backend
	ServerURL       string
	ServerToken     string
	Library         string
	Secret          string
	PublicURL       string
	AddonName       string
	RefreshInterval time.Duration
	Port            int
	LogLevel        slog.Level
	LogFormat       string
}

var secretPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,}$`)

var logLevels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

type variables struct{ url, token, library string }

var (
	plexVars     = variables{"PLEX_URL", "PLEX_TOKEN", "PLEX_SECTION"}
	jellyfinVars = variables{"JELLYFIN_URL", "JELLYFIN_API_KEY", "JELLYFIN_LIBRARY"}
)

func Load(getenv func(string) string) (Config, error) {
	var problems []error
	fail := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}
	get := func(key, fallback string) string {
		if value := strings.TrimSpace(getenv(key)); value != "" {
			return value
		}
		return fallback
	}
	required := func(key string) string {
		value := get(key, "")
		if value == "" {
			fail("%s is required", key)
		}
		return value
	}
	anySet := func(v variables) bool {
		return get(v.url, "") != "" || get(v.token, "") != "" || get(v.library, "") != ""
	}

	cfg := Config{
		Secret:    required("ADDON_SECRET"),
		PublicURL: strings.TrimRight(required("PUBLIC_URL"), "/"),
		AddonName: get("ADDON_NAME", ""),
	}
	choose := func(backend Backend, v variables) {
		cfg.Backend = backend
		cfg.ServerURL = strings.TrimRight(required(v.url), "/")
		cfg.ServerToken = required(v.token)
		cfg.Library = get(v.library, "")
		if cfg.ServerURL != "" && !isHTTP(cfg.ServerURL) {
			fail("%s must be an http or https URL", v.url)
		}
	}
	switch plexSet, jellyfinSet := anySet(plexVars), anySet(jellyfinVars); {
	case plexSet && jellyfinSet:
		fail("configure either Plex or Jellyfin, not both")
	case jellyfinSet:
		choose(Jellyfin, jellyfinVars)
	case plexSet:
		choose(Plex, plexVars)
	default:
		fail("set PLEX_URL and PLEX_TOKEN, or JELLYFIN_URL and JELLYFIN_API_KEY")
	}

	if cfg.PublicURL != "" {
		parsed, err := url.Parse(cfg.PublicURL)
		switch {
		case err != nil || parsed.Host == "":
			fail("PUBLIC_URL must be a full URL such as https://music.example.com")
		case parsed.Scheme != "https" && !(parsed.Scheme == "http" && parsed.Hostname() == "localhost"):
			fail("PUBLIC_URL must start with https:// unless the host is localhost")
		}
	}
	if cfg.Secret != "" && !secretPattern.MatchString(cfg.Secret) {
		fail("ADDON_SECRET must be at least 16 characters of letters, digits, '-' or '_'")
	}

	interval, err := time.ParseDuration(get("REFRESH_INTERVAL", "15m"))
	if err != nil || interval <= 0 {
		fail("REFRESH_INTERVAL must be a positive duration such as 15m")
	}
	cfg.RefreshInterval = interval

	port, err := strconv.Atoi(get("PORT", "8080"))
	if err != nil || port < 1 || port > 65535 {
		fail("PORT must be between 1 and 65535")
	}
	cfg.Port = port

	level, ok := logLevels[strings.ToLower(get("LOG_LEVEL", "info"))]
	if !ok {
		fail("LOG_LEVEL must be one of debug, info, warn, error")
	}
	cfg.LogLevel = level

	cfg.LogFormat = strings.ToLower(get("LOG_FORMAT", "text"))
	if cfg.LogFormat != "text" && cfg.LogFormat != "json" {
		fail("LOG_FORMAT must be text or json")
	}

	return cfg, errors.Join(problems...)
}

func isHTTP(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Host != "" && (parsed.Scheme == "http" || parsed.Scheme == "https")
}
