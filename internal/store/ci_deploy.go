// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// CIDeployKey identifies one deployment of an immutable artifact from a commit.
type CIDeployKey struct {
	Service, Digest, Commit string
}

// CIDeployChange returns the change already recorded for a deploy key.
func (t *Tx) CIDeployChange(ctx context.Context, key CIDeployKey) (string, error) {
	var change string
	err := t.queryRow(ctx, `SELECT change_id FROM ci_deploy_keys
		WHERE service_id = ? AND digest = ? AND commit_sha = ?`, key.Service, key.Digest, key.Commit).Scan(&change)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return change, err
}

// RecordCIDeployKey claims the key in the transaction recording the plan. A
// concurrent winner makes the caller roll back its plan and return the winner.
func (t *RevisionTx) RecordCIDeployKey(ctx context.Context, key CIDeployKey, change string) (bool, error) {
	res, err := t.exec(ctx, `INSERT INTO ci_deploy_keys (service_id, digest, commit_sha, change_id)
		VALUES (?, ?, ?, ?) ON CONFLICT (service_id, digest, commit_sha) DO NOTHING`, key.Service, key.Digest, key.Commit, change)
	if err != nil {
		return false, fmt.Errorf("can't record the CI deploy key: %w", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// CIDeployPlans reads only the correlated plans among the named changes.
func (t *Tx) CIDeployPlans(ctx context.Context, ids []string) (map[string][]byte, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := t.query(ctx, `SELECT c.id, c.plan FROM ci_deploy_keys k JOIN changes c ON c.id = k.change_id
		WHERE k.change_id IN (`+marks(len(ids))+")", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string][]byte)
	for rows.Next() {
		var id string
		var plan []byte
		if err := rows.Scan(&id, &plan); err != nil {
			return nil, err
		}
		out[id] = plan
	}
	return out, rows.Err()
}
