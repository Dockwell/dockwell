// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// APIToken is a secret for scripts, the CLI and integrations, acting for
// the user who made it within its scope.
type APIToken struct {
	ID        string
	TokenHash []byte
	UserID    string
	Name      string
	// Hint is the prefix and the first and last 4 of the secret's random
	// digits, such as "dwt_4f9K…x9Ta".
	Hint  string
	Scope string
	// Grants is what it may do beyond its scope, such as GrantLogs.
	Grants     []string
	CreatedAt  time.Time
	ExpiresAt  *time.Time // nil: it doesn't expire
	LastUsedAt *time.Time
	LastIP     string
	RevokedAt  *time.Time
	// LocalAccount is the shell account on the control server that asked for
	// a token for the CLI's local access, as "mira@hz-1". Empty for every
	// other token.
	LocalAccount string
	// Projects are the IDs of the projects it acts in. Nil, as NULL is stored,
	// is every project its maker holds; a list, even an empty one, is exactly
	// those, so a list emptied by a cleanup or a hand edit reaches no project
	// rather than every one (ADR 0071).
	Projects []string
	// Role is the role it acts as at most, RoleDeveloper or RoleViewer, or empty
	// for its maker's (ADR 0071).
	Role string
	// DeployService and DeployEnvironment bind a deploy-only token to one app and place.
	DeployService, DeployEnvironment string
	// DeployRepository is the canonical HTTPS repository URL this deploy token
	// may report CI deploy statuses to. Empty disables commit/check-run reporting.
	DeployRepository string
}

// ErrTokenNotDeploy reports an attempt to set a deploy repository on a non-deploy token.
var ErrTokenNotDeploy = errors.New("token is not a deploy token")

// Narrowed reports a token that acts in some projects or as a lower role than its
// maker's (ADR 0071).
func (t *APIToken) Narrowed() bool { return t.Projects != nil || t.Role != "" }

// Token scopes, as stored.
const (
	ScopeReadOnly  = "read_only"
	ScopeReadWrite = "read_write"
)

// GrantLogs lets a token read logs, which often hold secrets that apps print.
const GrantLogs = "logs"

// HasGrant reports a token that may do what grant says.
func (t *APIToken) HasGrant(grant string) bool { return slices.Contains(t.Grants, grant) }

// MaxTokenAddresses bounds the addresses kept for each token.
const MaxTokenAddresses = 256

const apiTokenColumns = `id, token_hash, user_id, name, hint, scope, created_at, expires_at, last_used_at, last_ip, revoked_at, grants, local_account, projects, role, deploy_service, deploy_environment, deploy_repository` //nolint:gosec // column names

func scanAPIToken(row interface{ Scan(...any) error }) (APIToken, error) {
	var t APIToken
	var created int64
	var expires, used, revoked sql.NullInt64
	var grants string
	var projects sql.NullString
	if err := row.Scan(&t.ID, &t.TokenHash, &t.UserID, &t.Name, &t.Hint, &t.Scope, &created, &expires, &used, &t.LastIP, &revoked, &grants, &t.LocalAccount,
		&projects, &t.Role, &t.DeployService, &t.DeployEnvironment, &t.DeployRepository); err != nil {
		return APIToken{}, err
	}
	if grants != "" {
		t.Grants = strings.Split(grants, ",")
	}
	t.Projects = projectsFrom(projects)
	t.CreatedAt, t.ExpiresAt, t.LastUsedAt, t.RevokedAt = fromMillis(created), timePtr(expires), timePtr(used), timePtr(revoked)
	return t, nil
}

// CreateAPIToken records a new token.
func (t *Tx) CreateAPIToken(ctx context.Context, tok APIToken) error {
	columns, values := `id, token_hash, user_id, name, hint, scope, created_at, expires_at, grants, local_account`, `?, ?, ?, ?, ?, ?, ?, ?, ?, ?`
	args := []any{
		tok.ID, tok.TokenHash, tok.UserID, tok.Name, tok.Hint, tok.Scope, millis(tok.CreatedAt), nullMillis(tok.ExpiresAt), strings.Join(tok.Grants, ","),
		tok.LocalAccount,
	}
	// A token that isn't narrowed leaves both to their defaults, which it is the
	// same as, so it's written the way it was before they were added.
	if tok.Narrowed() {
		columns, values = columns+`, projects, role`, values+`, ?, ?`
		args = append(args, projectsColumn(tok.Projects), tok.Role)
	}
	if tok.DeployService != "" {
		columns += `, deploy_service, deploy_environment, deploy_repository`
		values += `, ?, ?, ?`
		args = append(args, tok.DeployService, tok.DeployEnvironment, tok.DeployRepository)
	}
	_, err := t.exec(ctx, `INSERT INTO api_tokens (`+columns+`) VALUES (`+values+`)`, args...)
	if err != nil {
		return fmt.Errorf("can't record API token %s: %w", tok.ID, err)
	}
	return nil
}

