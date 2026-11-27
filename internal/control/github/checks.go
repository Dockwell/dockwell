// SPDX-License-Identifier: AGPL-3.0-only

package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// CheckRunOutput is the title and one-line summary shown on a GitHub check run.
type CheckRunOutput struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
}

// CheckRunRequest creates or updates a GitHub Check Run on a commit (ADR 0111).
type CheckRunRequest struct {
	Name        string          `json:"name,omitempty"`
	HeadSHA     string          `json:"head_sha,omitempty"`
	DetailsURL  string          `json:"details_url,omitempty"`
	ExternalID  string          `json:"external_id,omitempty"`
	Status      string          `json:"status,omitempty"`
	Conclusion  string          `json:"conclusion,omitempty"`
	StartedAt   *time.Time      `json:"started_at,omitempty"`
	CompletedAt *time.Time      `json:"completed_at,omitempty"`
	Output      *CheckRunOutput `json:"output,omitempty"`
}

// CheckRun is the check run GitHub returned.
type CheckRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	ExternalID string `json:"external_id"`
	HTMLURL    string `json:"html_url"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// HasChecksWrite reports whether a GitHub permissions map grants checks: write.
func HasChecksWrite(perms map[string]string) bool {
	return perms[string(Checks)] == "write"
}

// CreateCheckRun creates a check run on fullName's commit using a token scoped
// solely to repository with checks: write.
func (a *App) CreateCheckRun(ctx context.Context, installation, repository int64, fullName string, req CheckRunRequest) (*CheckRun, Rate, error) {
	tok, err := a.TokenWith(ctx, installation, []int64{repository}, map[Permission]string{Checks: "write"})
	if err != nil {
		return nil, Rate{}, err
	}
	path := "/repos/" + escapePath(fullName) + "/check-runs"
	var got CheckRun
	rate, _, err := call(ctx, a.Client, http.MethodPost, a.url(path), "Bearer "+tok, req, &got, nil)
	if err != nil {
		return nil, rate, err
	}
	return &got, rate, nil
}

// UpdateCheckRun updates an existing check run in place so each change has a
// single check run that advances through queued, in_progress and completed.
func (a *App) UpdateCheckRun(ctx context.Context, installation, repository int64, fullName string, checkRunID int64, req CheckRunRequest) (*CheckRun, Rate, error) {
	tok, err := a.TokenWith(ctx, installation, []int64{repository}, map[Permission]string{Checks: "write"})
	if err != nil {
		return nil, Rate{}, err
	}
	// GitHub rejects head_sha on PATCH; clear it while keeping name/details/status.
	req.HeadSHA = ""
	path := "/repos/" + escapePath(fullName) + "/check-runs/" + strconv.FormatInt(checkRunID, 10)
	var got CheckRun
	rate, _, err := call(ctx, a.Client, http.MethodPatch, a.url(path), "Bearer "+tok, req, &got, nil)
	if err != nil {
		return nil, rate, err
	}
	return &got, rate, nil
}

// FindCheckRun looks up an existing check run for commit matching name and
// externalID, so retrying after a lost response updates in place instead of
// creating a duplicate check run.
func (a *App) FindCheckRun(ctx context.Context, installation, repository int64, fullName, commit, name, externalID string) (*CheckRun, Rate, error) {
	tok, err := a.TokenWith(ctx, installation, []int64{repository}, map[Permission]string{Checks: "write"})
	if err != nil {
		return nil, Rate{}, err
	}
	path := fmt.Sprintf("/repos/%s/commits/%s/check-runs?check_name=%s&per_page=100",
		escapePath(fullName), url.PathEscape(commit), url.QueryEscape(name))
	var got struct {
		CheckRuns []CheckRun `json:"check_runs"`
	}
	rate, _, err := call(ctx, a.Client, http.MethodGet, a.url(path), "Bearer "+tok, nil, &got, nil)
	if err != nil {
		return nil, rate, err
	}
	for i := range got.CheckRuns {
		if got.CheckRuns[i].ExternalID == externalID {
			cr := got.CheckRuns[i]
			return &cr, rate, nil
		}
	}
	return nil, rate, nil
}
