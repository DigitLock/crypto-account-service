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
// API key and reports READ; it declares the streams balances and ops; ops holds a small fixed fictitious
// history in two pages and balances a snapshot of BTC and USDT.
type Connector struct {
	mu       sync.Mutex
	caps     connector.Capabilities
	check    CheckFunc
	streams  []connector.Stream
	budgets  []connector.Budget
	costs    map[string]Cost
	pages    map[string][][]connector.Entry
	snapshot connector.Snapshot
	notFinal map[string]bool
	failures map[string][]error
	before   BeforeFunc
	calls    []Call
}

// Cost is the reservation of one request of a stream.
type Cost struct {
	Budget string
	Units  int
}

// Call is a recorded call of the fake.
type Call struct {
	// Kind is check, page or snapshot.
	Kind       string
	Connection string
	// Account is the account of the key of a check call: AccountOf of the API key.
	Account string
	Stream  string
	// Page is the index of the page the cursor pointed at.
	Page int
	// Reserved: the cost was reserved in the limiter before the request.
	Reserved bool
	Limiter  connector.Limiter
}

// BeforeFunc runs inside a check, page or snapshot call after its reservation; an error is the result of
// the call.
type BeforeFunc func(ctx context.Context, call Call) error

// Default budget of the fake: requests per minute; each call costs 1.
const DefaultBudget = "requests"

// DefaultTime is the time of the fixed history and snapshot of the fake.
var DefaultTime = time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)

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
	c.streams = nil // not scripted: DefaultStreams with the intervals of sources.config
	c.budgets = []connector.Budget{{Name: DefaultBudget, Units: 1200, Window: time.Minute}}
	c.costs = map[string]Cost{}
	c.pages = map[string][][]connector.Entry{"ops": DefaultHistory()}
	c.snapshot = DefaultSnapshot()
	c.notFinal = map[string]bool{}
	c.failures = map[string][]error{}
	c.before = nil
	c.calls = nil
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

// CheckAccount implements connector.Connector: it reserves the cost of the stream name "check" in lim, records
// the call and runs the scripted check.
func (c *Connector) CheckAccount(ctx context.Context, src connector.Source, cred connector.Credentials, lim connector.Limiter) (connector.AccountInfo, error) {
	conn := connector.Connection{Source: src, Key: cred.ExchangeKey, Limiter: lim}
	if err := c.request(ctx, conn, "check", CheckStream, 0); err != nil {
		return connector.AccountInfo{}, err
	}
	c.mu.Lock()
	check := c.check
	c.mu.Unlock()
	return check(ctx, src, cred)
}

// CheckStream is the name under which the costs, failures and calls of the account check are kept.
const CheckStream = "check"

// Streams implements connector.Connector.
// Without a script the intervals come from sources.config of the source (connector.SyncInterval); scripted
// streams are returned as they are.
func (c *Connector) Streams(_ context.Context, src connector.Source, _ connector.AccountInfo) ([]connector.Stream, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.streams != nil {
		return append([]connector.Stream(nil), c.streams...), nil
	}
	streams := DefaultStreams()
	for i := range streams {
		streams[i].Interval = connector.SyncInterval(src, streams[i].Family)
	}
	return streams, nil
}

// DefaultStreams are the streams of the fake without a script: balances, then ops, each with an empty cursor,
// with the default intervals of SRS — Core §3.1.
func DefaultStreams() []connector.Stream {
	empty := json.RawMessage(`{}`)
	return []connector.Stream{
		{Name: "balances", Family: "balances", Interval: connector.DefaultBalanceInterval, FirstMode: connector.ModeIncremental, FirstCursor: empty},
		{Name: "ops", Family: "ops", Interval: connector.DefaultLedgerInterval, FirstMode: connector.ModeBackfill, FirstCursor: empty},
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

// SetBudgets scripts the budgets of the source.
func (c *Connector) SetBudgets(budgets []connector.Budget) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.budgets = budgets
}

// SetCost scripts the reservation of each request of a stream.
func (c *Connector) SetCost(stream string, cost Cost) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.costs[stream] = cost
}

// SetPages scripts the pages of a ledger stream: the cursor is the index of the next page.
func (c *Connector) SetPages(stream string, pages [][]connector.Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pages[stream] = pages
}

// SetSnapshot scripts the snapshot of the balance stream.
func (c *Connector) SetSnapshot(s connector.Snapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snapshot = s
}

// SetNotFinal holds a record back as not final (EC-110). A page with a held record does not move its cursor
// past that page, so a later run reads it again.
func (c *Connector) SetNotFinal(externalID string, notFinal bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.notFinal[externalID] = notFinal
}

// FailNext makes the next calls of a stream fail with errs, one per call, in order. A *RateLimitError is
// reported to the limiter as the source would.
func (c *Connector) FailNext(stream string, errs ...error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures[stream] = append(c.failures[stream], errs...)
}

// SetBefore scripts a hook that runs in every check, page and snapshot call.
func (c *Connector) SetBefore(fn BeforeFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.before = fn
}

// Calls returns the recorded calls.
func (c *Connector) Calls() []Call {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Call(nil), c.calls...)
}

// Budgets implements connector.Connector.
func (c *Connector) Budgets(connector.Source) []connector.Budget {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]connector.Budget(nil), c.budgets...)
}

type pageCursor struct {
	Page int `json:"page"`
}

