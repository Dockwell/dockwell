// SPDX-License-Identifier: AGPL-3.0-only

// Package github is GitHub's API as the control plane uses it for a GitHub App it
// made (ADR 0098): the manifest's conversion, the App and its installations, tokens
// narrowed to one repository, the App's webhook and its deliveries, and a branch's
// commit. The App's private key, a JWT and an installation's token stay in this
// package and the control plane's memory: nothing here writes one down, and an
// error says GitHub's status and message, never a request's headers.
package github

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dockwell/dockwell/internal/netguard"
)

// API is GitHub's API, and Web its pages, which a test may point elsewhere.
const (
	API = "https://api.github.com"
	Web = "https://github.com"
)

// Bounds on asking GitHub.
const (
	// timeout bounds one call.
	timeout = 15 * time.Second
	// maxBody bounds what one answer may come to.
	maxBody = 8 << 20
	// tokenSlack is how long before its end a token is asked for again.
	tokenSlack = 10 * time.Minute
	// pageSize is how many of a list GitHub is asked for at once, its most.
	pageSize = 100
	// maxPages bounds a list: 10,000 repositories.
	maxPages = 100
)

// Doer sends a request, as http.Client's Do does.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// NewClient reaches GitHub's API through netguard, for the internet alone.
func NewClient() Doer {
	return netguard.Client(netguard.Options{MaxBody: maxBody, Timeout: timeout})
}

// Error is what GitHub said when it said no: its status and its message.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("GitHub answered %d %s", e.Status, http.StatusText(e.Status))
	}
	return fmt.Sprintf("GitHub answered %d: %s", e.Status, e.Message)
}

// Gone says GitHub refused the App itself: deleted on GitHub, or its key replaced.
func (e *Error) Gone() bool { return e.Status == http.StatusUnauthorized }

// NotFound says what was asked for isn't there, or isn't the App's to see.
func (e *Error) NotFound() bool { return e.Status == http.StatusNotFound }

// ErrNotModified is a branch's commit that hasn't moved since the ETag given.
var ErrNotModified = errors.New("GitHub says it hasn't changed")

// Rate is what GitHub's last answer said of the calls left.
type Rate struct {
	// Limit and Remaining are the hour's calls and those left; zero where unsaid.
	Limit, Remaining int
	// Reset is when the hour's calls start over.
	Reset time.Time
	// RetryAfter is how long GitHub asked to wait, for a secondary limit.
	RetryAfter time.Duration
	// Limited says the answer was a refusal for asking too often.
	Limited bool
}

// rateOf reads an answer's rate-limit headers.
func rateOf(res *http.Response) Rate {
	var r Rate
	r.Limit, _ = strconv.Atoi(res.Header.Get("X-Ratelimit-Limit"))
	r.Remaining, _ = strconv.Atoi(res.Header.Get("X-Ratelimit-Remaining"))
	if s, err := strconv.ParseInt(res.Header.Get("X-Ratelimit-Reset"), 10, 64); err == nil && s > 0 {
		r.Reset = time.Unix(s, 0)
	}
	if s, err := strconv.Atoi(res.Header.Get("Retry-After")); err == nil && s > 0 {
		r.RetryAfter = time.Duration(s) * time.Second
	}
	limited := res.StatusCode == http.StatusTooManyRequests ||
		res.StatusCode == http.StatusForbidden && (r.RetryAfter > 0 || res.Header.Get("X-Ratelimit-Remaining") == "0")
	r.Limited = limited
	return r
}

