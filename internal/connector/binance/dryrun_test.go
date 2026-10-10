package binance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/migrations"
)

// The dry run of the real account (X1 D-43; X1-T601, X1-T605, X1-T607): the key check and the snapshot once, nothing
// stored, no connection created. Its report holds statuses, counts, field names of apiRestrictions and used-weight
// headers: never a uid, an asset code, an amount, the key, a signature or a URL. Answers are kept in memory only.

// observedCall is one answer the dry run saw.
type observedCall struct {
	method, path string
	status       int
	header       http.Header
	body         []byte
}

// observer is the transport of the dry run: it keeps every answer in memory for the report.
type observer struct {
	base  http.RoundTripper
	mu    sync.Mutex
	calls []observedCall
}

func (o *observer) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := o.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	o.mu.Lock()
	o.calls = append(o.calls, observedCall{method: req.Method, path: req.URL.Path, status: resp.StatusCode, header: resp.Header.Clone(), body: body})
	o.mu.Unlock()
	return resp, nil
}

// dryRun is the result of the dry run.
type dryRun struct {
	calls    []observedCall
	offset   time.Duration
	info     connector.AccountInfo
	checkErr error
	snapRun  bool
	snap     connector.Snapshot
	snapErr  error
	aliases  []connector.Alias
}

// runDryRun runs the key check and, when it passes, the snapshot, through an observing transport.
func runDryRun(ctx context.Context, c *Connector, src connector.Source, key *connector.ExchangeKey, lim connector.Limiter) dryRun {
	base := c.client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	o := &observer{base: base}
	c.client.Transport = o
	defer func() { c.client.Transport = base }()

	d := dryRun{aliases: src.Aliases}
	d.info, d.checkErr = c.CheckAccount(ctx, src, connector.Credentials{ExchangeKey: key}, lim)
	if d.checkErr == nil {
		d.snapRun = true
		d.snap, d.snapErr = c.FetchSnapshot(ctx, connector.Connection{Source: src, Key: key, Limiter: lim})
	}
	d.offset = c.clockOf(ParseConfig(src).BaseURL).get()
	o.mu.Lock()
	d.calls = append([]observedCall(nil), o.calls...)
	o.mu.Unlock()
	return d
}

// knownPermissions are the permission fields of apiRestrictions as of 2026-10-09 (UC-201 step 3).
var knownPermissions = []string{
	"enableReading", "enableFixReadOnly", "enableWithdrawals", "enableInternalTransfer", "permitsUniversalTransfer",
	"enableSpotAndMarginTrading", "enableMargin", "enableFutures", "enableVanillaOptions", "enablePortfolioMarginTrading",
	"enableFixApiTrade",
}

// errorClass names a failure by its kind, never by its text: a text may name an asset.
func errorClass(err error) string {
	var notReadOnly *connector.KeyNotReadOnlyError
	var rateLimit *connector.RateLimitError
	switch {
	case errors.Is(err, connector.ErrKeyRejected):
		return "rejected by Binance"
	case errors.As(err, &notReadOnly):
		return "not read-only: " + strings.Join(notReadOnly.Permissions, ", ")
	case errors.As(err, &rateLimit):
		return "rate limit, pause " + rateLimit.Pause.String()
	case errors.Is(err, connector.ErrUnreachable):
		return "unreachable"
	default:
		return "other failure"
	}
}

// lastBodies returns the bodies of the successful answers of a path, in order.
func (d dryRun) bodies(path string) [][]byte {
	var out [][]byte
	for _, c := range d.calls {
		if c.path == path && c.status == http.StatusOK {
			out = append(out, c.body)
		}
	}
	return out
}

