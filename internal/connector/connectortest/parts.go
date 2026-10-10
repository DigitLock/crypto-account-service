package connectortest

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/DigitLock/crypto-account-service/internal/connector"
)

// SnapshotCase is a case of the snapshot part.
type SnapshotCase string

const (
	// SnapshotComplete: every source of the snapshot answers.
	SnapshotComplete SnapshotCase = "snapshot_complete"
	// SnapshotSourceFails: one source answers with an error that is not a rate limit; the others may answer.
	SnapshotSourceFails SnapshotCase = "snapshot_source_fails"
	// SnapshotRateLimited: one source answers with a rate limit, as the last answer of the case.
	SnapshotRateLimited SnapshotCase = "snapshot_rate_limited"
)

// SnapshotSetup is one case of the snapshot part: a connector in its initial state, the connection, and the answers
// of the source, ready to be served. The clock of the connector is fixed by the harness, so the same answers give
// the same time.
type SnapshotSetup struct {
	Connector connector.Connector
	// Conn is the connection with its key; the suite sets its Limiter.
	Conn   connector.Connection
	Served func() (served, answers int)
}

// SnapshotHarness returns a new SnapshotSetup for a case.
type SnapshotHarness func(t *testing.T, c SnapshotCase) SnapshotSetup

// KeyCase is a case of the key-check part.
type KeyCase string

const (
	// KeyReadOnly: the source reports a key that can only read.
	KeyReadOnly KeyCase = "key_read_only"
	// KeyNotReadOnly: the source reports a key with one permission beyond reading.
	KeyNotReadOnly KeyCase = "key_not_read_only"
	// KeyRejected: the source rejects the key.
	KeyRejected KeyCase = "key_rejected"
)

// KeySetup is one case of the key-check part.
type KeySetup struct {
	Connector   connector.Connector
	Source      connector.Source
	Credentials connector.Credentials
	// Permission is the name of the permission beyond reading of the case KeyNotReadOnly, as the source names it.
	Permission string
	Served     func() (served, answers int)
}

// KeyHarness returns a new KeySetup for a case.
type KeyHarness func(t *testing.T, c KeyCase) KeySetup

func assertServed(t *testing.T, served func() (int, int)) {
	t.Helper()
	if s, a := served(); s != a {
		t.Errorf("%d of %d answers served", s, a)
	}
}

func fetchSnapshot(t *testing.T, h SnapshotHarness, c SnapshotCase) (connector.Snapshot, error, *Limiter) {
	t.Helper()
	s := h(t, c)
	lim := NewLimiter()
	s.Conn.Limiter = lim
	snap, err := s.Connector.FetchSnapshot(context.Background(), s.Conn)
	assertServed(t, s.Served)
	return snap, err, lim
}

func runSnapshot(t *testing.T, h SnapshotHarness) {
	t.Run("equal snapshot from the same answers", func(t *testing.T) {
		first, err, lim := fetchSnapshot(t, h, SnapshotComplete)
		if err != nil {
			t.Fatal(err)
		}
		second, err, _ := fetchSnapshot(t, h, SnapshotComplete)
		if err != nil {
			t.Fatal(err)
		}
		if len(first.Balances) == 0 || first.TakenAt.IsZero() {
			t.Fatalf("snapshot %+v: want a time and balances", first)
		}
		if !first.TakenAt.Equal(second.TakenAt) || !slices.Equal(first.Balances, second.Balances) {
			t.Errorf("snapshots differ:\n %v %+v\n %v %+v", first.TakenAt, first.Balances, second.TakenAt, second.Balances)
		}
		if len(lim.Reserved()) == 0 {
			t.Error("no reservation in the limiter")
		}
	})
	t.Run("one failing source fails the whole snapshot", func(t *testing.T) {
		snap, err, _ := fetchSnapshot(t, h, SnapshotSourceFails)
		var limit *connector.RateLimitError
		if err == nil || errors.As(err, &limit) {
			t.Errorf("error %v, want a failure that is not a rate limit", err)
		}
		if len(snap.Balances) != 0 || !snap.TakenAt.IsZero() {
			t.Errorf("snapshot %+v returned with the failure", snap)
		}
	})
	t.Run("limit handling", func(t *testing.T) {
		snap, err, lim := fetchSnapshot(t, h, SnapshotRateLimited)
		var limit *connector.RateLimitError
		if !errors.As(err, &limit) {
			t.Fatalf("error %v, want a RateLimitError", err)
		}
		if limit.Budget == "" || limit.Pause <= 0 {
			t.Errorf("RateLimitError %+v: want a budget and a pause", limit)
		}
		if paused := lim.Paused(); len(paused) != 1 || paused[limit.Budget] != limit.Pause {
			t.Errorf("pauses %v, want only %s for %s", paused, limit.Budget, limit.Pause)
		}
		if len(snap.Balances) != 0 {
			t.Errorf("balances returned with the rate limit: %+v", snap.Balances)
		}
		// The limit answer is the last of the case: a request after it would be beyond the answers.
	})
}

func checkKey(t *testing.T, h KeyHarness, c KeyCase) (connector.AccountInfo, error, KeySetup, *Limiter) {
	t.Helper()
	s := h(t, c)
	lim := NewLimiter()
	info, err := s.Connector.CheckAccount(context.Background(), s.Source, s.Credentials, lim)
	assertServed(t, s.Served)
	return info, err, s, lim
}

func runKeyCheck(t *testing.T, h KeyHarness) {
	t.Run("read-only key accepted", func(t *testing.T) {
		info, err, s, lim := checkKey(t, h, KeyReadOnly)
		if err != nil {
			t.Fatal(err)
		}
		if info.Identity == "" || !slices.Equal(info.Permissions, []string{connector.PermissionRead}) {
			t.Errorf("account %+v, want an identity and [READ]", info)
		}
		if !s.Connector.Capabilities().PermissionsReadable {
			t.Error("the connector checks keys but does not declare PermissionsReadable")
		}
		if len(lim.Reserved()) == 0 {
			t.Error("no reservation in the limiter")
		}
	})
	t.Run("key beyond reading rejected with its name", func(t *testing.T) {
		_, err, s, _ := checkKey(t, h, KeyNotReadOnly)
		var notReadOnly *connector.KeyNotReadOnlyError
		if !errors.As(err, &notReadOnly) || s.Permission == "" || !slices.Contains(notReadOnly.Permissions, s.Permission) {
			t.Errorf("error %v, want KeyNotReadOnlyError naming %q", err, s.Permission)
		}
	})
	t.Run("rejected key", func(t *testing.T) {
		if _, err, _, _ := checkKey(t, h, KeyRejected); !errors.Is(err, connector.ErrKeyRejected) {
			t.Errorf("error %v, want ErrKeyRejected", err)
		}
	})
}
