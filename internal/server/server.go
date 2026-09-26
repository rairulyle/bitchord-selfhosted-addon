package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/library"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/registry"
)

const searchLimit = 50

type Registry interface {
	Lookup(slug string) (*registry.Entry, bool)
	Healthy() bool
}

// Site is read per request so a public URL or secret changed on the setup
// page takes effect at once.
type Site struct {
	PublicURL string
	Secret    string
}

type Options struct {
	Site     func() Site
	Registry Registry
	Version  string
	Log      *slog.Logger
}

type server struct {
	Options
	lookupTimeout time.Duration
}

// source is one server resolved from the request path, with the base of its
// own URLs.
type source struct {
	*registry.Entry
	base string
}

func New(o Options) http.Handler { return newServer(o).handler() }

func newServer(o Options) *server {
	return &server{Options: o, lookupTimeout: 2500 * time.Millisecond}
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /{slug}/{secret}/manifest.json", s.guard(s.manifest))
	mux.HandleFunc("GET /{slug}/{secret}/search", s.guard(s.search))
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
		next(w, r, source{Entry: entry, base: site.PublicURL + "/" + entry.Slug + "/" + site.Secret})
	}
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
	w.Header().Set("Access-Control-Allow-Headers", "Range, If-Range, Content-Type")
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
	writeJSON(w, manifestJSON{
		ID:          "app.bitchord-selfhosted-addon." + src.Slug,
		Name:        src.DisplayName(),
		Version:     s.Version,
		Description: "Your " + src.Backend.Name() + " music library",
		Resources:   []string{"search", "stream"},
		Types:       []string{"track"},
		ContentType: "music",
	})
}

func (s *server) search(w http.ResponseWriter, r *http.Request, src source) {
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
	w.Header().Set("Content-Type", "application/json")
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

var slugPattern = regexp.MustCompile(`^(plex|jellyfin)-[1-9][0-9]*$`)

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
