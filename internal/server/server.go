package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/bitchordauth"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/library"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/registry"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/store"
)

const searchLimit = 50

type Registry interface {
	Lookup(slug string) (*registry.Entry, bool)
	Healthy() bool
}

// Site is read per request so a public URL, secret or app auth setting
// changed on the setup page takes effect at once.
type Site struct {
	PublicURL string
	Secret    string
	LinkKey   string
	AppAuth   store.AppAuth
}

type Options struct {
	Site     func() Site
	Registry Registry
	Version  string
	Log      *slog.Logger
	// Auth verifies BitChord app tokens. Its settings always come from Site;
	// a test passes one only to point it at a fake issuer.
	Auth     *bitchordauth.Auth
	ArtCache *ArtCache
}

type server struct {
	Options
	lookupTimeout time.Duration
	auth          *bitchordauth.Auth
	authFrom      atomic.Pointer[Site]
	artCache      *ArtCache
}

// source is one server resolved from the request path, with the base of its
// own URLs and the site settings read for this request.
type source struct {
	*registry.Entry
	base string
	site Site
}

func New(o Options) http.Handler { return newServer(o).handler() }

func newServer(o Options) *server {
	auth := o.Auth
	if auth == nil {
		auth = bitchordauth.New(bitchordauth.Options{})
	}
	cache := o.ArtCache
	if cache == nil {
		cache = NewArtCache(defaultArtMaxItems, defaultArtMaxBytes)
	}
	return &server{
		Options:       o,
		lookupTimeout: 2500 * time.Millisecond,
		auth:          auth,
		artCache:      cache,
	}
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /{slug}/{secret}/manifest.json", s.guard(s.manifest))
	mux.HandleFunc("GET /{slug}/{secret}/search", s.guard(s.search))
	mux.HandleFunc("POST /{slug}/{secret}/sync", s.guard(s.syncWebhook))
	mux.HandleFunc("GET /{slug}/{secret}/stream/{id}", s.guard(s.stream))
	mux.HandleFunc("GET /{slug}/{secret}/file/{id}", s.guard(s.file))
	mux.HandleFunc("GET /{slug}/{secret}/art/{id}", s.guard(s.art))
	mux.HandleFunc("OPTIONS /{slug}/{secret}/{rest...}", s.guard(s.preflight))
	mux.HandleFunc("/", quiet404)
	return s.logged(cors(cleanOnly(mux)))
}

func cleanOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if path.Clean(r.URL.Path) != r.URL.Path {
			quiet404(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// guard checks the secret before looking at the slug, so the answer time
// tells a caller nothing about which slugs exist until the secret is known.
// Digests are compared because ConstantTimeCompare returns early on a length mismatch.
func (s *server) guard(next func(http.ResponseWriter, *http.Request, source)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		site := s.Site()
		given, want := sha256.Sum256([]byte(r.PathValue("secret"))), sha256.Sum256([]byte(site.Secret))
		if subtle.ConstantTimeCompare(given[:], want[:]) != 1 {
			quiet404(w, r)
			return
		}
		entry, ok := s.Registry.Lookup(r.PathValue("slug"))
		if !ok {
			quiet404(w, r)
			return
		}
		next(w, r, source{Entry: entry, base: site.PublicURL + "/" + entry.Slug + "/" + site.Secret, site: site})
	}
}

// authMode applies the site's app auth settings when they differ from the
// last ones applied, so the allowed hosts are always the setup page's list
// plus the public URL's host, and returns the mode in force.
func (s *server) authMode(site Site) string {
	if last := s.authFrom.Load(); last == nil || !sameAuthSettings(*last, site) {
		hosts := slices.Clone(site.AppAuth.Hosts)
		if site.PublicURL != "" {
			hosts = append(hosts, bitchordauth.NormalizeHost(site.PublicURL))
		}
		s.auth.Configure(bitchordauth.Config{Mode: site.AppAuth.EffectiveMode(), Hosts: hosts, Issuer: site.AppAuth.Issuer})
		s.authFrom.Store(&site)
	}
	return s.auth.Mode()
}

func sameAuthSettings(a, b Site) bool {
	return a.PublicURL == b.PublicURL && a.AppAuth.Mode == b.AppAuth.Mode && a.AppAuth.Issuer == b.AppAuth.Issuer && slices.Equal(a.AppAuth.Hosts, b.AppAuth.Hosts)
}

func (s *server) checkAuth(w http.ResponseWriter, r *http.Request, src source) bool {
	mode := s.authMode(src.site)
	if mode == store.AppAuthDisabled {
		return true
	}
	res := s.auth.VerifyRequest(r)
	if res.OK {
		return true
	}
	if mode == store.AppAuthLog {
		src.Log.Warn("bitchord-auth would refuse request", "error", res.Error, "message", res.Message)
		return true
	}
	res.WriteRejection(w)
	return false
}

func quiet404(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Expose-Headers", "Content-Length, Content-Range, Accept-Ranges")
		next.ServeHTTP(w, r)
	})
}

