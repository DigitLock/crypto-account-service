package processorapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"unicode/utf8"

	"github.com/DigitLock/crypto-account-service/internal/decision"
)

// Validation of SRS — Card Spend §2.1.1, every endpoint.

// MaxBody is the largest request body; a larger one is 422 INVALID_REQUEST.
const MaxBody = 16 << 10

// maxCardRef is the length of card_ref in characters, as RegisterCard (D-19).
const maxCardRef = 64

var (
	// At most 14 digits before the point and 4 after it: NUMERIC(18,4) of fiat_amount (D-17).
	amountPattern   = regexp.MustCompile(`^(0|[1-9][0-9]{0,13})(\.[0-9]{1,4})?$`)
	currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)
)

// invalidError is a failed validation rule; its message names the rule and never echoes the value.
type invalidError struct{ msg string }

func (e *invalidError) Error() string { return e.msg }

func invalid(msg string) error { return &invalidError{msg: msg} }

// validProcessorID reports whether s is 1 to 64 printable ASCII characters, 0x20 to 0x7E (auth_id, return_id).
func validProcessorID(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for i := range len(s) {
		if s[i] < 0x20 || s[i] > 0x7E {
			return false
		}
	}
	return true
}

// authorizeRequest is a validated POST /v1/authorizations body.
type authorizeRequest struct {
	AuthID   string
	CardRef  string
	Amount   string // without trailing zeros
	Currency string
	Merchant []byte // the merchant object as received; nil when absent
	Hash     []byte // request_hash
}

// parseAuthorize validates body and builds the normalized request and its hash. Unknown fields are ignored.
func parseAuthorize(body []byte) (authorizeRequest, error) {
	fields, err := parseObject(body)
	if err != nil {
		return authorizeRequest{}, err
	}
	var req authorizeRequest
	if req.AuthID, err = stringField(fields, "auth_id"); err != nil {
		return req, err
	}
	if !validProcessorID(req.AuthID) {
		return req, invalid("auth_id must be 1 to 64 printable ASCII characters")
	}
	if req.CardRef, err = stringField(fields, "card_ref"); err != nil {
		return req, err
	}
	if n := utf8.RuneCountInString(req.CardRef); n == 0 || n > maxCardRef {
		return req, invalid("card_ref must be 1 to 64 characters")
	}
	amount, err := stringField(fields, "amount")
	if err != nil {
		return req, err
	}
	if !amountPattern.MatchString(amount) || decision.TrimDecimal(amount) == "0" {
		return req, invalid("amount must be a decimal greater than 0 with at most 14 digits before the point and 4 after it")
	}
	req.Amount = decision.TrimDecimal(amount)
	if req.Currency, err = stringField(fields, "currency"); err != nil {
		return req, err
	}
	if !currencyPattern.MatchString(req.Currency) {
		return req, invalid("currency must be three upper-case letters")
	}

	normalized := map[string]any{
		"auth_id": req.AuthID, "card_ref": req.CardRef, "amount": req.Amount, "currency": req.Currency,
	}
	if raw, ok := fields["merchant"]; ok {
		merchant, err := decodeValue(raw)
		if _, isObject := merchant.(map[string]any); err != nil || !isObject {
			return req, invalid("merchant must be an object")
		}
		normalized["merchant"] = merchant
		req.Merchant = raw
	}
	canonical, err := canonicalJSON(normalized)
	if err != nil {
		return req, invalid("the body cannot be normalized")
	}
	sum := sha256.Sum256(canonical)
	req.Hash = sum[:]
	return req, nil
}

// parseObject decodes a body that is exactly one JSON object.
func parseObject(body []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	var fields map[string]json.RawMessage
	if err := dec.Decode(&fields); err != nil || fields == nil {
		return nil, invalid("the body must be a JSON object")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, invalid("the body must be a JSON object")
	}
	return fields, nil
}

// stringField returns a required string field.
func stringField(fields map[string]json.RawMessage, name string) (string, error) {
	raw, ok := fields[name]
	if !ok {
		return "", invalid(name + " is required")
	}
	var s string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
		return "", invalid(name + " must be a string")
	}
	return s, nil
}

// decodeValue decodes any JSON value, keeping numbers as their literal text.
func decodeValue(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// canonicalJSON encodes v with sorted keys at every level and no whitespace: the normalized request of §2.1.1.
func canonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
