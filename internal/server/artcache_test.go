package server

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/fakes"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/registry"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/store"
)

func TestArtCacheLRUEviction(t *testing.T) {
	// Create cache with max 2 items, 1000 bytes
	cache := NewArtCache(2, 1000)

	cache.Put("item1", []byte("data1"), "image/jpeg", `"etag1"`, "")
	cache.Put("item2", []byte("data2"), "image/jpeg", `"etag2"`, "")

	if items, bytes := cache.Stats(); items != 2 || bytes != 10 {
		t.Fatalf("Stats = %d items, %d bytes; want 2 items, 10 bytes", items, bytes)
	}

	// Access item1 to make it most recently used
	if _, ok := cache.Get("item1"); !ok {
		t.Fatal("item1 should be in cache")
	}

	// Adding item3 should evict item2 (the LRU item)
	cache.Put("item3", []byte("data3"), "image/jpeg", `"etag3"`, "")

	if _, ok := cache.Get("item2"); ok {
		t.Fatal("item2 should have been evicted")
	}
	if _, ok := cache.Get("item1"); !ok {
		t.Fatal("item1 should still be present")
	}
	if _, ok := cache.Get("item3"); !ok {
		t.Fatal("item3 should be present")
	}

	// Test Clear
	cache.Clear()
	if items, bytes := cache.Stats(); items != 0 || bytes != 0 {
		t.Fatalf("Stats after Clear = %d items, %d bytes; want 0, 0", items, bytes)
	}
}

func TestArtCacheByteLimitEviction(t *testing.T) {
	// Max items 10, but max bytes 20
	cache := NewArtCache(10, 20)

	cache.Put("a", []byte("12345678"), "image/jpeg", "", "") // 8 bytes
	cache.Put("b", []byte("12345678"), "image/jpeg", "", "") // 8 bytes (total 16)

	// Adding 8 more bytes would make 24 > 20, so 'a' should be evicted
	cache.Put("c", []byte("12345678"), "image/jpeg", "", "")

	if _, ok := cache.Get("a"); ok {
		t.Fatal("'a' should have been evicted due to byte limit")
	}
	if _, ok := cache.Get("b"); !ok {
		t.Fatal("'b' should still be present")
	}
	if _, ok := cache.Get("c"); !ok {
		t.Fatal("'c' should be present")
	}
}

func TestArtCacheLargeItemNotCached(t *testing.T) {
	cache := NewArtCache(10, 100)
	// Item larger than maxBytes (100)
	large := make([]byte, 150)
	item := cache.Put("large", large, "image/jpeg", "", "")
	if item != nil {
		t.Fatal("large item should return nil")
	}
	if _, ok := cache.Get("large"); ok {
		t.Fatal("large item should not be in cache")
	}
}

func TestArtCacheConcurrentAccess(t *testing.T) {
	cache := NewArtCache(50, 10000)
	var wg sync.WaitGroup

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			key := fmt.Sprintf("key-%d", n%10)
			cache.Put(key, []byte("data"), "image/jpeg", "", "")
			cache.Get(key)
			cache.Stats()
		}(i)
	}
	wg.Wait()
}

func TestArtCacheHitsAvoidUpstreamAndSupportETag304(t *testing.T) {
	h := newHarness(t)
	artURL := "/plex-1/" + testSecret + "/art/101"

	// 1. Initial request (miss) fetches upstream
	reqCountBefore := len(h.fake.Requests())
	rec1 := h.get(artURL)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request status = %d", rec1.Code)
	}
	etag := rec1.Header().Get("ETag")
	if etag == "" {
		t.Fatal("first request missing ETag header")
	}
	if len(h.fake.Requests()) != reqCountBefore+1 {
		t.Fatalf("expected 1 upstream request, got %d", len(h.fake.Requests())-reqCountBefore)
	}

	// 2. Second request (cache hit) should NOT call upstream
	reqCountAfterFirst := len(h.fake.Requests())
	rec2 := h.get(artURL)
	if rec2.Code != http.StatusOK || rec2.Body.String() != rec1.Body.String() {
		t.Fatalf("cached response mismatch: code %d, body %s", rec2.Code, rec2.Body.String())
	}
	if len(h.fake.Requests()) != reqCountAfterFirst {
		t.Fatalf("cache hit called upstream: before %d, after %d", reqCountAfterFirst, len(h.fake.Requests()))
	}

	// 3. Request with matching If-None-Match should return 304 Not Modified
	req3 := httptest.NewRequest(http.MethodGet, artURL, nil)
	req3.Header.Set("If-None-Match", etag)
	rec3 := httptest.NewRecorder()
	h.handler.ServeHTTP(rec3, req3)

	if rec3.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match status = %d, want 304", rec3.Code)
	}
	if rec3.Body.Len() != 0 {
		t.Fatalf("304 response should have empty body, got %d bytes", rec3.Body.Len())
	}
	if len(h.fake.Requests()) != reqCountAfterFirst {
		t.Fatalf("304 revalidation called upstream: before %d, after %d", reqCountAfterFirst, len(h.fake.Requests()))
	}
}

