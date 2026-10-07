// Package ledger is the single writer of ledger entries and balance snapshots (ADR-5, SRS — Core UC-102
// steps 4 to 6, EC-117). It validates every page and snapshot as a whole before any write. Amounts are
// decimal strings in Go and NUMERIC in the database. Entries are never updated or deleted.
package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/repository"
)

// WriterLockKey is the key of pg_advisory_xact_lock that every transaction writing ledger entries takes
// first: one writer at a time, so seq order equals commit order (FR-114, SRS — Core §3.2).
const WriterLockKey int64 = 0x636173_6c6564 // "casled"

// Values of SRS — Core §2.1.3, §2.1.4.
var (
	entryTypes   = []string{"DEPOSIT", "WITHDRAWAL", "TRADE", "FEE", "CONVERT", "REWARD", "CARD_DEBIT", "CARD_REFUND"}
	legs         = []string{"SINGLE", "BASE", "QUOTE", "FEE"}
	directions   = []string{"IN", "OUT"}
	accountTypes = []string{"SPOT", "FUNDING", "EARN_FLEXIBLE", "EARN_LOCKED", "WALLET"}
	// A plain decimal: at most 20 integer digits and 18 decimal places, no sign, no exponent.
	plainDecimal = regexp.MustCompile(`^[0-9]{1,20}(\.[0-9]{1,18})?$`)
)

// InvalidError is a page or snapshot refused as a whole (EC-117). Its message names the item and the rule,
// never an amount of the source.
type InvalidError struct{ Reason string }

func (e *InvalidError) Error() string { return "ledger: refused: " + e.Reason }