// FetchPage implements connector.Connector.
func (c *Connector) FetchPage(ctx context.Context, conn connector.Connection, stream string, mode connector.Mode, cursor json.RawMessage) (connector.Page, error) {
	var cur pageCursor
	if len(cursor) > 0 {
		if err := json.Unmarshal(cursor, &cur); err != nil {
			return connector.Page{}, errors.New("fake: unreadable cursor")
		}
	}
	if err := c.request(ctx, conn, "page", stream, cur.Page); err != nil {
		return connector.Page{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	pages := c.pages[stream]
	if cur.Page >= len(pages) {
		return connector.Page{Cursor: encodeCursor(cur.Page), Mode: connector.ModeIncremental}, nil
	}
	var entries []connector.Entry
	held := false
	for _, e := range pages[cur.Page] {
		if c.notFinal[e.ExternalID] {
			held = true
			continue
		}
		entries = append(entries, e)
	}
	next := cur.Page + 1
	if held {
		next = cur.Page
	}
	page := connector.Page{Entries: entries, Cursor: encodeCursor(next), Mode: mode, More: !held && next < len(pages)}
	if next >= len(pages) {
		page.Mode = connector.ModeIncremental
	}
	return page, nil
}

// FetchSnapshot implements connector.Connector.
func (c *Connector) FetchSnapshot(ctx context.Context, conn connector.Connection) (connector.Snapshot, error) {
	if err := c.request(ctx, conn, "snapshot", "balances", 0); err != nil {
		return connector.Snapshot{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	snap := c.snapshot
	snap.Balances = append([]connector.Balance(nil), snap.Balances...)
	return snap, nil
}

// request reserves the cost of a request, records the call and returns a scripted failure.
func (c *Connector) request(ctx context.Context, conn connector.Connection, kind, stream string, page int) error {
	c.mu.Lock()
	cost, ok := c.costs[stream]
	if !ok {
		cost = Cost{Budget: DefaultBudget, Units: 1}
	}
	c.mu.Unlock()

	reserved := false
	if conn.Limiter != nil {
		if err := conn.Limiter.Reserve(ctx, cost.Budget, cost.Units); err != nil {
			return err
		}
		reserved = true
	}

	c.mu.Lock()
	call := Call{Kind: kind, Connection: conn.ID, Stream: stream, Page: page, Reserved: reserved, Limiter: conn.Limiter}
	if conn.Key != nil {
		call.Account = AccountOf(conn.Key.APIKey.Value())
	}
	c.calls = append(c.calls, call)
	before := c.before
	var fail error
	if q := c.failures[stream]; len(q) > 0 {
		fail, c.failures[stream] = q[0], q[1:]
	}
	c.mu.Unlock()

	if before != nil {
		if err := before(ctx, call); err != nil {
			return err
		}
	}
	var rateLimit *connector.RateLimitError
	if errors.As(fail, &rateLimit) && conn.Limiter != nil {
		conn.Limiter.Pause(rateLimit.Budget, rateLimit.Pause)
	}
	return fail
}

func encodeCursor(page int) json.RawMessage {
	b, _ := json.Marshal(pageCursor{Page: page})
	return b
}

// NewEntry returns a fictitious entry; its raw record is built from its fields.
func NewEntry(externalID, leg, typ, direction, asset, amount string, at time.Time) connector.Entry {
	raw, _ := json.Marshal(map[string]string{
		"id": externalID, "leg": leg, "type": typ, "side": direction, "coin": asset, "qty": amount,
		"time": at.UTC().Format(time.RFC3339),
	})
	return connector.Entry{
		ExternalID: externalID, Leg: leg, Type: typ, Direction: direction, NativeAsset: asset,
		Amount: amount, OccurredAt: at, Raw: raw,
	}
}

// DefaultHistory is the fixed fictitious history of the stream ops: a BTC and a USDT deposit and a trade with
// base, quote and fee legs, then a withdrawal. It adds up to DefaultSnapshot: BTC 0.5 + 0.0125 = 0.5125;
// USDT 2494 - 1493.75 - 1.49375 - 100 = 898.75625.
func DefaultHistory() [][]connector.Entry {
	t := DefaultTime
	return [][]connector.Entry{
		{
			NewEntry("dep-1", "SINGLE", "DEPOSIT", "IN", "BTC", "0.5", t),
			NewEntry("dep-2", "SINGLE", "DEPOSIT", "IN", "USDT", "2494", t.Add(30*time.Minute)),
			NewEntry("trade-1", "BASE", "TRADE", "IN", "BTC", "0.0125", t.Add(time.Hour)),
			NewEntry("trade-1", "QUOTE", "TRADE", "OUT", "USDT", "1493.75", t.Add(time.Hour)),
			NewEntry("trade-1", "FEE", "FEE", "OUT", "USDT", "1.49375", t.Add(time.Hour)),
		},
		{
			NewEntry("wd-1", "SINGLE", "WITHDRAWAL", "OUT", "USDT", "100", t.Add(2*time.Hour)),
		},
	}
}

// DefaultSnapshot is the fixed fictitious snapshot of the stream balances.
func DefaultSnapshot() connector.Snapshot {
	return connector.Snapshot{
		TakenAt: DefaultTime.Add(3 * time.Hour),
		Balances: []connector.Balance{
			{AccountType: "SPOT", NativeAsset: "BTC", Free: "0.5125", Locked: "0"},
			{AccountType: "SPOT", NativeAsset: "USDT", Free: "898.75625", Locked: "0"},
		},
	}
}
