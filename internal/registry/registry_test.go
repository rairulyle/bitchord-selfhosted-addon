package registry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/fakes"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/plex"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/store"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func plexServer(fake *fakes.Plex, slug string) store.Server {
	return store.Server{Slug: slug, Kind: store.Plex, Label: "Home", URL: fake.URL, Token: fakes.PlexToken, Auth: store.AuthToken, Library: "Music", Enabled: true}
}

func jellyfinServer(fake *fakes.Jellyfin, slug string) store.Server {
	return store.Server{Slug: slug, Kind: store.Jellyfin, URL: fake.URL, Token: fakes.JellyfinToken, Auth: store.AuthToken, Enabled: true}
}

func snapshot(servers ...store.Server) store.Snapshot {
	return store.Snapshot{ClientID: "addon-uuid", Secret: "s3cr3t-s3cr3t-s3cr3t", Servers: servers}
}

func newRegistry(t *testing.T, snap store.Snapshot) *Registry {
	t.Helper()
	r := New(snap, Options{Interval: time.Hour, Version: "1.2.3", Log: quiet, ProbeTimeout: time.Second})
	t.Cleanup(r.Stop)
	return r
}

func waitHealthy(t *testing.T, r *Registry) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !r.Healthy() {
		if time.Now().After(deadline) {
			t.Fatal("never became healthy")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitReplaced(t *testing.T, r *Registry, slug string, before *Entry) *Entry {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if entry, ok := r.Lookup(slug); ok && entry != before {
			return entry
		}
		if time.Now().After(deadline) {
			t.Fatal("the entry was never replaced")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func stopped(t *testing.T, entry *Entry, what string) {
	t.Helper()
	select {
	case <-entry.done:
	case <-time.After(time.Second):
		t.Fatalf("%s still running", what)
	}
}

// holdSections makes fake answer /library/sections only once release is
// closed, and closes the returned channel when the first request arrives.
func holdSections(fake *fakes.Plex, release <-chan struct{}) <-chan struct{} {
	held := make(chan struct{})
	var once sync.Once
	fake.Extra["/library/sections"] = func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(held) })
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"MediaContainer": map[string]any{"Directory": fakes.PlexSections()}})
	}
	return held
}

func (r *Registry) pendingEntry(slug string) *Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.pending[slug].next
}

func withoutCloser(fake *fakes.Plex) {
	fake.Tracks["3"] = slices.DeleteFunc(fake.Tracks["3"], func(track plex.Track) bool { return track.RatingKey == "103" })
}

func TestAChangedServerAnswersFromTheOldIndexUntilTheNewOneLoads(t *testing.T) {
	old, next := fakes.NewPlex(t), fakes.NewPlex(t)
	withoutCloser(next)
	release := make(chan struct{})
	held := holdSections(next, release)
	r := newRegistry(t, snapshot(plexServer(old, "plex-1")))
	waitHealthy(t, r)
	before, _ := r.Lookup("plex-1")

	r.Apply(snapshot(plexServer(next, "plex-1")))
	<-held
	if entry, _ := r.Lookup("plex-1"); entry != before || len(entry.Library.Search("Closer", 5)) == 0 {
		t.Fatal("search stopped answering from the old index during the restart")
	}
	if !r.Healthy() {
		t.Fatal("unhealthy during the restart")
	}
	close(release)
	after := waitReplaced(t, r, "plex-1", before)
	if !after.Library.Ready() || len(after.Library.Search("Closer", 5)) != 0 || len(after.Library.Search("New Religion", 5)) == 0 {
		t.Fatal("the new entry does not answer from the new index")
	}
	stopped(t, before, "old refresher")
}

func TestAChangedServerSwapsWhenItsFirstRefreshFails(t *testing.T) {
	old, next := fakes.NewPlex(t), fakes.NewPlex(t)
	next.FailWith(http.StatusInternalServerError)
	r := newRegistry(t, snapshot(plexServer(old, "plex-1")))
	waitHealthy(t, r)
	before, _ := r.Lookup("plex-1")
	r.Apply(snapshot(plexServer(next, "plex-1")))
	waitReplaced(t, r, "plex-1", before)
	if status, _ := r.Status("plex-1"); status.Ready || !strings.Contains(status.LastError, "500") {
		t.Fatalf("status after a failed first refresh = %+v", status)
	}
	stopped(t, before, "old refresher")
}

