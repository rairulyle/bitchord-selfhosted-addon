// Package config reads the process settings. Everything about servers, the
// public URL and the secret lives in the store and is set on the setup page.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	RefreshInterval time.Duration
	Port            int
	LogLevel        slog.Level
	LogFormat       string
}

var logLevels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

// RemovedVariables were read by 0.4 and earlier. They are ignored now, and
// Removed names the ones still set so main can say where they went.
var RemovedVariables = []string{
	"ADDON_SECRET", "PUBLIC_URL", "ADDON_NAME",
	"PLEX_URL", "PLEX_TOKEN", "PLEX_SECTION",
	"JELLYFIN_URL", "JELLYFIN_API_KEY", "JELLYFIN_LIBRARY",
}

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

	var cfg Config
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

func Removed(getenv func(string) string) []string {
	var out []string
	for _, name := range RemovedVariables {
		if strings.TrimSpace(getenv(name)) != "" {
			out = append(out, name)
		}
	}
	return out
}
