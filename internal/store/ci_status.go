// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/dockwell/dockwell/internal/id"
)

// CI deploy status transition states recorded in ci_status_outbox.
const (
	CIStatusQueued     = "queued"
	CIStatusApplying   = "applying"
	CIStatusHealthy    = "healthy"
	CIStatusFailed     = "failed"
	CIStatusRolledBack = "rolled_back"
	CIStatusDiscarded  = "discarded"
)

// Delivery states for ci_status_outbox entries.
const (
	CIStatusDeliveryPending    = "pending"
	CIStatusDeliveryDelivered  = "delivered"
	CIStatusDeliverySkipped    = "skipped"
	CIStatusDeliveryFailed     = "failed"
	CIStatusDeliverySuperseded = "superseded"
)

// Structured reason codes stored on ci_status_outbox entries so clients can
// localize delivery issues and link directly to the relevant fix.
const (
	CIStatusReasonNoTokenRepository       = "no_token_repository" //nolint:gosec // reason code name
	CIStatusReasonRepositoryMismatch      = "repository_mismatch"
	CIStatusReasonProviderUnsupported     = "provider_unsupported"
	CIStatusReasonNoGitHubApp             = "no_github_app"
	CIStatusReasonMissingChecksPermission = "missing_checks_permission"
	CIStatusReasonGitHubNotReached        = "github_not_reached"
	CIStatusReasonGitHubAuthFailed        = "github_auth_failed"
	CIStatusReasonNoGitLabConnection      = "no_gitlab_connection"
	CIStatusReasonGitLabNotReached        = "gitlab_not_reached"
	CIStatusReasonGitLabAuthFailed        = "gitlab_auth_failed"
	CIStatusReasonGitLabMissingScope      = "gitlab_missing_scope"
	CIStatusReasonNetguardBlocked         = "netguard_blocked"
	CIStatusReasonGaveUp                  = "gave_up"
)

// ErrCIStatusNotRetryable reports an attempt to retry a status that is not failed or skipped.
var ErrCIStatusNotRetryable = errors.New("CI deploy status is not in a retryable state")

// CIStatusEntry is one durable CI deploy status transition queued for delivery
// back to GitHub or GitLab.
type CIStatusEntry struct {
	ID                string
	ChangeID          string
	Transition        int
	State             string
	ServiceID         string
	ServiceName       string
	EnvironmentName   string
	Provider          int32
	Repository        string
	TokenID           string
	AllowedRepository string
	Commit            string
	Ref               string
	RunURL            string
	Digest            string
	ChangeURL         string
	Summary           string
	IdempotencyKey    string
	DeliveryState     string
	RemoteID          string
	RemoteURL         string
	CredentialRef     string
	ReasonCode        string
	Attempts          int
	NextAttemptAt     time.Time
	LastError         string
	CreatedAt         time.Time
	RetrySince        time.Time
	UpdatedAt         time.Time
	DeliveredAt       *time.Time
}

// CIStatusEnqueueOpts carries optional metadata recorded when enqueuing a CI
// deploy status transition. Unset fields inherit from earlier transitions of
// the same change or from the service resource.
type CIStatusEnqueueOpts struct {
	ServiceName       string
	EnvironmentName   string
	TokenID           string
	AllowedRepository string
	ChangeURL         string
	Summary           string
}

// CIStatusDue identifies a change with pending transitions due for delivery,
// with its repository and provider so the worker can group work by host.
type CIStatusDue struct {
	ChangeID   string
	Repository string
	Provider   int32
}

const ciStatusColumns = `id, change_id, transition, state, service_id, service_name, environment_name, provider, repository, token_id, allowed_repository, commit_sha, ref, run_url, digest, change_url, summary, idempotency_key, delivery_state, remote_id, remote_url, credential_ref, reason_code, attempts, next_attempt_at, last_error, created_at, retry_since, updated_at, delivered_at`

