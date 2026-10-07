package evm

import (
	"errors"
	"net/http"
	"strings"

	"github.com/ethereum/go-ethereum/rpc"
)

// tooLargeTexts are parts of the JSON-RPC error messages of eth_getLogs by which a provider rejects a block range,
// a result count or a response size (EC-306). Matched without regard to case. Provider texts differ, so they are
// kept here in one place; each has a test with the full text of its source (toolarge_test.go).
var tooLargeTexts = []struct {
	part   string
	source string
}{
	// Alchemy: "Log response size exceeded. You can make eth_getLogs requests with up to a 2K block range …".
	{"response size", "Alchemy"},
	// Alchemy free tier, geth --rpc.rangelimit, Reth, public endpoints: "… up to a 10 block range",
	// "exceed maximum block range: 10000", "query exceeds max block range 100000", "block range is too wide".
	{"block range", "Alchemy, geth, Reth, public endpoints"},
	// QuickNode: "eth_getLogs and eth_newFilter are limited to a 10,000 blocks range".
	{"blocks range", "QuickNode"},
	// Infura, code -32005: "query returned more than 10000 results".
	{"returned more than", "Infura"},
	// Reth: "query exceeds max results 20000".
	{"max results", "Reth"},
}

// isTooLarge reports whether an error of eth_getLogs rejects the range or the answer as too large: HTTP 413, or a
// JSON-RPC error whose message names a block range, a result count or a response size. It is checked before the
// rate limit: some providers answer -32005 for both.
func isTooLarge(err error) bool {
	var (
		httpErr rpc.HTTPError
		rpcErr  rpc.Error
	)
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode == http.StatusRequestEntityTooLarge
	}
	if !errors.As(err, &rpcErr) {
		return false
	}
	msg := strings.ToLower(rpcErr.Error())
	for _, t := range tooLargeTexts {
		if strings.Contains(msg, t.part) {
			return true
		}
	}
	return false
}

// tooLargeError is an RPC failure of eth_getLogs that rejects the range or the answer as too large: the range is
// halved and the step repeated (UC-303 step 6).
type tooLargeError struct{ *rpcFailure }

func (e *tooLargeError) Unwrap() error { return e.rpcFailure }
