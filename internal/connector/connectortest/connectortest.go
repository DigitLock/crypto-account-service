// Package connectortest is the shared connector test suite (ADR-2, SRS — EVM Connector §2.6, FR-316; S3 D-14):
// the behaviour every connector must show, run on answers served without network access. It takes any
// connector.Connector through a Harness. X1 reuses and extends it.
//
// The suite checks:
//   - idempotency: the same cursor gives the same entries, with the same external_id and leg, so a repeated page
//     adds nothing;
//   - cursor resume: reading page by page from the returned cursors gives the entries of one pass, none twice,
//     none missing;
//   - limit handling: a rate-limit answer gives connector.RateLimitError and a pause of its budget in the limiter,
//     and no further request in that run.
package connectortest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/DigitLock/crypto-account-service/internal/connector"
)

// Case is a history a harness serves.
type Case string

const (
	// Paged: a history of several pages of the ledger stream.
	Paged Case = "paged"
	// Whole: the same history, read in one page.
	Whole Case = "whole"
	// RateLimited: the source answers a request of the first page with a rate limit.
	RateLimited Case = "rate_limited"
)

// Setup is one case of a harness: a connector in its initial state, the connection and the stream to read, and the
// answers of the source, ready to be served.
type Setup struct {
	Connector connector.Connector
	// Conn is the connection; the suite sets its Limiter.
	Conn connector.Connection
	// Stream, Mode and Cursor are the ledger stream and where its first page starts.
	Stream string
	Mode   connector.Mode
	Cursor json.RawMessage
	// Served reports the requests answered so far and the answers of the case. A request beyond the answers fails
	// the test in the harness.
	Served func() (served, answers int)
}

// Harness returns a new Setup for a case: a fresh connector and answers served from the start.
type Harness func(t *testing.T, c Case) Setup

// MaxPages bounds a read of a history.
const MaxPages = 100

// Run runs the suite.
func Run(t *testing.T, h Harness) {
	t.Run("idempotency", func(t *testing.T) { idempotency(t, h) })
	t.Run("cursor resume", func(t *testing.T) { resume(t, h) })
	t.Run("limit handling", func(t *testing.T) { limits(t, h) })
}

// Limiter records the reservations, pauses and observations of a run; it never waits.
type Limiter struct {
	mu       sync.Mutex
	reserved map[string]int
	paused   map[string]time.Duration
	observed map[string][]int
}

// NewLimiter returns an empty Limiter.
func NewLimiter() *Limiter {
	return &Limiter{reserved: map[string]int{}, paused: map[string]time.Duration{}, observed: map[string][]int{}}
}

// Reserve implements connector.Limiter.
func (l *Limiter) Reserve(_ context.Context, budget string, cost int) error {
	l.mu.Lock()
	l.reserved[budget] += cost
	l.mu.Unlock()
	return nil
}

// Pause implements connector.Limiter.
func (l *Limiter) Pause(budget string, d time.Duration) {
	l.mu.Lock()
	l.paused[budget] = d
	l.mu.Unlock()
}

// Observe implements connector.Limiter.
func (l *Limiter) Observe(budget string, used int) {
	l.mu.Lock()
	l.observed[budget] = append(l.observed[budget], used)
	l.mu.Unlock()
}

// Observed returns the observed used units by budget, in order.
func (l *Limiter) Observed() map[string][]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string][]int, len(l.observed))
	for k, v := range l.observed {
		out[k] = append([]int(nil), v...)
	}
	return out
}

// Paused returns the pauses by budget.
func (l *Limiter) Paused() map[string]time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]time.Duration, len(l.paused))
	for k, v := range l.paused {
		out[k] = v
	}
	return out
}

// Reserved returns the cost reserved by budget.
func (l *Limiter) Reserved() map[string]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]int, len(l.reserved))
	for k, v := range l.reserved {
		out[k] = v
	}
	return out
}

// read reads pages from the first cursor until no more follow and returns the pages.
func read(t *testing.T, s Setup) []connector.Page {
	t.Helper()
	s.Conn.Limiter = NewLimiter()
	mode, cursor := s.Mode, s.Cursor
	var pages []connector.Page
	for range MaxPages {
		page, err := s.Connector.FetchPage(context.Background(), s.Conn, s.Stream, mode, cursor)
		if err != nil {
			t.Fatalf("page %d: %v", len(pages)+1, err)
		}
		if page.Mode != connector.ModeBackfill && page.Mode != connector.ModeIncremental {
			t.Fatalf("page %d: mode %q", len(pages)+1, page.Mode)
		}
		if !json.Valid(page.Cursor) {
			t.Fatalf("page %d: the cursor is not JSON: %s", len(pages)+1, page.Cursor)
		}
		pages = append(pages, page)
		if !page.More {
			assertAllServed(t, s)
			return pages
		}
		mode, cursor = page.Mode, page.Cursor
	}
	t.Fatalf("more than %d pages", MaxPages)
	return nil
}