// call asks GitHub's API, with auth as the Authorization header's value, and reads
// the answer into into. It returns the answer's rate, and its ETag.
func call(ctx context.Context, client Doer, method, u, auth string, body, into any, header http.Header) (Rate, http.Header, error) {
	var payload io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return Rate{}, nil, err
		}
		payload = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, payload)
	if err != nil {
		return Rate{}, nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "Dockwell")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	res, err := client.Do(req)
	if err != nil {
		// Said by what failed, never by the request, which holds its Authorization.
		return Rate{}, nil, fmt.Errorf("can't reach GitHub: %s", netguard.Words(err))
	}
	defer func() { _ = res.Body.Close() }()
	rate := rateOf(res)
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxBody+1))
	if err != nil {
		return rate, res.Header, fmt.Errorf("can't read GitHub's answer: %s", netguard.Words(err))
	}
	if len(raw) > maxBody {
		return rate, res.Header, fmt.Errorf("GitHub's answer is past %d MiB", maxBody>>20)
	}
	if res.StatusCode == http.StatusNotModified {
		return rate, res.Header, ErrNotModified
	}
	if res.StatusCode >= 300 {
		var said struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &said)
		return rate, res.Header, &Error{Status: res.StatusCode, Message: clip(said.Message)}
	}
	if into != nil && len(raw) > 0 {
		if s, ok := into.(*string); ok {
			*s = strings.TrimSpace(string(raw))
			return rate, res.Header, nil
		}
		if err := json.Unmarshal(raw, into); err != nil {
			return rate, res.Header, fmt.Errorf("can't read GitHub's answer: %w", err)
		}
	}
	return rate, res.Header, nil
}

// clip keeps what GitHub said to a line.
func clip(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// Conversion is what GitHub gives for a manifest's code, once: the App it made.
type Conversion struct {
	ID            int64             `json:"id"`
	Slug          string            `json:"slug"`
	Name          string            `json:"name"`
	ClientID      string            `json:"client_id"`
	WebhookSecret string            `json:"webhook_secret"`
	PEM           string            `json:"pem"`
	HTMLURL       string            `json:"html_url"`
	Owner         Owner             `json:"owner"`
	Permissions   map[string]string `json:"permissions"`
}

// Owner is who an App, or an installation, belongs to.
type Owner struct {
	Login string `json:"login"`
	// Type is "User" or "Organization".
	Type string `json:"type"`
}

// Convert trades a manifest's code for the App GitHub made from it, which GitHub
// allows once, within the hour. The answer holds the App's private key and webhook
// secret; its client secret is dropped here, as Dockwell signs nobody in with it.
func Convert(ctx context.Context, client Doer, base, code string) (*Conversion, error) {
	if code == "" || strings.ContainsAny(code, "/?#% ") {
		return nil, errors.New("GitHub's code for the App isn't one")
	}
	var c Conversion
	if _, _, err := call(ctx, client, http.MethodPost, strings.TrimSuffix(base, "/")+"/app-manifests/"+url.PathEscape(code)+"/conversions", "", nil, &c, nil); err != nil {
		return nil, err
	}
	if c.ID == 0 || c.Slug == "" || c.PEM == "" || c.WebhookSecret == "" {
		return nil, errors.New("GitHub's answer for the App left out its ID, name, key or webhook secret")
	}
	return &c, nil
}

// App is a GitHub App, as the control plane acts for it.
type App struct {
	// ID is the App's.
	ID int64
	// Key is its private key, which signs each JWT.
	Key *rsa.PrivateKey
	// Base is GitHub's API: API unless a test's.
	Base string
	// Client reaches it.
	Client Doer
	// Now is the clock: time.Now unless a test's.
	Now func() time.Time

	mu     sync.Mutex
	tokens map[string]token
}

// token is an installation's token, for what it was asked for.
type token struct {
	value string
	ends  time.Time
}

func (a *App) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *App) url(path string) string {
	base := a.Base
	if base == "" {
		base = API
	}
	return strings.TrimSuffix(base, "/") + path
}

// asApp calls GitHub as the App, with a JWT made for the call.
func (a *App) asApp(ctx context.Context, method, path string, body, into any) error {
	jwt, err := a.jwt()
	if err != nil {
		return err
	}
	_, _, err = call(ctx, a.Client, method, a.url(path), "Bearer "+jwt, body, into, nil)
	return err
}

// Self is the App as GitHub has it, which checks the key is this App's.
func (a *App) Self(ctx context.Context) (*Conversion, error) {
	var c Conversion
	if err := a.asApp(ctx, http.MethodGet, "/app", nil, &c); err != nil {
		return nil, err
	}
	if c.ID != a.ID {
		return nil, fmt.Errorf("the key is GitHub App %d's, not this App's (%d)", c.ID, a.ID)
	}
	return &c, nil
}

