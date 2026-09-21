package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/library"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/plex"
)

const searchLimit = 50

type Library interface {
	Ready() bool
	Search(query string, limit int) []library.Track
	Get(id string) (library.Track, bool)
}

type Plex interface {
	Track(ctx context.Context, id string) (plex.Track, error)
	Open(ctx context.Context, method, path string, header http.Header) (*http.Response, error)
}

type Options struct {
	Secret    string
	PublicURL string
	AddonName string
	Version   string
	Library   Library
	Plex      Plex
	Log       *slog.Logger
}

type server struct {
	Options
	secretSum     [sha256.Size]byte
	lookupTimeout time.Duration
}

func New(o Options) http.Handler { return newServer(o).handler() }

func newServer(o Options) *server {
	return &server{Options: o, secretSum: sha256.Sum256([]byte(o.Secret)), lookupTimeout: 2500 * time.Millisecond}
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /{secret}/manifest.json", s.guard(s.manifest))
	mux.HandleFunc("GET /{secret}/search", s.guard(s.search))
	mux.HandleFunc("GET /{secret}/stream/{id}", s.guard(s.stream))
	mux.HandleFunc("GET /{secret}/file/{id}", s.guard(s.file))
	mux.HandleFunc("GET /{secret}/art/{id}", s.guard(s.art))
	mux.HandleFunc("OPTIONS /{secret}/{rest...}", s.guard(s.preflight))
	mux.HandleFunc("/", quiet404)
	return s.logged(cors(mux))
}

func (s *server) base() string { return s.PublicURL + "/" + s.Secret }

func (s *server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Digests are compared because ConstantTimeCompare returns early on a length mismatch.
		given := sha256.Sum256([]byte(r.PathValue("secret")))
		if subtle.ConstantTimeCompare(given[:], s.secretSum[:]) != 1 {
			quiet404(w, r)
			return
		}
		next(w, r)
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

func (s *server) preflight(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Range, If-Range, Content-Type")
	w.Header().Set("Access-Control-Max-Age", "86400")
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) healthz(w http.ResponseWriter, _ *http.Request) {
	if !s.Library.Ready() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *server) manifest(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, manifestJSON{
		ID:          "app.bitchord-selfhosted-addon",
		Name:        s.AddonName,
		Version:     s.Version,
		Description: "Your Plex music library",
		Resources:   []string{"search", "stream"},
		Types:       []string{"track"},
		ContentType: "music",
	})
}

func (s *server) search(w http.ResponseWriter, r *http.Request) {
	found := s.Library.Search(r.URL.Query().Get("q"), searchLimit)
	tracks := make([]trackJSON, len(found))
	for i, track := range found {
		tracks[i] = toTrackJSON(s.base(), track)
	}
	writeJSON(w, searchJSON{Tracks: tracks, Albums: []any{}, Artists: []any{}, Playlists: []any{}})
}

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
		level := slog.LevelInfo
		if r.URL.Path == "/healthz" {
			level = slog.LevelDebug
		}
		s.Log.Log(r.Context(), level, "request",
			"method", r.Method, "path", redact(r.URL.Path), "status", rec.status,
			"bytes", rec.bytes, "took", time.Since(started).String())
	})
}

func redact(path string) string {
	if path == "/healthz" || path == "/" {
		return path
	}
	rest := strings.TrimPrefix(path, "/")
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return "/***" + rest[i:]
	}
	return "/***"
}
