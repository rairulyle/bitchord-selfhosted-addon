package server

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
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
	sent := s.pipe(w, r, func() (*http.Response, error) {
		return s.Backend.OpenFile(r.Context(), track, r.Method, header)
	}, "")
	switch {
	case sent.ended == "":
	case r.Method == http.MethodHead:
		s.Log.Debug("probe", "id", track.ID, "track", label(track), "status", sent.status)
	default:
		s.Log.Info("play", "id", track.ID, "track", label(track), "range", r.Header.Get("Range"),
			"status", sent.status, "bytes", sent.bytes, "ended", sent.ended, "took", time.Since(started).String())
	}
}

func (s *server) art(w http.ResponseWriter, r *http.Request) {
	track, status := s.resolve(r)
	if status == http.StatusOK && track.ArtRef == "" {
		status = http.StatusNotFound
	}
	if status != http.StatusOK {
		w.WriteHeader(status)
		return
	}
	s.pipe(w, r, func() (*http.Response, error) { return s.Backend.OpenArt(r.Context(), track) }, "public, max-age=86400")
}

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

func (s *server) pipe(w http.ResponseWriter, r *http.Request, open func() (*http.Response, error), cacheControl string) sent {
	upstream, err := open()
	switch {
	case errors.Is(err, media.ErrNotFound):
		w.WriteHeader(http.StatusNotFound)
		return sent{}
	case err != nil:
		s.logFailure(r, err)
		w.WriteHeader(http.StatusBadGateway)
		return sent{}
	}
	defer upstream.Body.Close()
	switch upstream.StatusCode {
	case http.StatusOK, http.StatusPartialContent, http.StatusRequestedRangeNotSatisfiable:
	default:
		s.Log.Error("upstream answered the byte request badly", "status", upstream.StatusCode)
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
