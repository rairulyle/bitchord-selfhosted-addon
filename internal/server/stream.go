package server

import (
	"context"
	"errors"
	"net/http"
	"regexp"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
)

var idPattern = regexp.MustCompile(`^[0-9a-fA-F-]{1,36}$`)

func trackID(r *http.Request) (string, bool) {
	id := r.PathValue("id")
	return id, idPattern.MatchString(id)
}

func (s *server) stream(w http.ResponseWriter, r *http.Request, src source) {
	track, status := s.resolve(r, src)
	if status != http.StatusOK {
		w.WriteHeader(status)
		return
	}
	descriptor := toStreamJSON(src.base, track)
	src.Log.Info("stream", "id", track.ID, "track", label(track), "quality", descriptor.Quality, "format", descriptor.Format)
	writeJSON(w, descriptor)
}

// resolve answers from the index and asks the backend only on a miss, so a
// track that arrived after the last refresh still plays.
func (s *server) resolve(r *http.Request, src source) (media.Track, int) {
	id, ok := trackID(r)
	if !ok {
		return media.Track{}, http.StatusNotFound
	}
	track, found := src.Library.Get(id)
	if !found {
		var status int
		if track, status = s.lookup(r, src, id); status != http.StatusOK {
			return media.Track{}, status
		}
	}
	if !track.Playable() {
		return media.Track{}, http.StatusNotFound
	}
	return track, http.StatusOK
}

func (s *server) lookup(r *http.Request, src source, id string) (media.Track, int) {
	ctx, cancel := context.WithTimeout(r.Context(), s.lookupTimeout)
	defer cancel()
	track, err := src.Backend.Track(ctx, id)
	switch {
	case err == nil:
		return track, http.StatusOK
	case errors.Is(err, media.ErrNotFound):
		return media.Track{}, http.StatusNotFound
	}
	logFailure(r, src, err)
	return media.Track{}, http.StatusBadGateway
}

func logFailure(r *http.Request, src source, err error) {
	if r.Context().Err() != nil {
		return
	}
	src.Log.Error("upstream request failed", "error", err.Error())
}
