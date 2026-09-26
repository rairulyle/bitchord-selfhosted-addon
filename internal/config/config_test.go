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

func validJellyfin() map[string]string {
	return map[string]string{
		"JELLYFIN_URL":     "http://jellyfin:8096/",
		"JELLYFIN_API_KEY": "key",
		"ADDON_SECRET":     "abcdefghijklmnop",
		"PUBLIC_URL":       "https://music.example.com/",
	}
}

func TestLoadAppliesDefaultsAndTrimsSlashes(t *testing.T) {
	cfg, err := Load(env(valid()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Backend != Plex || cfg.ServerURL != "http://plex:32400" || cfg.ServerToken != "tok" {
		t.Errorf("backend = %q %q %q", cfg.Backend, cfg.ServerURL, cfg.ServerToken)
	}
	if cfg.PublicURL != "https://music.example.com" {
		t.Errorf("PublicURL = %q", cfg.PublicURL)
	}
	if cfg.RefreshInterval != 15*time.Minute {
		t.Errorf("RefreshInterval = %v", cfg.RefreshInterval)
	}
	if cfg.AddonName != "" || cfg.Port != 8080 || cfg.LogLevel != slog.LevelInfo || cfg.Library != "" {
		t.Errorf("defaults wrong: %+v", cfg)
	}
}

func TestLoadInfersTheBackend(t *testing.T) {
	cases := map[string]struct {
		vars    map[string]string
		backend Backend
		url     string
		library string
		wantErr string
	}{
		"plex":     {valid(), Plex, "http://plex:32400", "", ""},
		"jellyfin": {validJellyfin(), Jellyfin, "http://jellyfin:8096", "", ""},
		"jellyfin with a library": {func() map[string]string {
			v := validJellyfin()
			v["JELLYFIN_LIBRARY"] = "Music"
			return v
		}(), Jellyfin, "http://jellyfin:8096", "Music", ""},
		"both": {func() map[string]string {
			v := valid()
			v["JELLYFIN_URL"] = "http://jellyfin:8096"
			return v
		}(), "", "", "", "either Plex or Jellyfin, not both"},
		"an optional variable from the other set counts": {func() map[string]string {
			v := valid()
			v["JELLYFIN_LIBRARY"] = "Music"
			return v
		}(), "", "", "", "either Plex or Jellyfin, not both"},
		"neither": {map[string]string{"ADDON_SECRET": "abcdefghijklmnop", "PUBLIC_URL": "https://m.example.com"},
			"", "", "", "set PLEX_URL and PLEX_TOKEN, or JELLYFIN_URL and JELLYFIN_API_KEY"},
		"half plex": {func() map[string]string {
			v := valid()
			delete(v, "PLEX_TOKEN")
			return v
		}(), Plex, "http://plex:32400", "", "PLEX_TOKEN is required"},
		"half jellyfin": {func() map[string]string {
			v := validJellyfin()
			delete(v, "JELLYFIN_URL")
			return v
		}(), Jellyfin, "", "", "JELLYFIN_URL is required"},
		"only a section names the set": {map[string]string{"PLEX_SECTION": "Music", "ADDON_SECRET": "abcdefghijklmnop", "PUBLIC_URL": "https://m.example.com"},
			Plex, "", "Music", "PLEX_URL is required"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg, err := Load(env(tc.vars))
			if tc.wantErr == "" && err != nil {
				t.Fatalf("Load: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want mention of %q", err, tc.wantErr)
			}
			if cfg.Backend != tc.backend || cfg.ServerURL != tc.url || cfg.Library != tc.library {
				t.Errorf("got backend %q, url %q, library %q", cfg.Backend, cfg.ServerURL, cfg.Library)
			}
		})
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
	if cfg.Library != "Music" || cfg.RefreshInterval != 5*time.Minute || cfg.AddonName != "Home Plex" ||
		cfg.Port != 9000 || cfg.LogLevel != slog.LevelDebug {
		t.Errorf("got %+v", cfg)
	}
}

func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	_, err := Load(env(map[string]string{}))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, key := range []string{"PLEX_URL", "PLEX_TOKEN", "JELLYFIN_URL", "JELLYFIN_API_KEY", "ADDON_SECRET", "PUBLIC_URL"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error does not mention %s: %v", key, err)
		}
	}
	_, err = Load(env(map[string]string{"JELLYFIN_URL": "ftp://x"}))
	if err == nil || !strings.Contains(err.Error(), "JELLYFIN_API_KEY is required") || !strings.Contains(err.Error(), "JELLYFIN_URL must be") {
		t.Errorf("half-filled jellyfin with a bad URL: %v", err)
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	cases := map[string]struct{ key, value, want string }{
		"short secret":        {"ADDON_SECRET", "short", "ADDON_SECRET"},
		"secret with slash":   {"ADDON_SECRET", "abcdefgh/ijklmnop", "ADDON_SECRET"},
		"plain http public":   {"PUBLIC_URL", "http://music.example.com", "PUBLIC_URL"},
		"public without host": {"PUBLIC_URL", "https://", "PUBLIC_URL"},
		"plex url scheme":     {"PLEX_URL", "ftp://plex", "PLEX_URL must be"},
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

func TestLoadLogFormat(t *testing.T) {
	cfg, err := Load(env(valid()))
	if err != nil || cfg.LogFormat != "text" {
		t.Fatalf("default LogFormat = %q, err %v", cfg.LogFormat, err)
	}
	vars := valid()
	vars["LOG_FORMAT"] = "JSON"
	if cfg, err := Load(env(vars)); err != nil || cfg.LogFormat != "json" {
		t.Fatalf("LogFormat = %q, err %v", cfg.LogFormat, err)
	}
	vars["LOG_FORMAT"] = "xml"
	if _, err := Load(env(vars)); err == nil || !strings.Contains(err.Error(), "LOG_FORMAT") {
		t.Fatalf("err = %v", err)
	}
}