// Installation is where an App is installed: an account, and the repositories it
// may see there.
type Installation struct {
	ID      int64 `json:"id"`
	AppID   int64 `json:"app_id"`
	Account Owner `json:"account"`
	// Selection is "all", every repository the account has, or "selected".
	Selection   string            `json:"repository_selection"`
	Permissions map[string]string `json:"permissions"`
	Suspended   *time.Time        `json:"suspended_at"`
	HTMLURL     string            `json:"html_url"`
}

// Installation is the App's installation id, which says it is the App's: GitHub
// answers 404 for another App's.
func (a *App) Installation(ctx context.Context, id int64) (*Installation, error) {
	var in Installation
	if err := a.asApp(ctx, http.MethodGet, "/app/installations/"+strconv.FormatInt(id, 10), nil, &in); err != nil {
		return nil, err
	}
	if in.ID != id || in.AppID != 0 && in.AppID != a.ID {
		return nil, fmt.Errorf("installation %d isn't this App's", id)
	}
	return &in, nil
}

// Uninstall removes the App from installation id, as the App.
func (a *App) Uninstall(ctx context.Context, id int64) error {
	err := a.asApp(ctx, http.MethodDelete, "/app/installations/"+strconv.FormatInt(id, 10), nil, nil)
	var e *Error
	if errors.As(err, &e) && e.NotFound() {
		return nil // gone already
	}
	return err
}

// Permission is what a token may do, as GitHub names it.
type Permission string

// What Dockwell asks a token for.
const (
	// Contents reads a repository's branches and fetches its commits.
	Contents Permission = "contents"
	// Metadata lists repositories.
	Metadata Permission = "metadata"
	// Checks writes CI deploy check runs on commits and pull requests (ADR 0111).
	Checks Permission = "checks"
)

// Token is a token for installation, narrowed to repositories by ID and to reading
// what perms names. It's kept in memory until 10 minutes before it ends, for the
// same question, and asked for again after. None is ever written down.
func (a *App) Token(ctx context.Context, installation int64, repositories []int64, perms ...Permission) (string, error) {
	asked := make(map[Permission]string, len(perms))
	for _, p := range perms {
		asked[p] = "read"
	}
	return a.TokenWith(ctx, installation, repositories, asked)
}

// TokenWith is Token with explicit permission levels (such as checks: write).
func (a *App) TokenWith(ctx context.Context, installation int64, repositories []int64, perms map[Permission]string) (string, error) {
	repos := slices.Clone(repositories)
	slices.Sort(repos)
	ps := make([]string, 0, len(perms))
	for p, level := range perms {
		ps = append(ps, string(p)+":"+level)
	}
	slices.Sort(ps)
	key := fmt.Sprint(installation, repos, ps)
	now := a.now()
	a.mu.Lock()
	if t, ok := a.tokens[key]; ok && now.Before(t.ends.Add(-tokenSlack)) {
		a.mu.Unlock()
		return t.value, nil
	}
	a.mu.Unlock()
	body := map[string]any{}
	if len(repos) > 0 {
		body["repository_ids"] = repos
	}
	if len(perms) > 0 {
		asked := make(map[string]string, len(perms))
		for p, level := range perms {
			asked[string(p)] = level
		}
		body["permissions"] = asked
	}
	var got struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := a.asApp(ctx, http.MethodPost, "/app/installations/"+strconv.FormatInt(installation, 10)+"/access_tokens", body, &got); err != nil {
		return "", err
	}
	if got.Token == "" {
		return "", errors.New("GitHub gave no token for the installation")
	}
	a.mu.Lock()
	if a.tokens == nil {
		a.tokens = map[string]token{}
	}
	for k, t := range a.tokens {
		if !now.Before(t.ends) {
			delete(a.tokens, k)
		}
	}
	a.tokens[key] = token{value: got.Token, ends: got.ExpiresAt}
	a.mu.Unlock()
	return got.Token, nil
}

// Forget drops every token kept for installation, as one that's removed ends them.
func (a *App) Forget(installation int64) {
	prefix := strconv.FormatInt(installation, 10) + " "
	a.mu.Lock()
	defer a.mu.Unlock()
	for k := range a.tokens {
		if strings.HasPrefix(k, prefix) {
			delete(a.tokens, k)
		}
	}
}