func scanCIStatus(row interface{ Scan(...any) error }) (CIStatusEntry, error) {
	var e CIStatusEntry
	var nextAt, createdAt, retrySince, updatedAt int64
	var deliveredAt sql.NullInt64
	err := row.Scan(
		&e.ID, &e.ChangeID, &e.Transition, &e.State, &e.ServiceID, &e.ServiceName, &e.EnvironmentName,
		&e.Provider, &e.Repository, &e.TokenID, &e.AllowedRepository, &e.Commit, &e.Ref, &e.RunURL, &e.Digest,
		&e.ChangeURL, &e.Summary, &e.IdempotencyKey, &e.DeliveryState,
		&e.RemoteID, &e.RemoteURL, &e.CredentialRef, &e.ReasonCode, &e.Attempts,
		&nextAt, &e.LastError, &createdAt, &retrySince, &updatedAt, &deliveredAt,
	)
	if err != nil {
		return CIStatusEntry{}, err
	}
	e.NextAttemptAt = fromMillis(nextAt)
	e.CreatedAt = fromMillis(createdAt)
	if retrySince > 0 {
		e.RetrySince = fromMillis(retrySince)
	}
	e.UpdatedAt = fromMillis(updatedAt)
	e.DeliveredAt = timePtr(deliveredAt)
	return e, nil
}

// EnqueueCIStatusForChange writes a status transition to the durable outbox in
// the same transaction as the change's state transition. Changes without a
// recorded deploy origin get no outbox entries.
func (t *RevisionTx) EnqueueCIStatusForChange(ctx context.Context, changeID, state string, opts CIStatusEnqueueOpts, now time.Time) (bool, error) {
	var o CIDeployOrigin
	err := t.queryRow(ctx, `SELECT service_id, provider, repository, commit_sha, ref, run_url, commit_url, image, digest
		FROM ci_deploy_origins WHERE change_id = ?`, changeID).Scan(
		&o.Service, &o.Provider, &o.Repository, &o.Commit, &o.Ref, &o.RunURL, &o.CommitURL, &o.Image, &o.Digest,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading CI deploy origin for %s: %w", changeID, err)
	}
	var lastTransition int
	if err := t.queryRow(ctx, `SELECT COALESCE(MAX(transition), 0) FROM ci_status_outbox WHERE change_id = ?`, changeID).Scan(&lastTransition); err != nil {
		return false, fmt.Errorf("reading CI status transition for %s: %w", changeID, err)
	}
	var prevState, prevRemoteID, prevRemoteURL, prevCred, prevServiceName, prevEnvName, prevTokenID, prevAllowedRepo, prevChangeURL string
	if lastTransition > 0 {
		_ = t.queryRow(ctx, `SELECT state, remote_id, remote_url, credential_ref, service_name, environment_name, token_id, allowed_repository, change_url
			FROM ci_status_outbox WHERE change_id = ? ORDER BY transition DESC LIMIT 1`, changeID).Scan(
			&prevState, &prevRemoteID, &prevRemoteURL, &prevCred, &prevServiceName, &prevEnvName, &prevTokenID, &prevAllowedRepo, &prevChangeURL,
		)
		// One final status per change (N9): healthy, rolled_back, and discarded
		// never enqueue a second terminal transition (e.g. discarding a change
		// that was already marked discarded when superseded).
		if prevState == CIStatusHealthy || prevState == CIStatusRolledBack || prevState == CIStatusDiscarded {
			return false, nil
		}
	}
	serviceName := opts.ServiceName
	if serviceName == "" {
		serviceName = prevServiceName
	}
	envName := opts.EnvironmentName
	if envName == "" {
		envName = prevEnvName
	}
	if serviceName == "" || envName == "" {
		var dbSvc, dbEnv string
		_ = t.queryRow(ctx, `SELECT s.name, COALESCE(e.name, '')
			FROM resources s LEFT JOIN resources e ON e.id = s.scope
			WHERE s.id = ?`, o.Service).Scan(&dbSvc, &dbEnv)
		if serviceName == "" {
			serviceName = dbSvc
		}
		if envName == "" {
			envName = dbEnv
		}
	}
	tokenID := opts.TokenID
	if tokenID == "" {
		tokenID = prevTokenID
	}
	allowedRepo := opts.AllowedRepository
	if allowedRepo == "" {
		allowedRepo = prevAllowedRepo
	}
	changeURL := opts.ChangeURL
	if changeURL == "" {
		changeURL = prevChangeURL
	}
	transition := lastTransition + 1
	key := fmt.Sprintf("dockwell:%s:%d:%s", changeID, transition, state)
	ms := millis(now)
	if _, err := t.exec(ctx, `INSERT INTO ci_status_outbox (`+ciStatusColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id.New(id.CIStatus), changeID, transition, state, o.Service, serviceName, envName,
		o.Provider, o.Repository, tokenID, allowedRepo, o.Commit, o.Ref, o.RunURL, o.Digest,
		changeURL, opts.Summary, key, CIStatusDeliveryPending,
		prevRemoteID, prevRemoteURL, prevCred, "", 0,
		ms, "", ms, 0, ms, nil,
	); err != nil {
		return false, fmt.Errorf("recording CI status %s for %s: %w", state, changeID, err)
	}
	return true, nil
}

