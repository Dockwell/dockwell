// SPDX-License-Identifier: AGPL-3.0-only

package cistatus_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/cistatus"
	"github.com/dockwell/dockwell/internal/control/github"
	"github.com/dockwell/dockwell/internal/control/github/ghtest"
	"github.com/dockwell/dockwell/internal/control/gitlab"
	"github.com/dockwell/dockwell/internal/control/gitlab/gltest"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

type fakeTargets struct {
	mu             sync.Mutex
	gh             *cistatus.GitHubTarget
	ghSkip         string
	gl             *cistatus.GitLabTarget
	glSkip         string
	ghFindCalls    int
	glFindCalls    int
	verifyCalls    int
	verifyMissing  bool
	glNotedStates  []dockwellv1.GitLabConnectionState
	glTargetByRepo map[string]*cistatus.GitLabTarget
}

func (f *fakeTargets) FindGitHubStatusTarget(_ context.Context, _, _ string) (*cistatus.GitHubTarget, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ghFindCalls++
	return f.gh, f.ghSkip, nil
}

func (f *fakeTargets) VerifyChecksPermission(_ context.Context, _ string, _ int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verifyCalls++
	return f.verifyMissing
}

func (f *fakeTargets) FindGitLabStatusTarget(_ context.Context, repoURL, _ string) (*cistatus.GitLabTarget, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.glFindCalls++
	if f.glTargetByRepo != nil {
		if t, ok := f.glTargetByRepo[repoURL]; ok {
			return t, "", nil
		}
	}
	return f.gl, f.glSkip, nil
}

