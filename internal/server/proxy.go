package server

import (
	"io"
	"net/http"

	"github.com/rairulyle/eclipse-plex-addon/internal/plex"
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
	s.pipe(w, r, r.Method, track.partKey, header, "")
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

type resolved struct{ partKey, thumb string }

func (s *server) resolve(r *http.Request) (resolved, int) {
	id, ok := trackID(r)
	if !ok {
		return resolved{}, http.StatusNotFound
	}
	if track, found := s.Library.Get(id); found {
		return resolved{track.PartKey, track.Thumb}, http.StatusOK
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
	return resolved{part.Key, thumb}, http.StatusOK
}

func (s *server) pipe(w http.ResponseWriter, r *http.Request, method, path string, header http.Header, cacheControl string) {
	upstream, err := s.Plex.Open(r.Context(), method, path, header)
	if err != nil {
		s.logPlexFailure(r, err)
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	defer upstream.Body.Close()
	switch upstream.StatusCode {
	case http.StatusOK, http.StatusPartialContent, http.StatusRequestedRangeNotSatisfiable:
	case http.StatusNotFound:
		w.WriteHeader(http.StatusNotFound)
		return
	case http.StatusUnauthorized:
		s.logPlexFailure(r, plex.ErrUnauthorized)
		w.WriteHeader(http.StatusBadGateway)
		return
	default:
		s.Log.Error("plex answered the byte request badly", "status", upstream.StatusCode)
		w.WriteHeader(http.StatusBadGateway)
		return
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
	if r.Method != http.MethodHead {
		io.Copy(w, upstream.Body)
	}
}
