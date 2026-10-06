package tracker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/DigitLock/crypto-account-service/internal/chain"
	"github.com/DigitLock/crypto-account-service/internal/config"
)

// The RPC budget of a cycle (SRS — Card Spend UC-3, rules of S2 st9b). The decision path uses the same endpoint, so
// the reads of the tracker are bounded per cycle:
//   - the final block number is read once per cycle and shared by every row: the block of finality_tag in mode tag,
//     the latest block − finality_confirmations in mode confirmations; the latest block is read once as well;
//   - a row whose block is above the final block makes no chain call for finality;
//   - the reorg check (ADR-10) reads a block at most once per cycle. A block is a hash chain: when the highest stored
//     block of the rows not yet final still has its stored hash, the rows below it are checked with it, so the rows
//     go in descending order of their block and the steady state costs one read. The check right before a row is set
//     final reads its block;
//   - a rate-limit answer ends the cycle at once: every later chain call of the cycle is skipped, and one WARN line
//     is written for the cycle;
//   - failures of the same kind across rows are logged once per cycle, with the number of rows.

// errRateLimited is the error of every chain call of a cycle after the endpoint answered with a rate limit.
var errRateLimited = errors.New("the endpoint answered with a rate limit; the cycle ended")

// cycle is the state of one cycle. One cycle runs at a time: Start, then Cycle after Cycle (Run).
type cycle struct {
	limited  bool
	limitErr string // the first rate-limit answer, as text without a URL

	headRead           bool
	headNumber, headTS uint64
	headErr            error

	finalRead bool
	final     uint64
	finalOK   bool // false while the chain is shorter than the finality rule
	finalErr  error

	hashes map[uint64]common.Hash // block number → hash on chain, read this cycle
	// verified is the highest block whose stored hash matched the chain this cycle: the stored blocks below it are
	// on the same chain.
	verified    uint64
	hasVerified bool

	fails []*failure
}

// failure is one kind of failure of a cycle: the attributes of its first row and the number of rows.
type failure struct {
	level slog.Level
	msg   string
	err   string
	attrs []any
	rows  int
}

func newCycle() *cycle { return &cycle{hashes: map[uint64]common.Hash{}} }

// begin starts a cycle; end writes its log lines.
func (t *Tracker) begin() { t.cyc = newCycle() }

func (t *Tracker) end(ctx context.Context) {
	c := t.cyc
	for _, f := range c.fails {
		t.failed(ctx, f.level, f.msg, append(append([]any{}, f.attrs...), "error", f.err, "rows", f.rows)...)
	}
	if c.limited {
		t.failed(ctx, slog.LevelWarn, "tracker: the endpoint answered with a rate limit; the cycle ended", "error", c.limitErr)
	}
}

// limited reports whether the cycle has ended on a rate limit.
func (t *Tracker) limited() bool { return t.cyc.limited }

// check marks the cycle as ended when err is a rate-limit answer and returns errRateLimited then; else err.
func (t *Tracker) check(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, errRateLimited) {
		return err
	}
	if chain.IsRateLimit(err) {
		if !t.cyc.limited {
			t.cyc.limited, t.cyc.limitErr = true, err.Error()
		}
		return errRateLimited
	}
	return err
}

// rpcErr turns an error of a contract call into an error without a URL that keeps its cause, checked for a rate limit.
func (t *Tracker) rpcErr(what string, err error) error {
	return t.check(fmt.Errorf("%s: %w", what, t.queue.Reader().DescribeErr(err, t.cfg.Interval)))
}

// rowFailed records a failure of a row; end logs each kind once. A rate limit is logged once by end.
func (t *Tracker) rowFailed(level slog.Level, msg string, err error, attrs ...any) {
	if errors.Is(err, errRateLimited) {
		return
	}
	text := err.Error()
	for _, f := range t.cyc.fails {
		if f.level == level && f.msg == msg && f.err == text {
			f.rows++
			return
		}
	}
	t.cyc.fails = append(t.cyc.fails, &failure{level: level, msg: msg, err: text, attrs: attrs, rows: 1})
}

