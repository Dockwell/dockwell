// SPDX-License-Identifier: AGPL-3.0-only

package cistatus_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	mu            sync.Mutex
	gh            *cistatus.GitHubTarget
	ghSkip        string
	gl            *cistatus.GitLabTarget
	glSkip        string
	missingMarked int
	glNotedStates []dockwellv1.GitLabConnectionState
}

func (f *fakeTargets) FindGitHubStatusTarget(_ context.Context, _, _ string) (*cistatus.GitHubTarget, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gh, f.ghSkip, nil
}

func (f *fakeTargets) MarkChecksPermissionMissing(_ context.Context, _ string, _ int64) {
	f.mu.Lock()
	f.missingMarked++
	f.mu.Unlock()
}

func (f *fakeTargets) FindGitLabStatusTarget(_ context.Context, _, _ string) (*cistatus.GitLabTarget, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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

func seedCIChange(t *testing.T, db *store.DB, now time.Time, provider dockwellv1.DeployProvider, repo string, states ...string) string {
	t.Helper()
	changeID := id.New(id.Change)
	serviceID := id.New(id.Service)
	if err := db.Revise(t.Context(), func(tx *store.RevisionTx) error {
		if err := tx.CreateChange(t.Context(), store.Change{
			ID: changeID, Summary: "Deploy web", Actor: "tok_ci", State: store.ChangePlanned,
			CreatedAt: now, Plan: []byte(`{}`), Fingerprint: "fp",
		}); err != nil {
			return err
		}
		if err := tx.RecordCIDeploy(t.Context(), store.CIDeployOrigin{
			Service:    serviceID,
			Provider:   int32(provider),
			Repository: repo,
			Commit:     "0123456789abcdef0123456789abcdef01234567",
			Ref:        "refs/heads/main",
			Image:      "ghcr.io/acme/web:1.0",
			Digest:     "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		}, changeID); err != nil {
			return err
		}
		for i, st := range states {
			if _, err := tx.EnqueueCIStatusForChange(t.Context(), changeID, st, "web", "https://panel.example.com/changes/"+changeID, "Deploy web", now.Add(time.Duration(i)*time.Second)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return changeID
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

// Transitions deliver in order and update a single GitHub Check Run in place.
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
	if runs[0].Name != "Dockwell / web" || runs[0].Status != "completed" || runs[0].Conclusion != "success" || runs[0].ExternalID != chgID {
		t.Fatalf("unexpected check run: %+v", runs[0])
	}
	if !strings.HasSuffix(runs[0].DetailsURL, "/changes/"+chgID) {
		t.Fatalf("details_url = %q", runs[0].DetailsURL)
	}
	var all []store.CIStatusEntry
	_ = db.View(t.Context(), func(tx *store.Tx) error {
		var err error
		all, err = tx.CIStatusAllForChange(t.Context(), chgID)
		return err
	})
	for _, e := range all {
		if e.DeliveryState != store.CIStatusDeliveryDelivered || e.RemoteID == "" {
			t.Fatalf("entry %d not delivered cleanly: %+v", e.Transition, e)
		}
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
		_, err := tx.EnqueueCIStatusForChange(t.Context(), chgID, store.CIStatusHealthy, "web", "https://panel.example.com/changes/"+chgID, "Deploy web", clk.now())
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

// Persistent transient failures retry with backoff and eventually give up after
// the bounded window, recording the reason on the outbox and in the audit log.
func TestBoundedRetryGiveUpAndAuditEvent(t *testing.T) {
	t.Parallel()
	db, _ := storetest.Open(t, store.SQLite)
	gl := gltest.New()
	gl.FailNext(10)
	glSrv := httptest.NewServer(gl)
	t.Cleanup(glSrv.Close)
	clk := &fakeClock{t: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)}

	targets := &fakeTargets{gl: &cistatus.GitLabTarget{
		ConnectionID: "glc_1", Revision: 1, Name: "gitlab", BaseURL: "https://gitlab.example.com",
		Token: gltest.TokenAPI, ProjectPath: "acme/web", CredentialRef: "gitlab_connection:glc_1",
	}}
	w := cistatus.New(cistatus.Config{
		DB:          db,
		Targets:     targets,
		GitLab:      &gitlab.Client{HTTP: glSrv.Client(), TestBase: glSrv.URL},
		Now:         clk.now,
		BaseBackoff: 5 * time.Second,
		MaxBackoff:  30 * time.Second,
		GiveUpAfter: 2 * time.Minute,
		MaxAttempts: 3,
	})

	chgID := seedCIChange(t, db, clk.now(), dockwellv1.DeployProvider_DEPLOY_PROVIDER_GITLAB_CI, "https://gitlab.example.com/acme/web", store.CIStatusHealthy)
	for range 3 {
		if err := w.DeliverOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		clk.advance(time.Minute)
	}

	var latest map[string]store.CIStatusEntry
	var events []store.Event
	err := db.View(t.Context(), func(tx *store.Tx) error {
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
	if err != nil {
		t.Fatal(err)
	}
	st := latest[chgID]
	if st.DeliveryState != store.CIStatusDeliveryFailed || st.Attempts != 3 || !strings.Contains(st.LastError, "503") {
		t.Fatalf("latest outbox = %+v", st)
	}
	if len(events) != 1 || events[0].Subject != chgID || !strings.Contains(events[0].Detail, "503") {
		t.Fatalf("audit events = %+v", events)
	}
}

// Missing GitHub checks:write permission marks the permission missing, fails
// without retrying forever, and succeeds once granted and manually retried.
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

	targets := &fakeTargets{gh: &cistatus.GitHubTarget{
		App: app, AppID: "gha_1", InstallationID: 7, RepositoryID: 101, FullName: "acme/web", CredentialRef: "github_app:gha_1/7",
	}}
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
	if latest[chgID].DeliveryState != store.CIStatusDeliveryFailed || !strings.Contains(latest[chgID].LastError, "Checks permission") {
		t.Fatalf("expected Checks permission failure, got %+v", latest[chgID])
	}
	if targets.missingMarked == 0 {
		t.Fatal("MarkChecksPermissionMissing was not called")
	}

	// Grant checks:write and manually retry.
	gh.SetPermissions(map[string]string{"contents": "read", "metadata": "read", "checks": "write"})
	if err := w.Retry(t.Context(), chgID); err != nil {
		t.Fatal(err)
	}
	if err := w.DeliverOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	_ = db.View(t.Context(), func(tx *store.Tx) error {
		var err error
		latest, err = tx.CIStatusLatest(t.Context(), []string{chgID})
		return err
	})
	if latest[chgID].DeliveryState != store.CIStatusDeliveryDelivered || latest[chgID].RemoteURL == "" {
		t.Fatalf("retry did not deliver: %+v", latest[chgID])
	}
}

// Missing installation skips cleanly with a one-line reason instead of retrying.
func TestMissingInstallationSkips(t *testing.T) {
	t.Parallel()
	db, _ := storetest.Open(t, store.SQLite)
	clk := &fakeClock{t: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)}
	targets := &fakeTargets{ghSkip: "No GitHub App installation reaches acme/web."}
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
	if latest[chgID].DeliveryState != store.CIStatusDeliverySkipped || latest[chgID].LastError != targets.ghSkip {
		t.Fatalf("unexpected skipped state: %+v", latest[chgID])
	}
}
