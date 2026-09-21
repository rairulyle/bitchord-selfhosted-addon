package server

import (
	"io"
	"net/http"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/library"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/plex"
)

var (
	forwardedRequestHeaders  = []string{"Range", "If-Range"}
	forwardedResponseHeaders = []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"}
)

func (s *server) file(w http.ResponseWriter, r *http.Request) {
	track, status := s.resolve(r)
	if status != http.StatusOK {
		w.WriteHeader(status)
		return
	}
	header := http.Header{}
	for _, name := range forwardedRequestHeaders {
		if value := r.Header.Get(name); value != "" {
			header.Set(name, value)
		}
	}
	started := time.Now()
	sent := s.pipe(w, r, r.Method, track.partKey, header, "")
	switch {
	case sent.ended == "":
	case r.Method == http.MethodHead:
		s.Log.Debug("probe", "id", r.PathValue("id"), "track", track.label, "status", sent.status)
	default:
		s.Log.Info("play", "id", r.PathValue("id"), "track", track.label, "range", r.Header.Get("Range"),
			"status", sent.status, "bytes", sent.bytes, "ended", sent.ended, "took", time.Since(started).String())
	}
}

func (s *server) art(w http.ResponseWriter, r *http.Request) {
	track, status := s.resolve(r)
	if status == http.StatusOK && track.thumb == "" {
		status = http.StatusNotFound
	}
	if status != http.StatusOK {
		w.WriteHeader(status)
		return
	}
	s.pipe(w, r, http.MethodGet, plex.ArtPath(track.thumb), nil, "public, max-age=86400")
}

type resolved struct{ partKey, thumb, label string }

type sent struct {
	status int
	bytes  int64
	ended  string
}

type clientWriter struct {
	io.Writer
	failed bool
}

func (c *clientWriter) Write(p []byte) (int, error) {
	n, err := c.Writer.Write(p)
	if err != nil {
		c.failed = true
	}
	return n, err
}

func (s *server) resolve(r *http.Request) (resolved, int) {
	id, ok := trackID(r)
	if !ok {
		return resolved{}, http.StatusNotFound
	}
	if track, found := s.Library.Get(id); found {
		return resolved{track.PartKey, track.Thumb, label(track)}, http.StatusOK
	}
	item, status := s.lookup(r, id)
	if status != http.StatusOK {
		return resolved{}, status
	}
	_, part, ok := item.FirstPart()
	if !ok {
		return resolved{}, http.StatusNotFound
	}
	thumb := item.Thumb
	if thumb == "" {
		thumb = item.ParentThumb
	}
	name := item.Title
	if track, ok := library.FromPlex(item); ok {
		name = label(track)
	}
	return resolved{part.Key, thumb, name}, http.StatusOK
}

func (s *server) pipe(w http.ResponseWriter, r *http.Request, method, path string, header http.Header, cacheControl string) sent {
	upstream, err := s.Plex.Open(r.Context(), method, path, header)
	if err != nil {
		s.logPlexFailure(r, err)
		w.WriteHeader(http.StatusBadGateway)
		return sent{}
	}
	defer upstream.Body.Close()
	switch upstream.StatusCode {
	case http.StatusOK, http.StatusPartialContent, http.StatusRequestedRangeNotSatisfiable:
	case http.StatusNotFound:
		w.WriteHeader(http.StatusNotFound)
		return sent{}
	case http.StatusUnauthorized:
		s.logPlexFailure(r, plex.ErrUnauthorized)
		w.WriteHeader(http.StatusBadGateway)
		return sent{}
	default:
		s.Log.Error("plex answered the byte request badly", "status", upstream.StatusCode)
		w.WriteHeader(http.StatusBadGateway)
		return sent{}
	}
	for _, name := range forwardedResponseHeaders {
		if value := upstream.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	if cacheControl != "" {
		w.Header().Set("Cache-Control", cacheControl)
	}
	w.WriteHeader(upstream.StatusCode)
	out := sent{status: upstream.StatusCode, ended: "complete"}
	if r.Method == http.MethodHead {
		return out
	}
	client := &clientWriter{Writer: w}
	out.bytes, err = io.Copy(client, upstream.Body)
	switch {
	case err == nil:
	case client.failed || r.Context().Err() != nil:
		out.ended = "client left"
	default:
		out.ended = "upstream error"
		s.Log.Debug("stream ended early", "error", err.Error())
	}
	return out
}