func TestArtCacheSkipsAnswersThatAreNotImages(t *testing.T) {
	h := newHarness(t)
	h.fake.AnswerRaw(`{"error":"not an image"}`)
	artURL := "/plex-1/" + testSecret + "/art/101"
	before := len(h.fake.Requests())
	h.get(artURL)
	h.get(artURL)
	if got := len(h.fake.Requests()) - before; got != 2 {
		t.Fatalf("upstream requests = %d, want 2: a non-image answer was cached", got)
	}
	if items, _ := h.server.artCache.Stats(); items != 0 {
		t.Fatalf("cache holds %d items", items)
	}
}

func TestOversizedArtIsServedInFullAndNotCached(t *testing.T) {
	h := newHarness(t)
	large := bytes.Repeat([]byte("x"), maxCachedItemSize+1024)
	h.fake.Extra["/photo/:/transcode"] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Content-Length", strconv.Itoa(len(large)))
		w.Write(large)
	}
	artURL := "/plex-1/" + testSecret + "/art/101"
	before := len(h.fake.Requests())
	for range 2 {
		rec := h.get(artURL)
		if rec.Code != http.StatusOK || rec.Body.Len() != len(large) || rec.Header().Get("Content-Length") != strconv.Itoa(len(large)) {
			t.Fatalf("status %d, %d of %d bytes, Content-Length %q", rec.Code, rec.Body.Len(), len(large), rec.Header().Get("Content-Length"))
		}
	}
	if got := len(h.fake.Requests()) - before; got != 2 {
		t.Fatalf("upstream requests = %d, want 2: oversized art was cached", got)
	}
}

func TestArtCacheIsKeyedByServerAddress(t *testing.T) {
	first, second := fakes.NewPlex(t), fakes.NewPlex(t)
	server := store.Server{Slug: "plex-1", Kind: store.Plex, URL: first.URL, Token: fakes.PlexToken, Auth: store.AuthToken, Library: "Music", Enabled: true}
	snapshot := func(s store.Server) store.Snapshot {
		return store.Snapshot{ClientID: "addon-uuid", Secret: testSecret, Servers: []store.Server{s}}
	}
	reg := registry.New(snapshot(server), registry.Options{Interval: time.Hour, Version: "1.2.3", Log: slog.New(slog.DiscardHandler)})
	t.Cleanup(reg.Stop)
	waitFor(t, func() bool { return reg.Healthy() })
	handler := newServer(Options{Site: func() Site { return Site{PublicURL: testPublic, Secret: testSecret} }, Registry: reg, Version: "1.2.3", Log: slog.New(slog.DiscardHandler)}).handler()
	artURL := "/plex-1/" + testSecret + "/art/101"
	if rec := do(handler, http.MethodGet, artURL, nil); rec.Code != http.StatusOK {
		t.Fatalf("first art = %d", rec.Code)
	}
	server.URL = second.URL
	reg.Apply(snapshot(server))
	waitFor(t, func() bool { entry, ok := reg.Lookup("plex-1"); return ok && entry.URL() == second.URL })
	before := len(second.Requests())
	if rec := do(handler, http.MethodGet, artURL, nil); rec.Code != http.StatusOK {
		t.Fatalf("second art = %d", rec.Code)
	}
	if len(second.Requests()) == before {
		t.Fatal("art cached for the old address was served for the new one")
	}
}

func waitFor(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatal("condition never held")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
