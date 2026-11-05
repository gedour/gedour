package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/gedour/gedour/pkg/audit"
	"github.com/gedour/gedour/pkg/testpg"
)

func txPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := Connect(ctx, testpg.NewDB(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE d (id int PRIMARY KEY); INSERT INTO d VALUES (1), (2)`); err != nil {
		t.Fatal(err)
	}
	return pool
}

// deadlock makes tx the victim of a real lock cycle with a peer: the short timeout on tx's side and the long
// one on the peer's decide who detects it. The peer ends once tx rolled back.
func deadlock(t *testing.T, pool *pgxpool.Pool, tx pgx.Tx) error {
	t.Helper()
	ctx := context.Background()
	peer, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{`SET LOCAL deadlock_timeout = '60s'`, `UPDATE d SET id = id WHERE id = 2`} {
		if _, err := peer.Exec(ctx, sql); err != nil {
			t.Fatalf("peer %s: %v", sql, err)
		}
	}
	for _, sql := range []string{`SET LOCAL deadlock_timeout = '20ms'`, `UPDATE d SET id = id WHERE id = 1`} {
		if _, err := tx.Exec(ctx, sql); err != nil {
			return fmt.Errorf("%s: %w", sql, err)
		}
	}
	go func() {
		_, _ = peer.Exec(ctx, `UPDATE d SET id = id WHERE id = 1`)
		_ = peer.Rollback(ctx)
	}()
	testpg.WaitLockWaiters(t, pool, 1)
	_, err = tx.Exec(ctx, `UPDATE d SET id = id WHERE id = 2`)
	return err
}

// The second attempt starts clean: the closure's reset gives one result, the rollback one audit row.
func TestInTxRetriesDeadlock(t *testing.T) {
	pool := txPool(t)
	retries := testpg.AllowDeadlockRetries(t)
	ctx := context.Background()
	var results []string
	calls := 0
	err := InTx(ctx, pool, func(tx pgx.Tx) error {
		results = nil
		calls++
		results = append(results, "ran")
		if err := audit.Record(ctx, tx, audit.Entry{Action: "t.attempt", TargetType: "t", TargetID: "1"}); err != nil {
			return err
		}
		if calls == 1 {
			// As a caller wraps it.
			return fmt.Errorf("step: %w", deadlock(t, pool, tx))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(results) != 1 || retries() != 1 {
		t.Fatalf("calls %d, results %v, logged retries %d: want 2, one result, 1", calls, results, retries())
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 't.attempt'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("%d audit rows, want 1: the rolled-back attempt must leave none", rows)
	}
}

func TestInTxGivesUpAfterTwoDeadlocks(t *testing.T) {
	pool := txPool(t)
	retries := testpg.AllowDeadlockRetries(t)
	calls := 0
	err := InTx(context.Background(), pool, func(tx pgx.Tx) error {
		calls++
		return deadlock(t, pool, tx)
	})
	if !isDeadlock(err) || calls != 2 || retries() != 1 {
		t.Fatalf("err %v, calls %d, logged retries %d: want a deadlock after 2 calls, 1 retry", err, calls, retries())
	}
}

// A commit is where a deferred trigger fails: it counts as much as a failed statement.
func TestInTxRetriesDeadlockAtCommit(t *testing.T) {
	pool := txPool(t)
	testpg.AllowDeadlockRetries(t)
	release := testpg.ForceDeadlockOnce(t, pool, "d")
	release()
	calls := 0
	err := InTx(context.Background(), pool, func(tx pgx.Tx) error {
		calls++
		_, err := tx.Exec(context.Background(), `UPDATE d SET id = id WHERE id = 1`)
		return err
	})
	if err != nil || calls != 2 {
		t.Fatalf("err %v, calls %d: want success on the second call", err, calls)
	}
}

func TestInTxRunsOnceOnOtherErrors(t *testing.T) {
	pool := txPool(t)
	sentinel := errors.New("nope")
	for name, fn := range map[string]func(pgx.Tx) error{
		"plain":                 func(pgx.Tx) error { return sentinel },
		"serialization failure": func(pgx.Tx) error { return fmt.Errorf("x: %w", &pgconn.PgError{Code: "40001"}) },
		"real SQL error": func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), `INSERT INTO d VALUES (1)`)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			err := InTx(context.Background(), pool, func(tx pgx.Tx) error {
				calls++
				return fn(tx)
			})
			if err == nil || calls != 1 {
				t.Fatalf("err %v, calls %d: want the error after one call", err, calls)
			}
		})
	}
}

// A real victim whose request ended meanwhile: no second attempt, and the caller gets the deadlock.
func TestInTxCancelledDoesNotRetry(t *testing.T) {
	pool := txPool(t)
	retries := testpg.AllowDeadlockRetries(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	err := InTx(ctx, pool, func(tx pgx.Tx) error {
		calls++
		err := deadlock(t, pool, tx)
		cancel()
		return err
	})
	if calls != 1 || retries() != 0 || !isDeadlock(err) || errors.Is(err, context.Canceled) {
		t.Fatalf("calls %d, retries %d, err %v: want one call, no retry, the deadlock itself", calls, retries(), err)
	}
}

// Warn, named tx_deadlock_retry, on the line of InTx's caller.
func TestInTxLogsRetry(t *testing.T) {
	pool := txPool(t)
	core, logs := observer.New(zap.InfoLevel)
	defer zap.ReplaceGlobals(zap.New(core, zap.AddCaller()))()
	calls := 0
	err := InTx(context.Background(), pool, func(pgx.Tx) error {
		if calls++; calls == 1 {
			return &pgconn.PgError{Code: "40P01"}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	all := logs.All()
	if len(all) != 1 || all[0].Message != "tx_deadlock_retry" || all[0].Level != zap.WarnLevel || !strings.Contains(all[0].Caller.File, "tx_test.go") {
		t.Fatalf("logged %+v: want one warn tx_deadlock_retry from this file", all)
	}
}

func TestIsDeadlock(t *testing.T) {
	pe := &pgconn.PgError{Code: "40P01"}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"bare", pe, true},
		{"wrapped twice", fmt.Errorf("a: %w", fmt.Errorf("b: %w", pe)), true},
		{"joined", errors.Join(errors.New("x"), pe), true},
		{"other code", &pgconn.PgError{Code: "40001"}, false},
		{"text only", errors.New("deadlock detected (SQLSTATE 40P01)"), false},
	} {
		if got := isDeadlock(tc.err); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}