func TestAChangedServerSwapsWhenItsFirstRefreshTakesTooLong(t *testing.T) {
	old, next := fakes.NewPlex(t), fakes.NewPlex(t)
	release := make(chan struct{})
	defer close(release)
	held := holdSections(next, release)
	r := New(snapshot(plexServer(old, "plex-1")), Options{Interval: time.Hour, Log: quiet, HandoverTimeout: 100 * time.Millisecond})
	t.Cleanup(r.Stop)
	waitHealthy(t, r)
	before, _ := r.Lookup("plex-1")
	r.Apply(snapshot(plexServer(next, "plex-1")))
	<-held
	if after := waitReplaced(t, r, "plex-1", before); after.Library.Ready() {
		t.Fatal("swapped only after the refresh finished")
	}
	stopped(t, before, "old refresher")
}

func TestALaterChangeCancelsAPendingHandover(t *testing.T) {
	old, next := fakes.NewPlex(t), fakes.NewPlex(t)
	release := make(chan struct{})
	held := holdSections(next, release)
	r := newRegistry(t, snapshot(plexServer(old, "plex-1")))
	waitHealthy(t, r)
	before, _ := r.Lookup("plex-1")

	r.Apply(snapshot(plexServer(next, "plex-1")))
	<-held
	pending := r.pendingEntry("plex-1")
	r.Apply(snapshot(plexServer(old, "plex-1")))
	stopped(t, pending, "cancelled refresher")
	close(release)
	time.Sleep(50 * time.Millisecond)
	if entry, _ := r.Lookup("plex-1"); entry != before {
		t.Fatal("a cancelled handover still swapped")
	}

	again := fakes.NewPlex(t)
	holdSections(again, make(chan struct{}))
	r.Apply(snapshot(plexServer(again, "plex-1")))
	pending = r.pendingEntry("plex-1")
	r.Apply(snapshot())
	stopped(t, pending, "refresher of a removed server")
	stopped(t, before, "old refresher of a removed server")
	if _, ok := r.Lookup("plex-1"); ok {
		t.Fatal("removed server still listed")
	}
}

func TestStopDuringAHandoverEndsBothRefreshers(t *testing.T) {
	old, next := fakes.NewPlex(t), fakes.NewPlex(t)
	held := holdSections(next, make(chan struct{}))
	r := New(snapshot(plexServer(old, "plex-1")), Options{Interval: time.Hour, Log: quiet})
	waitHealthy(t, r)
	before, _ := r.Lookup("plex-1")
	r.Apply(snapshot(plexServer(next, "plex-1")))
	<-held
	pending := r.pendingEntry("plex-1")
	r.Stop()
	stopped(t, before, "old refresher")
	stopped(t, pending, "pending refresher")
	if _, ok := r.Lookup("plex-1"); ok {
		t.Fatal("entry still listed after Stop")
	}
}

func TestNewStartsOneRefresherPerEnabledServer(t *testing.T) {
	plexFake, jellyfinFake := fakes.NewPlex(t), fakes.NewJellyfin(t)
	disabled := plexServer(plexFake, "plex-2")
	disabled.Enabled = false
	r := newRegistry(t, snapshot(plexServer(plexFake, "plex-1"), jellyfinServer(jellyfinFake, "jellyfin-1"), disabled))
	waitHealthy(t, r)
	plexEntry, ok := r.Lookup("plex-1")
	if !ok || plexEntry.DisplayName() != "Plex - Home" || plexEntry.Kind != store.Plex {
		t.Fatalf("plex-1 = %+v, %v", plexEntry, ok)
	}
	jellyfinEntry, ok := r.Lookup("jellyfin-1")
	if !ok || jellyfinEntry.DisplayName() != "Jellyfin" {
		t.Fatalf("jellyfin-1 = %+v, %v", jellyfinEntry, ok)
	}
	if _, ok := r.Lookup("plex-2"); ok {
		t.Error("a disabled server was started")
	}
	status, ok := r.Status("plex-1")
	if !ok || !status.Ready || status.Tracks != 5 || status.Refreshed.IsZero() || status.LastError != "" {
		t.Fatalf("status = %+v", status)
	}
	if _, ok := r.Status("nope"); ok {
		t.Error("status for an unknown slug")
	}
	if _, found := plexEntry.Library.Get("101"); !found {
		t.Error("plex index empty")
	}
	if _, found := jellyfinEntry.Library.Get("f101"); !found {
		t.Error("jellyfin index empty")
	}
}

