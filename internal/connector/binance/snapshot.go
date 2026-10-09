package binance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/DigitLock/crypto-account-service/internal/connector"
)

// Account types of a snapshot (SRS — Core §2.1.3; UC-202).
const (
	AccountSpot         = "SPOT"
	AccountFunding      = "FUNDING"
	AccountEarnFlexible = "EARN_FLEXIBLE"
	AccountEarnLocked   = "EARN_LOCKED"
)

// accountOrder is the order of the account types in a snapshot.
var accountOrder = []string{AccountSpot, AccountFunding, AccountEarnFlexible, AccountEarnLocked}

// Paging of the Earn positions (SRS — Binance §2.1.2): pages of earnPageSize rows, current from 1. A read of more
// than maxEarnPages pages of one endpoint fails the snapshot.
const (
	earnPageSize = 100
	maxEarnPages = 100
)

// wrapperPrefix is the prefix of a wrapper asset of a flexible Earn position in the spot wallet (SRS — Binance §1).
const wrapperPrefix = "LD"

// Streams declares the stream balances of a connection: family balances, the interval sync_interval.balances of
// sources.config, cursor {} (UC-202 trigger; X1 D-33). No request.
func (c *Connector) Streams(_ context.Context, src connector.Source, _ connector.AccountInfo) ([]connector.Stream, error) {
	return []connector.Stream{{
		Name: FamilyBalances, Family: FamilyBalances, Interval: ParseConfig(src).BalancesInterval,
		FirstMode: connector.ModeIncremental, FirstCursor: json.RawMessage(`{}`),
	}}, nil
}

// FetchSnapshot takes the balance snapshot of UC-202: spot, funding, Earn flexible and Earn locked, all four or
// nothing (FR-204, EC-215). Every request reserves its weight in conn.Limiter first. The time of the snapshot is the
// server time at the first request of the run (X1 D-9). A balance whose free and locked are both 0 is not returned
// (X1 D-35). Errors hold no key, secret, signature, uid or amount.
func (c *Connector) FetchSnapshot(ctx context.Context, conn connector.Connection) (connector.Snapshot, error) {
	if conn.Key == nil {
		return connector.Snapshot{}, errors.New("binance: the connection has no key")
	}
	s, err := c.session(conn.Source, conn.Key, conn.Limiter)
	if err != nil {
		return connector.Snapshot{}, err
	}
	if err := s.syncTime(ctx, false); err != nil {
		return connector.Snapshot{}, err
	}
	takenAt := s.serverNow()

	b := balances{}
	spot, err := s.spot(ctx)
	if err != nil {
		return connector.Snapshot{}, err
	}
	if err := s.funding(ctx, b); err != nil {
		return connector.Snapshot{}, err
	}
	flexible, err := s.earn(ctx, endpointFlexible, b)
	if err != nil {
		return connector.Snapshot{}, err
	}
	if _, err := s.earn(ctx, endpointLocked, b); err != nil {
		return connector.Snapshot{}, err
	}
	for _, sb := range spot {
		if isWrapper(sb.asset, flexible, conn.Source.Aliases) {
			continue // EC-209: counted once, in EARN_FLEXIBLE
		}
		if err := b.add(AccountSpot, sb.asset, sb.free, sb.locked); err != nil {
			return connector.Snapshot{}, err
		}
	}
	out, err := b.list()
	if err != nil {
		return connector.Snapshot{}, err
	}
	return connector.Snapshot{TakenAt: takenAt, Balances: out}, nil
}

// serverNow is the local clock with the time offset of the base URL: the Binance clock.
func (s *session) serverNow() time.Time {
	return s.c.now().Add(-s.c.clockOf(s.cfg.BaseURL).get()).UTC()
}

// isWrapper applies the wrapper rule (UC-202 step 5; EC-209, EC-216): LD + X is a wrapper when X has a flexible
// position and LD + X is not a native asset of the alias rows of the source.
func isWrapper(code string, flexible map[string]bool, aliases []connector.Alias) bool {
	x, ok := strings.CutPrefix(code, wrapperPrefix)
	if !ok || x == "" || !flexible[x] {
		return false
	}
	return !slices.ContainsFunc(aliases, func(a connector.Alias) bool { return a.NativeAsset == code })
}

// spotBalance is a balance of the account answer, as given.
type spotBalance struct {
	asset, free, locked string
}

// spot reads step 1: GET /api/v3/account with omitZeroBalances=true.
func (s *session) spot(ctx context.Context) ([]spotBalance, error) {
	body, err := s.call(ctx, endpointAccount, param{"omitZeroBalances", "true"})
	if err != nil {
		return nil, err
	}
	var answer struct {
		Balances []struct {
			Asset  string `json:"asset"`
			Free   string `json:"free"`
			Locked string `json:"locked"`
		} `json:"balances"`
	}
	if err := json.Unmarshal(body, &answer); err != nil || answer.Balances == nil {
		return nil, notExpected(endpointAccount)
	}
	out := make([]spotBalance, 0, len(answer.Balances))
	for _, b := range answer.Balances {
		if b.Asset == "" {
			return nil, notExpected(endpointAccount)
		}
		out = append(out, spotBalance{asset: b.Asset, free: b.Free, locked: b.Locked})
	}
	return out, nil
}

