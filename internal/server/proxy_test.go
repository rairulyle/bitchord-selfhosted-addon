package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rairulyle/eclipse-plex-addon/internal/plex"
	"github.com/rairulyle/eclipse-plex-addon/internal/plextest"
)

const flacBody = "0123456789abcdefghijklmnopqrstuvwxyz"

func filePath(id string) string { return "/" + testSecret + "/file/" + id }

func TestFileWithoutRange(t *testing.T) {
	rec := newHarness(t).get(filePath("101"))
	if rec.Code != http.StatusOK || rec.Body.String() != flacBody {
		t.Fatalf("status %d, body %q", rec.Code, rec.Body.String())
	}
	want := map[string]string{
		"Content-Type": "audio/flac", "Content-Length": "36", "Accept-Ranges": "bytes",
		"Etag": `"fake-etag"`, "Last-Modified": "Fri, 02 Jan 2026 03:04:05 GMT",
	}
	for name, value := range want {
		if got := rec.Header().Get(name); got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}
}

func TestFileWithRange(t *testing.T) {
	rec := newHarness(t).do(http.MethodGet, filePath("101"), http.Header{"Range": {"bytes=10-19"}})
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "abcdefghij" {
		t.Fatalf("status %d, body %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 10-19/36" {
		t.Errorf("Content-Range = %q", got)
	}
}

func TestFileRangePastTheEndIsPassedThrough(t *testing.T) {
	rec := newHarness(t).do(http.MethodGet, filePath("101"), http.Header{"Range": {"bytes=100-"}})
	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestFileHead(t *testing.T) {
	h := newHarness(t)
	rec := h.do(http.MethodHead, filePath("101"), nil)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 || rec.Header().Get("Content-Length") != "36" {
		t.Fatalf("status %d, body %q, length %q", rec.Code, rec.Body.String(), rec.Header().Get("Content-Length"))
	}
	requests := h.fake.Requests()
	if last := requests[len(requests)-1]; last.Method != http.MethodHead {
		t.Errorf("upstream method = %s", last.Method)
	}
}

func TestFileForwardsOnlyWhitelistedHeaders(t *testing.T) {
	h := newHarness(t)
	rec := h.do(http.MethodGet, filePath("101"), http.Header{
		"Range": {"bytes=0-3"}, "If-Range": {`"fake-etag"`}, "Cookie": {"session=1"}, "Authorization": {"Bearer x"},
	})
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d", rec.Code)
	}
	requests := h.fake.Requests()
	upstream := requests[len(requests)-1].Header
	if upstream.Get("Range") != "bytes=0-3" || upstream.Get("If-Range") != `"fake-etag"` {
		t.Errorf("range headers not forwarded: %v", upstream)
	}
	if upstream.Get("Cookie") != "" || upstream.Get("Authorization") != "" {
		t.Errorf("private request headers reached Plex: %v", upstream)
	}
	if upstream.Get("Accept-Encoding") != "identity" {
		t.Errorf("Accept-Encoding = %q", upstream.Get("Accept-Encoding"))
	}
	if rec.Header().Get("X-Plex-Protocol") != "" {
		t.Error("a Plex response header outside the whitelist reached the client")
	}
	for name, values := range rec.Header() {
		if strings.Contains(strings.Join(values, ","), plextest.Token) {
			t.Errorf("token leaked in response header %s", name)
		}
	}
}

func TestFileFallsBackToPlexOnAnIndexMiss(t *testing.T) {
	h := newHarnessWith(t, plex.Options{}, false)
	if rec := h.get(filePath("101")); rec.Code != http.StatusOK || rec.Body.String() != flacBody {
		t.Fatalf("status %d, body %q", rec.Code, rec.Body.String())
	}
}

func TestFileMissesAndFailures(t *testing.T) {
	h := newHarness(t)
	for name, id := range map[string]string{"unknown id": "999", "file gone from plex": "103", "malformed id": "1x"} {
		if rec := h.get(filePath(id)); rec.Code != http.StatusNotFound || rec.Body.Len() != 0 {
			t.Errorf("%s: status %d", name, rec.Code)
		}
	}
	h.fake.FailWith(http.StatusInternalServerError)
	if rec := h.get(filePath("101")); rec.Code != http.StatusBadGateway {
		t.Errorf("plex 500: status %d", rec.Code)
	}
}

func TestClientDisconnectCancelsThePlexRequest(t *testing.T) {
	h := newHarness(t)
	cancelled := make(chan struct{})
	h.fake.Extra["/library/parts/102/1/file.mp3"] = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		for {
			select {
			case <-r.Context().Done():
				close(cancelled)
				return
			default:
				w.Write([]byte("chunk"))
				w.(http.Flusher).Flush()
				time.Sleep(time.Millisecond)
			}
		}
	}
	front := httptest.NewServer(h.handler)
	defer front.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, front.URL+filePath("102"), nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(res.Body, make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	cancel()
	res.Body.Close()

	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("Plex request was still running 5s after the client left")
	}
}

func TestClientDisconnectBeforePlexAnswersCancelsThePlexRequest(t *testing.T) {
	h := newHarness(t)
	reached := make(chan struct{})
	cancelled := make(chan struct{})
	h.fake.Extra["/library/parts/102/1/file.mp3"] = func(w http.ResponseWriter, r *http.Request) {
		close(reached)
		<-r.Context().Done()
		close(cancelled)
	}
	front := httptest.NewServer(h.handler)
	defer front.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, front.URL+filePath("102"), nil)
	go http.DefaultClient.Do(req)
	<-reached
	cancel()

	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("Plex request was still running 5s after the client left")
	}
}

