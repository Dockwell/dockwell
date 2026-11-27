// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"errors"
)

// CIDeployKey identifies a deployment of an immutable artifact from a commit.
type CIDeployKey struct {
	Service, Digest, Commit string
}

// CIDeployOrigin is the source of one attempt, including its original image reference.
type CIDeployOrigin struct {
	CIDeployKey
	Provider                                  int32
	Repository, Ref, RunURL, CommitURL, Image string
}

// CIDeployChange returns the latest attempt for a deploy key.
func (t *Tx) CIDeployChange(ctx context.Context, key CIDeployKey) (string, error) {
	var change string
	err := t.queryRow(ctx, `SELECT change_id FROM ci_deploy_keys
		WHERE service_id = ? AND digest = ? AND commit_sha = ?`, key.Service, key.Digest, key.Commit).Scan(&change)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return change, err
}

// RecordCIDeploy records the source and points the key at the new attempt.
// The caller checks key eligibility inside this same serialized write transaction.
func (t *RevisionTx) RecordCIDeploy(ctx context.Context, o CIDeployOrigin, change string) error {
	if _, err := t.exec(ctx, `INSERT INTO ci_deploy_origins
		(change_id, service_id, provider, repository, commit_sha, ref, run_url, commit_url, image, digest)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, change, o.Service, o.Provider, o.Repository, o.Commit,
		o.Ref, o.RunURL, o.CommitURL, o.Image, o.Digest); err != nil {
		return err
	}
	_, err := t.exec(ctx, `INSERT INTO ci_deploy_keys (service_id, digest, commit_sha, change_id)
		VALUES (?, ?, ?, ?) ON CONFLICT (service_id, digest, commit_sha)
		DO UPDATE SET change_id = excluded.change_id`, o.Service, o.Digest, o.Commit, change)
	return err
}

// ForgetStaleCIDeploy releases only the key still pointing at this planned
// change. Its immutable origin stays on record.
func (t *RevisionTx) ForgetStaleCIDeploy(ctx context.Context, change string) error {
	_, err := t.exec(ctx, `DELETE FROM ci_deploy_keys WHERE change_id = ?
		AND EXISTS (SELECT 1 FROM changes WHERE id = ? AND state = ?)`, change, change, ChangePlanned)
	return err
}

// CIDeployOrigins reads the normalized origins among the named changes.
func (t *Tx) CIDeployOrigins(ctx context.Context, ids []string) (map[string]CIDeployOrigin, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := t.query(ctx, `SELECT change_id, service_id, provider, repository, commit_sha, ref, run_url, commit_url, image, digest
		FROM ci_deploy_origins WHERE change_id IN (`+marks(len(ids))+")", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]CIDeployOrigin)
	for rows.Next() {
		var id string
		var o CIDeployOrigin
		if err := rows.Scan(&id, &o.Service, &o.Provider, &o.Repository, &o.Commit, &o.Ref, &o.RunURL, &o.CommitURL, &o.Image, &o.Digest); err != nil {
			return nil, err
		}
		out[id] = o
	}
	return out, rows.Err()
}