// funding reads step 2: POST /sapi/v1/asset/get-funding-asset; locked = locked + freeze + withdrawing.
func (s *session) funding(ctx context.Context, b balances) error {
	body, err := s.call(ctx, endpointFunding)
	if err != nil {
		return err
	}
	var rows []struct {
		Asset       string `json:"asset"`
		Free        string `json:"free"`
		Locked      string `json:"locked"`
		Freeze      string `json:"freeze"`
		Withdrawing string `json:"withdrawing"`
	}
	if err := json.Unmarshal(body, &rows); err != nil || rows == nil {
		return notExpected(endpointFunding)
	}
	for _, r := range rows {
		if r.Asset == "" {
			return notExpected(endpointFunding)
		}
		if err := b.add(AccountFunding, r.Asset, r.Free, r.Locked, r.Freeze, r.Withdrawing); err != nil {
			return err
		}
	}
	return nil
}

// earn reads all pages of an Earn position endpoint (steps 3 and 4): size 100, current from 1, until the rows read
// reach total or a page has fewer than 100 rows. Flexible: totalAmount in free of EARN_FLEXIBLE; locked: amount in
// locked of EARN_LOCKED; positions of one asset are summed. It returns the assets that have a position.
func (s *session) earn(ctx context.Context, ep endpoint, b balances) (map[string]bool, error) {
	assets := map[string]bool{}
	read := 0
	for page := 1; ; page++ {
		if page > maxEarnPages {
			return nil, fmt.Errorf("binance: %s %s: more than %d pages", ep.method, ep.path, maxEarnPages)
		}
		body, err := s.call(ctx, ep, param{"size", strconv.Itoa(earnPageSize)}, param{"current", strconv.Itoa(page)})
		if err != nil {
			return nil, err
		}
		var answer struct {
			Rows []struct {
				Asset       string `json:"asset"`
				TotalAmount string `json:"totalAmount"`
				Amount      string `json:"amount"`
			} `json:"rows"`
			Total *int `json:"total"`
		}
		if err := json.Unmarshal(body, &answer); err != nil || answer.Total == nil {
			return nil, notExpected(ep)
		}
		for _, r := range answer.Rows {
			if r.Asset == "" {
				return nil, notExpected(ep)
			}
			assets[r.Asset] = true
			if ep == endpointFlexible {
				err = b.add(AccountEarnFlexible, r.Asset, r.TotalAmount, "0")
			} else {
				err = b.add(AccountEarnLocked, r.Asset, "0", r.Amount)
			}
			if err != nil {
				return nil, err
			}
		}
		read += len(answer.Rows)
		if read >= *answer.Total || len(answer.Rows) < earnPageSize {
			return assets, nil
		}
	}
}

func notExpected(ep endpoint) error {
	return fmt.Errorf("binance: %s %s: the answer is not as expected", ep.method, ep.path)
}

// balances sums free and locked per account type and native asset, exactly (math/big; no float).
type balances map[[2]string]*pair

type pair struct{ free, locked sum }

// add adds free and the locked parts of one row of an account type. An amount that is not a plain decimal, or is
// negative, fails the snapshot; the error quotes no amount.
func (b balances) add(accountType, asset, free string, locked ...string) error {
	key := [2]string{accountType, asset}
	p, ok := b[key]
	if !ok {
		p = &pair{}
		b[key] = p
	}
	if err := p.free.add(free); err != nil {
		return fmt.Errorf("binance: %s %s: free %w", accountType, asset, err)
	}
	for _, l := range locked {
		if err := p.locked.add(l); err != nil {
			return fmt.Errorf("binance: %s %s: locked %w", accountType, asset, err)
		}
	}
	return nil
}

// list returns the balances in the order of the account types, then of the native assets, without the rows whose
// free and locked are both 0 (X1 D-35).
func (b balances) list() ([]connector.Balance, error) {
	keys := make([][2]string, 0, len(b))
	for k := range b {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(x, y [2]string) int {
		if c := slices.Index(accountOrder, x[0]) - slices.Index(accountOrder, y[0]); c != 0 {
			return c
		}
		return strings.Compare(x[1], y[1])
	})
	var out []connector.Balance
	for _, k := range keys {
		free, err := b[k].free.String()
		if err != nil {
			return nil, err
		}
		locked, err := b[k].locked.String()
		if err != nil {
			return nil, err
		}
		if free == "0" && locked == "0" {
			continue
		}
		out = append(out, connector.Balance{AccountType: k[0], NativeAsset: k[1], Free: free, Locked: locked})
	}
	return out, nil
}

// sum is an exact sum of decimal strings: the value and the largest number of decimal places of its addends.
type sum struct {
	v     big.Rat
	scale int
}

// add adds a decimal string of Binance: a plain decimal, not negative.
func (s *sum) add(amount string) error {
	d, err := decimal(amount)
	if err != nil {
		return errors.New("is not a plain decimal")
	}
	if strings.HasPrefix(d, "-") {
		return errors.New("is negative")
	}
	var r big.Rat
	if _, ok := r.SetString(d); !ok {
		return errors.New("is not a plain decimal")
	}
	if _, frac, ok := strings.Cut(d, "."); ok {
		s.scale = max(s.scale, len(frac))
	}
	s.v.Add(&s.v, &r)
	return nil
}

// String is the sum as a plain decimal, without trailing zeros and without exponent.
func (s *sum) String() (string, error) {
	return decimal(s.v.FloatString(s.scale))
}