// projectsColumn is how a token's or a hook's projects are kept: their IDs apart
// by commas, NULL for nil, every project its maker holds (ADR 0071).
func projectsColumn(projects []string) sql.NullString {
	if projects == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: strings.Join(projects, ","), Valid: true}
}

// projectsFrom reads projectsColumn back: nil for NULL, and a list, even an empty
// one, for anything else.
func projectsFrom(column sql.NullString) []string {
	if !column.Valid {
		return nil
	}
	if column.String == "" {
		return []string{}
	}
	return strings.Split(column.String, ",")
}

func (t *Tx) getAPIToken(ctx context.Context, where string, arg any) (APIToken, error) {
	tok, err := scanAPIToken(t.queryRow(ctx, `SELECT `+apiTokenColumns+` FROM api_tokens WHERE `+where+` = ?`, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return APIToken{}, fmt.Errorf("API token: %w", ErrNotFound)
	}
	if err != nil {
		return APIToken{}, fmt.Errorf("can't read API token: %w", err)
	}
	return tok, nil
}

// GetAPITokenByHash returns a token by the hash of its secret, or
// ErrNotFound. Revoked and expired tokens are returned too; callers check.
func (t *Tx) GetAPITokenByHash(ctx context.Context, hash []byte) (APIToken, error) {
	return t.getAPIToken(ctx, "token_hash", hash)
}

// GetAPIToken returns a token by ID, or ErrNotFound.
func (t *Tx) GetAPIToken(ctx context.Context, id string) (APIToken, error) {
	return t.getAPIToken(ctx, "id", id)
}

// ListLiveAPITokens returns a user's tokens that are neither revoked nor
// expired at now, newest first.
func (t *Tx) ListLiveAPITokens(ctx context.Context, userID string, now time.Time) ([]APIToken, error) {
	rows, err := t.query(ctx, `SELECT `+apiTokenColumns+` FROM api_tokens
		WHERE user_id = ? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?) ORDER BY id DESC`, userID, millis(now))
	if err != nil {
		return nil, fmt.Errorf("can't list API tokens: %w", err)
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		tok, err := scanAPIToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, tok)
	}
	return out, rows.Err()
}

// CountLiveAPITokens counts a user's tokens that are neither revoked nor
// expired at now.
func (t *Tx) CountLiveAPITokens(ctx context.Context, userID string, now time.Time) (int, error) {
	var n int
	if err := t.queryRow(ctx, `SELECT COUNT(*) FROM api_tokens
		WHERE user_id = ? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)`, userID, millis(now)).Scan(&n); err != nil {
		return 0, fmt.Errorf("can't count API tokens: %w", err)
	}
	return n, nil
}

// RevokeLocalTokens revokes the tokens the CLI made for a person's local access,
// as ownership leaving them takes what acted for them (ADR 0049).
func (t *Tx) RevokeLocalTokens(ctx context.Context, userID string, at time.Time) error {
	if _, err := t.exec(ctx, `UPDATE api_tokens SET revoked_at = ? WHERE user_id = ? AND local_account <> '' AND revoked_at IS NULL`,
		millis(at), userID); err != nil {
		return fmt.Errorf("can't revoke the local tokens of %s: %w", userID, err)
	}
	return nil
}

