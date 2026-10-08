package chain

import (
	"context"
	"errors"
	"testing"
)

// S3-T214, shared finality code — Req: S3 D-4. FinalBlock makes exactly the read of its mode: one tag header, or
// one head. The tracker of card-auth calls it; its S2 tests run unchanged in internal/processorapi.
func TestT214_SharedFinalBlock(t *testing.T) {
	var tags []string
	heads := 0
	reads := FinalReads{
		TagNumber: func(_ context.Context, tag string) (uint64, error) { tags = append(tags, tag); return 90, nil },
		Head:      func(context.Context) (uint64, error) { heads++; return 100, nil },
	}
	for _, c := range []struct {
		rule   FinalityRule
		want   uint64
		ok     bool
		tag    string
		nHeads int
	}{
		{FinalityRule{Mode: FinalityModeTag, Tag: FinalityTagFinalized}, 90, true, "finalized", 0},
		{FinalityRule{Mode: FinalityModeTag, Tag: FinalityTagSafe}, 90, true, "safe", 0},
		{FinalityRule{Mode: FinalityModeConfirmations, Confirmations: 10}, 90, true, "", 1},
		{FinalityRule{Mode: FinalityModeConfirmations, Confirmations: 100}, 0, true, "", 1},
		{FinalityRule{Mode: FinalityModeConfirmations, Confirmations: 101}, 0, false, "", 1},
	} {
		tags, heads = nil, 0
		n, ok, err := FinalBlock(context.Background(), c.rule, reads)
		if err != nil || n != c.want || ok != c.ok || heads != c.nHeads || (c.tag != "") != (len(tags) == 1) ||
			(c.tag != "" && tags[0] != c.tag) {
			t.Errorf("%+v: %d %v %v, tags %v, heads %d", c.rule, n, ok, err, tags, heads)
		}
	}

	failed := errors.New("read failed")
	reads = FinalReads{
		TagNumber: func(context.Context, string) (uint64, error) { return 0, failed },
		Head:      func(context.Context) (uint64, error) { return 0, failed },
	}
	for _, rule := range []FinalityRule{{Mode: FinalityModeTag}, {Mode: FinalityModeConfirmations, Confirmations: 1}} {
		if _, ok, err := FinalBlock(context.Background(), rule, reads); !errors.Is(err, failed) || ok {
			t.Errorf("%+v: %v %v, want the error of the read", rule, ok, err)
		}
	}
	if _, _, err := FinalBlock(context.Background(), FinalityRule{Mode: "latest"}, reads); err == nil {
		t.Error("an unknown mode was accepted")
	}
}