// DB is what the writer needs from the database. *pgxpool.Pool satisfies it.
type DB interface {
	repository.DBTX
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Writer writes pages and snapshots.
type Writer struct {
	db  DB
	now func() time.Time
}

// NewWriter returns a writer on db; now is the clock of created_at.
func NewWriter(db DB, now func() time.Time) *Writer {
	return &Writer{db: db, now: now}
}

// PageResult is the outcome of a page.
type PageResult struct {
	Inserted, Skipped int
	// Gaps are the gaps of the balance checkpoint of the page, one per native asset; none without a checkpoint.
	Gaps []Gap
	// Unmapped lists the native asset of every inserted entry that has no alias: stored under its native code.
	Unmapped []string
	// Gone: the connection was deleted; nothing was written and the run stops without a failure.
	Gone bool
}

// Gap is the comparison of one checkpoint balance with the ledger (SRS — Core Connector contract, Balance
// checkpoint): LedgerTotal = Σ IN − Σ OUT of the connection for the native asset, Gap = balance − LedgerTotal.
// Plain decimals, possibly negative.
type Gap struct {
	NativeAsset, Asset string
	LedgerTotal, Gap   string
}

// SnapshotResult is the outcome of a snapshot.
type SnapshotResult struct {
	Unmapped []string
	Gone     bool
}

// WritePage stores the entries of a page of a ledger stream and moves its cursor and mode, in one
// transaction (FR-107). stream is the full stream name; family is stored as ledger_entries.stream. A balance
// checkpoint of the page is compared with the ledger after the entries, in the same transaction, and stored in
// balance_checkpoints (S3 D-5); the same rule for every source.
func (w *Writer) WritePage(ctx context.Context, connectionID uuid.UUID, stream, family string, page connector.Page) (PageResult, error) {
	if err := ValidatePage(page); err != nil {
		return PageResult{}, err
	}
	cursor := page.Cursor
	if len(cursor) == 0 {
		cursor = json.RawMessage(`{}`)
	}
	var res PageResult
	err := pgx.BeginFunc(ctx, w.db, func(tx pgx.Tx) error {
		res = PageResult{}
		q := repository.New(tx)
		if err := q.LockLedgerWriter(ctx, WriterLockKey); err != nil {
			return fmt.Errorf("lock the ledger writer: %w", err)
		}
		conn, err := q.LockConnectionForWrite(ctx, connectionID)
		if errors.Is(err, pgx.ErrNoRows) {
			res.Gone = true
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock the connection: %w", err)
		}
		aliases, err := aliasesOf(ctx, q, conn.SourceID, append(entryAssets(page.Entries), checkpointAssets(page.Checkpoint)...))
		if err != nil {
			return err
		}
		for _, e := range page.Entries {
			asset, mapped := aliases[e.NativeAsset]
			if !mapped {
				asset = e.NativeAsset
			}
			n, err := q.InsertLedgerEntry(ctx, repository.InsertLedgerEntryParams{
				TenantID: conn.TenantID, ConnectionID: connectionID, Stream: family, ExternalID: e.ExternalID,
				Leg: e.Leg, GroupID: family + ":" + e.ExternalID, Type: e.Type, Direction: e.Direction,
				Asset: asset, NativeAsset: e.NativeAsset, Amount: e.Amount, OccurredAt: e.OccurredAt, Raw: e.Raw,
			})
			if err != nil {
				return fmt.Errorf("insert an entry: %w", err)
			}
			if n == 0 {
				res.Skipped++
				continue
			}
			res.Inserted++
			if !mapped {
				res.Unmapped = append(res.Unmapped, e.NativeAsset)
			}
		}
		n, err := q.UpdateStreamCursor(ctx, repository.UpdateStreamCursorParams{
			ConnectionID: connectionID, Stream: stream, Cursor: cursor, Mode: string(page.Mode),
		})
		if err != nil {
			return fmt.Errorf("move the cursor: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("move the cursor: no cursor of stream %s", stream)
		}
		gaps, unmapped, err := w.storeCheckpoint(ctx, q, connectionID, page.Checkpoint, aliases)
		res.Gaps, res.Unmapped = gaps, append(res.Unmapped, unmapped...)
		return err
	})
	if err != nil {
		return PageResult{}, err
	}
	return res, nil
}

// WriteSnapshot stores a snapshot with its balances in one transaction. A snapshot without balances
// stores the header only.
func (w *Writer) WriteSnapshot(ctx context.Context, connectionID uuid.UUID, snap connector.Snapshot) (SnapshotResult, error) {
	if err := ValidateSnapshot(snap); err != nil {
		return SnapshotResult{}, err
	}
	var res SnapshotResult
	err := pgx.BeginFunc(ctx, w.db, func(tx pgx.Tx) error {
		res = SnapshotResult{}
		q := repository.New(tx)
		conn, err := q.LockConnectionForWrite(ctx, connectionID)
		if errors.Is(err, pgx.ErrNoRows) {
			res.Gone = true
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock the connection: %w", err)
		}
		natives := make([]string, 0, len(snap.Balances))
		for _, b := range snap.Balances {
			natives = append(natives, b.NativeAsset)
		}
		aliases, err := aliasesOf(ctx, q, conn.SourceID, natives)
		if err != nil {
			return err
		}
		snapshotID, err := q.InsertBalanceSnapshot(ctx, repository.InsertBalanceSnapshotParams{
			ConnectionID: connectionID, TakenAt: snap.TakenAt, CreatedAt: w.now().UTC().Truncate(time.Microsecond),
		})
		if err != nil {
			return fmt.Errorf("insert the snapshot: %w", err)
		}
		for _, b := range snap.Balances {
			asset, mapped := aliases[b.NativeAsset]
			if !mapped {
				asset = b.NativeAsset
				res.Unmapped = append(res.Unmapped, b.NativeAsset)
			}
			if err := q.InsertSnapshotBalance(ctx, repository.InsertSnapshotBalanceParams{
				SnapshotID: snapshotID, AccountType: b.AccountType, NativeAsset: b.NativeAsset, Asset: asset,
				Free: b.Free, Locked: b.Locked,
			}); err != nil {
				return fmt.Errorf("insert a balance: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return SnapshotResult{}, err
	}
	return res, nil
}

// ValidatePage checks every entry of a page; one invalid entry refuses the page (EC-117).
func ValidatePage(page connector.Page) error {
	if page.Mode != connector.ModeBackfill && page.Mode != connector.ModeIncremental {
		return &InvalidError{Reason: "the page has an unknown mode"}
	}
	if len(page.Cursor) > 0 && !json.Valid(page.Cursor) {
		return &InvalidError{Reason: "the cursor of the page is not JSON"}
	}
	if err := ValidateCheckpoint(page.Checkpoint); err != nil {
		return err
	}
	for i, e := range page.Entries {
		item := fmt.Sprintf("entry %d", i+1)
		switch {
		case e.ExternalID == "":
			return &InvalidError{Reason: item + ": external_id is empty"}
		case !slices.Contains(entryTypes, e.Type):
			return &InvalidError{Reason: item + ": unknown type"}
		case !slices.Contains(legs, e.Leg):
			return &InvalidError{Reason: item + ": unknown leg"}
		case !slices.Contains(directions, e.Direction):
			return &InvalidError{Reason: item + ": unknown direction"}
		case e.NativeAsset == "":
			return &InvalidError{Reason: item + ": native asset is empty"}
		case e.OccurredAt.IsZero():
			return &InvalidError{Reason: item + ": occurred_at is not set"}
		case len(e.Raw) == 0 || !json.Valid(e.Raw):
			return &InvalidError{Reason: item + ": the raw record is not JSON"}
		}
		if err := checkAmount(e.Amount, true); err != nil {
			return &InvalidError{Reason: item + ": amount " + err.Error()}
		}
	}
	return nil
}

// storeCheckpoint compares each balance of a checkpoint with the entries of the connection, those of the page
// included, and replaces the row of the connection and native asset in balance_checkpoints.
func (w *Writer) storeCheckpoint(ctx context.Context, q *repository.Queries, connectionID uuid.UUID, cp *connector.Checkpoint,
	aliases map[string]string) ([]Gap, []string, error) {
	if cp == nil {
		return nil, nil, nil
	}
	var block *int64
	if cp.BlockNumber != nil {
		n := int64(*cp.BlockNumber)
		block = &n
	}
	var hash *string
	if cp.BlockHash != "" {
		hash = &cp.BlockHash
	}
	checkedAt := w.now().UTC().Truncate(time.Microsecond)
	var gaps []Gap
	var unmapped []string
	for _, b := range cp.Balances {
		asset, mapped := aliases[b.NativeAsset]
		if !mapped {
			asset = b.NativeAsset
			unmapped = append(unmapped, b.NativeAsset)
		}
		row, err := q.UpsertBalanceCheckpoint(ctx, repository.UpsertBalanceCheckpointParams{
			ConnectionID: connectionID, NativeAsset: b.NativeAsset, Asset: asset, BlockNumber: block, BlockHash: hash,
			TakenAt: cp.TakenAt, CheckedAt: checkedAt, Free: b.Free, Locked: b.Locked,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("store the balance checkpoint: %w", err)
		}
		gaps = append(gaps, Gap{NativeAsset: b.NativeAsset, Asset: asset, LedgerTotal: row.LedgerTotal, Gap: row.Gap})
	}
	return gaps, unmapped, nil
}

func checkpointAssets(cp *connector.Checkpoint) []string {
	if cp == nil {
		return nil
	}
	natives := make([]string, 0, len(cp.Balances))
	for _, b := range cp.Balances {
		natives = append(natives, b.NativeAsset)
	}
	return natives
}

// ValidateCheckpoint checks a balance checkpoint by the rules of a snapshot balance, one balance per native asset;
// an invalid checkpoint refuses its page (EC-117).
func ValidateCheckpoint(cp *connector.Checkpoint) error {
	if cp == nil {
		return nil
	}
	if cp.TakenAt.IsZero() {
		return &InvalidError{Reason: "the balance checkpoint has no time"}
	}
	if cp.BlockNumber != nil && *cp.BlockNumber > math.MaxInt64 {
		return &InvalidError{Reason: "the block of the balance checkpoint is out of range"}
	}
	seen := make(map[string]bool, len(cp.Balances))
	for i, b := range cp.Balances {
		if seen[b.NativeAsset] {
			return &InvalidError{Reason: fmt.Sprintf("checkpoint balance %d: the native asset appears twice", i+1)}
		}
		seen[b.NativeAsset] = true
	}
	if err := ValidateSnapshot(connector.Snapshot{TakenAt: cp.TakenAt, Balances: cp.Balances}); err != nil {
		var inv *InvalidError
		if errors.As(err, &inv) {
			return &InvalidError{Reason: "balance checkpoint: " + inv.Reason}
		}
		return err
	}
	return nil
}

// ValidateSnapshot checks every balance of a snapshot; one invalid balance refuses the snapshot (EC-117).
func ValidateSnapshot(snap connector.Snapshot) error {
	if snap.TakenAt.IsZero() {
		return &InvalidError{Reason: "the snapshot has no time"}
	}
	seen := make(map[[2]string]bool, len(snap.Balances))
	for i, b := range snap.Balances {
		item := fmt.Sprintf("balance %d", i+1)
		key := [2]string{b.AccountType, b.NativeAsset}
		switch {
		case !slices.Contains(accountTypes, b.AccountType):
			return &InvalidError{Reason: item + ": unknown account type"}
		case b.NativeAsset == "":
			return &InvalidError{Reason: item + ": native asset is empty"}
		case seen[key]:
			return &InvalidError{Reason: item + ": the account type and asset appear twice"}
		}
		seen[key] = true
		if err := checkAmount(b.Free, false); err != nil {
			return &InvalidError{Reason: item + ": free " + err.Error()}
		}
		if err := checkAmount(b.Locked, false); err != nil {
			return &InvalidError{Reason: item + ": locked " + err.Error()}
		}
	}
	return nil
}

// checkAmount accepts a plain decimal with at most 20 integer digits and 18 decimal places; positive
// when required, else not negative. The message does not quote the amount.
func checkAmount(s string, positive bool) error {
	if !plainDecimal.MatchString(s) {
		return errors.New("is not a plain decimal of at most 20 integer digits and 18 decimal places")
	}
	if positive && strings.Trim(s, "0.") == "" {
		return errors.New("is not positive")
	}
	return nil
}

func entryAssets(entries []connector.Entry) []string {
	natives := make([]string, 0, len(entries))
	for _, e := range entries {
		natives = append(natives, e.NativeAsset)
	}
	return natives
}

func aliasesOf(ctx context.Context, q *repository.Queries, sourceID int16, natives []string) (map[string]string, error) {
	rows, err := q.ListAssetAliases(ctx, repository.ListAssetAliasesParams{SourceID: sourceID, NativeAssets: natives})
	if err != nil {
		return nil, fmt.Errorf("read the asset aliases: %w", err)
	}
	aliases := make(map[string]string, len(rows))
	for _, r := range rows {
		aliases[r.NativeAsset] = r.Asset
	}
	return aliases, nil
}
