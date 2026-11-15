// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// GitHubInstallation is where a GitHub App Dockwell made is installed, as GitHub
// says (ADR 0098).
type GitHubInstallation struct {
	// ID is GitHub's.
	ID int64
	// AppID is the github.app resource's ID.
	AppID        string
	Account      string
	Organization bool
	// Selection is "all", every repository the account has, or "selected".
	Selection    string
	Repositories []GitHubRepository
	SuspendedAt  *time.Time
	RemovedAt    *time.Time
	UpdatedAt    time.Time
}

// Usable says the installation is neither suspended nor removed.
func (i GitHubInstallation) Usable() bool { return i.SuspendedAt == nil && i.RemovedAt == nil }

// GitHubRepository is one an installation may see.
type GitHubRepository struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"`
	Private       bool   `json:"private,omitempty"`
	DefaultBranch string `json:"default_branch,omitempty"`
}

const githubInstallationColumns = `id, app_id, account, organization, selection, repositories, suspended_at, removed_at, updated_at`

func scanGitHubInstallation(row interface{ Scan(...any) error }) (GitHubInstallation, error) {
	var in GitHubInstallation
	var repos string
	var suspended, removed sql.NullInt64
	var updated int64
	if err := row.Scan(&in.ID, &in.AppID, &in.Account, &in.Organization, &in.Selection, &repos, &suspended, &removed, &updated); err != nil {
		return GitHubInstallation{}, err
	}
	if err := json.Unmarshal([]byte(repos), &in.Repositories); err != nil {
		return GitHubInstallation{}, fmt.Errorf("installation %d's repositories: %w", in.ID, err)
	}
	if suspended.Valid {
		at := fromMillis(suspended.Int64)
		in.SuspendedAt = &at
	}
	if removed.Valid {
		at := fromMillis(removed.Int64)
		in.RemovedAt = &at
	}
	in.UpdatedAt = fromMillis(updated)
	return in, nil
}

// SetGitHubInstallation records an installation as GitHub says it now, in place of
// what was said before.
func (t *Tx) SetGitHubInstallation(ctx context.Context, in GitHubInstallation) error {
	repos := in.Repositories
	if repos == nil {
		repos = []GitHubRepository{}
	}
	b, err := json.Marshal(repos)
	if err != nil {
		return err
	}
	var suspended, removed any
	if in.SuspendedAt != nil {
		suspended = millis(*in.SuspendedAt)
	}
	if in.RemovedAt != nil {
		removed = millis(*in.RemovedAt)
	}
	_, err = t.exec(ctx, `INSERT INTO github_installations (`+githubInstallationColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET app_id = excluded.app_id, account = excluded.account, organization = excluded.organization,
		selection = excluded.selection, repositories = excluded.repositories, suspended_at = excluded.suspended_at,
		removed_at = excluded.removed_at, updated_at = excluded.updated_at`,
		in.ID, in.AppID, in.Account, in.Organization, in.Selection, string(b), suspended, removed, millis(in.UpdatedAt))
	if err != nil {
		return fmt.Errorf("can't record GitHub installation %d: %w", in.ID, err)
	}
	return nil
}

// GetGitHubInstallation returns an installation by GitHub's ID, or ErrNotFound.
func (t *Tx) GetGitHubInstallation(ctx context.Context, id int64) (GitHubInstallation, error) {
	in, err := scanGitHubInstallation(t.queryRow(ctx, `SELECT `+githubInstallationColumns+` FROM github_installations WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return GitHubInstallation{}, fmt.Errorf("GitHub installation %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return GitHubInstallation{}, fmt.Errorf("can't read GitHub installation %d: %w", id, err)
	}
	return in, nil
}

// GitHubInstallationsOf lists an App's installations, removed ones too, by ID.
func (t *Tx) GitHubInstallationsOf(ctx context.Context, appID string) ([]GitHubInstallation, error) {
	rows, err := t.query(ctx, `SELECT `+githubInstallationColumns+` FROM github_installations WHERE app_id = ? ORDER BY id`, appID)
	if err != nil {
		return nil, fmt.Errorf("can't list the installations of %s: %w", appID, err)
	}
	var out []GitHubInstallation
	for rows.Next() {
		in, err := scanGitHubInstallation(rows)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("can't read an installation of %s: %w", appID, err)
		}
		out = append(out, in)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("can't list the installations of %s: %w", appID, err)
	}
	return out, nil
}

// DeleteGitHubInstallationsOf forgets every installation of an App that's removed.
func (t *Tx) DeleteGitHubInstallationsOf(ctx context.Context, appID string) error {
	if _, err := t.exec(ctx, `DELETE FROM github_installations WHERE app_id = ?`, appID); err != nil {
		return fmt.Errorf("can't forget the installations of %s: %w", appID, err)
	}
	return nil
}

// GitHooksVia lists the hooks whose pushes come one way, such as ViaGitHub, by ID.
func (t *Tx) GitHooksVia(ctx context.Context, via string) ([]GitHook, error) {
	rows, err := t.query(ctx, `SELECT `+gitHookColumns+` FROM git_hooks WHERE via = ? ORDER BY id`, via)
	if err != nil {
		return nil, fmt.Errorf("can't list the hooks %s delivers to: %w", via, err)
	}
	var out []GitHook
	for rows.Next() {
		h, err := scanGitHook(rows)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("can't read a hook %s delivers to: %w", via, err)
		}
		out = append(out, h)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("can't list the hooks %s delivers to: %w", via, err)
	}
	return out, nil
}
