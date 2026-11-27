// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"
	"unicode"
)

// NormalizeDeployRepository validates and canonicalizes an HTTPS repository URL
// used to bind a deploy token or compare CI deploy provenance. An empty input
// returns "", nil.
func NormalizeDeployRepository(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if len(raw) > 2048 || strings.ContainsAny(raw, "\\#?") || strings.ContainsFunc(raw, func(r rune) bool {
		return unicode.IsControl(r) || unicode.IsSpace(r)
	}) {
		return "", errors.New("supply an HTTPS repository URL without credentials, query parameters or fragments")
	}
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("supply an HTTPS repository URL without credentials, query parameters or fragments")
	}
	host := strings.ToLower(u.Hostname())
	if port := u.Port(); port != "" && port != "443" {
		host += ":" + port
	}
	p := strings.Trim(u.Path, "/")
	p = strings.TrimSuffix(p, ".git")
	p = strings.TrimSuffix(p, "/")
	if p == "" {
		return "", errors.New("supply a repository URL that includes the owner and repository name")
	}
	parts := strings.Split(p, "/")
	if len(parts) < 2 {
		return "", errors.New("supply a repository URL that includes the owner and repository name")
	}
	for _, seg := range parts {
		if seg == "" || seg == "." || seg == ".." || strings.ContainsAny(seg, "\\#?") || strings.ContainsFunc(seg, func(r rune) bool {
			return unicode.IsControl(r) || unicode.IsSpace(r)
		}) {
			return "", errors.New("repository URL contains an invalid path segment")
		}
	}
	return "https://" + host + "/" + strings.Join(parts, "/"), nil
}

// SameDeployRepository reports whether two repository URLs refer to the same
// canonical HTTPS repository.
func SameDeployRepository(a, b string) bool {
	na, errA := NormalizeDeployRepository(a)
	nb, errB := NormalizeDeployRepository(b)
	return errA == nil && errB == nil && na != "" && strings.EqualFold(na, nb)
}

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
// change, reporting whether a key was released. Its immutable origin stays on record.
func (t *RevisionTx) ForgetStaleCIDeploy(ctx context.Context, change string) (bool, error) {
	res, err := t.exec(ctx, `DELETE FROM ci_deploy_keys WHERE change_id = ?
		AND EXISTS (SELECT 1 FROM changes WHERE id = ? AND state = ?)`, change, change, ChangePlanned)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
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
