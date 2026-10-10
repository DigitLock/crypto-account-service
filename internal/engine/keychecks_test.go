package engine_test

import (
	"maps"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/connector/fake"
	"github.com/DigitLock/crypto-account-service/internal/engine"
)

// X1-T606 — Req: done-when 3; X1 D-50. key_checks_total{source,result}: each periodic key check of the engine is
// counted once, as success (accepted and read-only), invalid (rejected: CREDENTIALS_INVALID) or failure (unreachable:
// status unchanged); CreateConnection counts nothing.
func TestT606_KeyChecksCounted(t *testing.T) {
	h := setup(t)
	id, k := h.create(t)
	e := h.engine(h.server)
	h.pass(t, e)
	if got := h.rec.keyChecksOf(); len(got) != 0 {
		t.Fatalf("key checks after CreateConnection and a pass before the check is due: %v, want none", got)
	}

	check := func(at time.Duration, want map[string]int) {
		t.Helper()
		before := len(h.checkCalls(k))
		h.clock.Set(testNow.Add(at))
		h.pass(t, e)
		if n := len(h.checkCalls(k)) - before; n != 1 {
			t.Fatalf("at +%v: %d checks of the key, want 1", at, n)
		}
		if got := h.rec.keyChecksOf(); !maps.Equal(got, want) {
			t.Errorf("at +%v: key checks %v, want %v", at, got, want)
		}
	}
	check(24*time.Hour, map[string]int{"fake/success": 1})

	h.fake.FailNext(fake.CheckStream, connector.ErrUnreachable)
	check(48*time.Hour, map[string]int{"fake/success": 1, "fake/failure": 1})

	h.scriptChecks(map[string]func() (connector.AccountInfo, error){
		k.apiKey: func() (connector.AccountInfo, error) { return connector.AccountInfo{}, connector.ErrKeyRejected },
	})
	check(48*time.Hour+30*time.Second, map[string]int{"fake/success": 1, "fake/failure": 1, "fake/invalid": 1})
	if got := h.status(t, id); got != "CREDENTIALS_INVALID" {
		t.Errorf("status %s after the invalid check", got)
	}

	t.Run("key_checks_total of the PromReporter", func(t *testing.T) {
		h := setup(t)
		h.create(t)
		reg := prometheus.NewRegistry()
		e := h.engineWith(engine.NewPromReporter(reg), h.logger)
		h.clock.Set(testNow.Add(24 * time.Hour))
		h.pass(t, e)
		families, err := reg.Gather()
		if err != nil {
			t.Fatal(err)
		}
		series := map[string]float64{}
		for _, f := range families {
			if f.GetName() != "key_checks_total" {
				continue
			}
			for _, m := range f.GetMetric() {
				labels := map[string]string{}
				for _, l := range m.GetLabel() {
					labels[l.GetName()] = l.GetValue()
				}
				if len(labels) != 2 {
					t.Errorf("labels %v, want source and result only", labels)
				}
				series[labels["source"]+"/"+labels["result"]] = m.GetCounter().GetValue()
			}
		}
		if !maps.Equal(series, map[string]float64{"fake/success": 1}) {
			t.Errorf("key_checks_total = %v, want fake/success 1", series)
		}
	})
}
