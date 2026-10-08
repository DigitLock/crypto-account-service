package evm

import (
	"context"
	"errors"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/DigitLock/crypto-account-service/internal/connector"
)

// checkpointDue reports whether completeness_interval has passed since the last checkpoint of the connection. The
// time is kept in memory: after a restart the first eligible page carries one.
func (s *session) checkpointDue() bool {
	s.net.mu.Lock()
	defer s.net.mu.Unlock()
	last, ok := s.net.checkpointAt[s.conn.ID]
	return !ok || s.c.now().Sub(last) >= s.cfg.CompletenessInterval
}

// checkpoint reads balanceOf(account) of every tracked token at the range end F, pinned by its hash (UC-304 step 2).
// A rate limit fails the run, as any rate limit. Any other failure skips the check (EC-317), and so does a balance
// above the ledger limit (EC-319, S3 D-39), which would make the writer refuse the whole page: no checkpoint, the
// counter and a WARN line; the page is returned as usual and the next eligible page tries again.
func (s *session) checkpoint(ctx context.Context, account common.Address, end *header) (*connector.Checkpoint, error) {
	pin := map[string]any{"blockHash": end.Hash}
	balances := make([]connector.Balance, 0, len(s.src.Aliases))
	for _, a := range s.src.Aliases {
		free, err := s.balanceAt(ctx, a, account, pin)
		if err == nil && !fitsLedger(free) {
			s.c.metrics.CompletenessSkipped(s.src.Code)
			s.c.logger.WarnContext(ctx, "completeness check skipped: a balance has more than 20 integer digits (EC-319)",
				"source", s.src.Code, "connection_id", s.conn.ID, "block", uint64(end.Number), "native_asset", a.NativeAsset)
			return nil, nil
		}
		if err == nil {
			balances = append(balances, connector.Balance{AccountType: AccountTypeWallet, NativeAsset: a.NativeAsset,
				Free: free, Locked: "0"})
			continue
		}
		var limit *connector.RateLimitError
		if errors.As(err, &limit) || ctx.Err() != nil {
			return nil, err
		}
		s.c.metrics.CompletenessSkipped(s.src.Code)
		s.c.logger.WarnContext(ctx, "completeness check skipped: the balance at the final block is not served (EC-317)",
			"source", s.src.Code, "connection_id", s.conn.ID, "block", uint64(end.Number), "error", err.Error())
		return nil, nil
	}
	s.net.mu.Lock()
	s.net.checkpointAt[s.conn.ID] = s.c.now()
	s.net.mu.Unlock()
	block := uint64(end.Number)
	return &connector.Checkpoint{BlockNumber: &block, BlockHash: end.Hash.Hex(),
		TakenAt: time.Unix(int64(end.Time), 0).UTC(), Balances: balances}, nil
}

// balanceAt is the balance of one tracked token at the pinned block, as a plain decimal.
func (s *session) balanceAt(ctx context.Context, a connector.Alias, account common.Address, pin map[string]any) (string, error) {
	decimals, err := tokenDecimals(a)
	if err != nil {
		return "", err
	}
	units, err := s.balanceOf(ctx, a.NativeAsset, account, pin)
	if err != nil {
		return "", err
	}
	return FormatUnits(units, decimals), nil
}
