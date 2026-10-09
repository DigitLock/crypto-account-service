package httpfixture

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// Weight is the cost of a request in one budget of the source (SRS — Binance §2.1.2).
type Weight struct {
	Budget string
	Weight int
}

// Weights is the weight table of a test by endpoint, keyed "METHOD /path", such as "GET /api/v3/time".
type Weights map[string]Weight

// Request is a request as the server received it, for assertions of a test: the raw query keeps timestamp and
// signature.
type Request struct {
	Method   string
	Path     string
	RawQuery string
	Header   http.Header
}

// Server is a fake exchange API that serves the calls of a fixture in order, with their status, headers and body.
// A request that does not match the next call by method, path and query (without timestamp and signature), or
// that comes after the last call, fails the test. It counts the weight of every request it receives by the weight
// table of the test, per budget; a request whose endpoint is not in a given table fails the test. It listens on
// the loopback interface only and never calls out.
type Server struct {
	t       testing.TB
	srv     *httptest.Server
	weights Weights

	mu       sync.Mutex
	calls    []Call
	next     int
	used     map[string]int
	received []Request
}

// Serve starts a server for the calls of f; weights may be nil when the test does not count weight. It stops on
// cleanup.
func Serve(t testing.TB, f File, weights Weights) *Server {
	t.Helper()
	if err := f.check(); err != nil {
		t.Fatalf("httpfixture: %v", err)
	}
	s := &Server{t: t, calls: f.Calls, weights: weights, used: map[string]int{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	t.Cleanup(s.srv.Close)
	return s
}

// ServeFile reads the fixture at path and serves it.
func ServeFile(t testing.TB, path string, weights Weights) *Server {
	t.Helper()
	f, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	return Serve(t, f, weights)
}

// URL is the base URL of the server.
func (s *Server) URL() string { return s.srv.URL }

// Served returns the number of calls served so far.
func (s *Server) Served() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.next
}

// Used returns the weight counted in a budget so far.
func (s *Server) Used(budget string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.used[budget]
}

// Received returns the requests received so far, in order.
func (s *Server) Received() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.received...)
}

// AssertAllServed fails the test when a call of the fixture was not served.
func (s *Server) AssertAllServed() {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.next != len(s.calls) {
		c := s.calls[s.next]
		s.t.Errorf("httpfixture: %d of %d calls served; the next one is %s %s?%s", s.next, len(s.calls), c.Method, c.Path, c.Query)
	}
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.received = append(s.received, Request{Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery, Header: r.Header.Clone()})
	endpoint := r.Method + " " + r.URL.Path
	if s.weights != nil {
		weight, ok := s.weights[endpoint]
		if !ok {
			s.t.Errorf("httpfixture: %s is not in the weight table of the test", endpoint)
		}
		s.used[weight.Budget] += weight.Weight
	}
	query := Query(r.URL.RawQuery)
	if s.next >= len(s.calls) {
		s.fail(w, "httpfixture: unexpected request after %d of %d calls: %s?%s", s.next, len(s.calls), endpoint, query)
		return
	}
	c := s.calls[s.next]
	if r.Method != c.Method || r.URL.Path != c.Path || query != c.Query {
		s.fail(w, "httpfixture: call %d: got %s?%s, want %s %s?%s", s.next, endpoint, query, c.Method, c.Path, c.Query)
		return
	}
	s.next++
	for name, value := range c.Headers {
		w.Header().Set(name, value)
	}
	if len(c.Body) > 0 {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(c.HTTPStatus)
	_, _ = w.Write(c.Body)
}

// fail reports a mismatch to the test and answers 500, so the client under test fails too. The handler runs
// outside the test goroutine, so it uses Errorf, not Fatalf.
func (s *Server) fail(w http.ResponseWriter, format string, args ...any) {
	s.t.Errorf(format, args...)
	http.Error(w, "httpfixture: request does not match the fixture", http.StatusInternalServerError)
}