// CIStatusEnqueueDiscardedForPlanned records a discarded transition for any
// other planned CI deploy changes touching serviceIDs that have been superseded
// by a newer change committing on those services.
func (t *RevisionTx) CIStatusEnqueueDiscardedForPlanned(ctx context.Context, serviceIDs []string, excludeChangeID, summary string, changeURL func(string) string, now time.Time) (bool, error) {
	if len(serviceIDs) == 0 {
		return false, nil
	}
	args := make([]any, 0, len(serviceIDs)+3)
	args = append(args, ChangePlanned, excludeChangeID)
	for _, sid := range serviceIDs {
		args = append(args, sid)
	}
	args = append(args, CIStatusDiscarded)
	rows, err := t.query(ctx, `SELECT o.change_id FROM ci_deploy_origins o
		JOIN changes c ON c.id = o.change_id
		WHERE c.state = ? AND o.change_id != ? AND o.service_id IN (`+marks(len(serviceIDs))+`)
		  AND NOT EXISTS (
			SELECT 1 FROM ci_status_outbox s WHERE s.change_id = o.change_id AND s.state = ?
		  )
		ORDER BY o.change_id ASC`, args...)
	if err != nil {
		return false, fmt.Errorf("finding superseded planned CI deploys: %w", err)
	}
	var ids []string
	for rows.Next() {
		var cid string
		if err := rows.Scan(&cid); err != nil {
			_ = rows.Close()
			return false, err
		}
		ids = append(ids, cid)
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	enqueuedAny := false
	for _, cid := range ids {
		url := ""
		if changeURL != nil {
			url = changeURL(cid)
		}
		enqueued, err := t.EnqueueCIStatusForChange(ctx, cid, CIStatusDiscarded, CIStatusEnqueueOpts{
			ChangeURL: url,
			Summary:   summary,
		}, now)
		if err != nil {
			return false, err
		}
		if enqueued {
			enqueuedAny = true
		}
		if _, err := t.ForgetStaleCIDeploy(ctx, cid); err != nil {
			return false, err
		}
	}
	return enqueuedAny, nil
}

// CIStatusDueChanges returns up to limit distinct changes that have at least
// one pending transition due at or before now.
func (t *Tx) CIStatusDueChanges(ctx context.Context, now time.Time, limit int) ([]CIStatusDue, error) {
	if limit <= 0 {
		limit = 32
	}
	rows, err := t.query(ctx, `SELECT change_id, MIN(repository), MIN(provider) FROM ci_status_outbox
		WHERE delivery_state = ? AND next_attempt_at <= ?
		GROUP BY change_id
		ORDER BY MIN(next_attempt_at) ASC, change_id ASC
		LIMIT ?`, CIStatusDeliveryPending, millis(now), limit)
	if err != nil {
		return nil, fmt.Errorf("listing due CI statuses: %w", err)
	}
	defer rows.Close()
	var out []CIStatusDue
	for rows.Next() {
		var d CIStatusDue
		if err := rows.Scan(&d.ChangeID, &d.Repository, &d.Provider); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CIStatusNextDue returns the earliest scheduled retry time among pending entries.
func (t *Tx) CIStatusNextDue(ctx context.Context) (time.Time, bool, error) {
	var next sql.NullInt64
	if err := t.queryRow(ctx, `SELECT MIN(next_attempt_at) FROM ci_status_outbox WHERE delivery_state = ?`, CIStatusDeliveryPending).Scan(&next); err != nil {
		return time.Time{}, false, fmt.Errorf("reading next due CI status: %w", err)
	}
	if !next.Valid {
		return time.Time{}, false, nil
	}
	return fromMillis(next.Int64), true, nil
}

// CIStatusPendingForChange returns all pending entries for changeID in ascending
// transition order, along with the highest already-delivered transition number.
func (t *Tx) CIStatusPendingForChange(ctx context.Context, changeID string) ([]CIStatusEntry, int, error) {
	var maxDelivered int
	if err := t.queryRow(ctx, `SELECT COALESCE(MAX(transition), 0) FROM ci_status_outbox WHERE change_id = ? AND delivery_state = ?`,
		changeID, CIStatusDeliveryDelivered).Scan(&maxDelivered); err != nil {
		return nil, 0, fmt.Errorf("reading max delivered CI status for %s: %w", changeID, err)
	}
	rows, err := t.query(ctx, `SELECT `+ciStatusColumns+` FROM ci_status_outbox
		WHERE change_id = ? AND delivery_state = ?
		ORDER BY transition ASC`, changeID, CIStatusDeliveryPending)
	if err != nil {
		return nil, 0, fmt.Errorf("listing pending CI statuses for %s: %w", changeID, err)
	}
	defer rows.Close()
	var out []CIStatusEntry
	for rows.Next() {
		e, err := scanCIStatus(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, e)
	}
	return out, maxDelivered, rows.Err()
}

// CIStatusLatestRemote returns the latest recorded remote status ID, URL and
// credential reference for changeID across all transitions.
func (t *Tx) CIStatusLatestRemote(ctx context.Context, changeID string) (remoteID, remoteURL, credentialRef string, err error) {
	err = t.queryRow(ctx, `SELECT remote_id, remote_url, credential_ref FROM ci_status_outbox
		WHERE change_id = ? AND remote_id != ''
		ORDER BY transition DESC LIMIT 1`, changeID).Scan(&remoteID, &remoteURL, &credentialRef)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", nil
	}
	return remoteID, remoteURL, credentialRef, err
}

// CIStatusMarkDelivered records successful delivery of one transition, supersedes
// any older pending transitions so they can never regress it, and carries the
// provider's remote ID forward to newer pending transitions.
func (t *Tx) CIStatusMarkDelivered(ctx context.Context, entryID, changeID string, transition int, remoteID, remoteURL, credentialRef string, at time.Time) error {
	ms := millis(at)
	if _, err := t.exec(ctx, `UPDATE ci_status_outbox
		SET delivery_state = ?, remote_id = ?, remote_url = ?, credential_ref = ?,
			reason_code = '', attempts = attempts + 1, last_error = '', delivered_at = ?, updated_at = ?
		WHERE id = ?`,
		CIStatusDeliveryDelivered, remoteID, remoteURL, credentialRef, ms, ms, entryID); err != nil {
		return fmt.Errorf("marking CI status %s delivered: %w", entryID, err)
	}
	if _, err := t.exec(ctx, `UPDATE ci_status_outbox
		SET delivery_state = ?, updated_at = ?
		WHERE change_id = ? AND transition < ? AND delivery_state = ?`,
		CIStatusDeliverySuperseded, ms, changeID, transition, CIStatusDeliveryPending); err != nil {
		return fmt.Errorf("superseding older CI statuses for %s: %w", changeID, err)
	}
	if remoteID != "" {
		if _, err := t.exec(ctx, `UPDATE ci_status_outbox
			SET remote_id = CASE WHEN remote_id = '' THEN ? ELSE remote_id END,
				remote_url = CASE WHEN remote_url = '' THEN ? ELSE remote_url END,
				credential_ref = CASE WHEN credential_ref = '' THEN ? ELSE credential_ref END
			WHERE change_id = ? AND transition > ? AND delivery_state = ?`,
			remoteID, remoteURL, credentialRef, changeID, transition, CIStatusDeliveryPending); err != nil {
			return fmt.Errorf("propagating remote CI status ID for %s: %w", changeID, err)
		}
	}
	return nil
}

// CIStatusMarkSuperseded marks an older pending transition as superseded so it
// will never overwrite a newer status.
func (t *Tx) CIStatusMarkSuperseded(ctx context.Context, entryID string, at time.Time) error {
	_, err := t.exec(ctx, `UPDATE ci_status_outbox SET delivery_state = ?, updated_at = ? WHERE id = ? AND delivery_state = ?`,
		CIStatusDeliverySuperseded, millis(at), entryID, CIStatusDeliveryPending)
	return err
}

// CIStatusMarkRetry records a transient failure and schedules the next retry.
// The retry window starts at the first attempt (retry_since = 0 until then).
func (t *Tx) CIStatusMarkRetry(ctx context.Context, entryID, credentialRef, reasonCode, lastErr string, nextAttempt, at time.Time) error {
	ms := millis(at)
	_, err := t.exec(ctx, `UPDATE ci_status_outbox
		SET attempts = attempts + 1,
			credential_ref = CASE WHEN ? != '' THEN ? ELSE credential_ref END,
			reason_code = ?, last_error = ?, next_attempt_at = ?,
			retry_since = CASE WHEN retry_since = 0 THEN ? ELSE retry_since END,
			updated_at = ?
		WHERE id = ? AND delivery_state = ?`,
		credentialRef, credentialRef, reasonCode, lastErr, millis(nextAttempt), ms, ms, entryID, CIStatusDeliveryPending)
	return err
}

// CIStatusMarkFinal ends delivery attempts for an entry as failed or skipped.
func (t *Tx) CIStatusMarkFinal(ctx context.Context, entryID, deliveryState, credentialRef, reasonCode, lastErr string, countAttempt bool, at time.Time) error {
	inc := 0
	if countAttempt {
		inc = 1
	}
	_, err := t.exec(ctx, `UPDATE ci_status_outbox
		SET delivery_state = ?, attempts = attempts + ?,
			credential_ref = CASE WHEN ? != '' THEN ? ELSE credential_ref END,
			reason_code = ?, last_error = ?, updated_at = ?
		WHERE id = ? AND delivery_state = ?`,
		deliveryState, inc, credentialRef, credentialRef, reasonCode, lastErr, millis(at), entryID, CIStatusDeliveryPending)
	return err
}

// CIStatusLatest returns the newest transition for each named change ID.
func (t *Tx) CIStatusLatest(ctx context.Context, ids []string) (map[string]CIStatusEntry, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, len(ids))
	for i, cid := range ids {
		args[i] = cid
	}
	rows, err := t.query(ctx, `SELECT `+ciStatusColumns+` FROM ci_status_outbox
		WHERE change_id IN (`+marks(len(ids))+`)
		ORDER BY change_id ASC, transition DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("reading latest CI statuses: %w", err)
	}
	defer rows.Close()
	out := make(map[string]CIStatusEntry)
	for rows.Next() {
		e, err := scanCIStatus(rows)
		if err != nil {
			return nil, err
		}
		if _, exists := out[e.ChangeID]; !exists {
			out[e.ChangeID] = e
		}
	}
	return out, rows.Err()
}

// CIStatusAllForChange lists every recorded transition for changeID in order.
func (t *Tx) CIStatusAllForChange(ctx context.Context, changeID string) ([]CIStatusEntry, error) {
	rows, err := t.query(ctx, `SELECT `+ciStatusColumns+` FROM ci_status_outbox
		WHERE change_id = ? ORDER BY transition ASC`, changeID)
	if err != nil {
		return nil, fmt.Errorf("listing CI statuses for %s: %w", changeID, err)
	}
	defer rows.Close()
	var out []CIStatusEntry
	for rows.Next() {
		e, err := scanCIStatus(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CIStatusHasNewerDeliveredForName reports whether a newer change for the same
// provider, repository, commit, environment, and service name has already
// delivered a status (N10). GitLab keys commit statuses by (sha, name) without
// an external_id, so a retried transition from an older change (such as a
// backed-off discard) must never overwrite a newer deploy's delivered status.
func (t *Tx) CIStatusHasNewerDeliveredForName(ctx context.Context, entry CIStatusEntry) (bool, error) {
	var exists int
	err := t.queryRow(ctx, `SELECT 1 FROM ci_status_outbox o
		WHERE o.provider = ?
		  AND LOWER(o.repository) = LOWER(?)
		  AND o.commit_sha = ?
		  AND o.environment_name = ?
		  AND o.service_name = ?
		  AND o.delivery_state = ?
		  AND o.change_id != ?
		  AND (
			(SELECT MIN(a.created_at) FROM ci_status_outbox a WHERE a.change_id = o.change_id) >
			(SELECT MIN(b.created_at) FROM ci_status_outbox b WHERE b.change_id = ?)
			OR (
				(SELECT MIN(a.created_at) FROM ci_status_outbox a WHERE a.change_id = o.change_id) =
				(SELECT MIN(b.created_at) FROM ci_status_outbox b WHERE b.change_id = ?)
				AND o.change_id > ?
			)
		  )
		LIMIT 1`,
		entry.Provider, entry.Repository, entry.Commit, entry.EnvironmentName, entry.ServiceName,
		CIStatusDeliveryDelivered, entry.ChangeID, entry.ChangeID, entry.ChangeID, entry.ChangeID,
	).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking newer delivered CI status for %s: %w", entry.ChangeID, err)
	}
	return true, nil
}

// CIStatusRequeueLatest schedules another delivery of the latest transition for
// changeID, starting a fresh bounded retry window. Only failed or skipped
// entries may be retried; pending or delivered entries return ErrCIStatusNotRetryable.
// When the deploy was created by a deploy token, allowed_repository is refreshed
// from the token so setting the repository on a slice-1 token takes effect on retry.
func (t *Tx) CIStatusRequeueLatest(ctx context.Context, changeID, changeURL string, now time.Time) (CIStatusEntry, error) {
	e, err := scanCIStatus(t.queryRow(ctx, `SELECT `+ciStatusColumns+` FROM ci_status_outbox
		WHERE change_id = ? ORDER BY transition DESC LIMIT 1`, changeID))
	if errors.Is(err, sql.ErrNoRows) {
		return CIStatusEntry{}, ErrNotFound
	}
	if err != nil {
		return CIStatusEntry{}, fmt.Errorf("reading latest CI status for %s: %w", changeID, err)
	}
	if e.DeliveryState != CIStatusDeliveryFailed && e.DeliveryState != CIStatusDeliverySkipped {
		return CIStatusEntry{}, ErrCIStatusNotRetryable
	}
	allowedRepo := e.AllowedRepository
	if e.TokenID != "" {
		var current string
		if err := t.queryRow(ctx, `SELECT deploy_repository FROM api_tokens WHERE id = ?`, e.TokenID).Scan(&current); err == nil {
			allowedRepo = current
		}
	}
	ms := millis(now)
	urlToKeep := changeURL
	if urlToKeep == "" {
		urlToKeep = e.ChangeURL
	}
	if _, err := t.exec(ctx, `UPDATE ci_status_outbox
		SET delivery_state = ?, attempts = 0, next_attempt_at = ?, reason_code = '', last_error = '',
			change_url = ?, allowed_repository = ?, retry_since = 0, updated_at = ?
		WHERE id = ?`,
		CIStatusDeliveryPending, ms, urlToKeep, allowedRepo, ms, e.ID); err != nil {
		return CIStatusEntry{}, fmt.Errorf("requeueing CI status for %s: %w", changeID, err)
	}
	e.DeliveryState = CIStatusDeliveryPending
	e.Attempts = 0
	e.NextAttemptAt = now
	e.ReasonCode = ""
	e.LastError = ""
	e.ChangeURL = urlToKeep
	e.AllowedRepository = allowedRepo
	e.RetrySince = time.Time{}
	e.UpdatedAt = now
	return e, nil
}