func assertAllServed(t *testing.T, s Setup) {
	t.Helper()
	if served, answers := s.Served(); served != answers {
		t.Errorf("%d of %d answers served", served, answers)
	}
}

// key is the idempotency key of an entry in the ledger: external_id and leg.
func key(e connector.Entry) string { return e.ExternalID + "/" + e.Leg }

// same reports whether two entries are the same record.
func same(a, b connector.Entry) error {
	switch {
	case key(a) != key(b), a.Type != b.Type, a.Direction != b.Direction, a.NativeAsset != b.NativeAsset,
		a.Amount != b.Amount, !a.OccurredAt.Equal(b.OccurredAt):
		return fmt.Errorf("%+v differs from %+v", a, b)
	case !jsonEqual(a.Raw, b.Raw):
		return fmt.Errorf("the raw record of %s differs", key(a))
	}
	return nil
}

func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return bytes.Equal(a, b)
	}
	ax, _ := json.Marshal(x)
	by, _ := json.Marshal(y)
	return bytes.Equal(ax, by)
}

// entries returns the entries of pages in order and fails on a key that appears twice.
func entries(t *testing.T, pages []connector.Page) []connector.Entry {
	t.Helper()
	seen := map[string]bool{}
	var out []connector.Entry
	for _, p := range pages {
		for _, e := range p.Entries {
			if seen[key(e)] {
				t.Errorf("%s returned twice", key(e))
			}
			seen[key(e)] = true
			out = append(out, e)
		}
	}
	return out
}

// idempotency: two reads of the same history from the same cursor give the same pages: the same entries and the
// same next cursors.
func idempotency(t *testing.T, h Harness) {
	for _, c := range []Case{Paged, Whole} {
		first, second := read(t, h(t, c)), read(t, h(t, c))
		if len(first) != len(second) {
			t.Fatalf("%s: %d pages, then %d", c, len(first), len(second))
		}
		for i := range first {
			a, b := first[i], second[i]
			if len(a.Entries) != len(b.Entries) || !jsonEqual(a.Cursor, b.Cursor) || a.Mode != b.Mode || a.More != b.More {
				t.Fatalf("%s, page %d: %d entries, cursor %s, then %d entries, cursor %s", c, i+1, len(a.Entries), a.Cursor,
					len(b.Entries), b.Cursor)
			}
			for j := range a.Entries {
				if err := same(a.Entries[j], b.Entries[j]); err != nil {
					t.Errorf("%s, page %d: %v", c, i+1, err)
				}
			}
		}
	}
}

// resume: the entries of a history read page by page are the entries of one pass, in the same order.
func resume(t *testing.T, h Harness) {
	paged, whole := read(t, h(t, Paged)), read(t, h(t, Whole))
	if len(paged) < 2 {
		t.Fatalf("the paged history has %d page, want several", len(paged))
	}
	if len(whole) != 1 {
		t.Fatalf("the whole history has %d pages, want 1", len(whole))
	}
	byPage, onePass := entries(t, paged), entries(t, whole)
	if len(onePass) == 0 {
		t.Fatal("the history has no entry")
	}
	if len(byPage) != len(onePass) {
		t.Fatalf("%d entries page by page, %d in one pass", len(byPage), len(onePass))
	}
	for i := range byPage {
		if err := same(byPage[i], onePass[i]); err != nil {
			t.Errorf("entry %d: %v", i+1, err)
		}
	}
	if last, one := paged[len(paged)-1], whole[0]; !jsonEqual(last.Cursor, one.Cursor) || last.Mode != one.Mode {
		t.Errorf("last cursor %s %s page by page, %s %s in one pass", last.Cursor, last.Mode, one.Cursor, one.Mode)
	}
}

// limits: a rate limit fails the run with RateLimitError, pauses its budget for the pause of the error, and no
// request follows it in that run.
func limits(t *testing.T, h Harness) {
	s := h(t, RateLimited)
	lim := NewLimiter()
	s.Conn.Limiter = lim
	page, err := s.Connector.FetchPage(context.Background(), s.Conn, s.Stream, s.Mode, s.Cursor)
	var limit *connector.RateLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("run: %v, page %+v; want a RateLimitError", err, page)
	}
	if limit.Budget == "" || limit.Pause <= 0 {
		t.Errorf("RateLimitError %+v: want a budget and a pause", limit)
	}
	paused := lim.Paused()
	if len(paused) != 1 || paused[limit.Budget] != limit.Pause {
		t.Errorf("pauses %v, want only %s for %s", paused, limit.Budget, limit.Pause)
	}
	if page.Cursor != nil || len(page.Entries) != 0 {
		t.Errorf("page %+v returned with the rate limit", page)
	}
	// The limit answer is the last of the case: a request after it would be beyond the answers.
	assertAllServed(t, s)
}
