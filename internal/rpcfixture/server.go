package rpcfixture

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
)

// Server is a fake JSON-RPC endpoint that serves the calls of a fixture in order. A request that does not
// match the next call by method and JSON-equal params fails the test. It needs no network access.
type Server struct {
	t     testing.TB
	srv   *httptest.Server
	mu    sync.Mutex
	calls []Call
	next  int
}

// Serve starts a server for the calls of f. It stops on cleanup.
func Serve(t testing.TB, f File) *Server {
	t.Helper()
	for i, c := range f.Calls {
		if err := c.check(); err != nil {
			t.Fatalf("rpcfixture: call %d: %v", i, err)
		}
	}
	s := &Server{t: t, calls: f.Calls}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	t.Cleanup(s.srv.Close)
	return s
}

// ServeFile reads the fixture at path and serves it.
func ServeFile(t testing.TB, path string) *Server {
	t.Helper()
	f, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	return Serve(t, f)
}

// URL is the endpoint of the server.
func (s *Server) URL() string { return s.srv.URL }

// Served returns the number of calls served so far.
func (s *Server) Served() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.next
}

// AssertAllServed fails the test when a call of the fixture was not served.
func (s *Server) AssertAllServed() {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.next != len(s.calls) {
		s.t.Errorf("rpcfixture: %d of %d calls served; the next one is %s %s",
			s.next, len(s.calls), s.calls[s.next].Method, s.calls[s.next].Params)
	}
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.fail(w, "rpcfixture: read the request: %v", err)
		return
	}
	reqs, batch, err := parseBody[request](body)
	if err != nil || len(reqs) == 0 {
		s.fail(w, "rpcfixture: the request is not JSON-RPC: %s", body)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.next+len(reqs) > len(s.calls) {
		s.fail(w, "rpcfixture: unexpected request after %d of %d calls: %s", s.next, len(s.calls), body)
		return
	}
	calls := s.calls[s.next : s.next+len(reqs)]
	for i, q := range reqs {
		want := calls[i]
		if q.Method != want.Method || !jsonEqual(params(q.Params), params(want.Params)) {
			s.fail(w, "rpcfixture: call %d: got %s %s, want %s %s",
				s.next+i, q.Method, params(q.Params), want.Method, params(want.Params))
			return
		}
	}
	s.next += len(reqs)

	// An HTTP status answers the whole request without a JSON-RPC body.
	if status := calls[0].HTTPStatus; status != 0 {
		for _, c := range calls {
			if c.HTTPStatus != status {
				s.fail(w, "rpcfixture: a batch mixes the HTTP status %d with other answers", status)
				return
			}
		}
		w.WriteHeader(status)
		return
	}
	resps := make([]response, len(reqs))
	for i, q := range reqs {
		c := calls[i]
		if c.HTTPStatus != 0 {
			s.fail(w, "rpcfixture: a batch mixes the HTTP status %d with other answers", c.HTTPStatus)
			return
		}
		resps[i] = response{JSONRPC: "2.0", ID: q.ID, Result: c.Result, Error: c.Error}
	}
	w.Header().Set("Content-Type", "application/json")
	if batch {
		_ = json.NewEncoder(w).Encode(resps)
		return
	}
	_ = json.NewEncoder(w).Encode(resps[0])
}

// fail reports a mismatch to the test and answers 500, so the client under test fails too. The handler runs
// outside the test goroutine, so it uses Errorf, not Fatalf.
func (s *Server) fail(w http.ResponseWriter, format string, args ...any) {
	s.t.Errorf(format, args...)
	http.Error(w, "rpcfixture: request does not match the fixture", http.StatusInternalServerError)
}

func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}
