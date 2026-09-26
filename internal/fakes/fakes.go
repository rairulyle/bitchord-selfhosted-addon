// Package fakes holds in-process stand-ins for the media servers the addon
// talks to. Tests in several packages import it, which is why it is not a
// _test file inside each adapter.
package fakes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// DeadURL is an address that refuses connections at once. A never-bound port
// such as 127.0.0.1:1 can hang for the whole dial timeout on some hosts.
func DeadURL(t testing.TB) string {
	t.Helper()
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	return server.URL
}

type Request struct {
	Method string
	Path   string
	Query  string
	Header http.Header
}

type recorder struct {
	mu       sync.Mutex
	status   int
	rawBody  string
	requests []Request
}

func (r *recorder) FailWith(status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = status
}

func (r *recorder) AnswerRaw(body string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rawBody = body
}

func (r *recorder) Requests() []Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Request(nil), r.requests...)
}

func (r *recorder) record(req *http.Request) (status int, rawBody string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, Request{req.Method, req.URL.Path, req.URL.RawQuery, req.Header.Clone()})
	return r.status, r.rawBody
}

// override answers a forced status or raw body when one is set, and reports
// whether it did.
func (r *recorder) override(w http.ResponseWriter, status int, rawBody string) bool {
	switch {
	case status != 0:
		w.WriteHeader(status)
	case rawBody != "":
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(rawBody))
	default:
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(value)
}
