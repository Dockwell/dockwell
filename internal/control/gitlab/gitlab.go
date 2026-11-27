// SPDX-License-Identifier: AGPL-3.0-only

// Package gitlab is GitLab's REST API as the control plane uses it to verify a
// GitLab connection and post CI deploy commit statuses (ADR 0111). Outbound
// calls go through netguard, tokens stay on the control plane, and errors never
// carry request headers or token values.
package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/netguard"
)

const (
	timeout = 15 * time.Second
	maxBody = 1 << 20
)

// Doer sends an HTTP request, as netguard.HTTP does.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// NewClient reaches gitlab.com and self-managed GitLab instances through
// netguard, allowing public and admin-configured private networks while
// refusing loopback, link-local, metadata, multicast, reserved addresses and
// redirects.
func NewClient() Doer {
	return netguard.Client(netguard.Options{Private: true, MaxBody: maxBody, Timeout: timeout})
}

// Error is an HTTP refusal from GitLab.
type Error struct {
	Status     int
	Message    string
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("GitLab answered %d %s", e.Status, http.StatusText(e.Status))
	}
	return fmt.Sprintf("GitLab answered %d: %s", e.Status, e.Message)
}

// Transient reports whether retrying later may succeed.
func (e *Error) Transient() bool {
	return e.Status == http.StatusTooManyRequests || e.Status == http.StatusRequestTimeout || e.Status >= 500
}

// Client calls a GitLab instance's REST API v4.
type Client struct {
	// HTTP reaches GitLab; defaults to NewClient().
	HTTP Doer
	// TestBase overrides the scheme and host for tests (DOCKWELL_TEST_GITLAB)
	// so production HTTPS validation stays unchanged while tests never touch the
	// network.
	TestBase string
}

func (c *Client) http() Doer {
	if c != nil && c.HTTP != nil {
		return c.HTTP
	}
	return NewClient()
}

func (c *Client) endpoint(baseURL, apiPath string) (string, error) {
	base := strings.TrimRight(baseURL, "/")
	if c != nil && c.TestBase != "" {
		u, err := url.Parse(base)
		if err != nil {
			return "", err
		}
		tb, err := url.Parse(strings.TrimRight(c.TestBase, "/"))
		if err != nil {
			return "", err
		}
		u.Scheme, u.Host = tb.Scheme, tb.Host
		base = strings.TrimRight(u.String(), "/")
	}
	return base + apiPath, nil
}

func (c *Client) call(ctx context.Context, method, baseURL, apiPath, token string, body, into any) error {
	target, err := c.endpoint(baseURL, apiPath)
	if err != nil {
		return err
	}
	var payload io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, payload)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Dockwell")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("PRIVATE-TOKEN", token)
	}
	res, err := c.http().Do(req)
	if err != nil {
		if errors.Is(err, netguard.ErrForbidden) {
			return fmt.Errorf("won't reach GitLab: %s: %w", netguard.Words(err), netguard.ErrForbidden)
		}
		return fmt.Errorf("can't reach GitLab: %s", netguard.Words(err))
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxBody+1))
	if err != nil {
		return fmt.Errorf("can't read GitLab's answer: %s", netguard.Words(err))
	}
	if len(raw) > maxBody {
		return fmt.Errorf("GitLab's answer is past %d MiB", maxBody>>20)
	}
	if res.StatusCode >= 300 {
		var retry time.Duration
		if s, err := strconv.Atoi(res.Header.Get("Retry-After")); err == nil && s > 0 {
			retry = time.Duration(s) * time.Second
		}
		return &Error{Status: res.StatusCode, Message: extractMessage(raw), RetryAfter: retry}
	}
	if into != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, into); err != nil {
			return fmt.Errorf("can't read GitLab's answer: %w", err)
		}
	}
	return nil
}

