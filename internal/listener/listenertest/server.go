// Package listenertest is the fake WebSocket JSON-RPC server of phase 6 of docs/test-plan-s2.md. It records every
// request, answers eth_chainId and eth_subscribe, emits logs on demand, sends pings and counts the pongs, and can be closed and
// started again on the same address. It is used only by tests.
//
// The WebSocket protocol (RFC 6455) is implemented here on net/http, enough for the go-ethereum RPC client: the
// handshake, masked client frames, fragmentation, ping, pong and close. No dependency is added.
package listenertest

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Opcodes of RFC 6455 §5.2.
const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

const acceptGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// ChainID is the chain ID the server answers until SetChainID: the local chain of internal/testchain.
const ChainID = 31337

// Call is one JSON-RPC request the server received.
type Call struct {
	Method string
	Params []json.RawMessage
}

// Server is a fake WebSocket JSON-RPC endpoint.
type Server struct {
	// URL is ws://host:port/ followed by a random path, as a provider URL carries its key.
	URL string
	// Path is the random path of URL, without the leading slash.
	Path string
	addr string
	t    testing.TB

	mu     sync.Mutex
	ln     net.Listener
	srv    *http.Server
	conns  map[*conn]struct{}
	calls  []Call
	pongs  int
	nextID int
	// refuse makes eth_subscribe answer a JSON-RPC error.
	refuse  bool
	chainID uint64
}

// conn is one WebSocket connection with its subscriptions.
type conn struct {
	nc   net.Conn
	wmu  sync.Mutex
	subs []string
}

// Start starts the server on a free loopback port. It stops on cleanup.
func Start(t testing.TB) *Server {
	t.Helper()
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	s := &Server{Path: "v2/" + hex.EncodeToString(key), t: t, conns: map[*conn]struct{}{}, chainID: ChainID}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.addr = ln.Addr().String()
	s.URL = "ws://" + s.addr + "/" + s.Path
	s.serve(ln)
	t.Cleanup(s.Close)
	return s
}

func (s *Server) serve(ln net.Listener) {
	srv := &http.Server{Handler: http.HandlerFunc(s.handle), ReadHeaderTimeout: 5 * time.Second}
	s.mu.Lock()
	s.ln, s.srv = ln, srv
	s.mu.Unlock()
	go func() { _ = srv.Serve(ln) }()
}

// Close stops listening and drops every connection without a close frame, as a lost network does.
func (s *Server) Close() {
	s.mu.Lock()
	srv := s.srv
	s.srv, s.ln = nil, nil
	conns := s.conns
	s.conns = map[*conn]struct{}{}
	s.mu.Unlock()
	if srv != nil {
		_ = srv.Close()
	}
	for c := range conns {
		_ = c.nc.Close()
	}
}

// Restart listens again on the same address.
func (s *Server) Restart() {
	s.t.Helper()
	var ln net.Listener
	var err error
	// The port was just released; a short retry covers a slow release.
	for i := 0; i < 50; i++ {
		if ln, err = net.Listen("tcp", s.addr); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		s.t.Fatalf("listenertest: listen again: %v", err)
	}
	s.serve(ln)
}

// Refuse makes every later eth_subscribe fail with a JSON-RPC error, or succeed again.
func (s *Server) Refuse(on bool) {
	s.mu.Lock()
	s.refuse = on
	s.mu.Unlock()
}

// SetChainID sets the chain ID that eth_chainId answers.
func (s *Server) SetChainID(id uint64) {
	s.mu.Lock()
	s.chainID = id
	s.mu.Unlock()
}

// Calls returns every request received so far.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// Subscriptions returns the params of every eth_subscribe received so far.
func (s *Server) Subscriptions() [][]json.RawMessage {
	var out [][]json.RawMessage
	for _, c := range s.Calls() {
		if c.Method == "eth_subscribe" {
			out = append(out, c.Params)
		}
	}
	return out
}

// Active returns the number of live subscriptions.
func (s *Server) Active() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for c := range s.conns {
		n += len(c.subs)
	}
	return n
}

// Emit sends result as a notification of every live subscription and returns how many got it.
func (s *Server) Emit(result any) int {
	s.t.Helper()
	raw, err := json.Marshal(result)
	if err != nil {
		s.t.Fatal(err)
	}
	s.mu.Lock()
	type target struct {
		c  *conn
		id string
	}
	var targets []target
	for c := range s.conns {
		for _, id := range c.subs {
			targets = append(targets, target{c, id})
		}
	}
	s.mu.Unlock()
	n := 0
	for _, tg := range targets {
		msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "eth_subscription",
			"params": map[string]any{"subscription": tg.id, "result": json.RawMessage(raw)}})
		if tg.c.write(opText, msg) == nil {
			n++
		}
	}
	return n
}

