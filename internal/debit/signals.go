package debit

import (
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// Sources of an inclusion signal (SRS — Card Spend §2.5.1 inclusion_signals_total{source}).
const (
	SourcePolling      = "polling"
	SourceSubscription = "subscription"
)

// Signal is an inclusion signal for one debit. Polling sets Receipt: a successful one approves, a reverted one
// declines as DEBIT_REVERTED. The chain listener sets none: its Debited log of the authId approves at once, with
// the operator transaction INCLUDED and no block (UC-1 step 12, ADR-12, ADR-13). Nothing of the log is stored; the
// tracker fills the block from the sealed receipt (UC-3 row 10).
type Signal struct {
	Source  string
	Receipt *types.Receipt
}

// Signals routes inclusion signals to the debit waiting for them, by the authId of the contract. Receipt polling
// and the chain listener deliver into it; the first signal decides (FR-23).
type Signals struct {
	mu      sync.Mutex
	waiting map[common.Hash]chan Signal
}

// NewSignals returns an empty registry.
func NewSignals() *Signals { return &Signals{waiting: map[common.Hash]chan Signal{}} }

// wait registers the debit of authID. The returned stop must be called when the debit stops waiting.
func (s *Signals) wait(authID common.Hash) (<-chan Signal, func()) {
	ch := make(chan Signal, 1)
	s.mu.Lock()
	s.waiting[authID] = ch
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.waiting[authID] == ch {
			delete(s.waiting, authID)
		}
	}
}

// Deliver hands a signal to the debit of authID. It reports false when no debit waits for it or a signal is
// already pending: the first signal decides, a later one changes nothing.
func (s *Signals) Deliver(authID common.Hash, sig Signal) bool {
	s.mu.Lock()
	ch := s.waiting[authID]
	s.mu.Unlock()
	if ch == nil {
		return false
	}
	select {
	case ch <- sig:
		return true
	default:
		return false
	}
}
