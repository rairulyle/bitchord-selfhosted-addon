package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/fakes"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/plex"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/registry"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/store"
)

type siteBox struct {
	mu   sync.Mutex
	site Site
}

func (b *siteBox) get() Site {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.site
}

func (b *siteBox) setMode(mode string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.site.AppAuth.Mode = mode
}

// newLinkHarness leaves the index empty, so any track lookup reaches the fake
// Plex and a test can tell whether a request got that far.
func newLinkHarness(t *testing.T, mode string) (*harness, *siteBox) {
	t.Helper()
	fake := fakes.NewPlex(t)
	logs := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	entry := newEntry(t, "plex-1", store.Plex, "Home", plex.New(plex.Options{BaseURL: fake.URL, Token: fakes.PlexToken, Log: log}), log, false)
	box := &siteBox{site: Site{PublicURL: testPublic, Secret: testSecret, LinkKey: testLinkKey, AppAuth: store.AppAuth{Mode: mode}}}
	s := newServer(Options{Site: box.get, Registry: &stubRegistry{entries: map[string]*registry.Entry{"plex-1": entry}}, Version: "1.2.3", Log: log})
	return &harness{fake: fake, entry: entry, server: s, handler: s.handler(), logs: logs}, box
}

func signedPath(slug, id string, exp int64, key string) string {
	return fmt.Sprintf("/plex-1/%s/file/%s?exp=%d&sig=%s", testSecret, id, exp, signFileLink(key, slug, id, exp))
}

func TestStreamSignsTheFileLinkOnlyWhileAuthIsOn(t *testing.T) {
	h, box := newLinkHarness(t, store.AppAuthLog)
	rec := h.get("/plex-1/" + testSecret + "/stream/101")
	if rec.Code != http.StatusOK {
		t.Fatalf("stream status %d", rec.Code)
	}
	link, err := url.Parse(decode[map[string]any](t, rec)["url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	exp, _ := strconv.ParseInt(link.Query().Get("exp"), 10, 64)
	if until := time.Until(time.Unix(exp, 0)); until < fileLinkLifetime-time.Minute || until > fileLinkLifetime {
		t.Errorf("link lives %s, want %s", until, fileLinkLifetime)
	}
	if strings.Contains(link.String(), testLinkKey) {
		t.Error("the link key reached the link")
	}
	box.setMode(store.AppAuthEnforce)
	if rec := h.get(link.RequestURI()); rec.Code != http.StatusOK {
		t.Fatalf("following the signed link under enforce = %d", rec.Code)
	}
	box.setMode(store.AppAuthDisabled)
	if got := decode[map[string]any](t, h.get("/plex-1/"+testSecret+"/stream/101"))["url"]; got != testBase+"/file/101" {
		t.Fatalf("disabled mode link = %v", got)
	}
}

func TestEnforceRefusesBadFileLinksBeforeAnyLookup(t *testing.T) {
	now := time.Now()
	cases := map[string]string{
		"unsigned":           "/plex-1/" + testSecret + "/file/101",
		"signed for a slug":  signedPath("plex-2", "101", now.Add(time.Hour).Unix(), testLinkKey),
		"signed for a track": strings.Replace(signedPath("plex-1", "102", now.Add(time.Hour).Unix(), testLinkKey), "/file/102", "/file/101", 1),
		"another key":        signedPath("plex-1", "101", now.Add(time.Hour).Unix(), "another-key-another-key-another-key"),
		"expired":            signedPath("plex-1", "101", now.Add(-time.Second).Unix(), testLinkKey),
		"beyond the cap":     signedPath("plex-1", "101", now.Add(fileLinkLifetime+time.Hour).Unix(), testLinkKey),
		"malformed expiry":   "/plex-1/" + testSecret + "/file/101?exp=soon&sig=x",
		"signature only":     "/plex-1/" + testSecret + "/file/101?sig=x",
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			h, _ := newLinkHarness(t, store.AppAuthEnforce)
			before := len(h.fake.Requests())
			if rec := h.get(path); rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d", rec.Code)
			}
			if len(h.fake.Requests()) != before {
				t.Error("a refused link reached Plex")
			}
		})
	}
}

func TestOldUnsignedLinksPlayUnlessEnforced(t *testing.T) {
	h, box := newLinkHarness(t, store.AppAuthDisabled)
	unsigned := "/plex-1/" + testSecret + "/file/101"
	forged := unsigned + "?exp=1&sig=forged"
	for _, path := range []string{unsigned, forged} {
		if rec := h.get(path); rec.Code != http.StatusOK {
			t.Fatalf("disabled %s = %d", path, rec.Code)
		}
	}
	if strings.Contains(h.logs.String(), "file link") {
		t.Errorf("disabled mode checked a link:\n%s", h.logs.String())
	}
	box.setMode(store.AppAuthLog)
	for _, path := range []string{unsigned, forged} {
		if rec := h.get(path); rec.Code != http.StatusOK {
			t.Fatalf("log %s = %d", path, rec.Code)
		}
	}
	if !strings.Contains(h.logs.String(), "bitchord-auth would refuse file link") {
		t.Errorf("log mode did not log the refusal:\n%s", h.logs.String())
	}
	box.setMode(store.AppAuthEnforce)
	if rec := h.get(unsigned); rec.Code != http.StatusForbidden {
		t.Fatalf("enforce unsigned = %d", rec.Code)
	}
}
