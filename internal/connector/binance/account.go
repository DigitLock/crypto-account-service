package binance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/DigitLock/crypto-account-service/internal/connector"
)

// Code is the source code the connector is registered under.
const Code = "binance"

var _ connector.Connector = (*Connector)(nil)

// Permissions of apiRestrictions that a key may hold when true (UC-201 step 3; X1 D-1). enableReading is also
// required (step 2).
var allowedPermissions = []string{"enableReading", "enableFixReadOnly"}

// Capabilities: the key check reads the permissions of a key (EC-103).
func (c *Connector) Capabilities() connector.Capabilities {
	return connector.Capabilities{PermissionsReadable: true}
}

// CheckAccount checks a key (UC-201): apiRestrictions, then the account. Every request reserves its weight in lim
// first. A key Binance rejects, or one that cannot read, is connector.ErrKeyRejected; a key with any other
// permission, known or unknown, is *connector.KeyNotReadOnlyError with the Binance names. Errors hold no key,
// secret, signature or body of an answer.
func (c *Connector) CheckAccount(ctx context.Context, src connector.Source, cred connector.Credentials, lim connector.Limiter) (connector.AccountInfo, error) {
	if cred.ExchangeKey == nil {
		return connector.AccountInfo{}, &connector.InvalidInputError{Message: "binance takes an exchange key, not a wallet"}
	}
	s, err := c.session(src, cred.ExchangeKey, lim)
	if err != nil {
		return connector.AccountInfo{}, err
	}

	// Step 1.
	body, err := s.call(ctx, endpointRestrictions)
	if err != nil {
		return connector.AccountInfo{}, err
	}
	r, err := parseRestrictions(body)
	if err != nil {
		return connector.AccountInfo{}, err
	}
	// Step 2.
	if !r.reading {
		return connector.AccountInfo{}, fmt.Errorf("binance: enableReading of the key is not true: %w", connector.ErrKeyRejected)
	}
	// Step 3.
	if len(r.beyond) > 0 {
		return connector.AccountInfo{}, &connector.KeyNotReadOnlyError{Permissions: r.beyond}
	}

	// Step 4.
	body, err = s.call(ctx, endpointAccount, param{"omitZeroBalances", "true"})
	if err != nil {
		return connector.AccountInfo{}, err
	}
	uid, err := parseUID(body)
	if err != nil {
		return connector.AccountInfo{}, err
	}

	// Step 5.
	return connector.AccountInfo{Identity: uid, Permissions: []string{connector.PermissionRead}, IPRestricted: r.ipRestrict}, nil
}

// restrictions is what the key check reads from apiRestrictions.
type restrictions struct {
	reading    bool
	beyond     []string // permissions beyond reading that are true, sorted
	ipRestrict *bool    // nil when missing or not a boolean
}

// parseRestrictions reads apiRestrictions without losing a field (X1 D-1): a permission is a boolean field whose name
// starts with enable or permits; any that is true and not allowed is beyond reading. Other fields, such as
// ipRestrict and createTime, or a non-boolean field, are not permissions.
func parseRestrictions(body []byte) (restrictions, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return restrictions{}, fmt.Errorf("binance: %s %s: the answer is not a JSON object", endpointRestrictions.method, endpointRestrictions.path)
	}
	var r restrictions
	for name, raw := range fields {
		var v bool
		isBool := json.Unmarshal(raw, &v) == nil && string(raw) != "null"
		switch {
		case name == "ipRestrict":
			if isBool {
				r.ipRestrict = &v
			}
		case !isBool || !(strings.HasPrefix(name, "enable") || strings.HasPrefix(name, "permits")):
			// Not a permission.
		case name == "enableReading":
			r.reading = v
		case v && !slices.Contains(allowedPermissions, name):
			r.beyond = append(r.beyond, name)
		}
	}
	slices.Sort(r.beyond)
	return r, nil
}

// uidForm is the account identity of Binance: a JSON integer, not negative.
var uidForm = regexp.MustCompile(`^[0-9]+$`)

// parseUID returns the uid of the account answer as its decimal string.
func parseUID(body []byte) (string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return "", fmt.Errorf("binance: %s %s: the answer is not a JSON object", endpointAccount.method, endpointAccount.path)
	}
	uid := strings.TrimSpace(string(fields["uid"]))
	if !uidForm.MatchString(uid) {
		return "", fmt.Errorf("binance: %s %s: the answer has no uid", endpointAccount.method, endpointAccount.path)
	}
	return uid, nil
}

// errNoLedgerStream is the answer of FetchPage: Binance declares no ledger stream in X1.
var errNoLedgerStream = errors.New("binance: no ledger stream in X1")

// FetchPage: Binance has no ledger stream in X1.
func (c *Connector) FetchPage(context.Context, connector.Connection, string, connector.Mode, json.RawMessage) (connector.Page, error) {
	return connector.Page{}, errNoLedgerStream
}
