package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/DigitLock/crypto-account-service/internal/chain"
	"github.com/DigitLock/crypto-account-service/internal/testchain"
	"github.com/DigitLock/crypto-account-service/internal/testdb"
)

var ctx = context.Background()

// S2-T107 — Req: ADR-10, SRS — Card Spend §3.2 Chain access. The transaction count is read at the pending block
// tag; operator_accounts is written by the role cas_card_auth.
func TestT107_NonceCheckedAtStart(t *testing.T) {
	owner := testdb.Open(t)
	testdb.Clean(t)
	databaseURL := testdb.CardAuthURL(t)
	c := testchain.Start(t)

	// The chain's transaction count of the operator: 5.
	rc, err := rpc.Dial(c.RPCURL)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if err := rc.Call(nil, "anvil_setNonce", c.Operator, hexutil.Uint64(5)); err != nil {
		t.Fatal(err)
	}

	nextNonce := func(t *testing.T) int64 {
		t.Helper()
		var n int64
		if err := owner.QueryRow(ctx, `SELECT next_nonce FROM operator_accounts WHERE chain_id = 31337 AND address = $1`,
			c.Operator.Bytes()).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	startAndStop := func(t *testing.T) string {
		t.Helper()
		s := start(t, testEnv(t, c, databaseURL))
		if err := s.stop(); err != nil {
			t.Fatal(err)
		}
		return s.logs.String()
	}

	t.Run("no row: created with the chain's count", func(t *testing.T) {
		out := startAndStop(t)
		if n := nextNonce(t); n != 5 {
			t.Errorf("next_nonce = %d, want 5", n)
		}
		if strings.Count(out, "operator account created") != 1 {
			t.Errorf("no log line of the new row:\n%s", out)
		}
	})

	t.Run("below: raised, one log line", func(t *testing.T) {
		if _, err := owner.Exec(ctx, `UPDATE operator_accounts SET next_nonce = 2`); err != nil {
			t.Fatal(err)
		}
		out := startAndStop(t)
		if n := nextNonce(t); n != 5 {
			t.Errorf("next_nonce = %d, want 5", n)
		}
		if strings.Count(out, "operator next_nonce raised") != 1 || !strings.Contains(out, `"from":2,"to":5`) {
			t.Errorf("want one log line of the raise from 2 to 5:\n%s", out)
		}
	})

	t.Run("above: kept", func(t *testing.T) {
		if _, err := owner.Exec(ctx, `UPDATE operator_accounts SET next_nonce = 9`); err != nil {
			t.Fatal(err)
		}
		out := startAndStop(t)
		if n := nextNonce(t); n != 9 {
			t.Errorf("next_nonce = %d, want 9: reserved slots are kept", n)
		}
		if strings.Contains(out, "raised") || strings.Contains(out, "operator account created") {
			t.Errorf("a kept next_nonce is not logged as a change:\n%s", out)
		}
	})

	var rows int
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM operator_accounts`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("operator_accounts holds %d rows, want 1", rows)
	}
}

// S2-T307 — Req: ADR-3. The card-auth half: card-auth with the role cas_server fails its first write, the nonce
// check at start. The server half is in cmd/server.
func TestT307_RoleSeparation(t *testing.T) {
	testdb.Open(t)
	testdb.Clean(t)
	c := testchain.Start(t)

	err := runFailing(t, testEnv(t, c, testdb.ServerURL(t)), io.Discard)
	var checkErr *chain.StartCheckError
	if !errors.As(err, &checkErr) || checkErr.Check != nonceCheck || !strings.Contains(err.Error(), "42501") {
		t.Errorf("run = %v, want the nonce check refused with insufficient privilege", err)
	}
}