// lines is the report of the dry run.
func (d dryRun) lines() []string {
	out := []string{"dry run of the key check and the snapshot: nothing stored, no connection created"}
	for _, ep := range endpoints {
		var statuses []string
		for _, c := range d.calls {
			if c.method == ep.method && c.path == ep.path {
				if s := fmt.Sprint(c.status); !slices.Contains(statuses, s) {
					statuses = append(statuses, s)
				}
			}
		}
		if len(statuses) == 0 {
			statuses = []string{"not called"}
		}
		out = append(out, fmt.Sprintf("endpoint %s %s: %s", ep.method, ep.path, strings.Join(statuses, ", ")))
	}
	out = append(out, fmt.Sprintf("time offset (local minus Binance): %d ms", d.offset.Milliseconds()))

	// Key check.
	if d.checkErr != nil {
		out = append(out, "key check: "+errorClass(d.checkErr))
	} else {
		out = append(out, "key check: accepted, permissions "+strings.Join(d.info.Permissions, ","))
	}
	ip := "not reported"
	if d.info.IPRestricted != nil {
		ip = fmt.Sprint(*d.info.IPRestricted)
	}
	out = append(out, "ip_restricted: "+ip)
	var unknown, nonBoolean []string
	if bodies := d.bodies(endpointRestrictions.path); len(bodies) > 0 {
		var fields map[string]json.RawMessage
		if json.Unmarshal(bodies[len(bodies)-1], &fields) == nil {
			for name, raw := range fields {
				if !strings.HasPrefix(name, "enable") && !strings.HasPrefix(name, "permits") {
					continue
				}
				var b bool
				switch {
				case json.Unmarshal(raw, &b) != nil || string(raw) == "null":
					nonBoolean = append(nonBoolean, name)
				case !slices.Contains(knownPermissions, name):
					unknown = append(unknown, name)
				}
			}
		}
	}
	out = append(out, "unknown permission flags: "+listOrNone(unknown), "enable/permits fields that are not booleans: "+listOrNone(nonBoolean))

	// Snapshot.
	switch {
	case !d.snapRun:
		out = append(out, "snapshot: not run, the key check did not pass")
	case d.snapErr != nil:
		out = append(out, "snapshot: "+errorClass(d.snapErr))
	default:
		out = append(out, "snapshot: complete")
	}
	counts := map[string]int{}
	natives := map[string]bool{}
	for _, b := range d.snap.Balances {
		counts[b.AccountType]++
		natives[b.NativeAsset] = true
	}
	out = append(out, fmt.Sprintf("assets per account type: SPOT %d, FUNDING %d, EARN_FLEXIBLE %d, EARN_LOCKED %d",
		counts[AccountSpot], counts[AccountFunding], counts[AccountEarnFlexible], counts[AccountEarnLocked]))

	// Wrapper assets (SRS — Binance §4 issue 2): LD codes of the spot answer, before the wrapper rule.
	ld, withPosition, equal := d.wrappers()
	out = append(out, fmt.Sprintf("spot codes starting with LD: %d; with a flexible position of the rest of the code: %d; "+
		"of them with an amount equal to that position: %d", ld, withPosition, equal))
	unmapped := 0
	for n := range natives {
		if !slices.ContainsFunc(d.aliases, func(a connector.Alias) bool { return a.NativeAsset == n }) {
			unmapped++
		}
	}
	out = append(out, fmt.Sprintf("assets without an alias row: %d", unmapped))

	// Used-weight headers of the /sapi endpoints (X1-T607): names and values, never a body.
	for _, ep := range endpoints[2:] {
		var headers []string
		for _, c := range d.calls {
			if c.path != ep.path {
				continue
			}
			for name, values := range c.header {
				if strings.HasPrefix(strings.ToUpper(name), "X-SAPI-USED-") || strings.HasPrefix(strings.ToUpper(name), "X-MBX-USED-") {
					headers = append(headers, strings.ToUpper(name)+"="+strings.Join(values, ","))
				}
			}
		}
		slices.Sort(headers)
		out = append(out, fmt.Sprintf("used-weight headers of %s %s: %s", ep.method, ep.path, listOrNone(slices.Compact(headers))))
	}
	return out
}

func listOrNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

// wrappers counts the spot codes LD + X of the last account answer; of them those whose X has a flexible position;
// of them those whose spot amount (free + locked) equals the sum of the positions of X.
func (d dryRun) wrappers() (ld, withPosition, equal int) {
	accounts := d.bodies(endpointAccount.path)
	if len(accounts) == 0 {
		return 0, 0, 0
	}
	var account struct {
		Balances []struct{ Asset, Free, Locked string } `json:"balances"`
	}
	if json.Unmarshal(accounts[len(accounts)-1], &account) != nil {
		return 0, 0, 0
	}
	flexible := map[string]*big.Rat{}
	for _, body := range d.bodies(endpointFlexible.path) {
		var page struct {
			Rows []struct{ Asset, TotalAmount string } `json:"rows"`
		}
		if json.Unmarshal(body, &page) != nil {
			continue
		}
		for _, r := range page.Rows {
			if flexible[r.Asset] == nil {
				flexible[r.Asset] = new(big.Rat)
			}
			if v, ok := new(big.Rat).SetString(r.TotalAmount); ok {
				flexible[r.Asset].Add(flexible[r.Asset], v)
			}
		}
	}
	for _, b := range account.Balances {
		rest, ok := strings.CutPrefix(b.Asset, wrapperPrefix)
		if !ok || rest == "" {
			continue
		}
		ld++
		position, ok := flexible[rest]
		if !ok {
			continue
		}
		withPosition++
		free, ok1 := new(big.Rat).SetString(b.Free)
		locked, ok2 := new(big.Rat).SetString(b.Locked)
		if ok1 && ok2 && new(big.Rat).Add(free, locked).Cmp(position) == 0 {
			equal++
		}
	}
	return ld, withPosition, equal
}

// aliasRow is a row of the VALUES list of migration 000010: ('CODE'), a quote doubled.
var aliasRow = regexp.MustCompile(`^\s*\('((?:[^']|'')*)'\),?$`)

// migrationAliases returns the identity alias rows of binance of migration 000010, as the engine loads them.
func migrationAliases() ([]connector.Alias, error) {
	data, err := fs.ReadFile(migrations.FS, "000010_binance_source.up.sql")
	if err != nil {
		return nil, err
	}
	var out []connector.Alias
	for _, line := range strings.Split(string(data), "\n") {
		if m := aliasRow.FindStringSubmatch(line); m != nil {
			code := strings.ReplaceAll(m[1], "''", "'")
			out = append(out, connector.Alias{NativeAsset: code, Asset: code})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no alias row in migration 000010")
	}
	return out, nil
}
