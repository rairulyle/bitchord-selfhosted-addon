package config

import (
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"
)

func env(pairs map[string]string) func(string) string {
	return func(key string) string { return pairs[key] }
}

func TestLoadNeedsNothingAndAppliesDefaults(t *testing.T) {
	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Config{RefreshInterval: 15 * time.Minute, Port: 8080, LogLevel: slog.LevelInfo, LogFormat: "text"}
	if cfg != want {
		t.Errorf("defaults = %+v", cfg)
	}
}

func TestLoadReadsEveryValue(t *testing.T) {
	cfg, err := Load(env(map[string]string{"REFRESH_INTERVAL": " 5m ", "PORT": "9000", "LOG_LEVEL": "DEBUG", "LOG_FORMAT": "JSON"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Config{RefreshInterval: 5 * time.Minute, Port: 9000, LogLevel: slog.LevelDebug, LogFormat: "json"}
	if cfg != want {
		t.Errorf("got %+v", cfg)
	}
}

func TestLoadRejectsBadValuesAllAtOnce(t *testing.T) {
	cases := map[string]struct{ key, value, want string }{
		"bad interval":   {"REFRESH_INTERVAL", "soon", "REFRESH_INTERVAL"},
		"zero interval":  {"REFRESH_INTERVAL", "0s", "REFRESH_INTERVAL"},
		"bad port":       {"PORT", "70000", "PORT"},
		"bad log level":  {"LOG_LEVEL", "loud", "LOG_LEVEL"},
		"bad log format": {"LOG_FORMAT", "xml", "LOG_FORMAT"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(env(map[string]string{tc.key: tc.value}))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want mention of %s", err, tc.want)
			}
		})
	}
	_, err := Load(env(map[string]string{"PORT": "x", "LOG_LEVEL": "y"}))
	if err == nil || !strings.Contains(err.Error(), "PORT") || !strings.Contains(err.Error(), "LOG_LEVEL") {
		t.Fatalf("problems not joined: %v", err)
	}
}

func TestRemovedNamesTheVariablesStillSet(t *testing.T) {
	got := Removed(env(map[string]string{"PLEX_URL": "http://plex:32400", "ADDON_SECRET": " ", "JELLYFIN_API_KEY": "k", "PORT": "1"}))
	if !reflect.DeepEqual(got, []string{"PLEX_URL", "JELLYFIN_API_KEY"}) {
		t.Fatalf("Removed = %v", got)
	}
	if got := Removed(env(nil)); got != nil {
		t.Fatalf("Removed with nothing set = %v", got)
	}
}
