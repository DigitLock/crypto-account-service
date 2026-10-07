package evm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/DigitLock/crypto-account-service/internal/connector"
)

// cursor is the cursor of the logs stream (§2.4 Cursor formats). An empty cursor {} is the first run: next_block is
// backfill_floor and there is no hash to guard.
type cursor struct {
	NextBlock *uint64 `json:"next_block,omitempty"`
	LastHash  string  `json:"last_hash,omitempty"`
	LastTime  string  `json:"last_time,omitempty"`
}

func parseCursor(raw json.RawMessage) (cursor, error) {
	var c cursor
	if len(bytes.TrimSpace(raw)) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return cursor{}, errors.New("evm: the logs cursor is malformed")
	}
	if c.LastHash != "" && (c.NextBlock == nil || *c.NextBlock == 0) {
		return cursor{}, errors.New("evm: the logs cursor has last_hash without a next_block above 0")
	}
	return c, nil
}

// ReorgError is REORG_BELOW_FINAL (UC-303 steps 3 and 10, EC-308): the block before the cursor is missing or its
// hash changed. The cursor does not move and no entry is changed; every later run fails the same way until an
// operator acts. Found is empty when the block is missing.
type ReorgError struct {
	Block  uint64
	Stored string
	Found  string
}

func (e *ReorgError) Error() string {
	found := e.Found
	if found == "" {
		found = "no block"
	}
	return fmt.Sprintf("evm: REORG_BELOW_FINAL: block %d: stored hash %s, found %s", e.Block, e.Stored, found)
}

// logs runs UC-303 steps 2 to 10 on the endpoint of the run; step 1 is open. Steps 5 to 9 are in logread.go.
func (s *session) logs(ctx context.Context, mode connector.Mode, cur cursor, raw json.RawMessage) (connector.Page, error) {
	next := s.cfg.BackfillFloor
	if cur.NextBlock != nil {
		next = *cur.NextBlock
	}

	// Step 2.
	final, ok, err := s.finalBlock(ctx)
	if err != nil {
		return connector.Page{}, err
	}
	s.final, s.finalOK = final, ok

	// Step 3: skipped on the first run, which has no hash.
	if cur.LastHash != "" {
		h, err := s.headerAt(ctx, hexutil.EncodeUint64(next-1))
		if err != nil {
			return connector.Page{}, err
		}
		if h == nil || !strings.EqualFold(h.Hash.Hex(), cur.LastHash) {
			reorg := &ReorgError{Block: next - 1, Stored: cur.LastHash}
			if h != nil {
				reorg.Found = h.Hash.Hex()
			}
			s.c.metrics.ReorgBelowFinal(s.src.Code)
			s.c.logger.ErrorContext(ctx, "critical: REORG_BELOW_FINAL: the last processed block is missing or its hash changed; "+
				"the stream stays failed until an operator acts", "source", s.src.Code, "connection_id", s.conn.ID,
				"block", reorg.Block, "stored_hash", reorg.Stored, "found_hash", reorg.Found)
			return connector.Page{}, reorg
		}
	}

	// Step 4: nothing final yet, or nothing new: a successful run with the same cursor.
	if !ok || next > final {
		same := raw
		if len(bytes.TrimSpace(same)) == 0 {
			same = json.RawMessage(`{}`)
		}
		return connector.Page{Cursor: same, Mode: mode}, nil
	}

	// Steps 5 to 9 (logread.go).
	return s.readPage(ctx, mode, next, final)
}

// logRange is UC-303 step 5: from next to min(next + size − 1, final). It never ends above final (FR-306). The
// caller ensures next ≤ final and size ≥ 1.
func logRange(next, final, size uint64) (from, to uint64) {
	to = final
	if size >= 1 && final-next >= size {
		to = next + size - 1
	}
	return next, to
}

// indexerLag updates evm_indexer_lag_blocks at the end of a run (§2.5.1): the final block minus the oldest last
// processed block (next_block − 1) among the connections of the source whose logs stream is INCREMENTAL. The
// connector keeps the last processed block of each connection it ran; a failed run keeps the previous value, a page
// in BACKFILL removes it, and a value not updated for 3 × sync_interval.logs is dropped (a deleted connection).
// Nothing is set before the final block of the run is known.
func (s *session) indexerLag(page connector.Page, err error) {
	if !s.finalOK {
		return
	}
	now := s.c.now()
	n := s.net
	n.mu.Lock()
	if err == nil {
		c, perr := parseCursor(page.Cursor)
		switch {
		case page.Mode != connector.ModeIncremental:
			delete(n.processed, s.conn.ID)
		case perr == nil && c.NextBlock != nil && *c.NextBlock > 0:
			n.processed[s.conn.ID] = processed{block: *c.NextBlock - 1, at: now}
		}
	}
	var oldest uint64
	found := false
	for id, p := range n.processed {
		if now.Sub(p.at) > 3*s.cfg.LogsInterval {
			delete(n.processed, id)
			continue
		}
		if !found || p.block < oldest {
			oldest, found = p.block, true
		}
	}
	n.mu.Unlock()
	lag := uint64(0)
	if found && s.final > oldest {
		lag = s.final - oldest
	}
	s.c.metrics.IndexerLag(s.src.Code, lag)
}