// receipt reads the receipt of hash: nil when the node has none yet or the read failed. A rate limit is an error.
func (t *Tracker) receipt(ctx context.Context, hash common.Hash) (*types.Receipt, error) {
	if t.limited() {
		return nil, errRateLimited
	}
	r, err := t.queue.ReceiptErr(ctx, hash)
	if err := t.check(err); errors.Is(err, errRateLimited) {
		return nil, err
	}
	return r, nil
}

// head returns the number and timestamp of the latest block, read once per cycle.
func (t *Tracker) head(ctx context.Context) (number, timestamp uint64, err error) {
	if t.limited() {
		return 0, 0, errRateLimited
	}
	c := t.cyc
	if !c.headRead {
		n, ts, err := t.queue.Head(ctx)
		c.headRead, c.headNumber, c.headTS, c.headErr = true, n, ts, t.check(err)
	}
	return c.headNumber, c.headTS, c.headErr
}

// finalNumber returns the final block of the finality rule of the network, read once per cycle (S2 st9b a). ok is
// false while the chain is shorter than the rule: no block is final yet.
func (t *Tracker) finalNumber(ctx context.Context) (number uint64, ok bool, err error) {
	if t.limited() {
		return 0, false, errRateLimited
	}
	c := t.cyc
	if !c.finalRead {
		c.finalRead = true
		c.final, c.finalOK, c.finalErr = t.readFinal(ctx)
	}
	return c.final, c.finalOK, c.finalErr
}

// readFinal reads the final block of the finality rule; a failed read is shared by the rows of the cycle.
func (t *Tracker) readFinal(ctx context.Context) (uint64, bool, error) {
	if t.cfg.FinalityMode == config.FinalityModeTag {
		tag := rpc.FinalizedBlockNumber
		if t.cfg.FinalityTag == config.FinalityTagSafe {
			tag = rpc.SafeBlockNumber
		}
		cctx, cancel := context.WithTimeout(ctx, t.cfg.Interval)
		defer cancel()
		h, err := t.queue.Reader().Endpoint().Client.HeaderByNumber(cctx, big.NewInt(int64(tag)))
		if err != nil {
			return 0, false, t.rpcErr("final block", err)
		}
		return h.Number.Uint64(), true, nil
	}
	latest, _, err := t.head(ctx)
	if err != nil {
		return 0, false, err
	}
	n := uint64(t.cfg.FinalityConfirmations)
	if latest < n {
		return 0, false, nil
	}
	return latest - n, true, nil
}

// blockHash returns the hash of block number on chain now, read at most once per cycle; zero when there is none.
func (t *Tracker) blockHash(ctx context.Context, number uint64) (common.Hash, error) {
	if t.limited() {
		return common.Hash{}, errRateLimited
	}
	if h, ok := t.cyc.hashes[number]; ok {
		return h, nil
	}
	h, err := t.queue.BlockHash(ctx, number)
	if err != nil {
		return common.Hash{}, t.check(err)
	}
	t.cyc.hashes[number] = h
	return h, nil
}

// unchanged is the reorg check of a row not yet final (ADR-10, S2 st9b c): true when its stored block is still on the
// chain. A block at or below the highest block found unchanged in this cycle needs no read of its own.
func (t *Tracker) unchanged(ctx context.Context, number uint64, stored []byte) (bool, error) {
	c := t.cyc
	if c.hasVerified && number <= c.verified {
		return true, nil
	}
	h, err := t.blockHash(ctx, number)
	if err != nil {
		return false, err
	}
	if h != common.BytesToHash(stored) {
		return false, nil
	}
	if !c.hasVerified || number > c.verified {
		c.verified, c.hasVerified = number, true
	}
	return true, nil
}

// nonceCount is the operator's transaction count at the latest block.
func (t *Tracker) nonceCount(ctx context.Context) (uint64, error) {
	if t.limited() {
		return 0, errRateLimited
	}
	n, err := t.queue.NonceCount(ctx)
	return n, t.check(err)
}

// blockTime is the timestamp of a block.
func (t *Tracker) blockTime(ctx context.Context, number *big.Int) (uint64, error) {
	if t.limited() {
		return 0, errRateLimited
	}
	ts, err := t.queue.BlockTime(ctx, number)
	return ts, t.check(err)
}
