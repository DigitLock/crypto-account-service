package chain

import (
	"context"
	"fmt"
)

// Modes and tags of the finality rule (SRS — EVM Connector §2.1.1 Finality rule, SRS — Card Spend §3.1).
const (
	FinalityModeTag           = "tag"
	FinalityModeConfirmations = "confirmations"
	FinalityTagFinalized      = "finalized"
	FinalityTagSafe           = "safe"
)

// FinalityRule is the finality rule of a network. card-auth takes it from CARD_AUTH_FINALITY_*, the EVM connector
// of server from sources.config (S3 D-18).
type FinalityRule struct {
	Mode          string // FinalityModeTag or FinalityModeConfirmations
	Tag           string // FinalityTagFinalized or FinalityTagSafe; mode tag
	Confirmations uint64 // mode confirmations
}

// FinalReads are the two reads FinalBlock may need. The caller makes them through its own endpoint, budget and
// cache: the tracker of card-auth reads the head once per cycle, the connector reserves each call in its limiter.
type FinalReads struct {
	// TagNumber returns the number of the header of the block tag "finalized" or "safe".
	TagNumber func(ctx context.Context, tag string) (uint64, error)
	// Head returns the number of the latest block.
	Head func(ctx context.Context) (uint64, error)
}

// FinalBlock is the one read of the final block for card-auth and the EVM connector (S3 D-4). Mode tag: the
// header of the tag, one read. Mode confirmations: the head minus Confirmations, one read; ok is false while the
// chain is shorter than Confirmations: no block is final yet. Errors of the reads are returned as they are.
func FinalBlock(ctx context.Context, rule FinalityRule, reads FinalReads) (number uint64, ok bool, err error) {
	switch rule.Mode {
	case FinalityModeTag:
		tag := rule.Tag
		if tag != FinalityTagSafe {
			tag = FinalityTagFinalized
		}
		n, err := reads.TagNumber(ctx, tag)
		if err != nil {
			return 0, false, err
		}
		return n, true, nil
	case FinalityModeConfirmations:
		head, err := reads.Head(ctx)
		if err != nil {
			return 0, false, err
		}
		if head < rule.Confirmations {
			return 0, false, nil
		}
		return head - rule.Confirmations, true, nil
	default:
		return 0, false, fmt.Errorf("chain: unknown finality mode %q", rule.Mode)
	}
}
