// SPDX-License-Identifier: AGPL-3.0-only

package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fake is GitHub's API as these tests need it: each call as the App must carry a
// JWT the App's key signed, and each with a token one it gave.
type fake struct {
	t   *testing.T
	key *rsa.PublicKey
	app int64
	mu  sync.Mutex
	// asked is each token asked for, by its body.
	asked []map[string]any
	// given are the tokens it gave.
	given map[string]bool
	// expires is how long a token it gives lasts.
	expires time.Duration
	now     func() time.Time
	srv     *httptest.Server
}

func newFake(t *testing.T, key *rsa.PrivateKey, app int64, now func() time.Time) *fake {
	f := &fake{t: t, key: &key.PublicKey, app: app, given: map[string]bool{}, expires: time.Hour, now: now}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) asApp(r *http.Request) bool {
	jwt, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(f.key, crypto.SHA256, sum[:], sig) != nil {
		return false
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct {
		Iat, Exp int64
		Iss      string
	}
	if json.Unmarshal(raw, &c) != nil || c.Iss != strconv.FormatInt(f.app, 10) {
		return false
	}
	now := f.now().Unix()
	return c.Iat <= now && now < c.Exp && c.Exp-c.Iat <= 600
}

func (f *fake) withToken(r *http.Request) bool {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	defer f.mu.Unlock()
	return ok && f.given[tok]
}

func (f *fake) serve(w http.ResponseWriter, r *http.Request) {
	no := func(code int, msg string) {
		w.WriteHeader(code)
		_, _ = fmt.Fprintf(w, `{"message":%q}`, msg)
	}
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/conversions"):
		_, _ = fmt.Fprintf(w, `{"id":%d,"slug":"dockwell-panel","client_id":"Iv1.abc","client_secret":"never-kept","webhook_secret":"hook-secret","pem":"PEM","owner":{"login":"acme","type":"Organization"}}`, f.app)
	case r.URL.Path == "/app":
		if !f.asApp(r) {
			no(401, "A JSON web token could not be decoded")
			return
		}
		_, _ = fmt.Fprintf(w, `{"id":%d,"slug":"dockwell-panel"}`, f.app)
	case r.URL.Path == "/app/installations/7":
		if !f.asApp(r) {
			no(401, "bad JWT")
			return
		}
		_, _ = fmt.Fprintf(w, `{"id":7,"app_id":%d,"account":{"login":"acme","type":"Organization"},"repository_selection":"selected"}`, f.app)
	case r.URL.Path == "/app/installations/8":
		no(404, "Not Found")
	case r.Method == http.MethodPost && r.URL.Path == "/app/installations/7/access_tokens":
		if !f.asApp(r) {
			no(401, "bad JWT")
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.asked = append(f.asked, body)
		tok := fmt.Sprintf("ghs_%d", len(f.asked))
		f.given[tok] = true
		f.mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"token":%q,"expires_at":%q}`, tok, f.now().Add(f.expires).UTC().Format(time.RFC3339))
	case r.URL.Path == "/installation/repositories":
		if !f.withToken(r) {
			no(401, "Bad credentials")
			return
		}
		page := r.URL.Query().Get("page")
		var repos []string
		n := 100
		if page == "2" {
			n = 3
		}
		for i := range n {
			repos = append(repos, fmt.Sprintf(`{"id":%s%d,"full_name":"acme/r%d","private":true,"default_branch":"main"}`, page, i, i))
		}
		_, _ = fmt.Fprintf(w, `{"total_count":103,"repositories":[%s]}`, strings.Join(repos, ","))
	case strings.HasPrefix(r.URL.Path, "/repos/acme/web/commits/"):
		if !f.withToken(r) {
			no(401, "Bad credentials")
			return
		}
		w.Header().Set("X-Ratelimit-Limit", "5000")
		w.Header().Set("X-Ratelimit-Remaining", "4321")
		w.Header().Set("X-Ratelimit-Reset", "2000000000")
		if r.Header.Get("If-None-Match") == `"e1"` {
			w.Header().Set("Etag", `"e1"`)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if r.Header.Get("Accept") != "application/vnd.github.sha" {
			no(415, "wanted the SHA alone")
			return
		}
		w.Header().Set("Etag", `"e1"`)
		_, _ = io.WriteString(w, strings.Repeat("ab", 20))
	case strings.HasPrefix(r.URL.Path, "/repos/acme/busy/commits/"):
		w.Header().Set("Retry-After", "60")
		no(403, "You have exceeded a secondary rate limit")
	default:
		no(404, "Not Found")
	}
}

func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.mu.Unlock()
}

func testApp(t *testing.T) (*App, *fake, *clock) {
	t.Helper()
	k := testKey(t)
	c := &clock{at: time.Now()}
	f := newFake(t, k, 42, c.now)
	return &App{ID: 42, Key: k, Base: f.srv.URL, Client: f.srv.Client(), Now: c.now}, f, c
}

// A call as the App carries a JWT its key signed, for the App, lasting 9 minutes
// from a minute ago, and GitHub refuses another key's.
func TestTheAppCallsWithAJWTItsKeySigned(t *testing.T) {
	t.Parallel()
	a, _, _ := testApp(t)
	if _, err := a.Self(t.Context()); err != nil {
		t.Fatal(err)
	}
	jwt, err := a.jwt()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(strings.Split(jwt, ".")[1])
	var c struct{ Iat, Exp int64 }
	_ = json.Unmarshal(raw, &c)
	if c.Exp-c.Iat != int64((jwtLife+jwtSkew)/time.Second) {
		t.Errorf("the JWT lasts %d s", c.Exp-c.Iat)
	}
	other := &App{ID: 42, Key: testKey(t), Base: a.Base, Client: a.Client, Now: a.Now}
	var e *Error
	if _, err := other.Self(t.Context()); !errors.As(err, &e) || !e.Gone() {
		t.Errorf("another key's JWT = %v, want GitHub's 401", err)
	}
}

// A token is asked for narrowed, and kept in memory until 10 minutes before it
// ends; another repository or permission asks for its own.
func TestATokenIsNarrowAndKeptUntilNearItsEnd(t *testing.T) {
	t.Parallel()
	a, f, c := testApp(t)
	ctx := t.Context()
	first, err := a.Token(ctx, 7, []int64{99}, Contents)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := a.Token(ctx, 7, []int64{99}, Contents); again != first {
		t.Errorf("the same question asked again for %s, not %s", again, first)
	}
	f.mu.Lock()
	body := f.asked[0]
	asked := len(f.asked)
	f.mu.Unlock()
	if asked != 1 || fmt.Sprint(body["repository_ids"]) != "[99]" || fmt.Sprint(body["permissions"]) != "map[contents:read]" {
		t.Errorf("asked %d times, for %v", asked, body)
	}
	if other, _ := a.Token(ctx, 7, []int64{100}, Contents); other == first {
		t.Error("another repository got the first one's token")
	}
	c.add(51 * time.Minute)
	if later, _ := a.Token(ctx, 7, []int64{99}, Contents); later == first {
		t.Error("a token 9 minutes from its end was used again")
	}
	a.Forget(7)
	a.mu.Lock()
	left := len(a.tokens)
	a.mu.Unlock()
	if left != 0 {
		t.Errorf("forgetting the installation left %d tokens", left)
	}
}

// What GitHub refused says its status and message, and never what was sent.
func TestARefusalSaysGitHubsWordsNotTheRequest(t *testing.T) {
	t.Parallel()
	a, f, _ := testApp(t)
	_, err := a.Installation(t.Context(), 8)
	var e *Error
	if !errors.As(err, &e) || !e.NotFound() || err.Error() != "GitHub answered 404: Not Found" {
		t.Errorf("another App's installation = %v", err)
	}
	tok, _ := a.Token(t.Context(), 7, []int64{1}, Contents)
	_, _, rate, err := a.Head(t.Context(), 7, 1, "acme/busy", "main", "")
	if err == nil || strings.Contains(err.Error(), tok) || strings.Contains(err.Error(), "Bearer") {
		t.Errorf("a refused call says %v", err)
	}
	if !rate.Limited || rate.RetryAfter != time.Minute {
		t.Errorf("a secondary limit reads as %+v", rate)
	}
	_ = f
}

// A branch's commit is read as its SHA alone, an ETag makes an unmoved branch
// not modified, and the hour's calls left are read.
func TestABranchsCommitComesWithItsETag(t *testing.T) {
	t.Parallel()
	a, _, _ := testApp(t)
	sha, etag, rate, err := a.Head(t.Context(), 7, 5, "acme/web", "main", "")
	if err != nil || sha != strings.Repeat("ab", 20) || etag != `"e1"` {
		t.Fatalf("the branch = %s %s %v", sha, etag, err)
	}
	if rate.Limit != 5000 || rate.Remaining != 4321 || rate.Reset.Unix() != 2000000000 || rate.Limited {
		t.Errorf("the rate reads %+v", rate)
	}
	if _, _, _, err := a.Head(t.Context(), 7, 5, "acme/web", "main", etag); !errors.Is(err, ErrNotModified) {
		t.Errorf("an unmoved branch = %v", err)
	}
}

// An installation is the App's only when GitHub says so as the App, and its
// repositories are read page by page with a token that lists them.
func TestAnInstallationAndItsRepositories(t *testing.T) {
	t.Parallel()
	a, f, _ := testApp(t)
	in, err := a.Installation(t.Context(), 7)
	if err != nil || in.Account.Login != "acme" || in.Selection != "selected" {
		t.Fatalf("installation 7 = %+v, %v", in, err)
	}
	repos, err := a.Repositories(t.Context(), 7)
	if err != nil || len(repos) != 103 || !repos[0].Private {
		t.Fatalf("its repositories = %d, %v", len(repos), err)
	}
	f.mu.Lock()
	body := f.asked[len(f.asked)-1]
	f.mu.Unlock()
	if fmt.Sprint(body["permissions"]) != "map[metadata:read]" || body["repository_ids"] != nil {
		t.Errorf("listing asked for %v", body)
	}
}

// The conversion keeps the App's key and webhook secret, and drops the client
// secret Dockwell has no use for.
func TestTheConversionDropsTheClientSecret(t *testing.T) {
	t.Parallel()
	_, f, _ := testApp(t)
	c, err := Convert(t.Context(), f.srv.Client(), f.srv.URL, "code-1")
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != 42 || c.WebhookSecret != "hook-secret" || c.Owner.Type != "Organization" {
		t.Errorf("the conversion = %+v", c)
	}
	if b, _ := json.Marshal(c); strings.Contains(string(b), "never-kept") {
		t.Error("the conversion kept the client secret")
	}
	if _, err := Convert(context.Background(), f.srv.Client(), f.srv.URL, "../app"); err == nil {
		t.Error("a code with a path in it was sent")
	}
}

// The manifest asks for the least, with its webhook and pages on the panel, and a
// name GitHub takes.
func TestTheManifestAsksForTheLeast(t *testing.T) {
	t.Parallel()
	panel, _ := url.Parse("https://deploy.a-rather-long-host-name.example.com")
	m := NewManifest(panel, "gha_1")
	if len([]rune(m.Name)) > maxAppName || !strings.HasPrefix(m.Name, "Dockwell deploy.") {
		t.Errorf("the name is %q", m.Name)
	}
	if m.Hook.URL != "https://deploy.a-rather-long-host-name.example.com/hooks/github/gha_1" || !m.Hook.Active || m.Public ||
		m.SetupURL != "https://deploy.a-rather-long-host-name.example.com/settings/integrations/github/gha_1/installed" {
		t.Errorf("the manifest is %+v", m)
	}
	if fmt.Sprint(m.Permissions) != "map[checks:write contents:read metadata:read]" || fmt.Sprint(m.Events) != "[push]" {
		t.Errorf("it asks for %v and %v", m.Permissions, m.Events)
	}
	if u, _ := NewAppURL(Web, "", "s1"); u != "https://github.com/settings/apps/new?state=s1" {
		t.Errorf("an account's form is %s", u)
	}
	if u, _ := NewAppURL(Web, "acme-co", "s1"); u != "https://github.com/organizations/acme-co/settings/apps/new?state=s1" {
		t.Errorf("an organization's form is %s", u)
	}
	if _, err := NewAppURL(Web, "acme/../x", "s1"); err == nil {
		t.Error("a name with a path in it made a URL")
	}
}

// A key is read as GitHub gives it, and its fingerprint is as GitHub shows it.
func TestAKeyAndItsFingerprint(t *testing.T) {
	t.Parallel()
	k := testKey(t)
	pkcs1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	got, err := ParseKey(pkcs1)
	if err != nil || !got.Equal(k) {
		t.Fatalf("a PKCS#1 key = %v", err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	if got, err := ParseKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})); err != nil || !got.Equal(k) {
		t.Errorf("a PKCS#8 key = %v", err)
	}
	if _, err := ParseKey([]byte("not a key")); err == nil {
		t.Error("a file with no key was read")
	}
	fp, err := Fingerprint(k)
	if err != nil || !strings.HasPrefix(fp, "SHA256:") || len(fp) != len("SHA256:")+44 {
		t.Errorf("the fingerprint is %q, %v", fp, err)
	}
}
