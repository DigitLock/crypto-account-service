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

// Signal is an inclusion signal for one debit. Receipt is set when the source read it (polling); a signal without
// a receipt (the chain listener of st7) makes the step read the receipt itself: the row is written only from a
// receipt, never from a log (ADR-13).
type Signal struct {
	Source  string
	Receipt *types.Receipt
}

// Signals routes inclusion signals to the debit waiting for them, by the authId of the contract. Polling delivers
// into it now; the chain listener of st7 delivers into it too.
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
