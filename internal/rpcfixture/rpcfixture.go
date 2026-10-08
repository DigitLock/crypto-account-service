// Package rpcfixture records JSON-RPC calls into fixture files and serves them back (SRS — EVM Connector §2.6;
// S3 D-13, S3 D-28). It is used only by tests.
//
// A fixture is one JSON file per case: a description, optionally the context of the recording, and the ordered list
// of calls. The context holds the values a replay needs to send the same requests, such as the addresses of the
// deployment and the account: fictitious or local, never a URL or a key. A call has a method, its
// params and exactly one answer: a result, a JSON-RPC error, or an HTTP status without a JSON-RPC body (such as
// 429), written by hand. A batch request takes one entry per element, in order.
//
// The recorder stores methods, params and answers only: never a URL, a header or a query, because a provider
// URL usually carries an API key. Committed fixtures are recorded from Anvil, never from a public network.
package rpcfixture

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
)

// File is the content of one fixture file.
type File struct {
	Description string            `json:"description"`
	Context     map[string]string `json:"context,omitempty"`
	Calls       []Call            `json:"calls"`
}

// Call is one JSON-RPC call and its answer. Exactly one of Result, Error and HTTPStatus is set.
type Call struct {
	Method     string          `json:"method"`
	Params     json.RawMessage `json:"params"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      *Error          `json:"error,omitempty"`
	HTTPStatus int             `json:"http_status,omitempty"`
}

// Error is a JSON-RPC error answer.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
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
		return File{}, fmt.Errorf("rpcfixture: %s: %w", path, err)
	}
	for i, c := range f.Calls {
		if err := c.check(); err != nil {
			return File{}, fmt.Errorf("rpcfixture: %s: call %d: %w", path, i, err)
		}
	}
	return f, nil
}

// Write writes f as indented JSON.
func Write(path string, f File) error {
	for i, c := range f.Calls {
		if err := c.check(); err != nil {
			return fmt.Errorf("rpcfixture: call %d: %w", i, err)
		}
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func (c Call) check() error {
	answers := 0
	if c.Result != nil {
		answers++
	}
	if c.Error != nil {
		answers++
	}
	if c.HTTPStatus != 0 {
		answers++
		if c.HTTPStatus < 100 || c.HTTPStatus > 599 {
			return fmt.Errorf("http_status %d is not an HTTP status", c.HTTPStatus)
		}
	}
	if c.Method == "" || answers != 1 {
		return errors.New("a call needs a method and exactly one of result, error, http_status")
	}
	return nil
}

// request and response are the JSON-RPC 2.0 messages on the wire.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// parseBody reads a single request or a batch. batch reports whether the body was an array.
func parseBody[T any](body []byte) (items []T, batch bool, err error) {
	body = bytes.TrimSpace(body)
	if len(body) > 0 && body[0] == '[' {
		err = json.Unmarshal(body, &items)
		return items, true, err
	}
	var one T
	if err := json.Unmarshal(body, &one); err != nil {
		return nil, false, err
	}
	return []T{one}, false, nil
}

// Recorder is an http.RoundTripper that records every JSON-RPC call it forwards to Base and the answer. Use it
// as the transport of the http.Client given to rpc.DialOptions with rpc.WithHTTPClient.
type Recorder struct {
	Base http.RoundTripper // nil: http.DefaultTransport

	mu    sync.Mutex
	calls []Call
	err   error
}

// RoundTrip forwards req and records the calls of its body with their answers.
func (r *Recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		body = b
		req = req.Clone(req.Context())
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
	}
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
	r.record(body, resp.StatusCode, answer)
	return resp, nil
}

func (r *Recorder) record(body []byte, status int, answer []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	reqs, _, err := parseBody[request](body)
	if err != nil {
		r.err = errors.Join(r.err, fmt.Errorf("rpcfixture: a request is not JSON-RPC: %w", err))
		return
	}
	if status != http.StatusOK {
		for _, q := range reqs {
			r.calls = append(r.calls, Call{Method: q.Method, Params: params(q.Params), HTTPStatus: status})
		}
		return
	}
	resps, _, err := parseBody[response](answer)
	if err != nil {
		r.err = errors.Join(r.err, fmt.Errorf("rpcfixture: an answer is not JSON-RPC: %w", err))
		return
	}
	byID := make(map[string]response, len(resps))
	for _, a := range resps {
		byID[string(a.ID)] = a
	}
	for _, q := range reqs {
		a, ok := byID[string(q.ID)]
		if !ok {
			r.err = errors.Join(r.err, fmt.Errorf("rpcfixture: no answer to %s", q.Method))
			continue
		}
		c := Call{Method: q.Method, Params: params(q.Params), Result: a.Result, Error: a.Error}
		if c.Error == nil && c.Result == nil {
			c.Result = json.RawMessage("null")
		}
		r.calls = append(r.calls, c)
	}
}

// params gives an absent params member the form of an empty list.
func params(p json.RawMessage) json.RawMessage {
	if len(p) == 0 || string(p) == "null" {
		return json.RawMessage("[]")
	}
	return p
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