func TestApplyStartsStopsAndReplacesEntries(t *testing.T) {
	fake := fakes.NewPlex(t)
	r := newRegistry(t, snapshot(plexServer(fake, "plex-1")))
	waitHealthy(t, r)
	before, _ := r.Lookup("plex-1")

	relabelled := plexServer(fake, "plex-1")
	relabelled.Label = "Parents"
	r.Apply(snapshot(relabelled))
	after, _ := r.Lookup("plex-1")
	if after != before || after.DisplayName() != "Plex - Parents" {
		t.Fatal("a label change must not restart the entry")
	}

	other := fakes.NewPlex(t)
	moved := plexServer(other, "plex-1")
	r.Apply(snapshot(moved))
	waitReplaced(t, r, "plex-1", before)
	select {
	case <-before.done:
	case <-time.After(time.Second):
		t.Fatal("old refresher still running")
	}
	waitHealthy(t, r)
	if requests := other.Requests(); len(requests) == 0 {
		t.Fatal("new entry never asked the new server")
	}

	added := plexServer(fake, "plex-2")
	r.Apply(snapshot(moved, added))
	if _, ok := r.Lookup("plex-2"); !ok {
		t.Fatal("added server not started")
	}

	disabled := moved
	disabled.Enabled = false
	r.Apply(snapshot(disabled, added))
	if _, ok := r.Lookup("plex-1"); ok {
		t.Fatal("disabled server still running")
	}
	r.Apply(snapshot())
	if _, ok := r.Lookup("plex-2"); ok {
		t.Fatal("removed server still running")
	}
	if !r.Healthy() {
		t.Fatal("empty registry must be healthy")
	}
}