func (f *fakeTargets) NoteGitLabStatus(_ string, _ int, state dockwellv1.GitLabConnectionState, _ string) {
	f.mu.Lock()
	f.glNotedStates = append(f.glNotedStates, state)
	f.mu.Unlock()
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type seedOpts struct {
	envName     string
	svcName     string
	allowedRepo string
	tokenID     string
	commit      string
}

func seedCIChangeOpts(t *testing.T, db *store.DB, now time.Time, provider dockwellv1.DeployProvider, repo string, opts seedOpts, states ...string) string {
	t.Helper()
	changeID := id.New(id.Change)
	serviceID := id.New(id.Service)
	if opts.svcName == "" {
		opts.svcName = "web"
	}
	if opts.commit == "" {
		opts.commit = "0123456789abcdef0123456789abcdef01234567"
	}
	if err := db.Revise(t.Context(), func(tx *store.RevisionTx) error {
		if err := tx.CreateChange(t.Context(), store.Change{
			ID: changeID, Summary: "Deploy " + opts.svcName, Actor: "tok_ci", State: store.ChangePlanned,
			CreatedAt: now, Plan: []byte(`{}`), Fingerprint: "fp",
		}); err != nil {
			return err
		}
		if err := tx.RecordCIDeploy(t.Context(), store.CIDeployOrigin{
			CIDeployKey: store.CIDeployKey{
				Service: serviceID,
				Digest:  "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
				Commit:  opts.commit,
			},
			Provider:   int32(provider),
			Repository: repo,
			Ref:        "refs/heads/main",
			Image:      "ghcr.io/acme/web:1.0",
		}, changeID); err != nil {
			return err
		}
		for i, st := range states {
			if _, err := tx.EnqueueCIStatusForChange(t.Context(), changeID, st, store.CIStatusEnqueueOpts{
				ServiceName:       opts.svcName,
				EnvironmentName:   opts.envName,
				TokenID:           opts.tokenID,
				AllowedRepository: opts.allowedRepo,
				ChangeURL:         "https://panel.example.com/changes/" + changeID,
				Summary:           "Deploy " + opts.svcName,
			}, now.Add(time.Duration(i)*time.Second)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return changeID
}

func seedCIChange(t *testing.T, db *store.DB, now time.Time, provider dockwellv1.DeployProvider, repo string, states ...string) string {
	t.Helper()
	return seedCIChangeOpts(t, db, now, provider, repo, seedOpts{envName: "production", allowedRepo: repo}, states...)
}

func ghAppForFake(t *testing.T, gh *ghtest.Server, srv *httptest.Server) *github.App {
	t.Helper()
	gh.SeedCode("code-test", "dockwell-test")
	conv, err := github.Convert(t.Context(), srv.Client(), srv.URL, "code-test")
	if err != nil {
		t.Fatal(err)
	}
	key, err := github.ParseKey([]byte(conv.PEM))
	if err != nil {
		t.Fatal(err)
	}
	return &github.App{ID: conv.ID, Key: key, Base: srv.URL, Client: srv.Client()}
}

// H1: A deploy token bound to one repository never calls GitHub or GitLab for a
// foreign repository, and a slice-1 token with no bound repository skips cleanly.
func TestForeignAndUnboundRepositoryNeverContactsProvider(t *testing.T) {
	t.Parallel()
	db, _ := storetest.Open(t, store.SQLite)
	clk := &fakeClock{t: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)}
	targets := &fakeTargets{}
	w := cistatus.New(cistatus.Config{DB: db, Targets: targets, Now: clk.now})

	foreignGH := seedCIChangeOpts(t, db, clk.now(), dockwellv1.DeployProvider_DEPLOY_PROVIDER_GITHUB_ACTIONS,
		"https://github.com/acme/someone-elses-repo",
		seedOpts{envName: "production", allowedRepo: "https://github.com/acme/web"},
		store.CIStatusHealthy)
	foreignGL := seedCIChangeOpts(t, db, clk.now(), dockwellv1.DeployProvider_DEPLOY_PROVIDER_GITLAB_CI,
		"https://gitlab.example.com/acme/someone-elses-repo",
		seedOpts{envName: "production", allowedRepo: "https://gitlab.example.com/acme/web"},
		store.CIStatusHealthy)
	unbound := seedCIChangeOpts(t, db, clk.now(), dockwellv1.DeployProvider_DEPLOY_PROVIDER_GITHUB_ACTIONS,
		"https://github.com/acme/web",
		seedOpts{envName: "production", allowedRepo: ""},
		store.CIStatusHealthy)

	if err := w.DeliverOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if targets.ghFindCalls != 0 || targets.glFindCalls != 0 {
		t.Fatalf("provider lookup called for disallowed repos: gh=%d gl=%d", targets.ghFindCalls, targets.glFindCalls)
	}
	var latest map[string]store.CIStatusEntry
	_ = db.View(t.Context(), func(tx *store.Tx) error {
		var err error
		latest, err = tx.CIStatusLatest(t.Context(), []string{foreignGH, foreignGL, unbound})
		return err
	})
	if latest[foreignGH].DeliveryState != store.CIStatusDeliverySkipped || latest[foreignGH].ReasonCode != store.CIStatusReasonRepositoryMismatch {
		t.Fatalf("foreignGH = %+v", latest[foreignGH])
	}
	if latest[foreignGL].DeliveryState != store.CIStatusDeliverySkipped || latest[foreignGL].ReasonCode != store.CIStatusReasonRepositoryMismatch {
		t.Fatalf("foreignGL = %+v", latest[foreignGL])
	}
	if latest[unbound].DeliveryState != store.CIStatusDeliverySkipped || latest[unbound].ReasonCode != store.CIStatusReasonNoTokenRepository {
		t.Fatalf("unbound = %+v", latest[unbound])
	}
	if w.InFlightLen() != 0 {
		t.Fatalf("InFlightLen = %d, want 0 (L4)", w.InFlightLen())
	}
}

// Transitions deliver in order, include environment name (M2), update a single
// GitHub Check Run in place, and shrink the lock map back to 0 (L4).
func TestGitHubOrderingAndSingleCheckRun(t *testing.T) {
	t.Parallel()
	db, _ := storetest.Open(t, store.SQLite)
	gh, err := ghtest.New()
	if err != nil {
		t.Fatal(err)
	}
	ghSrv := httptest.NewServer(gh)
	t.Cleanup(ghSrv.Close)
	app := ghAppForFake(t, gh, ghSrv)
	clk := &fakeClock{t: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)}

	targets := &fakeTargets{gh: &cistatus.GitHubTarget{
		App: app, AppID: "gha_1", AppName: "dockwell", InstallationID: 7, RepositoryID: 101,
		FullName: "acme/web", CredentialRef: "github_app:gha_1/7",
	}}
	w := cistatus.New(cistatus.Config{DB: db, Targets: targets, Now: clk.now})

	chgID := seedCIChange(t, db, clk.now(), dockwellv1.DeployProvider_DEPLOY_PROVIDER_GITHUB_ACTIONS, "https://github.com/acme/web",
		store.CIStatusQueued, store.CIStatusApplying, store.CIStatusHealthy)
	clk.advance(5 * time.Second)
	if err := w.DeliverOnce(t.Context()); err != nil {
		t.Fatal(err)
	}

	runs := gh.CheckRuns()
	if len(runs) != 1 {
		t.Fatalf("got %d check runs, want 1 updated in place: %+v", len(runs), runs)
	}
	if runs[0].Name != "Dockwell / production / web" || runs[0].Status != "completed" || runs[0].Conclusion != "success" || runs[0].ExternalID != chgID {
		t.Fatalf("unexpected check run: %+v", runs[0])
	}
	if !strings.HasSuffix(runs[0].DetailsURL, "/changes/"+chgID) {
		t.Fatalf("details_url = %q", runs[0].DetailsURL)
	}
	if w.InFlightLen() != 0 {
		t.Fatalf("InFlightLen = %d, want 0", w.InFlightLen())
	}
}

// M1 & M2: Discarded deploys report cancelled/canceled, and two environments
// with the same service name on one commit get distinct status names on both
// GitHub and GitLab without colliding.
func TestDiscardedStatusAndMultiEnvironmentNames(t *testing.T) {
	t.Parallel()
	db, _ := storetest.Open(t, store.SQLite)
	gh, err := ghtest.New()
	if err != nil {
		t.Fatal(err)
	}
	ghSrv := httptest.NewServer(gh)
	t.Cleanup(ghSrv.Close)
	app := ghAppForFake(t, gh, ghSrv)

	gl := gltest.New()
	glSrv := httptest.NewServer(gl)
	t.Cleanup(glSrv.Close)

	clk := &fakeClock{t: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)}
	targets := &fakeTargets{
		gh: &cistatus.GitHubTarget{
			App: app, AppID: "gha_1", InstallationID: 7, RepositoryID: 101,
			FullName: "acme/web", CredentialRef: "github_app:gha_1/7",
		},
		gl: &cistatus.GitLabTarget{
			ConnectionID: "glc_1", Revision: 1, Name: "gitlab", BaseURL: "https://gitlab.example.com",
			Token: gltest.TokenAPI, ProjectPath: "acme/web", CredentialRef: "gitlab_connection:glc_1",
		},
	}
	w := cistatus.New(cistatus.Config{
		DB: db, Targets: targets, GitLab: &gitlab.Client{HTTP: glSrv.Client(), TestBase: glSrv.URL}, Now: clk.now,
	})

	sameCommit := strings.Repeat("e", 40)
	// Two environments deploying "web" on the same commit on GitHub:
	// staging is discarded (cancelled), production succeeds.
	seedCIChangeOpts(t, db, clk.now(), dockwellv1.DeployProvider_DEPLOY_PROVIDER_GITHUB_ACTIONS, "https://github.com/acme/web",
		seedOpts{envName: "staging", svcName: "web", allowedRepo: "https://github.com/acme/web", commit: sameCommit},
		store.CIStatusQueued, store.CIStatusDiscarded)
	seedCIChangeOpts(t, db, clk.now(), dockwellv1.DeployProvider_DEPLOY_PROVIDER_GITHUB_ACTIONS, "https://github.com/acme/web",
		seedOpts{envName: "production", svcName: "web", allowedRepo: "https://github.com/acme/web", commit: sameCommit},
		store.CIStatusQueued, store.CIStatusHealthy)

	// Same on GitLab: staging is discarded (canceled), production succeeds.
	seedCIChangeOpts(t, db, clk.now(), dockwellv1.DeployProvider_DEPLOY_PROVIDER_GITLAB_CI, "https://gitlab.example.com/acme/web",
		seedOpts{envName: "staging", svcName: "web", allowedRepo: "https://gitlab.example.com/acme/web", commit: sameCommit},
		store.CIStatusQueued, store.CIStatusDiscarded)
	seedCIChangeOpts(t, db, clk.now(), dockwellv1.DeployProvider_DEPLOY_PROVIDER_GITLAB_CI, "https://gitlab.example.com/acme/web",
		seedOpts{envName: "production", svcName: "web", allowedRepo: "https://gitlab.example.com/acme/web", commit: sameCommit},
		store.CIStatusQueued, store.CIStatusApplying, store.CIStatusHealthy)

	clk.advance(5 * time.Second)
	if err := w.DeliverOnce(t.Context()); err != nil {
		t.Fatal(err)
	}

	ghByName := map[string]ghtest.CheckRun{}
	for _, r := range gh.CheckRuns() {
		ghByName[r.Name] = r
	}
	if ghByName["Dockwell / staging / web"].Conclusion != "cancelled" || ghByName["Dockwell / production / web"].Conclusion != "success" {
		t.Fatalf("GitHub multi-env check runs = %+v", gh.CheckRuns())
	}

	glLast := map[string]string{}
	for _, s := range gl.Statuses() {
		glLast[s.Name] = s.State
	}
	if glLast["Dockwell / staging / web"] != "canceled" || glLast["Dockwell / production / web"] != "success" {
		t.Fatalf("GitLab multi-env statuses = %+v", gl.Statuses())
	}
}

// M3: Even when a process restarts between GitHub creating a check run and
// Dockwell recording remote_id (Attempts == 0), the worker looks up the
// existing check run by external_id before creating and updates it in place.
func TestLostResponseAndRestartLooksUpByExternalID(t *testing.T) {
	t.Parallel()
	db, _ := storetest.Open(t, store.SQLite)
	gh, err := ghtest.New()
	if err != nil {
		t.Fatal(err)
	}
	ghSrv := httptest.NewServer(gh)
	t.Cleanup(ghSrv.Close)
	app := ghAppForFake(t, gh, ghSrv)
	clk := &fakeClock{t: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)}

	targets := &fakeTargets{gh: &cistatus.GitHubTarget{
		App: app, AppID: "gha_1", InstallationID: 7, RepositoryID: 101,
		FullName: "acme/web", CredentialRef: "github_app:gha_1/7",
	}}
	w1 := cistatus.New(cistatus.Config{DB: db, Targets: targets, Now: clk.now, BaseBackoff: time.Second})

	chgID := seedCIChange(t, db, clk.now(), dockwellv1.DeployProvider_DEPLOY_PROVIDER_GITHUB_ACTIONS, "https://github.com/acme/web", store.CIStatusQueued)

	// Simulate GitHub creating the check run, then the response being lost (503).
	gh.DropNextCheckRunResponse()
	if err := w1.DeliverOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(gh.CheckRuns()) != 1 {
		t.Fatalf("expected 1 check run created on GitHub before drop, got %d", len(gh.CheckRuns()))
	}

	// Simulate an even harder crash: reset attempts back to 0 (as if process died
	// before recording either remote_id or attempts) and start a fresh Worker.
	if err := db.Update(t.Context(), func(tx *store.Tx) error {
		return tx.CIStatusMarkFinal(t.Context(), "", store.CIStatusDeliveryFailed, "", "", "", false, clk.now())
	}); err != nil {
		t.Fatal(err)
	}
	clk.advance(5 * time.Second)
	// Also enqueue a healthy transition so the retry updates the existing run to success.
	if err := db.Revise(t.Context(), func(tx *store.RevisionTx) error {
		_, err := tx.EnqueueCIStatusForChange(t.Context(), chgID, store.CIStatusHealthy, store.CIStatusEnqueueOpts{Summary: "Healthy"}, clk.now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	w2 := cistatus.New(cistatus.Config{DB: db, Targets: targets, Now: clk.now})
	if err := w2.DeliverOnce(t.Context()); err != nil {
		t.Fatal(err)
	}

	runs := gh.CheckRuns()
	if len(runs) != 1 || runs[0].Status != "completed" || runs[0].Conclusion != "success" || runs[0].ExternalID != chgID {
		t.Fatalf("expected 1 check run updated in place without duplicates, got %+v", runs)
	}
}

// An older backing-off transition is superseded when a newer transition is due
// so a newer status is never regressed or blocked by an older one.
func TestNoRegressionSupersedesStaleBackingOffTransition(t *testing.T) {
	t.Parallel()
	db, _ := storetest.Open(t, store.SQLite)
	clk := &fakeClock{t: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)}

	var mu sync.Mutex
	failFirst := true
	var received []string
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			_, _ = w.Write([]byte(`{"token":"ghs_tok","expires_at":"2030-01-01T00:00:00Z"}`))
			return
		}
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/check-runs") {
			_, _ = w.Write([]byte(`{"check_runs":[]}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/check-runs") && r.Method == http.MethodPost {
			var body struct {
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			if failFirst {
				failFirst = false
				mu.Unlock()
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"message":"temporarily down"}`))
				return
			}
			received = append(received, body.Status+":"+body.Conclusion)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"id":55,"html_url":"https://github.com/acme/web/runs/55","status":"completed","conclusion":"success"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	app := &github.App{ID: 1, Key: key, Base: srv.URL, Client: srv.Client(), Now: clk.now}
	targets := &fakeTargets{gh: &cistatus.GitHubTarget{
		App: app, AppID: "gha_1", InstallationID: 7, RepositoryID: 101, FullName: "acme/web", CredentialRef: "github_app:gha_1/7",
	}}
	w := cistatus.New(cistatus.Config{DB: db, Targets: targets, Now: clk.now, BaseBackoff: time.Minute})

	// Enqueue transition 1 (queued); first attempt hits 503 and enters 1m backoff.
	chgID := seedCIChange(t, db, clk.now(), dockwellv1.DeployProvider_DEPLOY_PROVIDER_GITHUB_ACTIONS, "https://github.com/acme/web", store.CIStatusQueued)
	if err := w.DeliverOnce(t.Context()); err != nil {
		t.Fatal(err)
	}

	// 10s later (while transition 1 is still in backoff), transition 2 (healthy) arrives.
	clk.advance(10 * time.Second)
	if err := db.Revise(t.Context(), func(tx *store.RevisionTx) error {
		_, err := tx.EnqueueCIStatusForChange(t.Context(), chgID, store.CIStatusHealthy, store.CIStatusEnqueueOpts{Summary: "Deploy web"}, clk.now())
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// Restart recovery: a brand-new Worker instance over the same DB delivers
	// transition 2 immediately and supersedes transition 1.
	w2 := cistatus.New(cistatus.Config{DB: db, Targets: targets, Now: clk.now, BaseBackoff: time.Minute})
	if err := w2.DeliverOnce(t.Context()); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	got := append([]string(nil), received...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "completed:success" {
		t.Fatalf("received check runs = %v, want only [completed:success]", got)
	}
	var all []store.CIStatusEntry
	_ = db.View(t.Context(), func(tx *store.Tx) error {
		var err error
		all, err = tx.CIStatusAllForChange(t.Context(), chgID)
		return err
	})
	if len(all) != 2 || all[0].DeliveryState != store.CIStatusDeliverySuperseded || all[1].DeliveryState != store.CIStatusDeliveryDelivered {
		t.Fatalf("outbox states = %+v", all)
	}
}

// L5: Retry-After is honored and capped at MaxBackoff, retry_since starts on the
// first delivery attempt (not enqueue time), and persistent transient failures
// give up after the bounded window with a ci_status.failed audit event.
func TestRetryAfterCappedAndFirstAttemptWindow(t *testing.T) {
	t.Parallel()
	db, _ := storetest.Open(t, store.SQLite)
	var retryAfterHeader atomic.Value
	retryAfterHeader.Store("120") // 120s > MaxBackoff (30s) -> must cap to 30s
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", retryAfterHeader.Load().(string))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"rate limited"}`))
	}))
	t.Cleanup(srv.Close)

	clk := &fakeClock{t: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)}
	targets := &fakeTargets{gl: &cistatus.GitLabTarget{
		ConnectionID: "glc_1", Revision: 1, Name: "gitlab", BaseURL: "https://gitlab.example.com",
		Token: gltest.TokenAPI, ProjectPath: "acme/web", CredentialRef: "gitlab_connection:glc_1",
	}}
	w := cistatus.New(cistatus.Config{
		DB:          db,
		Targets:     targets,
		GitLab:      &gitlab.Client{HTTP: srv.Client(), TestBase: srv.URL},
		Now:         clk.now,
		BaseBackoff: 5 * time.Second,
		MaxBackoff:  30 * time.Second,
		GiveUpAfter: 2 * time.Minute,
		MaxAttempts: 5,
	})

	chgID := seedCIChange(t, db, clk.now(), dockwellv1.DeployProvider_DEPLOY_PROVIDER_GITLAB_CI, "https://gitlab.example.com/acme/web", store.CIStatusHealthy)
	// Advance clock 1 hour BEFORE first attempt (simulates control plane downtime):
	// the entry must NOT immediately give up on attempt 1 because retry_since starts at first attempt!
	clk.advance(time.Hour)
	firstAttempt := clk.now()
	if err := w.DeliverOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	var latest map[string]store.CIStatusEntry
	_ = db.View(t.Context(), func(tx *store.Tx) error {
		var err error
		latest, err = tx.CIStatusLatest(t.Context(), []string{chgID})
		return err
	})
	st := latest[chgID]
	if st.DeliveryState != store.CIStatusDeliveryPending || st.Attempts != 1 {
		t.Fatalf("after downtime + 1st 429 = %+v, want pending with 1 attempt", st)
	}
	// Retry-After 120s must be capped to MaxBackoff (30s).
	if gotDelay := st.NextAttemptAt.Sub(firstAttempt); gotDelay != 30*time.Second {
		t.Fatalf("capped Retry-After delay = %v, want 30s", gotDelay)
	}
	// Next attempt with Retry-After: 15s (between BaseBackoff<<1=10s and MaxBackoff=30s) must honor 15s.
	retryAfterHeader.Store("15")
	clk.advance(30 * time.Second)
	secondAttempt := clk.now()
	if err := w.DeliverOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	_ = db.View(t.Context(), func(tx *store.Tx) error {
		var err error
		latest, err = tx.CIStatusLatest(t.Context(), []string{chgID})
		return err
	})
	if gotDelay := latest[chgID].NextAttemptAt.Sub(secondAttempt); gotDelay != 15*time.Second {
		t.Fatalf("honored Retry-After delay = %v, want 15s", gotDelay)
	}

	// Advancing past GiveUpAfter from firstAttempt gives up and records ci_status.failed.
	clk.advance(2 * time.Minute)
	if err := w.DeliverOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	var events []store.Event
	_ = db.View(t.Context(), func(tx *store.Tx) error {
		var err error
		latest, err = tx.CIStatusLatest(t.Context(), []string{chgID})
		if err != nil {
			return err
		}
		allEvents, err := tx.ListEvents(t.Context(), 50)
		for _, ev := range allEvents {
			if ev.Action == cistatus.ActionFailed {
				events = append(events, ev)
			}
		}
		return err
	})
	if latest[chgID].DeliveryState != store.CIStatusDeliveryFailed || latest[chgID].ReasonCode != store.CIStatusReasonGaveUp {
		t.Fatalf("after give up = %+v", latest[chgID])
	}
	if len(events) != 1 || events[0].Subject != chgID {
		t.Fatalf("audit events = %+v", events)
	}
}