func extractMessage(raw []byte) string {
	var m struct {
		Message any    `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(raw, &m) == nil {
		if s, ok := m.Message.(string); ok && s != "" {
			return clip(s)
		}
		if m.Error != "" {
			return clip(m.Error)
		}
		if m.Message != nil {
			if b, err := json.Marshal(m.Message); err == nil {
				return clip(string(b))
			}
		}
	}
	return ""
}

func clip(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// TokenInfo is what GitLab reports about an access token.
type TokenInfo struct {
	Name    string   `json:"name"`
	Scopes  []string `json:"scopes"`
	Active  bool     `json:"active"`
	Revoked bool     `json:"revoked"`
	// ScopesKnown is false when an older GitLab lacked /personal_access_tokens/self
	// and verification fell back to /user.
	ScopesKnown bool
}

// HasAPIScope reports whether the token carries the api scope required to post
// commit statuses (POST /projects/:id/statuses/:sha).
func (t TokenInfo) HasAPIScope() bool {
	if !t.ScopesKnown {
		return true
	}
	return slices.Contains(t.Scopes, "api")
}

// CheckToken verifies that token authenticates against baseURL and checks its
// scopes when GitLab supports /api/v4/personal_access_tokens/self.
func (c *Client) CheckToken(ctx context.Context, baseURL, token string) (TokenInfo, error) {
	var pat struct {
		Name    string   `json:"name"`
		Scopes  []string `json:"scopes"`
		Active  bool     `json:"active"`
		Revoked bool     `json:"revoked"`
	}
	err := c.call(ctx, http.MethodGet, baseURL, "/api/v4/personal_access_tokens/self", token, nil, &pat)
	if err == nil {
		return TokenInfo{
			Name: pat.Name, Scopes: pat.Scopes, Active: pat.Active, Revoked: pat.Revoked, ScopesKnown: true,
		}, nil
	}
	var ge *Error
	if errors.As(err, &ge) && (ge.Status == http.StatusNotFound || ge.Status == http.StatusMethodNotAllowed) {
		var user struct {
			Username string `json:"username"`
		}
		if uerr := c.call(ctx, http.MethodGet, baseURL, "/api/v4/user", token, nil, &user); uerr != nil {
			return TokenInfo{}, uerr
		}
		return TokenInfo{Name: user.Username, Active: true, ScopesKnown: false}, nil
	}
	return TokenInfo{}, err
}

// CommitStatusRequest is the payload for POST /api/v4/projects/:id/statuses/:sha.
type CommitStatusRequest struct {
	State       string `json:"state"`
	Name        string `json:"name"`
	TargetURL   string `json:"target_url,omitempty"`
	Description string `json:"description,omitempty"`
	Ref         string `json:"ref,omitempty"`
}

// CommitStatus is what GitLab returns after recording a commit status.
type CommitStatus struct {
	ID         int64  `json:"id"`
	Status     string `json:"status"`
	Name       string `json:"name"`
	TargetURL  string `json:"target_url"`
	PipelineID int64  `json:"pipeline_id"`
}

// PostCommitStatus posts a commit status for projectPath (e.g. "acme/web") and
// commitSHA on baseURL.
func (c *Client) PostCommitStatus(ctx context.Context, baseURL, token, projectPath, commitSHA string, req CommitStatusRequest) (*CommitStatus, error) {
	if len(req.Description) > 255 {
		req.Description = req.Description[:252] + "..."
	}
	apiPath := "/api/v4/projects/" + url.PathEscape(projectPath) + "/statuses/" + url.PathEscape(commitSHA)
	var got CommitStatus
	err := c.call(ctx, http.MethodPost, baseURL, apiPath, token, req, &got)
	if err != nil {
		// GitLab returns 400 "Cannot transition status via :enqueue from :pending"
		// (or similar) when a retry posts the exact state that already landed.
		var ge *Error
		if errors.As(err, &ge) && ge.Status == http.StatusBadRequest && strings.Contains(ge.Message, "Cannot transition status") {
			if existing, ferr := c.findCommitStatus(ctx, baseURL, token, projectPath, commitSHA, req.Name); ferr == nil && existing != nil && existing.Status == req.State {
				return existing, nil
			}
		}
		return nil, err
	}
	return &got, nil
}

func (c *Client) findCommitStatus(ctx context.Context, baseURL, token, projectPath, commitSHA, name string) (*CommitStatus, error) {
	apiPath := "/api/v4/projects/" + url.PathEscape(projectPath) + "/repository/commits/" + url.PathEscape(commitSHA) + "/statuses?name=" + url.QueryEscape(name)
	var list []CommitStatus
	if err := c.call(ctx, http.MethodGet, baseURL, apiPath, token, nil, &list); err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, nil
	}
	latest := list[len(list)-1]
	return &latest, nil
}

// ProjectPath extracts the GitLab project path (such as "acme/web" or
// "acme/group/web") from repoURL when it belongs to the GitLab instance at
// baseURL. Returns "" if the host or base path doesn't match.
func ProjectPath(baseURL, repoURL string) string {
	base, berr := url.Parse(strings.TrimSpace(baseURL))
	repo, rerr := url.Parse(strings.TrimSpace(repoURL))
	if berr != nil || rerr != nil || !strings.EqualFold(base.Host, repo.Host) {
		return ""
	}
	basePath := strings.TrimRight(base.Path, "/")
	repoPath := strings.TrimRight(repo.Path, "/")
	if basePath != "" {
		if !strings.HasPrefix(repoPath, basePath+"/") {
			return ""
		}
		repoPath = strings.TrimPrefix(repoPath, basePath)
	}
	project := strings.TrimSuffix(strings.Trim(repoPath, "/"), ".git")
	if project == "" || !strings.Contains(project, "/") {
		return ""
	}
	return project
}

// MatchesPrefix reports whether projectPath matches an optional pathPrefix
// (either exact match or path segment prefix, case-insensitive).
func MatchesPrefix(projectPath, pathPrefix string) bool {
	if pathPrefix == "" {
		return true
	}
	p := strings.ToLower(strings.Trim(projectPath, "/"))
	pre := strings.ToLower(strings.Trim(pathPrefix, "/"))
	return p == pre || strings.HasPrefix(p, pre+"/")
}
