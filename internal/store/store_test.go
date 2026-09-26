package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func plexServer() Server {
	return Server{Kind: Plex, Label: "Home", URL: "http://plex:32400", Token: "tok", Auth: AuthToken, Enabled: true}
}

func TestOpenCreatesTheFileWithASecretAndClientID(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	snap := s.Snapshot()
	if !secretPattern.MatchString(snap.Secret) || len(snap.Secret) != 32 {
		t.Errorf("secret = %q", snap.Secret)
	}
	if len(snap.ClientID) != 36 || strings.Count(snap.ClientID, "-") != 4 {
		t.Errorf("client id = %q", snap.ClientID)
	}
	if snap.Admin != nil || len(snap.Servers) != 0 || snap.PublicURL != "" {
		t.Errorf("fresh snapshot = %+v", snap)
	}
	info, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o", info.Mode().Perm())
	}
	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.Snapshot(); got.Secret != snap.Secret || got.ClientID != snap.ClientID {
		t.Error("secret or client id changed on reopen")
	}
}

func TestOpenNamesACorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	os.WriteFile(path, []byte("{not json"), 0o600)
	_, err := Open(dir)
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("err = %v", err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != "{not json" {
		t.Error("corrupt file was overwritten")
	}
}

func TestOpenNamesAFileThatParsesButIsInvalid(t *testing.T) {
	server := `{"slug":"plex-1","kind":"plex","url":"http://plex:32400","token":"t","auth":"token","enabled":true}`
	cases := map[string]string{
		"short secret":   `{"secret":"short","client_id":"c","servers":[]}`,
		"repeated slug":  `{"secret":"abcdefghijklmnop","client_id":"c","servers":[` + server + `,` + server + `]}`,
		"missing client": `{"secret":"abcdefghijklmnop","servers":[]}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, FileName)
			os.WriteFile(path, []byte(content), 0o600)
			_, err := Open(dir)
			if err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("err = %v", err)
			}
			if raw, _ := os.ReadFile(path); string(raw) != content {
				t.Error("invalid file was overwritten")
			}
		})
	}
}

func TestRoundTripAndAtomicWrite(t *testing.T) {
	s := open(t)
	if err := s.SetPublicURL("https://music.example.com"); err != nil {
		t.Fatal(err)
	}
	added, err := s.AddServer(plexServer())
	if err != nil {
		t.Fatal(err)
	}
	if added.Slug != "plex-1" || added.Created.IsZero() || added.Created.Location() != time.UTC {
		t.Fatalf("added = %+v", added)
	}
	entries, _ := os.ReadDir(filepath.Dir(s.Path()))
	if len(entries) != 1 {
		t.Errorf("temp file left behind: %v", entries)
	}
	reopened, err := Open(filepath.Dir(s.Path()))
	if err != nil {
		t.Fatal(err)
	}
	got := reopened.Snapshot()
	if got.PublicURL != "https://music.example.com" || len(got.Servers) != 1 || got.Servers[0] != added {
		t.Fatalf("reopened = %+v", got)
	}
	var raw map[string]any
	data, _ := os.ReadFile(s.Path())
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if _, present := raw["admin"]; present {
		t.Error("admin written while unset")
	}
}

func TestAFailedSaveLeavesTheFileAndTheMemoryUntouched(t *testing.T) {
	s := open(t)
	s.AddServer(plexServer())
	before, _ := os.ReadFile(s.Path())
	if err := os.Chmod(filepath.Dir(s.Path()), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Dir(s.Path()), 0o700) })
	if _, err := s.AddServer(plexServer()); err == nil {
		t.Skip("directory is writable regardless of mode, likely running as root")
	}
	after, _ := os.ReadFile(s.Path())
	if string(after) != string(before) {
		t.Error("file changed after a failed save")
	}
	if got := s.Snapshot(); len(got.Servers) != 1 {
		t.Errorf("memory changed after a failed save: %+v", got.Servers)
	}
}

func TestSlugsFillTheLowestGapAndNeverRenumber(t *testing.T) {
	s := open(t)
	for range 3 {
		if _, err := s.AddServer(plexServer()); err != nil {
			t.Fatal(err)
		}
	}
	jellyfin := plexServer()
	jellyfin.Kind = Jellyfin
	if added, _ := s.AddServer(jellyfin); added.Slug != "jellyfin-1" {
		t.Errorf("first jellyfin slug = %q", added.Slug)
	}
	if err := s.RemoveServer("plex-2"); err != nil {
		t.Fatal(err)
	}
	if got := s.Snapshot(); got.Servers[1].Slug != "plex-3" {
		t.Errorf("plex-3 was renumbered: %+v", got.Servers)
	}
	if added, _ := s.AddServer(plexServer()); added.Slug != "plex-2" {
		t.Errorf("gap not filled: %q", added.Slug)
	}
	if added, _ := s.AddServer(plexServer()); added.Slug != "plex-4" {
		t.Errorf("next slug = %q", added.Slug)
	}
	if err := s.RemoveServer("plex-9"); !errors.Is(err, ErrNoServer) {
		t.Errorf("removing an unknown slug: %v", err)
	}
}

func TestUpdateServerKeepsSlugKindAndCreated(t *testing.T) {
	s := open(t)
	added, _ := s.AddServer(plexServer())
	err := s.UpdateServer("plex-1", func(server *Server) error {
		server.Slug, server.Kind, server.Created = "plex-9", Jellyfin, time.Time{}
		server.Label, server.Enabled = "Parents", false
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.Snapshot().Server("plex-1")
	if got.Slug != "plex-1" || got.Kind != Plex || !got.Created.Equal(added.Created) || got.Label != "Parents" || got.Enabled {
		t.Fatalf("updated = %+v", got)
	}
	if err := s.UpdateServer("plex-2", func(*Server) error { return nil }); !errors.Is(err, ErrNoServer) {
		t.Errorf("unknown slug: %v", err)
	}
}

func TestRegenerateSecretAndSetAdmin(t *testing.T) {
	s := open(t)
	before := s.Snapshot().Secret
	secret, err := s.RegenerateSecret()
	if err != nil || secret == before || s.Snapshot().Secret != secret {
		t.Fatalf("secret = %q, %v", secret, err)
	}
	if err := s.SetAdmin(&Admin{PasswordHash: "hash", SessionKey: "key"}); err != nil {
		t.Fatal(err)
	}
	reopened, _ := Open(filepath.Dir(s.Path()))
	if got := reopened.Snapshot().Admin; got == nil || got.PasswordHash != "hash" || got.SessionKey != "key" {
		t.Fatalf("admin = %+v", got)
	}
	if err := s.SetAdmin(nil); err != nil || s.Snapshot().Admin != nil {
		t.Fatal("admin was not cleared")
	}
}

func TestValidation(t *testing.T) {
	s := open(t)
	cases := map[string]struct {
		change func(*Server)
		want   string
	}{
		"bad url":          {func(v *Server) { v.URL = "plex:32400" }, "http or https"},
		"no token":         {func(v *Server) { v.Token = "" }, "token or API key is required"},
		"unknown kind":     {func(v *Server) { v.Kind = "emby" }, `unknown server kind "emby"`},
		"unknown auth":     {func(v *Server) { v.Auth = "magic" }, `unknown auth "magic"`},
		"auth kind clash":  {func(v *Server) { v.Auth = AuthJellyfinSignIn }, "only a Jellyfin server"},
		"label too long":   {func(v *Server) { v.Label = strings.Repeat("é", 17) }, "at most 16 characters"},
		"label at the cap": {func(v *Server) { v.Label = strings.Repeat("é", 16) }, ""},
		"empty label":      {func(v *Server) { v.Label = "" }, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			server := plexServer()
			tc.change(&server)
			_, err := s.AddServer(server)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("err = %v, want %s", err, tc.want)
			}
		})
	}
	urls := map[string]string{
		"https://music.example.com":  "",
		"http://localhost:8080":      "",
		"http://music.example.com":   "must start with https://",
		"https://music.example.com/": "no path",
		"music.example.com":          "full URL",
	}
	for raw, want := range urls {
		err := s.SetPublicURL(raw)
		if (want == "" && err != nil) || (want != "" && (err == nil || !strings.Contains(err.Error(), want))) {
			t.Errorf("%s: %v, want %q", raw, err, want)
		}
	}
}

func TestOrderedSortsByCreationThenSlug(t *testing.T) {
	now := time.Now()
	snap := Snapshot{Servers: []Server{
		{Slug: "plex-2", Created: now.Add(time.Minute)},
		{Slug: "jellyfin-1", Created: now},
		{Slug: "plex-1", Created: now},
	}}
	got := snap.Ordered()
	if got[0].Slug != "jellyfin-1" || got[1].Slug != "plex-1" || got[2].Slug != "plex-2" {
		t.Fatalf("Ordered = %+v", got)
	}
}

func TestSnapshotDoesNotWaitForAnUpdate(t *testing.T) {
	s := open(t)
	s.AddServer(plexServer())
	entered, release := make(chan struct{}), make(chan struct{})
	updated := make(chan error)
	go func() {
		updated <- s.Update(func(data *Snapshot) error {
			close(entered)
			<-release
			data.PublicURL = "https://music.example.com"
			return nil
		})
	}()
	<-entered
	read := make(chan Snapshot)
	go func() { read <- s.Snapshot() }()
	select {
	case snap := <-read:
		if snap.PublicURL != "" || len(snap.Servers) != 1 {
			t.Errorf("snapshot during an update = %+v", snap)
		}
		snap.Servers[0].Label = "mutated"
	case <-time.After(2 * time.Second):
		t.Error("Snapshot waited for the update")
	}
	close(release)
	if err := <-updated; err != nil {
		t.Fatal(err)
	}
	if snap := s.Snapshot(); snap.PublicURL != "https://music.example.com" || snap.Servers[0].Label != "Home" {
		t.Fatalf("after the update = %+v", snap)
	}
}
