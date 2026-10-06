package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The processor simulator in process, against a fake card-auth (SRS — Card Spend §2.1.1 Processor simulator).
// S2-T701 runs the binary against the real card-auth in internal/hardtest.

// fakeAPI records the requests and answers each with status and body.
type fakeAPI struct {
	srv      *httptest.Server
	mu       sync.Mutex
	requests []fakeRequest
	status   int
	body     string
	delay    time.Duration
	inFlight atomic.Int32
	maxSeen  atomic.Int32
}

type fakeRequest struct {
	method, path, rawPath, user, password string
	body                                  map[string]any
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{status: http.StatusOK, body: `{"auth_id":"a","decision":"APPROVED","status":"APPROVED"}`}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := f.inFlight.Add(1)
		defer f.inFlight.Add(-1)
		for {
			m := f.maxSeen.Load()
			if n <= m || f.maxSeen.CompareAndSwap(m, n) {
				break
			}
		}
		raw, _ := io.ReadAll(r.Body)
		user, password, _ := r.BasicAuth()
		req := fakeRequest{method: r.Method, path: r.URL.Path, rawPath: r.URL.EscapedPath(), user: user, password: password}
		_ = json.Unmarshal(raw, &req.body)
		f.mu.Lock()
		f.requests = append(f.requests, req)
		status, body, delay := f.status, f.body, f.delay
		f.mu.Unlock()
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) got() []fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeRequest(nil), f.requests...)
}

func simPassword(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// sim runs casctl with the environment env only and records every variable it asked for.
func sim(t *testing.T, env map[string]string, args ...string) (result, []string) {
	t.Helper()
	var asked []string
	var mu sync.Mutex
	var out, errOut bytes.Buffer
	code := run(ctx, args, &out, &errOut, func(name string) string {
		mu.Lock()
		asked = append(asked, name)
		mu.Unlock()
		return env[name]
	})
	return result{stdout: out.String(), stderr: errOut.String(), code: code}, asked
}

func simEnv(f *fakeAPI, password string) map[string]string {
	return map[string]string{
		simURLVar: f.srv.URL, simUserVar: "0123456789ab", simPasswordVar: password,
		dbURLVar: "postgres://owner:never-read@127.0.0.1:1/cas",
	}
}

// lines decodes the output lines.
func lines(t *testing.T, stdout string) []answer {
	t.Helper()
	var out []answer
	for _, l := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if l == "" {
			continue
		}
		var a answer
		if err := json.Unmarshal([]byte(l), &a); err != nil {
			t.Fatalf("output line %q is not JSON: %v", l, err)
		}
		out = append(out, a)
	}
	return out
}

func TestSim_Authorize(t *testing.T) {
	f := newFakeAPI(t)
	password := simPassword(t)
	r, asked := sim(t, simEnv(f, password), "sim", "authorize", "--auth-id", "auth-1", "--card-ref", "card_A",
		"--amount", "25.40", "--currency", "USD", "--merchant-name", "Shop", "--merchant-mcc", "5411")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	got := f.got()
	if len(got) != 1 || got[0].method != http.MethodPost || got[0].path != "/v1/authorizations" ||
		got[0].user != "0123456789ab" || got[0].password != password {
		t.Fatalf("requests %+v", got)
	}
	body, _ := json.Marshal(got[0].body)
	if string(body) != `{"amount":"25.40","auth_id":"auth-1","card_ref":"card_A","currency":"USD","merchant":{"mcc":"5411","name":"Shop"}}` {
		t.Errorf("request body %s", body)
	}
	out := lines(t, r.stdout)
	if len(out) != 1 || out[0].Status != 200 || string(out[0].Body) != f.body {
		t.Errorf("output %q, want one line with status 200 and the body", r.stdout)
	}
	for _, name := range asked {
		if name == dbURLVar {
			t.Error("sim read " + dbURLVar)
		}
	}
	if strings.Contains(r.stdout+r.stderr, password) {
		t.Error("the password was printed")
	}

	t.Run("only the flags given are sent", func(t *testing.T) {
		f := newFakeAPI(t)
		if r, _ := sim(t, simEnv(f, password), "sim", "authorize", "--auth-id", "a"); r.code != 0 {
			t.Fatalf("exit %d", r.code)
		}
		if body, _ := json.Marshal(f.got()[0].body); string(body) != `{"auth_id":"a"}` {
			t.Errorf("request body %s", body)
		}
	})
}

