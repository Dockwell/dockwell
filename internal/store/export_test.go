// SPDX-License-Identifier: AGPL-3.0-only

package store

import "context"

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
