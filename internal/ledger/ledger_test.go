package ledger

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DigitLock/crypto-account-service/internal/connector"
)

func validEntry() connector.Entry {
	return connector.Entry{
		ExternalID: "op-1", Leg: "SINGLE", Type: "DEPOSIT", Direction: "IN", NativeAsset: "BTC",
		Amount: "0.0125", OccurredAt: time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC), Raw: json.RawMessage(`{"id":1}`),
	}
}

func validBalance() connector.Balance {
	return connector.Balance{AccountType: "SPOT", NativeAsset: "BTC", Free: "0.5", Locked: "0"}
}

// C1-T629 — Req: EC-117. The validation part; internal/engine checks that nothing is stored.
func TestT629_InvalidEntryOrBalance(t *testing.T) {
	page := func(mutate func(e *connector.Entry)) connector.Page {
		bad := validEntry()
		bad.ExternalID = "op-2"
		mutate(&bad)
		return connector.Page{Entries: []connector.Entry{validEntry(), bad}, Mode: connector.ModeIncremental}
	}
	for name, p := range map[string]connector.Page{
		"amount 0":            page(func(e *connector.Entry) { e.Amount = "0" }),
		"amount 0.000":        page(func(e *connector.Entry) { e.Amount = "0.000" }),
		"negative amount":     page(func(e *connector.Entry) { e.Amount = "-1" }),
		"19 decimal places":   page(func(e *connector.Entry) { e.Amount = "0." + strings.Repeat("1", 19) }),
		"21 integer digits":   page(func(e *connector.Entry) { e.Amount = "1" + strings.Repeat("0", 20) }),
		"exponent":            page(func(e *connector.Entry) { e.Amount = "1e3" }),
		"plus sign":           page(func(e *connector.Entry) { e.Amount = "+1" }),
		"no integer part":     page(func(e *connector.Entry) { e.Amount = ".5" }),
		"empty amount":        page(func(e *connector.Entry) { e.Amount = "" }),
		"empty external_id":   page(func(e *connector.Entry) { e.ExternalID = "" }),
		"unknown type":        page(func(e *connector.Entry) { e.Type = "AIRDROP" }),
		"unknown leg":         page(func(e *connector.Entry) { e.Leg = "OTHER" }),
		"unknown direction":   page(func(e *connector.Entry) { e.Direction = "SIDEWAYS" }),
		"occurred_at not set": page(func(e *connector.Entry) { e.OccurredAt = time.Time{} }),
		"raw not JSON":        page(func(e *connector.Entry) { e.Raw = json.RawMessage(`{`) }),
		"empty native asset":  page(func(e *connector.Entry) { e.NativeAsset = "" }),
		"unknown mode":        {Entries: []connector.Entry{validEntry()}, Mode: "SIDEWAYS"},
		"cursor not JSON":     {Entries: []connector.Entry{validEntry()}, Mode: connector.ModeIncremental, Cursor: json.RawMessage(`{`)},
	} {
		var invalid *InvalidError
		if err := ValidatePage(p); err == nil || !errors.As(err, &invalid) {
			t.Errorf("%s: ValidatePage = %v, want an InvalidError", name, err)
		}
	}
	for _, ok := range []string{"1", "0.000000000000000001", strings.Repeat("9", 20) + "." + strings.Repeat("9", 18), "1.50"} {
		e := validEntry()
		e.Amount = ok
		if err := ValidatePage(connector.Page{Entries: []connector.Entry{e}, Mode: connector.ModeBackfill}); err != nil {
			t.Errorf("amount %s refused: %v", ok, err)
		}
	}

	snap := func(mutate func(b *connector.Balance)) connector.Snapshot {
		bad := validBalance()
		bad.NativeAsset = "USDT"
		mutate(&bad)
		return connector.Snapshot{TakenAt: time.Now(), Balances: []connector.Balance{validBalance(), bad}}
	}
	for name, s := range map[string]connector.Snapshot{
		"negative free":        snap(func(b *connector.Balance) { b.Free = "-1" }),
		"negative locked":      snap(func(b *connector.Balance) { b.Locked = "-0.1" }),
		"19 decimal places":    snap(func(b *connector.Balance) { b.Free = "0." + strings.Repeat("1", 19) }),
		"21 integer digits":    snap(func(b *connector.Balance) { b.Locked = "1" + strings.Repeat("0", 20) }),
		"unknown account type": snap(func(b *connector.Balance) { b.AccountType = "MARGIN" }),
		"twice":                snap(func(b *connector.Balance) { b.NativeAsset = "BTC" }),
		"no time":              {Balances: []connector.Balance{validBalance()}},
	} {
		var invalid *InvalidError
		if err := ValidateSnapshot(s); err == nil || !errors.As(err, &invalid) {
			t.Errorf("%s: ValidateSnapshot = %v, want an InvalidError", name, err)
		}
	}
	zero := validBalance()
	zero.Free = "0"
	if err := ValidateSnapshot(connector.Snapshot{TakenAt: time.Now(), Balances: []connector.Balance{zero}}); err != nil {
		t.Errorf("a zero balance was refused: %v", err)
	}
	if err := ValidateSnapshot(connector.Snapshot{TakenAt: time.Now()}); err != nil {
		t.Errorf("an empty snapshot was refused: %v", err)
	}
}