func TestSim_RepeatParallel(t *testing.T) {
	f := newFakeAPI(t)
	f.delay = 50 * time.Millisecond
	r, _ := sim(t, simEnv(f, simPassword(t)), "sim", "authorize", "--auth-id", "a", "--repeat", "9", "--parallel", "3")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if out := lines(t, r.stdout); len(out) != 9 || len(f.got()) != 9 {
		t.Errorf("%d lines, %d requests; want 9 and 9", len(out), len(f.got()))
	}
	if m := f.maxSeen.Load(); m != 3 {
		t.Errorf("at most %d requests at a time, want 3", m)
	}
	for _, args := range [][]string{{"--repeat", "0"}, {"--parallel", "0"}} {
		if r, _ := sim(t, simEnv(f, "p"), append([]string{"sim", "authorize"}, args...)...); r.code == 0 {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestSim_ReturnAndGet(t *testing.T) {
	f := newFakeAPI(t)
	env := simEnv(f, simPassword(t))
	if r, _ := sim(t, env, "sim", "return", "--auth-id", "a/b c", "--return-id", "rv-1", "--type", "REVERSAL", "--amount", "10.00"); r.code != 0 {
		t.Fatalf("return: exit %d: %s", r.code, r.stderr)
	}
	if r, _ := sim(t, env, "sim", "get", "--auth-id", "a/b c"); r.code != 0 {
		t.Fatalf("get: exit %d: %s", r.code, r.stderr)
	}
	got := f.got()
	if len(got) != 2 || got[0].method != http.MethodPost || got[0].rawPath != "/v1/authorizations/a%2Fb%20c/returns" ||
		got[1].method != http.MethodGet || got[1].rawPath != "/v1/authorizations/a%2Fb%20c" {
		t.Fatalf("requests %+v", got)
	}
	if body, _ := json.Marshal(got[0].body); string(body) != `{"amount":"10.00","return_id":"rv-1","type":"REVERSAL"}` {
		t.Errorf("return body %s", body)
	}
	for _, cmd := range []string{"return", "get"} {
		if r, _ := sim(t, env, "sim", cmd); r.code == 0 {
			t.Errorf("%s without --auth-id accepted", cmd)
		}
	}
}

func TestSim_ExitCodes(t *testing.T) {
	password := simPassword(t)
	for _, c := range []struct {
		status int
		code   int
	}{{200, 0}, {401, 0}, {404, 0}, {409, 0}, {422, 0}, {500, 1}, {503, 1}} {
		f := newFakeAPI(t)
		f.status, f.body = c.status, `{"error":{"code":"X","message":"m"}}`
		r, _ := sim(t, simEnv(f, password), "sim", "get", "--auth-id", "a")
		out := lines(t, r.stdout)
		if r.code != c.code || len(out) != 1 || out[0].Status != c.status {
			t.Errorf("answer %d: exit %d, output %q; want exit %d and one line", c.status, r.code, r.stdout, c.code)
		}
		if strings.Contains(r.stdout+r.stderr, password) {
			t.Error("the password was printed")
		}
	}

	t.Run("transport error", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		r, _ := sim(t, map[string]string{simURLVar: "http://" + addr, simUserVar: "u", simPasswordVar: password},
			"sim", "authorize", "--auth-id", "a", "--repeat", "3")
		if r.code == 0 || r.stdout != "" || !strings.Contains(r.stderr, "no answer from "+simURLVar) {
			t.Errorf("exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
		}
		if strings.Contains(r.stderr, password) {
			t.Error("the password was printed")
		}
	})

	t.Run("not JSON", func(t *testing.T) {
		f := newFakeAPI(t)
		f.body = "plain text"
		r, _ := sim(t, simEnv(f, password), "sim", "get", "--auth-id", "a")
		if out := lines(t, r.stdout); len(out) != 1 || string(out[0].Body) != `"plain text"` {
			t.Errorf("output %q", r.stdout)
		}
	})
}

func TestSim_Environment(t *testing.T) {
	f := newFakeAPI(t)
	password := simPassword(t)
	full := simEnv(f, password)
	for _, missing := range []string{simURLVar, simUserVar, simPasswordVar} {
		env := map[string]string{}
		for k, v := range full {
			if k != missing {
				env[k] = v
			}
		}
		r, _ := sim(t, env, "sim", "get", "--auth-id", "a")
		if r.code == 0 || !strings.Contains(r.stderr, missing) {
			t.Errorf("without %s: exit %d, stderr %q", missing, r.code, r.stderr)
		}
		if strings.Contains(r.stderr, password) || strings.Contains(r.stderr, f.srv.URL) {
			t.Errorf("without %s the error prints a value: %q", missing, r.stderr)
		}
	}
	env := map[string]string{simURLVar: "ftp://" + password, simUserVar: "u", simPasswordVar: password}
	if r, _ := sim(t, env, "sim", "get", "--auth-id", "a"); r.code == 0 || strings.Contains(r.stderr, password) ||
		!strings.Contains(r.stderr, simURLVar) {
		t.Errorf("a bad URL: exit %d, stderr %q", r.code, r.stderr)
	}
	if len(f.got()) != 0 {
		t.Error("a request was sent without a complete environment")
	}
	for _, args := range [][]string{{"sim", "--help"}, {"sim", "authorize", "--help"}, {"sim", "return", "--help"}, {"sim", "get", "--help"}} {
		r, _ := sim(t, full, args...)
		if r.code != 0 || strings.Contains(r.stdout+r.stderr, password) {
			t.Errorf("%v: exit %d or the password printed", args, r.code)
		}
	}
}