func TestHealthyWaitsForEveryFirstLoad(t *testing.T) {
	good, bad := fakes.NewPlex(t), fakes.NewPlex(t)
	bad.FailWith(http.StatusInternalServerError)
	r := newRegistry(t, snapshot(plexServer(good, "plex-1"), plexServer(bad, "plex-2")))
	deadline := time.Now().Add(2 * time.Second)
	for {
		status, _ := r.Status("plex-2")
		if status.LastError != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("failing server never reported an error")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r.Healthy() {
		t.Fatal("healthy while one server has never loaded")
	}
	if status, _ := r.Status("plex-2"); status.Ready || !strings.Contains(status.LastError, "500") {
		t.Fatalf("failing status = %+v", status)
	}
	bad.FailWith(0)
	r.Apply(snapshot(plexServer(good, "plex-1")))
	waitHealthy(t, r)
}

func TestStopEndsEveryRefresher(t *testing.T) {
	fake := fakes.NewPlex(t)
	blocked := make(chan struct{})
	fake.Extra["/library/sections"] = func(w http.ResponseWriter, r *http.Request) {
		close(blocked)
		<-r.Context().Done()
	}
	r := New(snapshot(plexServer(fake, "plex-1")), Options{Interval: time.Hour, Log: quiet})
	entry, _ := r.Lookup("plex-1")
	<-blocked
	r.Stop()
	select {
	case <-entry.done:
	case <-time.After(time.Second):
		t.Fatal("refresher outlived Stop")
	}
	if _, ok := r.Lookup("plex-1"); ok {
		t.Fatal("entry still listed after Stop")
	}
}

func TestProbeListsLibrariesAndRejectsABadToken(t *testing.T) {
	r := newRegistry(t, snapshot())
	plexFake, jellyfinFake := fakes.NewPlex(t), fakes.NewJellyfin(t)
	got, err := r.Probe(context.Background(), store.Server{Kind: store.Plex, URL: plexFake.URL, Token: fakes.PlexToken})
	if err != nil || got.Version != "1.42.0.9999" || len(got.Libraries) != 2 || got.Libraries[0] != (media.Library{ID: "3", Name: "Music"}) {
		t.Fatalf("plex probe = %+v, %v", got, err)
	}
	got, err = r.Probe(context.Background(), store.Server{Kind: store.Jellyfin, URL: jellyfinFake.URL, Token: fakes.JellyfinToken})
	if err != nil || got.Version != "10.10.7" || len(got.Libraries) != 2 {
		t.Fatalf("jellyfin probe = %+v, %v", got, err)
	}
	if _, err := r.Probe(context.Background(), store.Server{Kind: store.Plex, URL: plexFake.URL, Token: "wrong"}); !errors.Is(err, media.ErrUnauthorized) {
		t.Fatalf("wrong plex token: %v", err)
	}
	if _, err := r.Probe(context.Background(), store.Server{Kind: store.Jellyfin, URL: jellyfinFake.URL, Token: "wrong"}); !errors.Is(err, media.ErrUnauthorized) {
		t.Fatalf("wrong jellyfin key: %v", err)
	}
	if _, err := r.Probe(context.Background(), store.Server{Kind: "emby", URL: plexFake.URL, Token: "x"}); err == nil {
		t.Fatal("unknown kind accepted")
	}
	if got := plexFake.Requests()[0].Header.Get("X-Plex-Client-Identifier"); got != "addon-uuid" {
		t.Errorf("probe sent client id %q", got)
	}
}

func TestPlexServersReportsWhichAddressesAnswer(t *testing.T) {
	home, plexTV := fakes.NewPlex(t), fakes.NewPlexTV(t)
	home.Token = fakes.PlexTVToken
	plexTV.Resources = fakes.PlexTVResources(home.URL, fakes.DeadURL(t))
	r := New(snapshot(), Options{Version: "1.2.3", Log: quiet, PlexTV: plexTV.URL, ProbeTimeout: time.Second})
	started := time.Now()
	got, err := r.PlexServers(context.Background(), fakes.PlexTVToken)
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(started); took > 2*time.Second {
		t.Fatalf("took %v, addresses must be tried concurrently", took)
	}
	if len(got) != 2 || got[0] != (PlexServer{Name: "Home", URL: home.URL, Reachable: true}) {
		t.Fatalf("servers = %+v", got)
	}
	if got[1].Name != "Parents" || got[1].Reachable || got[1].URL == "" {
		t.Fatalf("unreachable server = %+v", got[1])
	}
	if _, err := r.PlexServers(context.Background(), "wrong"); !errors.Is(err, media.ErrUnauthorized) {
		t.Fatalf("wrong token: %v", err)
	}
}

func TestSignInWrappersCarryTheClientID(t *testing.T) {
	plexTV := fakes.NewPlexTV(t)
	r := New(snapshot(), Options{Version: "1.2.3", Log: quiet, PlexTV: plexTV.URL})
	pin, err := r.PlexPIN(context.Background())
	if err != nil || pin.ID != 1 || !strings.Contains(pin.AuthURL, "clientID=addon-uuid") {
		t.Fatalf("pin = %+v, %v", pin, err)
	}
	if _, ok, err := r.PlexClaim(context.Background(), pin.ID); ok || err != nil {
		t.Fatalf("claimed early: %v, %v", ok, err)
	}
	plexTV.Claim(pin.ID)
	account, ok, err := r.PlexClaim(context.Background(), pin.ID)
	if err != nil || !ok || account != (Account{Token: fakes.PlexTVToken, Username: fakes.PlexTVUsername}) {
		t.Fatalf("claim = %+v, %v, %v", account, ok, err)
	}
}

func TestEveryJellyfinSignInGetsItsOwnDeviceID(t *testing.T) {
	jellyfinFake := fakes.NewJellyfin(t)
	r := New(snapshot(), Options{Version: "1.2.3", Log: quiet})
	var ids []string
	for i := range 2 {
		session, err := r.JellyfinSignIn(context.Background(), jellyfinFake.URL, fakes.JellyfinUser, fakes.JellyfinPassword)
		if err != nil || session.Token != fakes.JellyfinToken || session.Username != fakes.JellyfinUser || session.DeviceID == "" || session.DeviceID == "addon-uuid" {
			t.Fatalf("session = %+v, %v", session, err)
		}
		if got := jellyfinFake.Requests()[i].Header.Get("Authorization"); !strings.Contains(got, `DeviceId="`+session.DeviceID+`"`) {
			t.Errorf("Authorization = %q, want DeviceId %q", got, session.DeviceID)
		}
		ids = append(ids, session.DeviceID)
	}
	if ids[0] == ids[1] {
		t.Fatalf("two sign-ins share the device id %q", ids[0])
	}
}

func TestJellyfinBackendsSendTheirOwnDeviceID(t *testing.T) {
	jellyfinFake := fakes.NewJellyfin(t)
	signedIn := jellyfinServer(jellyfinFake, "jellyfin-1")
	signedIn.DeviceID = "device-of-jellyfin-1"
	cases := map[string]struct {
		server store.Server
		want   string
	}{
		"signed in":  {signedIn, `DeviceId="device-of-jellyfin-1"`},
		"pasted key": {jellyfinServer(jellyfinFake, "jellyfin-2"), `DeviceId="addon-uuid"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			backend, err := NewBackend(tc.server, "addon-uuid", "1.2.3", quiet)
			if err != nil {
				t.Fatal(err)
			}
			before := len(jellyfinFake.Requests())
			if _, err := backend.Version(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := jellyfinFake.Requests()[before].Header.Get("Authorization"); !strings.Contains(got, tc.want) {
				t.Errorf("Authorization = %q, want %s", got, tc.want)
			}
		})
	}
}

func TestDisplayNameIsSafeUnderConcurrentRelabel(t *testing.T) {
	fake := fakes.NewPlex(t)
	r := newRegistry(t, snapshot(plexServer(fake, "plex-1")))
	waitHealthy(t, r)
	entry, _ := r.Lookup("plex-1")

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				entry.DisplayName()
			}
		}
	}()

	for i := 0; i < 200; i++ {
		relabelled := plexServer(fake, "plex-1")
		if i%2 == 0 {
			relabelled.Label = "Parents"
		} else {
			relabelled.Label = "Home"
		}
		r.Apply(snapshot(relabelled))
	}
	close(stop)
	<-done
}
