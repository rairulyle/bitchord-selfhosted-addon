package server

import (
	"context"
	"errors"
	"net/http"
	"regexp"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/library"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/plex"
)

var idPattern = regexp.MustCompile(`^[0-9]{1,20}$`)

func trackID(r *http.Request) (string, bool) {
	id := r.PathValue("id")
	return id, idPattern.MatchString(id)
}

func (s *server) stream(w http.ResponseWriter, r *http.Request) {
	id, ok := trackID(r)
	if !ok {
		quiet404(w, r)
		return
	}
	item, status := s.lookup(r, id)
	if status != http.StatusOK {
		w.WriteHeader(status)
		return
	}
	descriptor, ok := toStreamJSON(s.base(), item)
	if !ok {
		quiet404(w, r)
		return
	}
	if track, ok := library.FromPlex(item); ok {
		s.Log.Info("stream", "id", id, "track", label(track), "quality", descriptor.Quality, "format", descriptor.Format)
	}
	writeJSON(w, descriptor)
}

func (s *server) lookup(r *http.Request, id string) (plex.Track, int) {
	ctx, cancel := context.WithTimeout(r.Context(), s.lookupTimeout)
	defer cancel()
	item, err := s.Plex.Track(ctx, id)
	switch {
	case err == nil:
		return item, http.StatusOK
	case errors.Is(err, plex.ErrNotFound):
		return plex.Track{}, http.StatusNotFound
	}
	s.logPlexFailure(r, err)
	return plex.Track{}, http.StatusBadGateway
}

func (s *server) logPlexFailure(r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	if errors.Is(err, plex.ErrUnauthorized) {
		s.Log.Error("plex rejected the token, check PLEX_TOKEN")
		return
	}
	s.Log.Error("plex request failed", "error", err.Error())
}