// Ping sends a ping frame on every connection and returns how many were sent.
func (s *Server) Ping() int {
	s.mu.Lock()
	conns := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	n := 0
	for _, c := range conns {
		if c.write(opPing, []byte("ping")) == nil {
			n++
		}
	}
	return n
}

// Pongs returns the number of pong frames received.
func (s *Server) Pongs() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pongs
}

// handle performs the handshake of RFC 6455 §4.2 and serves the connection.
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || key == "" {
		http.Error(w, "websocket only", http.StatusBadRequest)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "no hijack", http.StatusInternalServerError)
		return
	}
	nc, rw, err := hj.Hijack()
	if err != nil {
		return
	}
	sum := sha1.Sum([]byte(key + acceptGUID))
	_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n")
	if rw.Flush() != nil {
		_ = nc.Close()
		return
	}
	c := &conn{nc: nc}
	s.mu.Lock()
	if s.srv == nil {
		s.mu.Unlock()
		_ = nc.Close()
		return
	}
	s.conns[c] = struct{}{}
	s.mu.Unlock()
	go s.read(c, rw.Reader)
}

// read serves the frames of one connection until it ends.
func (s *Server) read(c *conn, br *bufio.Reader) {
	defer func() {
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
		_ = c.nc.Close()
	}()
	var message []byte
	for {
		fin, op, payload, err := readFrame(br)
		if err != nil {
			return
		}
		switch op {
		case opPing:
			_ = c.write(opPong, payload)
		case opPong:
			s.mu.Lock()
			s.pongs++
			s.mu.Unlock()
		case opClose:
			_ = c.write(opClose, payload)
			return
		case opText, opBinary, opContinuation:
			message = append(message, payload...)
			if fin {
				s.request(c, message)
				message = nil
			}
		}
	}
}

// request answers one JSON-RPC message, single or batch.
func (s *Server) request(c *conn, body []byte) {
	type req struct {
		ID     json.RawMessage   `json:"id"`
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	var batch []req
	if err := json.Unmarshal(body, &batch); err != nil {
		var one req
		if json.Unmarshal(body, &one) != nil {
			return
		}
		batch = []req{one}
	}
	for _, m := range batch {
		s.mu.Lock()
		s.calls = append(s.calls, Call{Method: m.Method, Params: m.Params})
		refuse, chainID := s.refuse, s.chainID
		s.mu.Unlock()
		answer := map[string]any{"jsonrpc": "2.0", "id": m.ID}
		switch {
		case m.Method == "eth_subscribe" && !refuse:
			s.mu.Lock()
			s.nextID++
			id := "0x" + hex.EncodeToString(binary.BigEndian.AppendUint64(nil, uint64(s.nextID)))
			c.subs = append(c.subs, id)
			s.mu.Unlock()
			answer["result"] = id
		case m.Method == "eth_chainId":
			answer["result"] = "0x" + strconv.FormatUint(chainID, 16)
		case m.Method == "eth_unsubscribe":
			answer["result"] = true
		default:
			answer["error"] = map[string]any{"code": -32601, "message": "refused by the fake server"}
		}
		out, _ := json.Marshal(answer)
		_ = c.write(opText, out)
	}
}

// write sends one unmasked frame (servers do not mask, RFC 6455 §5.1).
func (c *conn) write(op byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	header := []byte{0x80 | op}
	switch n := len(payload); {
	case n < 126:
		header = append(header, byte(n))
	case n <= 0xFFFF:
		header = append(header, 126)
		header = binary.BigEndian.AppendUint16(header, uint16(n))
	default:
		header = append(header, 127)
		header = binary.BigEndian.AppendUint64(header, uint64(n))
	}
	_ = c.nc.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := c.nc.Write(append(header, payload...))
	return err
}

// readFrame reads one frame and unmasks its payload.
func readFrame(br *bufio.Reader) (fin bool, op byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(br, h[:]); err != nil {
		return
	}
	fin, op = h[0]&0x80 != 0, h[0]&0x0F
	masked := h[1]&0x80 != 0
	n := uint64(h[1] & 0x7F)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(br, b[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(br, b[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if n > 1<<24 {
		return false, 0, nil, errors.New("frame too large")
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(br, mask[:]); err != nil {
			return
		}
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(br, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return
}
