// Package httpfixture records HTTP calls of an exchange API into fixture files and serves them back (SRS — Binance
// §2.6; X1 D-13). It is the HTTP counterpart of internal/rpcfixture and is used only by tests.
//
// A fixture is one JSON file per case: a description, optionally the context of the recording, and the ordered list
// of calls. The context holds values a replay needs: fictitious, never a URL, a host or a key. A call has the
// method, the path, the query without timestamp and signature, the HTTP status of the answer, the used-weight
// headers and Retry-After of the answer, and the body of the answer: JSON as received, or empty.
//
// The recorder stores exactly these members: never the host, the URL, the X-MBX-APIKEY header or the signature. In
// every JSON body it replaces the value of each field named "uid" by FictitiousUID.
package httpfixture

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
)

// FictitiousUID replaces the value of every field named "uid" in a recorded body (FR-217).
const FictitiousUID = 100000001

// Headers of an answer that a fixture keeps: the used-weight headers and Retry-After (SRS — Binance §2.1.1 Rate
// limits). Any other header is dropped by the recorder and refused by Read.
var Headers = []string{"X-MBX-USED-WEIGHT-1M", "X-SAPI-USED-IP-WEIGHT-1M", "X-SAPI-USED-UID-WEIGHT-1M", "Retry-After"}

// Parameters of a signed request that a fixture never holds.
const (
	paramTimestamp = "timestamp"
	paramSignature = "signature"
)

// File is the content of one fixture file.
type File struct {
	Description string            `json:"description"`
	Context     map[string]string `json:"context,omitempty"`
	Calls       []Call            `json:"calls"`
}

// Call is one request and its answer.
type Call struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	// Query is the parameter string as sent, without timestamp and signature; empty without parameters.
	Query      string            `json:"query"`
	HTTPStatus int               `json:"http_status"`
	Headers    map[string]string `json:"headers,omitempty"`
	// Body is the JSON answer as received; nil when the answer has no body or no JSON body.
	Body json.RawMessage `json:"body,omitempty"`
}

// Read reads and checks a fixture file.
func Read(path string) (File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return File{}, err
	}
	var f File
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return File{}, fmt.Errorf("httpfixture: %s: %w", path, err)
	}
	if err := f.check(); err != nil {
		return File{}, fmt.Errorf("httpfixture: %s: %w", path, err)
	}
	return f, nil
}

