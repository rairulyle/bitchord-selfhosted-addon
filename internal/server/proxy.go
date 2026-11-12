package server

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
)

var (
	forwardedRequestHeaders  = []string{"Range", "If-Range"}
	forwardedResponseHeaders = []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"}
)

func (s *server) file(w http.ResponseWriter, r *http.Request, src source) {
	track, status := s.resolve(r, src)
	if status != http.StatusOK {
		w.WriteHeader(status)
		return
	}
	if s.auth != nil && s.auth.Mode() != "disabled" && s.Site().Secret != "" {
		expStr := r.URL.Query().Get("exp")
		sigStr := r.URL.Query().Get("sig")
		exp, err := strconv.ParseInt(expStr, 10, 64)
		if err != nil || !verifyMediaSig(s.Site().Secret, track.ID, sigStr, exp) {
			if s.auth.Mode() == "log" {
				src.Log.Warn("bitchord-auth: media signature invalid or missing in log mode", "id", track.ID)
			} else {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
		}
	} else if expStr := r.URL.Query().Get("exp"); expStr != "" {
		sigStr := r.URL.Query().Get("sig")
		exp, err := strconv.ParseInt(expStr, 10, 64)
		if err != nil || !verifyMediaSig(s.Site().Secret, track.ID, sigStr, exp) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
	}
	header := http.Header{}
	for _, name := range forwardedRequestHeaders {
		if value := r.Header.Get(name); value != "" {
			header.Set(name, value)
		}
	}
	started := time.Now()
	sent := pipe(w, r, src, func() (*http.Response, error) {
		return src.Backend.OpenFile(r.Context(), track, r.Method, header)
	}, "")
	switch {
	case sent.ended == "":
	case r.Method == http.MethodHead:
		src.Log.Debug("probe", "id", track.ID, "track", label(track), "status", sent.status)
	default:
		src.Log.Info("play", "id", track.ID, "track", label(track), "range", r.Header.Get("Range"),
			"status", sent.status, "bytes", sent.bytes, "ended", sent.ended, "took", time.Since(started).String())
	}
}

func (s *server) art(w http.ResponseWriter, r *http.Request, src source) {
	track, status := s.resolve(r, src)
	if status == http.StatusOK && track.ArtRef == "" {
		status = http.StatusNotFound
	}
	if status != http.StatusOK {
		w.WriteHeader(status)
		return
	}

	cacheKey := src.Slug + ":" + track.ArtRef
	if s.artCache != nil {
		if item, ok := s.artCache.Get(cacheKey); ok {
			serveCachedArt(w, r, item)
			return
		}
	}

	upstream, err := src.Backend.OpenArt(r.Context(), track)
	switch {
	case errors.Is(err, media.ErrNotFound):
		w.WriteHeader(http.StatusNotFound)
		return
	case err != nil:
		logFailure(r, src, err)
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	defer upstream.Body.Close()

	if upstream.StatusCode != http.StatusOK {
		src.Log.Error("upstream answered the artwork request badly", "status", upstream.StatusCode)
		w.WriteHeader(http.StatusBadGateway)
		return
	}

	body, err := io.ReadAll(io.LimitReader(upstream.Body, maxCachedItemSize+1))
	if err != nil {
		src.Log.Error("failed reading upstream artwork", "error", err.Error())
		w.WriteHeader(http.StatusBadGateway)
		return
	}

	contentType := upstream.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "image/jpeg"
	}
	etag := upstream.Header.Get("ETag")
	lastModified := upstream.Header.Get("Last-Modified")

	if s.artCache != nil && int64(len(body)) <= maxCachedItemSize {
		item := s.artCache.Put(cacheKey, body, contentType, etag, lastModified)
		if item != nil {
			serveCachedArt(w, r, item)
			return
		}
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	if lastModified != "" {
		w.Header().Set("Last-Modified", lastModified)
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write(body)
	}
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

func pipe(w http.ResponseWriter, r *http.Request, src source, open func() (*http.Response, error), cacheControl string) sent {
	upstream, err := open()
	switch {
	case errors.Is(err, media.ErrNotFound):
		w.WriteHeader(http.StatusNotFound)
		return sent{}
	case err != nil:
		logFailure(r, src, err)
		w.WriteHeader(http.StatusBadGateway)
		return sent{}
	}
	defer upstream.Body.Close()
	switch upstream.StatusCode {
	case http.StatusOK, http.StatusPartialContent, http.StatusRequestedRangeNotSatisfiable:
	default:
		src.Log.Error("upstream answered the byte request badly", "status", upstream.StatusCode)
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
		src.Log.Debug("stream ended early", "error", err.Error())
	}
	return out
}