// L5: One slow/unreachable provider host cannot stall delivery to another host
// in the same pass.
func TestSlowHostIsolation(t *testing.T) {
	t.Parallel()
	db, _ := storetest.Open(t, store.SQLite)
	clk := &fakeClock{t: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)}

	fastDone := make(chan struct{})
	slowGate := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-slowGate:
		default:
			close(slowGate)
		}
	})

	fastGL := gltest.New()
	fastSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fastGL.ServeHTTP(w, r)
		if r.Method == http.MethodPost {
			select {
			case <-fastDone:
			default:
				close(fastDone)
			}
		}
	}))
	t.Cleanup(fastSrv.Close)

	var slowCalls atomic.Int32
	slowSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slowCalls.Add(1)
		// Block until the fast host has already finished!
		<-slowGate
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(slowSrv.Close)

	targets := &fakeTargets{
		glTargetByRepo: map[string]*cistatus.GitLabTarget{
			"https://slow.gitlab.example.com/acme/web": {
				ConnectionID: "glc_slow", Revision: 1, Name: "slow", BaseURL: slowSrv.URL,
				Token: gltest.TokenAPI, ProjectPath: "acme/web", CredentialRef: "gitlab_connection:glc_slow",
			},
			"https://fast.gitlab.example.com/acme/web": {
				ConnectionID: "glc_fast", Revision: 1, Name: "fast", BaseURL: fastSrv.URL,
				Token: gltest.TokenAPI, ProjectPath: "acme/web", CredentialRef: "gitlab_connection:glc_fast",
			},
		},
	}
	w := cistatus.New(cistatus.Config{
		DB: db, Targets: targets, GitLab: &gitlab.Client{HTTP: fastSrv.Client()}, Now: clk.now,
	})

	seedCIChange(t, db, clk.now(), dockwellv1.DeployProvider_DEPLOY_PROVIDER_GITLAB_CI, "https://slow.gitlab.example.com/acme/web", store.CIStatusHealthy)
	fastChg := seedCIChange(t, db, clk.now(), dockwellv1.DeployProvider_DEPLOY_PROVIDER_GITLAB_CI, "https://fast.gitlab.example.com/acme/web", store.CIStatusHealthy)

	done := make(chan error, 1)
	go func() { done <- w.DeliverOnce(t.Context()) }()

	// Fast host MUST complete while slow host is still blocked!
	select {
	case <-fastDone:
		close(slowGate)
	case <-time.After(5 * time.Second):
		close(slowGate)
		t.Fatal("fast host was stalled by slow host")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var latest map[string]store.CIStatusEntry
	_ = db.View(t.Context(), func(tx *store.Tx) error {
		var err error
		latest, err = tx.CIStatusLatest(t.Context(), []string{fastChg})
		return err
	})
	if latest[fastChg].DeliveryState != store.CIStatusDeliveryDelivered {
		t.Fatalf("fast host change = %+v, want delivered", latest[fastChg])
	}
}

