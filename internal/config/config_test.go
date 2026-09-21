package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func env(pairs map[string]string) func(string) string {
	return func(key string) string { return pairs[key] }
}

func valid() map[string]string {
	return map[string]string{
		"PLEX_URL":     "http://plex:32400/",
		"PLEX_TOKEN":   "tok",
		"ADDON_SECRET": "abcdefghijklmnop",
		"PUBLIC_URL":   "https://music.example.com/",
	}
}

func TestLoadAppliesDefaultsAndTrimsSlashes(t *testing.T) {
	cfg, err := Load(env(valid()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PlexURL != "http://plex:32400" {
		t.Errorf("PlexURL = %q", cfg.PlexURL)
	}
	if cfg.PublicURL != "https://music.example.com" {
		t.Errorf("PublicURL = %q", cfg.PublicURL)
	}
	if cfg.RefreshInterval != 15*time.Minute {
		t.Errorf("RefreshInterval = %v", cfg.RefreshInterval)
	}
	if cfg.AddonName != "Plex" || cfg.Port != 8080 || cfg.LogLevel != slog.LevelInfo || cfg.Section != "" {
		t.Errorf("defaults wrong: %+v", cfg)
	}
}

func TestLoadReadsOptionalValues(t *testing.T) {
	vars := valid()
	vars["PLEX_SECTION"] = "Music"
	vars["REFRESH_INTERVAL"] = "5m"
	vars["ADDON_NAME"] = "Home Plex"
	vars["PORT"] = "9000"
	vars["LOG_LEVEL"] = "debug"
	cfg, err := Load(env(vars))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Section != "Music" || cfg.RefreshInterval != 5*time.Minute || cfg.AddonName != "Home Plex" ||
		cfg.Port != 9000 || cfg.LogLevel != slog.LevelDebug {
		t.Errorf("got %+v", cfg)
	}
}

func TestLoadReportsEveryMissingVariableAtOnce(t *testing.T) {
	_, err := Load(env(map[string]string{}))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, key := range []string{"PLEX_URL", "PLEX_TOKEN", "ADDON_SECRET", "PUBLIC_URL"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error does not mention %s: %v", key, err)
		}
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	cases := map[string]struct{ key, value, want string }{
		"short secret":        {"ADDON_SECRET", "short", "ADDON_SECRET"},
		"secret with slash":   {"ADDON_SECRET", "abcdefgh/ijklmnop", "ADDON_SECRET"},
		"plain http public":   {"PUBLIC_URL", "http://music.example.com", "PUBLIC_URL"},
		"public without host": {"PUBLIC_URL", "https://", "PUBLIC_URL"},
		"plex url scheme":     {"PLEX_URL", "ftp://plex", "PLEX_URL"},
		"bad interval":        {"REFRESH_INTERVAL", "soon", "REFRESH_INTERVAL"},
		"zero interval":       {"REFRESH_INTERVAL", "0s", "REFRESH_INTERVAL"},
		"bad port":            {"PORT", "70000", "PORT"},
		"bad log level":       {"LOG_LEVEL", "loud", "LOG_LEVEL"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			vars := valid()
			vars[tc.key] = tc.value
			_, err := Load(env(vars))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want mention of %s", err, tc.want)
			}
		})
	}
}

func TestLoadAllowsPlainHTTPOnLocalhost(t *testing.T) {
	vars := valid()
	vars["PUBLIC_URL"] = "http://localhost:8080"
	if _, err := Load(env(vars)); err != nil {
		t.Fatalf("Load: %v", err)
	}
}