// Repository is one an installation may see.
type Repository struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
	CloneURL      string `json:"clone_url"`
}

// Repositories lists what installation may see, with a token that may list them.
func (a *App) Repositories(ctx context.Context, installation int64) ([]Repository, error) {
	tok, err := a.Token(ctx, installation, nil, Metadata)
	if err != nil {
		return nil, err
	}
	var out []Repository
	for page := 1; page <= maxPages; page++ {
		var got struct {
			Total        int          `json:"total_count"`
			Repositories []Repository `json:"repositories"`
		}
		path := fmt.Sprintf("/installation/repositories?per_page=%d&page=%d", pageSize, page)
		if _, _, err := call(ctx, a.Client, http.MethodGet, a.url(path), "Bearer "+tok, nil, &got, nil); err != nil {
			return nil, err
		}
		out = append(out, got.Repositories...)
		if len(got.Repositories) < pageSize || len(out) >= got.Total {
			return out, nil
		}
	}
	return out, nil
}

// Delivery is one of the App's webhook deliveries, as GitHub kept it.
type Delivery struct {
	ID          int64     `json:"id"`
	GUID        string    `json:"guid"`
	DeliveredAt time.Time `json:"delivered_at"`
	Event       string    `json:"event"`
	// StatusCode is what the panel answered; zero where GitHub couldn't reach it.
	StatusCode int `json:"status_code"`
}

// Reached says the panel took the delivery: answered it with a 2xx. One it
// refused, or a proxy before it answered for it, delivered nothing.
func (d Delivery) Reached() bool { return d.StatusCode >= 200 && d.StatusCode < 300 }

// Refused says the panel refused the delivery's signature: the webhook secret
// GitHub signs with isn't the one Dockwell keeps.
func (d Delivery) Refused() bool { return d.StatusCode == http.StatusUnauthorized }

// Deliveries are the App's latest webhook deliveries, newest first.
func (a *App) Deliveries(ctx context.Context) ([]Delivery, error) {
	var out []Delivery
	if err := a.asApp(ctx, http.MethodGet, "/app/hook/deliveries?per_page=30", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SetHook points the App's webhook at hookURL, signed under secret when it's set.
func (a *App) SetHook(ctx context.Context, hookURL, secret string) error {
	body := map[string]any{"content_type": "json"}
	if hookURL != "" {
		body["url"] = hookURL
	}
	if secret != "" {
		body["secret"] = secret
	}
	return a.asApp(ctx, http.MethodPatch, "/app/hook/config", body, nil)
}

// Head is the commit a repository's branch names, read with a token for that
// repository, and the answer's ETag. An etag from the last answer makes an unmoved
// branch ErrNotModified, which GitHub doesn't count against the installation.
func (a *App) Head(ctx context.Context, installation, repository int64, fullName, branch, etag string) (commit, newETag string, rate Rate, err error) {
	tok, err := a.Token(ctx, installation, []int64{repository}, Contents)
	if err != nil {
		return "", "", Rate{}, err
	}
	header := http.Header{"Accept": {"application/vnd.github.sha"}}
	if etag != "" {
		header["If-None-Match"] = []string{etag}
	}
	path := "/repos/" + escapePath(fullName) + "/commits/" + url.PathEscape(branch)
	var sha string
	rate, got, err := call(ctx, a.Client, http.MethodGet, a.url(path), "Bearer "+tok, nil, &sha, header)
	if got != nil {
		newETag = got.Get("Etag")
	}
	if err != nil {
		return "", newETag, rate, err
	}
	if !commitID(sha) {
		return "", newETag, rate, errors.New("GitHub's answer for the branch isn't a commit")
	}
	return sha, newETag, rate, nil
}

// escapePath escapes an owner/name for a path, keeping its one slash.
func escapePath(fullName string) string {
	owner, name, _ := strings.Cut(fullName, "/")
	return url.PathEscape(owner) + "/" + url.PathEscape(name)
}

// commitID says s is a commit's full ID.
func commitID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