func TestABodyLongerThanEveryTimeoutStillCompletes(t *testing.T) {
	h := newHarnessWith(t, plex.Options{HeaderTimeout: 50 * time.Millisecond, CallTimeout: 50 * time.Millisecond}, true)
	h.server.lookupTimeout = 50 * time.Millisecond
	h.fake.Extra["/library/parts/102/1/file.mp3"] = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		for range 6 {
			w.Write([]byte("slow!"))
			w.(http.Flusher).Flush()
			time.Sleep(40 * time.Millisecond)
		}
	}
	front := httptest.NewServer(h.handler)
	defer front.Close()
	res, err := http.Get(front.URL + filePath("102"))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil || string(body) != strings.Repeat("slow!", 6) {
		t.Fatalf("body %q, err %v", body, err)
	}
}

func TestArt(t *testing.T) {
	h := newHarness(t)
	rec := h.get("/" + testSecret + "/art/101")
	if rec.Code != http.StatusOK || rec.Body.String() != "jpeg:/library/metadata/101/thumb/1" {
		t.Fatalf("status %d, body %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "image/jpeg" || rec.Header().Get("Cache-Control") != "public, max-age=86400" {
		t.Errorf("headers = %v", rec.Header())
	}
	requests := h.fake.Requests()
	last := requests[len(requests)-1]
	if last.Path != "/photo/:/transcode" || !strings.Contains(last.Query, "width=600&height=600") {
		t.Errorf("upstream = %s?%s", last.Path, last.Query)
	}
	if strings.Contains(last.Query, plextest.Token) {
		t.Error("token leaked into the artwork URL")
	}
}

func TestArtFallbacksAndMisses(t *testing.T) {
	h := newHarness(t)
	if rec := h.get("/" + testSecret + "/art/105"); rec.Body.String() != "jpeg:/library/metadata/9105/thumb/1" {
		t.Errorf("album thumb fallback: %q", rec.Body.String())
	}
	for name, id := range map[string]string{"track without any thumb": "104", "unknown id": "999"} {
		if rec := h.get("/" + testSecret + "/art/" + id); rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d", name, rec.Code)
		}
	}
	cold := newHarnessWith(t, plex.Options{}, false)
	if rec := cold.get("/" + testSecret + "/art/101"); rec.Code != http.StatusOK {
		t.Errorf("index miss: status %d", rec.Code)
	}
	head := h.do(http.MethodHead, "/"+testSecret+"/art/101", nil)
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Errorf("HEAD: status %d, body %q", head.Code, head.Body.String())
	}
}
