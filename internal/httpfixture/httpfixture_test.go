package httpfixture_test

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/DigitLock/crypto-account-service/internal/httpfixture"
)

// recordingT stands in for the test given to the server, so a test can see the failures the server reports.
type recordingT struct {
	testing.TB
	mu     sync.Mutex
	errors []string
}

func (r *recordingT) Helper() {}

func (r *recordingT) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *recordingT) failures() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.errors...)
}

func get(t *testing.T, method, rawURL string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

// compact returns a JSON body without white space: a written fixture is indented.
func compact(t *testing.T, body []byte) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, body); err != nil {
		t.Fatalf("not JSON: %s", body)
	}
	return b.String()
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// X1-T109 — Req: SRS — Binance §2.6; X1 D-13. A fixture file of three calls is replayed in order with its status,
// headers and body; the weight of each request is counted per budget; a fourth request fails the test.
func TestT109_FixtureAndFakeServer(t *testing.T) {
	f := httpfixture.File{
		Description: "Three calls: server time, a signed account call, a rate-limit answer",
		Context:     map[string]string{"account": "fictitious"},
		Calls: []httpfixture.Call{
			{Method: "GET", Path: "/api/v3/time", HTTPStatus: 200,
				Headers: map[string]string{"X-MBX-USED-WEIGHT-1M": "1"}, Body: json.RawMessage(`{"serverTime":1790000000000}`)},
			{Method: "GET", Path: "/api/v3/account", Query: "omitZeroBalances=true&recvWindow=5000", HTTPStatus: 200,
				Headers: map[string]string{"X-MBX-USED-WEIGHT-1M": "21"}, Body: json.RawMessage(`{"uid":100000001,"balances":[]}`)},
			{Method: "POST", Path: "/sapi/v1/asset/get-funding-asset", Query: "recvWindow=5000", HTTPStatus: 429,
				Headers: map[string]string{"Retry-After": "30"}},
		},
	}
	path := filepath.Join(t.TempDir(), "three.json")
	if err := httpfixture.Write(path, f); err != nil {
		t.Fatal(err)
	}
	read, err := httpfixture.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	weights := httpfixture.Weights{
		"GET /api/v3/time":                      {Budget: "api", Weight: 1},
		"GET /api/v3/account":                   {Budget: "api", Weight: 20},
		"POST /sapi/v1/asset/get-funding-asset": {Budget: "funding", Weight: 1},
	}
	rt := &recordingT{TB: t}
	srv := httpfixture.Serve(rt, read, weights)
	if u, err := url.Parse(srv.URL()); err != nil || u.Hostname() != "127.0.0.1" {
		t.Fatalf("server URL %s: the server must listen on the loopback interface only", srv.URL())
	}

	resp, body := get(t, "GET", srv.URL()+"/api/v3/time")
	if resp.StatusCode != 200 || resp.Header.Get("X-MBX-USED-WEIGHT-1M") != "1" || compact(t, body) != `{"serverTime":1790000000000}` {
		t.Errorf("call 1: %d %v %s", resp.StatusCode, resp.Header, body)
	}
	resp, body = get(t, "GET", srv.URL()+"/api/v3/account?omitZeroBalances=true&recvWindow=5000&timestamp=1790000000001&signature=00ff")
	if resp.StatusCode != 200 || resp.Header.Get("X-MBX-USED-WEIGHT-1M") != "21" || !strings.Contains(compact(t, body), `"uid":100000001`) {
		t.Errorf("call 2: %d %v %s", resp.StatusCode, resp.Header, body)
	}
	resp, body = get(t, "POST", srv.URL()+"/sapi/v1/asset/get-funding-asset?recvWindow=5000&timestamp=1790000000002&signature=00ff")
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "30" || len(body) != 0 {
		t.Errorf("call 3: %d %v %q", resp.StatusCode, resp.Header, body)
	}
	srv.AssertAllServed()
	if got := rt.failures(); len(got) != 0 {
		t.Fatalf("failures after three matching calls: %v", got)
	}
	if srv.Served() != 3 || srv.Used("api") != 21 || srv.Used("funding") != 1 {
		t.Errorf("served %d, weight api %d, funding %d; want 3, 21, 1", srv.Served(), srv.Used("api"), srv.Used("funding"))
	}
	if got := srv.Received(); len(got) != 3 || !strings.HasSuffix(got[1].RawQuery, "&signature=00ff") {
		t.Errorf("received = %+v; want three requests with their raw query", got)
	}

	t.Run("fourth request fails the test", func(t *testing.T) {
		resp, _ := get(t, "GET", srv.URL()+"/api/v3/time")
		if got := rt.failures(); resp.StatusCode != 500 || len(got) != 1 || !strings.Contains(got[0], "unexpected request after 3 of 3 calls") {
			t.Errorf("status %d, failures %v; want 500 and one failure", resp.StatusCode, got)
		}
		if srv.Used("api") != 22 {
			t.Errorf("weight api = %d, want 22: a request that fails is counted too", srv.Used("api"))
		}
	})

	t.Run("mismatch and unknown endpoint fail the test", func(t *testing.T) {
		rt := &recordingT{TB: t}
		srv := httpfixture.Serve(rt, read, weights)
		resp, _ := get(t, "GET", srv.URL()+"/api/v3/account")
		if got := rt.failures(); resp.StatusCode != 500 || len(got) != 1 || !strings.Contains(got[0], "want GET /api/v3/time") {
			t.Errorf("status %d, failures %v; want 500 and one mismatch", resp.StatusCode, got)
		}
		get(t, "GET", srv.URL()+"/api/v3/exchangeInfo")
		if got := rt.failures(); len(got) != 3 || !strings.Contains(got[1], "not in the weight table") {
			t.Errorf("failures %v; want the endpoint outside the weight table named", got)
		}
		rt.mu.Lock()
		rt.errors = nil
		rt.mu.Unlock()
		srv.AssertAllServed()
		if got := rt.failures(); len(got) != 1 || !strings.Contains(got[0], "0 of 3 calls served") {
			t.Errorf("AssertAllServed: %v", got)
		}
	})

	t.Run("Read refuses what a fixture must not hold", func(t *testing.T) {
		for name, c := range map[string]httpfixture.Call{
			"timestamp in the query": {Method: "GET", Path: "/api/v3/account", Query: "recvWindow=5000&timestamp=1", HTTPStatus: 200},
			"signature in the query": {Method: "GET", Path: "/api/v3/account", Query: "signature=00", HTTPStatus: 200},
			"API key header":         {Method: "GET", Path: "/api/v3/account", HTTPStatus: 200, Headers: map[string]string{"X-MBX-APIKEY": "k"}},
			"host in the path":       {Method: "GET", Path: "https://api.example/api/v3/time", HTTPStatus: 200},
			"no status":              {Method: "GET", Path: "/api/v3/time"},
			"body not JSON":          {Method: "GET", Path: "/api/v3/time", HTTPStatus: 200, Body: json.RawMessage(`{`)},
		} {
			data, err := json.Marshal(httpfixture.File{Description: name, Calls: []httpfixture.Call{c}})
			if err != nil {
				data = []byte(`{"description":"body not JSON","calls":[{"method":"GET","path":"/api/v3/time","http_status":200,"body":{`)
			}
			p := filepath.Join(t.TempDir(), "bad.json")
			if err := os.WriteFile(p, data, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := httpfixture.Read(p); err == nil {
				t.Errorf("%s: Read accepted the file", name)
			}
		}
		p := filepath.Join(t.TempDir(), "unknown.json")
		if err := os.WriteFile(p, []byte(`{"description":"d","calls":[],"url":"x"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := httpfixture.Read(p); err == nil {
			t.Error("Read accepted an unknown member")
		}
	})
}

// X1-T110 — Req: FR-217, SRS — Binance §2.6; X1 D-13, X1 P-6. The recorder in front of an upstream that demands a
// marker key in X-MBX-APIKEY, called through a URL with a host: the file holds method, path, the query without
// timestamp and signature, status, the allowed headers and the body with uid replaced; no host, no key, no
// signature. The signed call of the Binance connector through the recorder: internal/connector/binance.
func TestT110_Recorder(t *testing.T) {
	key, signature := randomHex(t, 32), randomHex(t, 32)
	const realUID = 987654321012
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-MBX-APIKEY") != key {
			http.Error(w, `{"code":-2015,"msg":"Invalid API-key, IP, or permissions for action."}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("X-MBX-USED-WEIGHT-1M", "21")
		w.Header().Set("x-sapi-used-ip-weight-1m", "3")
		w.Header().Set("X-MBX-UUID", "a-request-id")
		w.Header().Set("Set-Cookie", "session="+key)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"makerCommission":10,"uid":%d,"nested":{"uid":"%d","list":[{"uid":{"a":[1,2]}}]},"balances":[{"asset":"BTC","free":"0.00100000"}],"note":"<&>"}`, realUID, realUID)
	}))
	defer upstream.Close()

	rec := &httpfixture.Recorder{}
	client := &http.Client{Transport: rec}
	do := func(method, query, apiKey string) int {
		req, err := http.NewRequest(method, upstream.URL+"/api/v3/account?"+query, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-MBX-APIKEY", apiKey)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 200 && !strings.Contains(string(body), fmt.Sprint(realUID)) {
			t.Errorf("the client under test must get the answer as received: %s", body)
		}
		return resp.StatusCode
	}
	if s := do("GET", "omitZeroBalances=true&recvWindow=5000&timestamp=1790000000000&signature="+signature, key); s != 200 {
		t.Fatalf("status %d", s)
	}
	if s := do("GET", "recvWindow=5000&timestamp=1790000000001&signature="+signature, "fictitious-"+key[:8]); s != 401 {
		t.Fatalf("status %d", s)
	}
	f, err := rec.File("Signed account call and a rejected key")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "account.json")
	if err := httpfixture.Write(path, f); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(upstream.URL)
	for _, s := range []string{key, key[:8], signature, upstream.URL, u.Host, u.Port(), "127.0.0.1", "http://", "timestamp",
		"signature", "X-MBX-APIKEY", "X-MBX-UUID", "Set-Cookie", fmt.Sprint(realUID)} {
		if strings.Contains(string(data), s) {
			t.Errorf("the fixture contains %q", s)
		}
	}

	want := []httpfixture.Call{
		{Method: "GET", Path: "/api/v3/account", Query: "omitZeroBalances=true&recvWindow=5000", HTTPStatus: 200,
			Headers: map[string]string{"X-MBX-USED-WEIGHT-1M": "21", "X-SAPI-USED-IP-WEIGHT-1M": "3"},
			Body: json.RawMessage(`{"makerCommission":10,"uid":100000001,"nested":{"uid":100000001,"list":[{"uid":100000001}]},` +
				`"balances":[{"asset":"BTC","free":"0.00100000"}],"note":"\u003c\u0026\u003e"}`)},
		{Method: "GET", Path: "/api/v3/account", Query: "recvWindow=5000", HTTPStatus: 401,
			Body: json.RawMessage(`{"code":-2015,"msg":"Invalid API-key, IP, or permissions for action."}`)},
	}
	if len(f.Calls) != len(want) {
		t.Fatalf("calls = %+v", f.Calls)
	}
	for i, c := range f.Calls {
		w := want[i]
		if c.Method != w.Method || c.Path != w.Path || c.Query != w.Query || c.HTTPStatus != w.HTTPStatus ||
			!maps.Equal(c.Headers, w.Headers) || string(c.Body) != string(w.Body) {
			t.Errorf("call %d:\n got  %+v %s\n want %+v %s", i, c, c.Body, w, w.Body)
		}
	}

	t.Run("a request body is refused", func(t *testing.T) {
		rec := &httpfixture.Recorder{}
		req, _ := http.NewRequest("POST", upstream.URL+"/api/v3/account", strings.NewReader("recvWindow=5000"))
		req.Header.Set("X-MBX-APIKEY", key)
		resp, err := (&http.Client{Transport: rec}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if _, err := rec.File("post"); err == nil {
			t.Error("File accepted a request with a body")
		}
	})
}
