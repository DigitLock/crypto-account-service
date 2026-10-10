package binance

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strconv"
	"strings"
)

// Parameters a signed request adds (SRS — Binance §2.1.1).
const (
	paramRecvWindow = "recvWindow"
	paramTimestamp  = "timestamp"
	paramSignature  = "signature"
)

// param is one query parameter. A request keeps its parameters in order: the signature covers the query as sent.
type param struct {
	key, value string
}

// encode returns the query of ps in order, with keys and values percent-encoded.
func encode(ps []param) string {
	var b strings.Builder
	for i, p := range ps {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(p.key))
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(p.value))
	}
	return b.String()
}

// signature is the HMAC-SHA256 of payload with the API secret, in hex (SRS — Binance §2.1.1 Signature; X1 D-4).
func signature(secret, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// signedQuery returns the query of a signed request: the parameters of the request, recvWindow and timestamp, then
// signature as the last parameter. The parameters of every method, POST included, go in the query: the request has
// no body. The result holds the signature: it is never logged or returned in an error (redactSignature).
func signedQuery(ps []param, recvWindowMS int, timestampMS int64, secret string) string {
	q := encode(append(append([]param(nil), ps...),
		param{paramRecvWindow, strconv.Itoa(recvWindowMS)},
		param{paramTimestamp, strconv.FormatInt(timestampMS, 10)}))
	return q + "&" + paramSignature + "=" + signature(secret, q)
}

// redactSignature returns a query without its signature parameter: the form of a signed query in a log line
// (SRS — Binance §3.2 Security).
func redactSignature(query string) string {
	var kept []string
	for _, p := range strings.Split(query, "&") {
		if key, _, _ := strings.Cut(p, "="); key != paramSignature {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "&")
}