// Write writes f as indented JSON.
func Write(path string, f File) error {
	if err := f.check(); err != nil {
		return fmt.Errorf("httpfixture: %w", err)
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func (f File) check() error {
	if f.Description == "" {
		return errors.New("a fixture needs a description")
	}
	for i, c := range f.Calls {
		if err := c.check(); err != nil {
			return fmt.Errorf("call %d: %w", i, err)
		}
	}
	return nil
}

func (c Call) check() error {
	if c.Method == "" || !strings.HasPrefix(c.Path, "/") || strings.ContainsAny(c.Path, "?#") {
		return errors.New("a call needs a method and a path that starts with / and has no query")
	}
	if c.HTTPStatus < 100 || c.HTTPStatus > 599 {
		return fmt.Errorf("http_status %d is not an HTTP status", c.HTTPStatus)
	}
	if Query(c.Query) != c.Query {
		return errors.New("the query holds timestamp or signature")
	}
	for name := range c.Headers {
		if canonicalHeader(name) != name {
			return fmt.Errorf("header %q is not one of %v", name, Headers)
		}
	}
	if len(c.Body) > 0 && !json.Valid(c.Body) {
		return errors.New("the body is not JSON")
	}
	return nil
}

// canonicalHeader returns the spelling of Headers for name, or "" when name is not one of them.
func canonicalHeader(name string) string {
	for _, h := range Headers {
		if strings.EqualFold(h, name) {
			return h
		}
	}
	return ""
}

// Query returns a raw query without its timestamp and signature parameters; the other parameters keep their order
// and encoding.
func Query(raw string) string {
	if raw == "" {
		return ""
	}
	var kept []string
	for _, p := range strings.Split(raw, "&") {
		key, _, _ := strings.Cut(p, "=")
		if key == paramTimestamp || key == paramSignature {
			continue
		}
		kept = append(kept, p)
	}
	return strings.Join(kept, "&")
}

// Recorder is an http.RoundTripper that records every request it forwards to Base and the answer. Use it as the
// transport of the HTTP client under test.
type Recorder struct {
	Base http.RoundTripper // nil: http.DefaultTransport

	mu    sync.Mutex
	calls []Call
	err   error
}

// RoundTrip forwards req and records it with its answer. A request with a body is forwarded but not recorded and
// makes File fail: the parameters of a request go in the query (SRS — Binance §2.1.1 Signature).
func (r *Recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	hasBody := req.Body != nil && req.Body != http.NoBody && req.ContentLength != 0
	base := r.Base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	answer, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(answer))

	r.mu.Lock()
	defer r.mu.Unlock()
	if hasBody {
		r.err = errors.Join(r.err, fmt.Errorf("httpfixture: %s %s has a request body", req.Method, req.URL.Path))
		return resp, nil
	}
	c := Call{Method: req.Method, Path: req.URL.Path, Query: Query(req.URL.RawQuery), HTTPStatus: resp.StatusCode}
	for name, values := range resp.Header {
		if h := canonicalHeader(name); h != "" && len(values) > 0 {
			if c.Headers == nil {
				c.Headers = map[string]string{}
			}
			c.Headers[h] = values[0]
		}
	}
	if len(bytes.TrimSpace(answer)) > 0 && json.Valid(answer) {
		body, err := replaceUID(answer)
		if err != nil {
			r.err = errors.Join(r.err, fmt.Errorf("httpfixture: %s %s: %w", req.Method, req.URL.Path, err))
			return resp, nil
		}
		c.Body = body
	}
	r.calls = append(r.calls, c)
	return resp, nil
}

// File returns the recorded calls as a fixture, or the first recording error.
func (r *Recorder) File(description string) (File, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return File{}, r.err
	}
	return File{Description: description, Calls: append([]Call(nil), r.calls...)}, nil
}

// replaceUID returns a JSON document with the value of every member named "uid", at any depth, replaced by
// FictitiousUID. The order of the members is kept; the output is compact.
func replaceUID(doc []byte) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	var out bytes.Buffer
	if err := copyValue(dec, &out); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after the JSON body")
	}
	return out.Bytes(), nil
}

// copyValue copies the next JSON value of dec to out, replacing the value of each member named "uid".
func copyValue(dec *json.Decoder, out *bytes.Buffer) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return writeScalar(out, tok)
	}
	out.WriteString(delim.String())
	closing := json.Delim('}')
	if delim == '[' {
		closing = ']'
	}
	for i := 0; dec.More(); i++ {
		if i > 0 {
			out.WriteByte(',')
		}
		if delim == '{' {
			keyTok, err := dec.Token()
			if err != nil {
				return err
			}
			key, _ := keyTok.(string)
			if err := writeScalar(out, key); err != nil {
				return err
			}
			out.WriteByte(':')
			if key == "uid" {
				if err := skipValue(dec); err != nil {
					return err
				}
				fmt.Fprintf(out, "%d", FictitiousUID)
				continue
			}
		}
		if err := copyValue(dec, out); err != nil {
			return err
		}
	}
	if tok, err := dec.Token(); err != nil || tok != closing {
		return fmt.Errorf("malformed JSON: %v", err)
	}
	out.WriteString(closing.String())
	return nil
}

// skipValue reads the next JSON value of dec and drops it.
func skipValue(dec *json.Decoder) error {
	var v json.RawMessage
	return dec.Decode(&v)
}

func writeScalar(out *bytes.Buffer, tok json.Token) error {
	data, err := json.Marshal(tok)
	if err != nil {
		return err
	}
	out.Write(data)
	return nil
}
