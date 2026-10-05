package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DigitLock/crypto-account-service/internal/repository"
)

// EngineLockKey is the key of the session-level advisory lock of the engine: one instance runs streams
// (SRS — Core §3.2). It differs from ledger.WriterLockKey.
const EngineLockKey int64 = 0x636173_656e67 // "caseng"

// Locker takes the engine lock.
type Locker interface {
	// TryLock takes the lock without waiting. It returns nil and no error when another instance holds it.
	TryLock(ctx context.Context) (Lease, error)
}

// Lease is a held engine lock.
type Lease interface {
	// Held returns an error when the lock is lost: its connection broke or the lock is no longer granted.
	Held(ctx context.Context) error
	// Release gives the lock up.
	Release()
}

// PGLocker takes the engine lock on a dedicated connection taken out of the pool.
type PGLocker struct {
	Pool *pgxpool.Pool
}

// TryLock implements Locker.
func (l PGLocker) TryLock(ctx context.Context) (Lease, error) {
	pooled, err := l.Pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("a connection for the engine lock: %w", err)
	}
	// The session holds the lock: the connection leaves the pool, so that no other query borrows it and
	// closing it ends the session and with it the lock.
	conn := pooled.Hijack()
	taken, err := repository.New(conn).TryEngineLock(ctx, EngineLockKey)
	if err != nil || !taken {
		closeConn(conn)
		if err != nil {
			return nil, fmt.Errorf("try the engine lock: %w", err)
		}
		return nil, nil
	}
	return &pgLease{conn: conn}, nil
}

type pgLease struct {
	conn *pgx.Conn
}

// Held checks on the dedicated connection that its session still holds the lock.
func (l *pgLease) Held(ctx context.Context) error {
	held, err := repository.New(l.conn).EngineLockHeld(ctx, EngineLockKey)
	if err != nil {
		return fmt.Errorf("the connection of the engine lock: %w", err)
	}
	if !held {
		return errors.New("the engine lock is no longer held")
	}
	return nil
}

// Release closes the dedicated connection: the end of the session releases the lock.
func (l *pgLease) Release() { closeConn(l.conn) }

func closeConn(conn *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = conn.Close(ctx)
}