// RevokeAPIToken stops one of a user's tokens working, or reports
// ErrNotFound when the user has no such token still unrevoked.
func (t *Tx) RevokeAPIToken(ctx context.Context, userID, id string, at time.Time) error {
	res, err := t.exec(ctx, `UPDATE api_tokens SET revoked_at = ? WHERE id = ? AND user_id = ? AND revoked_at IS NULL`,
		millis(at), id, userID)
	if err != nil {
		return fmt.Errorf("can't revoke API token %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("API token %s: %w", id, ErrNotFound)
	}
	return nil
}

// SetAPITokenDeployRepository updates the status-reporting repository bound to
// one of userID's live deploy tokens.
func (t *Tx) SetAPITokenDeployRepository(ctx context.Context, userID, id, repository string, now time.Time) (APIToken, error) {
	tok, err := t.GetAPIToken(ctx, id)
	if err != nil {
		return APIToken{}, err
	}
	if tok.UserID != userID || tok.RevokedAt != nil || (tok.ExpiresAt != nil && !now.Before(*tok.ExpiresAt)) {
		return APIToken{}, fmt.Errorf("API token %s: %w", id, ErrNotFound)
	}
	if tok.DeployService == "" {
		return APIToken{}, ErrTokenNotDeploy
	}
	if _, err := t.exec(ctx, `UPDATE api_tokens SET deploy_repository = ? WHERE id = ? AND user_id = ? AND revoked_at IS NULL`,
		repository, id, userID); err != nil {
		return APIToken{}, fmt.Errorf("can't update API token %s: %w", id, err)
	}
	tok.DeployRepository = repository
	return tok, nil
}

// DeployTokenBinding pairs a live deploy token's project with its bound status repository.
type DeployTokenBinding struct {
	ProjectID  string
	Repository string
}

// LiveDeployTokenRepositories lists every live (not revoked, not expired) deploy
// token that has a bound status repository.
func (t *Tx) LiveDeployTokenRepositories(ctx context.Context, now time.Time) ([]DeployTokenBinding, error) {
	rows, err := t.query(ctx, `SELECT projects, deploy_repository FROM api_tokens
		WHERE deploy_repository != '' AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)`, millis(now))
	if err != nil {
		return nil, fmt.Errorf("listing live deploy token repositories: %w", err)
	}
	defer rows.Close()
	var out []DeployTokenBinding
	for rows.Next() {
		var projectsCol sql.NullString
		var repo string
		if err := rows.Scan(&projectsCol, &repo); err != nil {
			return nil, err
		}
		for _, p := range projectsFrom(projectsCol) {
			if p != "" {
				out = append(out, DeployTokenBinding{ProjectID: p, Repository: repo})
			}
		}
	}
	return out, rows.Err()
}

// APITokenOwners returns the owning user_id for each named API token ID in one query.
func (t *Tx) APITokenOwners(ctx context.Context, ids []string) (map[string]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := t.query(ctx, `SELECT id, user_id FROM api_tokens WHERE id IN (`+marks(len(ids))+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("reading API token owners: %w", err)
	}
	defer rows.Close()
	out := make(map[string]string, len(ids))
	for rows.Next() {
		var id, userID string
		if err := rows.Scan(&id, &userID); err != nil {
			return nil, err
		}
		out[id] = userID
	}
	return out, rows.Err()
}

// TouchAPIToken records that a token was used at at from ip.
func (t *Tx) TouchAPIToken(ctx context.Context, id string, at time.Time, ip string) error {
	if _, err := t.exec(ctx, `UPDATE api_tokens SET last_used_at = ?, last_ip = ? WHERE id = ? AND revoked_at IS NULL`,
		millis(at), ip, id); err != nil {
		return fmt.Errorf("can't update API token %s: %w", id, err)
	}
	return nil
}

// SeeAPITokenAddress records that a token was used from address, and
// reports whether that was the first time. It keeps MaxTokenAddresses for
// each token, forgetting the least recently seen.
func (t *Tx) SeeAPITokenAddress(ctx context.Context, id, address string, at time.Time) (bool, error) {
	res, err := t.exec(ctx, `INSERT INTO api_token_addresses (token_id, address, first_seen, last_seen) VALUES (?, ?, ?, ?)
		ON CONFLICT (token_id, address) DO NOTHING`, id, address, millis(at), millis(at))
	if err != nil {
		return false, fmt.Errorf("can't record an address for API token %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := t.exec(ctx, `UPDATE api_token_addresses SET last_seen = ? WHERE token_id = ? AND address = ?`,
			millis(at), id, address); err != nil {
			return false, fmt.Errorf("can't record an address for API token %s: %w", id, err)
		}
		return false, nil
	}
	if _, err := t.exec(ctx, `DELETE FROM api_token_addresses WHERE token_id = ?1 AND address NOT IN (
		SELECT address FROM api_token_addresses WHERE token_id = ?1 ORDER BY last_seen DESC, address LIMIT ?2)`,
		id, MaxTokenAddresses); err != nil {
		return false, fmt.Errorf("can't forget old addresses for API token %s: %w", id, err)
	}
	return true, nil
}

// PruneAPITokens deletes tokens revoked or expired before cutoff, with the
// addresses they were used from.
func (t *Tx) PruneAPITokens(ctx context.Context, cutoff time.Time) error {
	if _, err := t.exec(ctx, `DELETE FROM api_tokens WHERE revoked_at < ?1 OR expires_at < ?1`, millis(cutoff)); err != nil {
		return fmt.Errorf("can't prune API tokens: %w", err)
	}
	return nil
}
