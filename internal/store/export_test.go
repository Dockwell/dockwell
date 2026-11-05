// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"strings"
)

// For the store's tests, which live in package store_test so they can use
// storetest.
var (
	RebindDollar   = rebindDollar
	PostgresSchema = postgresSchema
)

// ExecForTest runs a raw statement in tx, as a bug in a query would.
func ExecForTest(ctx context.Context, tx *Tx, query string) error {
	_, err := tx.tx.ExecContext(ctx, query)
	return err
}

// OpenAt opens the store at location and migrates it only as far as
// version, as an older dockwell left it.
func OpenAt(ctx context.Context, location string, version int) (*DB, error) {
	db, err := open(ctx, location)
	if err != nil {
		return nil, err
	}
	all, err := loadMigrations()
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := db.migrateTo(ctx, all, version); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// OpenKnowing opens the store at location as a dockwell that knows only the
// first n migrations would.
func OpenKnowing(ctx context.Context, location string, n int) (*DB, error) {
	db, err := open(ctx, location)
	if err != nil {
		return nil, err
	}
	all, err := loadMigrations()
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := db.migrateTo(ctx, all[:n], n); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// ChangesInPlanForTest says how the database would run ChangesIn's query,
// as the planner would with a table too big to read whole: Postgres is told
// to avoid reading the whole table where it can, since a test's table is
// small enough that it would.
func ChangesInPlanForTest(ctx context.Context, tx *Tx, state string) (string, error) {
	explain := "EXPLAIN QUERY PLAN "
	if tx.dialect.backend == Postgres {
		if _, err := tx.tx.ExecContext(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
			return "", err
		}
		explain = "EXPLAIN "
	}
	rows, err := tx.query(ctx, explain+changesIn, state)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}
	var plan strings.Builder
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "", err
		}
		fmt.Fprintln(&plan, vals[len(vals)-1])
	}
	return plan.String(), rows.Err()
}

// QueryIntForTest reads one number with a raw query in tx.
func QueryIntForTest(ctx context.Context, tx *Tx, query string, into *int) error {
	return tx.queryRow(ctx, query).Scan(into)
}
