// Package fake is the scripted connector of tests and demos (SRS — Core §2.1.1 Connector contract).
// Fictitious data only. In a running server it is registered only with ENABLE_FAKE_SOURCE.
package fake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/DigitLock/crypto-account-service/internal/connector"
)

// Code is the source code of the fake connector.
const Code = "fake"

// CheckFunc scripts the account check.
type CheckFunc func(ctx context.Context, src connector.Source, cred connector.Credentials) (connector.AccountInfo, error)

// Connector is the fake connector. Without a script it accepts any key, derives the account from the
// API key and reports READ; it declares the streams balances and ops.
type Connector struct {
	mu      sync.Mutex
	caps    connector.Capabilities
	check   CheckFunc
	streams []connector.Stream
}

var _ connector.Connector = (*Connector)(nil)

// New returns the fake connector without a script.
func New() *Connector {
	c := &Connector{}
	c.Reset()
	return c
}

// Reset drops the script.
func (c *Connector) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.caps = connector.Capabilities{PermissionsReadable: true}
	c.check = defaultCheck
	c.streams = DefaultStreams()
}

// SetCapabilities scripts the capabilities.
func (c *Connector) SetCapabilities(caps connector.Capabilities) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.caps = caps
}

// SetCheck scripts the account check.
func (c *Connector) SetCheck(fn CheckFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.check = fn
}

// SetCheckResult scripts the account check to return info and err.
func (c *Connector) SetCheckResult(info connector.AccountInfo, err error) {
	c.SetCheck(func(context.Context, connector.Source, connector.Credentials) (connector.AccountInfo, error) {
		return info, err
	})
}

// SetStreams scripts the declared streams.
func (c *Connector) SetStreams(streams []connector.Stream) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.streams = streams
}

// Capabilities implements connector.Connector.
func (c *Connector) Capabilities() connector.Capabilities {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.caps
}

// CheckAccount implements connector.Connector.
func (c *Connector) CheckAccount(ctx context.Context, src connector.Source, cred connector.Credentials) (connector.AccountInfo, error) {
	c.mu.Lock()
	check := c.check
	c.mu.Unlock()
	return check(ctx, src, cred)
}

// Streams implements connector.Connector.
func (c *Connector) Streams(context.Context, connector.Source, connector.AccountInfo) ([]connector.Stream, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]connector.Stream(nil), c.streams...), nil
}

// DefaultStreams are the streams of the fake without a script: balances, then ops, each with an empty cursor.
func DefaultStreams() []connector.Stream {
	empty := json.RawMessage(`{}`)
	return []connector.Stream{
		{Name: "balances", Family: "balances", Interval: 15 * time.Minute, FirstMode: connector.ModeIncremental, FirstCursor: empty},
		{Name: "ops", Family: "ops", Interval: time.Hour, FirstMode: connector.ModeBackfill, FirstCursor: empty},
	}
}

// AccountOf is the account the fake derives from an API key: "fake-" and the first 16 hexadecimal
// characters of the SHA-256 of the key.
func AccountOf(apiKey string) string {
	sum := sha256.Sum256([]byte(apiKey))
	return "fake-" + hex.EncodeToString(sum[:])[:16]
}

func defaultCheck(_ context.Context, _ connector.Source, cred connector.Credentials) (connector.AccountInfo, error) {
	if cred.ExchangeKey == nil {
		return connector.AccountInfo{}, errors.New("fake: an exchange key is required")
	}
	return connector.AccountInfo{
		Identity:    AccountOf(cred.ExchangeKey.APIKey.Value()),
		Permissions: []string{connector.PermissionRead},
	}, nil
}