func (s *server) preflight(w http.ResponseWriter, _ *http.Request, _ source) {
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Range, If-Range, Content-Type, X-BitChord-Token, X-BitChord-Proof")
	w.Header().Set("Access-Control-Max-Age", "86400")
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) health(w http.ResponseWriter, _ *http.Request) {
	if !s.Registry.Healthy() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *server) manifest(w http.ResponseWriter, _ *http.Request, src source) {
	mode := s.authMode(src.site)
	writeJSON(w, manifestJSON{
		ID:           "app.bitchord-selfhosted-addon." + src.Slug,
		Name:         src.DisplayName(),
		Version:      s.Version,
		Description:  "Your " + src.Backend.Name() + " music library",
		Resources:    []string{"search", "stream"},
		Types:        []string{"track"},
		ContentType:  "music",
		BitchordAuth: mode != store.AppAuthDisabled,
	})
}

func (s *server) search(w http.ResponseWriter, r *http.Request, src source) {
	if !s.checkAuth(w, r, src) {
		return
	}
	started := time.Now()
	query := r.URL.Query().Get("q")
	found := src.Library.Find(query, searchLimit)
	tracks := make([]trackJSON, len(found.Tracks))
	for i, track := range found.Tracks {
		tracks[i] = toTrackJSON(src.base, track)
	}
	logSearch(src.Log, query, found, time.Since(started))
	writeJSON(w, searchJSON{Tracks: tracks, Albums: []any{}, Artists: []any{}, Playlists: []any{}})
}

// syncWebhook runs a refresh detached from the caller, so a webhook sender
// that hangs up does not cancel the scan, and answers 202 while one is
// already running instead of queueing another.
func (s *server) syncWebhook(w http.ResponseWriter, r *http.Request, src source) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), library.ManualTimeout)
	defer cancel()
	ran, err := src.Library.TryRefresh(ctx)
	switch {
	case !ran:
		writeJSONStatus(w, http.StatusAccepted, map[string]any{"ok": true, "server": src.Slug, "status": "already syncing"})
		return
	case err != nil:
		src.Log.Error("webhook sync failed", "error", err.Error())
		writeJSONStatus(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "sync failed, see the addon log"})
		return
	}
	stats := src.Library.Stats()
	src.Log.Info("webhook sync complete", "tracks", stats.Tracks)
	writeJSON(w, map[string]any{
		"ok":        true,
		"server":    src.Slug,
		"name":      src.DisplayName(),
		"tracks":    stats.Tracks,
		"refreshed": stats.Refreshed.Format(time.RFC3339),
	})
}

const loggedQueryRunes = 200

func logSearch(log *slog.Logger, query string, found library.Result, took time.Duration) {
	if strings.TrimSpace(query) == "" {
		return
	}
	if runes := []rune(query); len(runes) > loggedQueryRunes {
		query = string(runes[:loggedQueryRunes]) + "…"
	}
	attrs := []any{"q", query, "strict", found.Strict, "fallback", found.Fallback, "returned", len(found.Tracks)}
	if len(found.Tracks) == 0 {
		log.Info("search miss", append(attrs, "took", took.String())...)
		return
	}
	log.Info("search", append(attrs, "top", label(found.Tracks[0]), "took", took.String())...)
}

func label(track media.Track) string { return track.Title + " — " + track.Artist }

func writeJSON(w http.ResponseWriter, value any) {
	writeJSONStatus(w, http.StatusOK, value)
}

func writeJSONStatus(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

type recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *recorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (s *server) logged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rec := &recorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		s.Log.Debug("request",
			"method", r.Method, "path", redact(r.URL.Path, s.Site().Secret), "status", rec.status,
			"bytes", rec.bytes, "took", time.Since(started).String())
	})
}

var slugPattern = regexp.MustCompile(`^(plex|jellyfin|navidrome)-[1-9][0-9]*$`)

// redact keeps a slug and masks the segment after it. A first segment that
// is not a slug is masked too, since a client on a 0.4 URL sends the secret
// there. A segment matching the live secret is masked wherever it lands,
// since a non-slug first segment shifts the secret one place to the right.
func redact(p, secret string) string {
	if p == "/health" || p == "/" {
		return p
	}
	if path.Clean(p) != p {
		return "/***"
	}
	segments := strings.Split(strings.TrimPrefix(p, "/"), "/")
	if len(segments) > 1 && slugPattern.MatchString(segments[0]) {
		segments[1] = "***"
	} else {
		segments[0] = "***"
	}
	if secret != "" {
		for i, segment := range segments {
			if segment == secret {
				segments[i] = "***"
			}
		}
	}
	return "/" + strings.Join(segments, "/")
}