// L1 & L6: Missing checks:write verifies live installation permissions, fails
// with MISSING_CHECKS_PERMISSION, records ci_status.retried on manual Retry,
// succeeds after granting, and refuses Retry on already-delivered entries.
func TestMissingChecksPermissionAndManualRetry(t *testing.T) {
	t.Parallel()
	db, _ := storetest.Open(t, store.SQLite)
	gh, err := ghtest.New()
	if err != nil {
		t.Fatal(err)
	}
	gh.SetPermissions(map[string]string{"contents": "read", "metadata": "read"})
	ghSrv := httptest.NewServer(gh)
	t.Cleanup(ghSrv.Close)
	app := ghAppForFake(t, gh, ghSrv)
	clk := &fakeClock{t: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)}

	targets := &fakeTargets{
		gh: &cistatus.GitHubTarget{
			App: app, AppID: "gha_1", InstallationID: 7, RepositoryID: 101, FullName: "acme/web", CredentialRef: "github_app:gha_1/7",
		},
		verifyMissing: true,
	}
	w := cistatus.New(cistatus.Config{DB: db, Targets: targets, Now: clk.now})

	chgID := seedCIChange(t, db, clk.now(), dockwellv1.DeployProvider_DEPLOY_PROVIDER_GITHUB_ACTIONS, "https://github.com/acme/web", store.CIStatusHealthy)
	if err := w.DeliverOnce(t.Context()); err != nil {
		t.Fatal(err)
	}

	var latest map[string]store.CIStatusEntry
	_ = db.View(t.Context(), func(tx *store.Tx) error {
		var err error
		latest, err = tx.CIStatusLatest(t.Context(), []string{chgID})
		return err
	})
	if latest[chgID].DeliveryState != store.CIStatusDeliveryFailed || latest[chgID].ReasonCode != store.CIStatusReasonMissingChecksPermission {
		t.Fatalf("expected MISSING_CHECKS_PERMISSION failure, got %+v", latest[chgID])
	}
	if targets.verifyCalls == 0 {
		t.Fatal("VerifyChecksPermission was not called")
	}

	// Grant checks:write and manually retry -> records ci_status.retried audit event.
	gh.SetPermissions(map[string]string{"contents": "read", "metadata": "read", "checks": "write"})
	if err := w.Retry(t.Context(), chgID, "usr_1", store.SurfacePanel); err != nil {
		t.Fatal(err)
	}
	if err := w.DeliverOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	var retriedEvents []store.Event
	_ = db.View(t.Context(), func(tx *store.Tx) error {
		var err error
		latest, err = tx.CIStatusLatest(t.Context(), []string{chgID})
		if err != nil {
			return err
		}
		allEvents, err := tx.ListEvents(t.Context(), 50)
		for _, ev := range allEvents {
			if ev.Action == cistatus.ActionRetried {
				retriedEvents = append(retriedEvents, ev)
			}
		}
		return err
	})
	if latest[chgID].DeliveryState != store.CIStatusDeliveryDelivered || latest[chgID].RemoteURL == "" {
		t.Fatalf("retry did not deliver: %+v", latest[chgID])
	}
	if len(retriedEvents) != 1 || retriedEvents[0].Subject != chgID || retriedEvents[0].Actor != "usr_1" {
		t.Fatalf("retried audit events = %+v", retriedEvents)
	}
	// Retrying an already-delivered status is refused (L6).
	if err := w.Retry(t.Context(), chgID, "usr_1", store.SurfacePanel); !errors.Is(err, store.ErrCIStatusNotRetryable) {
		t.Fatalf("Retry on delivered = %v, want ErrCIStatusNotRetryable", err)
	}
}

// Missing installation skips cleanly with a structured reason code instead of retrying.
func TestMissingInstallationSkips(t *testing.T) {
	t.Parallel()
	db, _ := storetest.Open(t, store.SQLite)
	clk := &fakeClock{t: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)}
	targets := &fakeTargets{ghSkip: store.CIStatusReasonGitHubNotReached}
	w := cistatus.New(cistatus.Config{DB: db, Targets: targets, Now: clk.now})

	chgID := seedCIChange(t, db, clk.now(), dockwellv1.DeployProvider_DEPLOY_PROVIDER_GITHUB_ACTIONS, "https://github.com/acme/web", store.CIStatusHealthy)
	if err := w.DeliverOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	var latest map[string]store.CIStatusEntry
	_ = db.View(t.Context(), func(tx *store.Tx) error {
		var err error
		latest, err = tx.CIStatusLatest(t.Context(), []string{chgID})
		return err
	})
	if latest[chgID].DeliveryState != store.CIStatusDeliverySkipped || latest[chgID].ReasonCode != store.CIStatusReasonGitHubNotReached {
		t.Fatalf("unexpected skipped state: %+v", latest[chgID])
	}
}
