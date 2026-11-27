// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"crypto/sha256"
	"fmt"
	"hash/crc32"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/cistatus"
	"github.com/dockwell/dockwell/internal/control/github/ghtest"
	"github.com/dockwell/dockwell/internal/control/gitlab/gltest"
	logapi "github.com/dockwell/dockwell/internal/control/logs"
	"github.com/dockwell/dockwell/internal/control/settings"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/https"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/store"
)

func connectFakeGitHubApp(t *testing.T, gp dockwellv1connect.GitProviderServiceClient, c dockwellv1connect.ChangeServiceClient, b *browser) *v1.GitHubAppInfo {
	t.Helper()
	started, err := gp.StartGitHubApp(t.Context(), connect.NewRequest(&v1.StartGitHubAppRequest{
		Proof: stepUpFor(t, b, v1.StepUpAction_STEP_UP_ACTION_CONNECT_GITHUB_APP, ""),
	}))
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(started.Msg.GetActionUrl())
	if err != nil {
		t.Fatal(err)
	}
	finished, err := gp.FinishGitHubApp(t.Context(), connect.NewRequest(&v1.FinishGitHubAppRequest{
		Code:  "code-1",
		State: u.Query().Get("state"),
	}))
	if err != nil {
		t.Fatal(err)
	}
	waitWatch(t, watch(t, c, finished.Msg.GetChangeId()))
	app := finished.Msg.GetApp()
	if _, err := gp.AddGitHubInstallation(t.Context(), connect.NewRequest(&v1.AddGitHubInstallationRequest{
		Id: app.GetId(), InstallationId: 7,
	})); err != nil {
		t.Fatal(err)
	}
	return app
}

func makeDeployToken(t *testing.T, b *browser, serviceID, repo string) (secret string, tok *v1.Token) {
	t.Helper()
	tokens := dockwellv1connect.NewTokenServiceClient(b, b.origin, connect.WithProtoJSON())
	made, err := tokens.CreateToken(t.Context(), connect.NewRequest(&v1.CreateTokenRequest{
		Name:             "CI deploy " + serviceID,
		Scope:            v1.TokenScope_TOKEN_SCOPE_DEPLOY,
		DeployService:    serviceID,
		DeployRepository: repo,
		Proof:            stepUpFor(t, b, v1.StepUpAction_STEP_UP_ACTION_CREATE_TOKEN, ""),
	}))
	if err != nil {
		t.Fatalf("CreateToken(deploy, %s): %v", repo, err)
	}
	return made.Msg.GetSecret(), made.Msg.GetToken()
}

// setupStagingWeb imports shopFile into an unprotected "staging" environment so
// a developer-role deploy token can both plan and apply its own CI deploys, and
// returns the staging service plus the initial non-CI import change.
func setupStagingWeb(t *testing.T, c dockwellv1connect.ChangeServiceClient, cc dockwellv1connect.ComposeServiceClient, s dockwellv1connect.ServiceServiceClient) (*v1.ServiceInfo, *v1.Change) {
	t.Helper()
	imp, err := cc.ImportCompose(t.Context(), connect.NewRequest(&v1.ImportComposeRequest{Compose: shopFile}))
	if err != nil {
		t.Fatal(err)
	}
	first := doneChange(t, c, imp.Msg.GetChange())
	envProp, err := c.ProposeChange(t.Context(), connect.NewRequest(&v1.ProposeChangeRequest{
		Summary: "Add staging",
		Edits: []*v1.Edit{{
			Resource: &v1.ResourceRef{Kind: "environment", Scope: projectOf(first), Name: "staging"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Environment{Environment: &resourcev1.Environment{}}},
		}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	doneChange(t, c, envProp.Msg.GetChange())
	stageImp, err := cc.ImportCompose(t.Context(), connect.NewRequest(&v1.ImportComposeRequest{
		Project: "shop", Environment: "staging", Compose: shopFile,
	}))
	if err != nil {
		t.Fatal(err)
	}
	stage := doneChange(t, c, stageImp.Msg.GetChange())
	wantScope := serviceScope(stage)
	listed, err := s.ListServices(t.Context(), connect.NewRequest(&v1.ListServicesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, svc := range listed.Msg.GetServices() {
		if svc.GetResource().GetScope() == wantScope {
			return svc, first
		}
	}
	t.Fatalf("staging service in scope %s not found in %+v", wantScope, listed.Msg.GetServices())
	return nil, nil
}

// End-to-end GitHub CI deploy status reporting: missing Checks permission shows
// on Settings/Integrations and surfaces structured reason + admin action_url;
// granting Checks and refreshing clears the warning; RetryDeployStatus audits
// ci_status.retried and delivers the env-scoped check run; delivered retry is
// refused; failing deploy reports rolled_back; discarded and superseded planned
// deploys report cancelled; repo-local 403 does not poison the installation.
func TestCIStatusEndToEndGitHubAndRollback(t *testing.T) {
	t.Parallel()
	eachStoreParallel(t, func(t *testing.T, backend store.Backend) {
		gh, err := ghtest.New()
		if err != nil {
			t.Fatal(err)
		}
		gh.SetPermissions(map[string]string{"contents": "read", "metadata": "read"})
		gh.SetRepos([]ghtest.Repository{
			{FullName: "acme/web", Path: "web.git", Private: true, DefaultBranch: "main"},
			{FullName: "acme/archived", Path: "archived.git", Private: true, DefaultBranch: "main"},
		})
		gh.SeedCode("code-1", "dockwell-e2e")
		ghSrv := httptest.NewServer(gh)
		t.Cleanup(ghSrv.Close)
		f := startFullConfig(t, backend, nil, slog.New(slog.DiscardHandler), nil, logapi.Options{}, func(cfg *Config) {
			cfg.GitHub = GitHubConfig{API: ghSrv.URL, Web: ghSrv.URL, Client: ghSrv.Client()}
			cfg.PanelURL = "https://panel.example.com"
			cfg.ManualCIStatus = true
		})
		b := signedIn(t, f)
		e := f.addDockerServer(t)
		c := changeClient(b, b.origin)
		cc := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
		s := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
		gp := dockwellv1connect.NewGitProviderServiceClient(b, b.origin, connect.WithProtoJSON())

		// Connect GitHub App while checks:write is missing.
		app := connectFakeGitHubApp(t, gp, c, b)
		listed, err := gp.ListGitProviders(t.Context(), connect.NewRequest(&v1.ListGitProvidersRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		if len(listed.Msg.GetApps()) != 1 || !listed.Msg.GetApps()[0].GetChecksPermissionMissing() || listed.Msg.GetApps()[0].GetPermissionsUrl() == "" {
			t.Fatalf("expected ChecksPermissionMissing with PermissionsUrl, got %+v", listed.Msg.GetApps())
		}

		// Import a service into unprotected "staging" ("web" -> repo acme/web in ghtest).
		svc, _ := setupStagingWeb(t, c, cc, s)

		// Create deploy token bound to https://github.com/acme/web.
		tokSecret, _ := makeDeployToken(t, b, svc.GetId(), "https://github.com/acme/web")
		tokClient := bearer{token: tokSecret, rt: b.client.Transport}
		tokServices := dockwellv1connect.NewServiceServiceClient(tokClient, b.origin, connect.WithProtoJSON())
		tokChanges := changeClient(tokClient, b.origin)

		// Deploy via CI while checks:write is missing -> skipped with structured reason + admin action_url.
		d1 := "sha256:" + strings.Repeat("1", 64)
		img1 := "ghcr.io/acme/web:1.0@" + d1
		e.Registry[img1] = dockertest.Image{Config: docker.Config{Cmd: []string{"web"}}, Digest: d1}
		planned, err := tokServices.DeployService(t.Context(), connect.NewRequest(&v1.DeployServiceRequest{
			Id: svc.GetId(), Image: img1,
			DeployOrigin: &v1.DeployOrigin{
				Provider:   v1.DeployProvider_DEPLOY_PROVIDER_GITHUB_ACTIONS,
				Repository: "https://github.com/acme/web",
				Commit:     strings.Repeat("a", 40),
				Ref:        "refs/heads/main",
				RunUrl:     "https://github.com/acme/web/actions/runs/100",
			},
		}))
		if err != nil {
			t.Fatal(err)
		}
		chg1 := doneChange(t, tokChanges, planned.Msg.GetChange())
		if err := f.server.CIStatus().DeliverOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		got1, err := c.GetChange(t.Context(), connect.NewRequest(&v1.GetChangeRequest{Id: chg1.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		st1 := got1.Msg.GetChange().GetDeployStatus()
		if st1 == nil || st1.GetState() != v1.DeployStatusState_DEPLOY_STATUS_STATE_SKIPPED || !st1.GetCanRetry() ||
			st1.GetReason() != v1.DeployStatusReason_DEPLOY_STATUS_REASON_MISSING_CHECKS_PERMISSION || st1.GetActionUrl() == "" {
			t.Fatalf("expected skipped due to MISSING_CHECKS_PERMISSION with action_url, got %+v", st1)
		}

		// Grant checks:write on fake GitHub and refresh via RefreshGitHubApp.
		gh.SetPermissions(map[string]string{"contents": "read", "metadata": "read", "checks": "write"})
		refreshed, err := gp.RefreshGitHubApp(t.Context(), connect.NewRequest(&v1.RefreshGitHubAppRequest{Id: app.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		if refreshed.Msg.GetApp().GetChecksPermissionMissing() {
			t.Fatalf("ChecksPermissionMissing still true after refresh: %+v", refreshed.Msg.GetApp())
		}

		// RetryDeployStatus audits ci_status.retried and delivers Check Run with env-scoped name (M2).
		if _, err := c.RetryDeployStatus(t.Context(), connect.NewRequest(&v1.RetryDeployStatusRequest{Id: chg1.GetId()})); err != nil {
			t.Fatal(err)
		}
		if err := f.server.CIStatus().DeliverOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		got1, err = c.GetChange(t.Context(), connect.NewRequest(&v1.GetChangeRequest{Id: chg1.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		st1 = got1.Msg.GetChange().GetDeployStatus()
		if st1.GetState() != v1.DeployStatusState_DEPLOY_STATUS_STATE_DELIVERED || st1.GetRemoteUrl() == "" || st1.GetTransition() != v1.DeployStatusTransition_DEPLOY_STATUS_TRANSITION_HEALTHY {
			t.Fatalf("expected delivered healthy status after retry, got %+v", st1)
		}
		// L6: retrying an already-delivered status is refused.
		if _, err := c.RetryDeployStatus(t.Context(), connect.NewRequest(&v1.RetryDeployStatusRequest{Id: chg1.GetId()})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Fatalf("RetryDeployStatus on delivered = %v, want FailedPrecondition", err)
		}
		// Verify ci_status.retried was audited.
		evClient := dockwellv1connect.NewEventServiceClient(b, b.origin, connect.WithProtoJSON())
		evs, err := evClient.ListEvents(t.Context(), connect.NewRequest(&v1.ListEventsRequest{PageSize: 50}))
		if err != nil {
			t.Fatal(err)
		}
		sawRetried := false
		for _, ev := range evs.Msg.GetEvents() {
			if ev.GetAction() == cistatus.ActionRetried && ev.GetSubject().GetId() == svc.GetId() {
				sawRetried = true
			}
		}
		if !sawRetried {
			t.Fatalf("expected %s audit event for %s", cistatus.ActionRetried, chg1.GetId())
		}

		runs := gh.CheckRuns()
		if len(runs) != 1 || runs[0].Name != "Dockwell / staging / web" || runs[0].Status != "completed" || runs[0].Conclusion != "success" ||
			runs[0].DetailsURL != "https://panel.example.com/changes/"+chg1.GetId() {
			t.Fatalf("unexpected GitHub check runs: %+v", runs)
		}

		// Verify ListServiceRevisions and ListServices expose DeployStatus too.
		revs, err := s.ListServiceRevisions(t.Context(), connect.NewRequest(&v1.ListServiceRevisionsRequest{Id: svc.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		if revs.Msg.GetRevisions()[0].GetChange().GetDeployStatus().GetState() != v1.DeployStatusState_DEPLOY_STATUS_STATE_DELIVERED ||
			revs.Msg.GetRevisions()[0].GetDeployments()[0].GetDeployStatus().GetRemoteUrl() == "" {
			t.Fatalf("revision missing deploy_status: %+v", revs.Msg.GetRevisions()[0])
		}

		// A failing deploy rolls back automatically and reports rolled_back to GitHub.
		d2 := "sha256:" + strings.Repeat("2", 64)
		img2 := "ghcr.io/acme/web:2.0@" + d2
		e.Registry[img2] = dockertest.Image{Config: docker.Config{Cmd: []string{"web"}}, Digest: d2}
		e.SetExits(img2, true)
		planned2, err := tokServices.DeployService(t.Context(), connect.NewRequest(&v1.DeployServiceRequest{
			Id: svc.GetId(), Image: img2,
			DeployOrigin: &v1.DeployOrigin{
				Provider:   v1.DeployProvider_DEPLOY_PROVIDER_GITHUB_ACTIONS,
				Repository: "https://github.com/acme/web",
				Commit:     strings.Repeat("b", 40),
				Ref:        "refs/heads/main",
				RunUrl:     "https://github.com/acme/web/actions/runs/101",
			},
		}))
		if err != nil {
			t.Fatal(err)
		}
		failed := applied(t, tokChanges, planned2.Msg.GetChange())
		f.engine.Wait()
		if failed.GetState() != v1.ChangeState_CHANGE_STATE_FAILED {
			t.Fatalf("expected failed deploy, got %v", failed.GetState())
		}
		if err := f.server.CIStatus().DeliverOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		got2, err := c.GetChange(t.Context(), connect.NewRequest(&v1.GetChangeRequest{Id: failed.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		if got2.Msg.GetChange().GetDeployStatus().GetTransition() != v1.DeployStatusTransition_DEPLOY_STATUS_TRANSITION_ROLLED_BACK ||
			got2.Msg.GetChange().GetDeployStatus().GetState() != v1.DeployStatusState_DEPLOY_STATUS_STATE_DELIVERED {
			t.Fatalf("expected delivered rolled_back status, got %+v", got2.Msg.GetChange().GetDeployStatus())
		}

		// M1: Discarding a planned CI deploy delivers a final cancelled check run.
		d3 := "sha256:" + strings.Repeat("3", 64)
		img3 := "ghcr.io/acme/web:3.0@" + d3
		e.Registry[img3] = dockertest.Image{Config: docker.Config{Cmd: []string{"web"}}, Digest: d3}
		planned3, err := tokServices.DeployService(t.Context(), connect.NewRequest(&v1.DeployServiceRequest{
			Id: svc.GetId(), Image: img3,
			DeployOrigin: &v1.DeployOrigin{
				Provider:   v1.DeployProvider_DEPLOY_PROVIDER_GITHUB_ACTIONS,
				Repository: "https://github.com/acme/web",
				Commit:     strings.Repeat("c", 40),
				Ref:        "refs/heads/main",
				RunUrl:     "https://github.com/acme/web/actions/runs/102",
			},
		}))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.DiscardChange(t.Context(), connect.NewRequest(&v1.DiscardChangeRequest{Id: planned3.Msg.GetChange().GetId()})); err != nil {
			t.Fatal(err)
		}
		if err := f.server.CIStatus().DeliverOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		got3, err := c.GetChange(t.Context(), connect.NewRequest(&v1.GetChangeRequest{Id: planned3.Msg.GetChange().GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		if got3.Msg.GetChange().GetDeployStatus().GetTransition() != v1.DeployStatusTransition_DEPLOY_STATUS_TRANSITION_DISCARDED ||
			got3.Msg.GetChange().GetDeployStatus().GetState() != v1.DeployStatusState_DEPLOY_STATUS_STATE_DELIVERED {
			t.Fatalf("expected delivered discarded status, got %+v", got3.Msg.GetChange().GetDeployStatus())
		}
		var discRun *ghtest.CheckRun
		for i := range gh.CheckRuns() {
			r := gh.CheckRuns()[i]
			if r.ExternalID == planned3.Msg.GetChange().GetId() {
				discRun = &r
				break
			}
		}
		if discRun == nil || discRun.Status != "completed" || discRun.Conclusion != "cancelled" {
			t.Fatalf("expected cancelled check run for discarded change, got %+v", discRun)
		}

		// M1: Superseding a planned CI deploy by applying a newer deploy marks the old one cancelled.
		d4 := "sha256:" + strings.Repeat("4", 64)
		img4 := "ghcr.io/acme/web:4.0@" + d4
		e.Registry[img4] = dockertest.Image{Config: docker.Config{Cmd: []string{"web"}}, Digest: d4}
		plannedOld, err := tokServices.DeployService(t.Context(), connect.NewRequest(&v1.DeployServiceRequest{
			Id: svc.GetId(), Image: img4,
			DeployOrigin: &v1.DeployOrigin{
				Provider:   v1.DeployProvider_DEPLOY_PROVIDER_GITHUB_ACTIONS,
				Repository: "https://github.com/acme/web",
				Commit:     strings.Repeat("d", 40),
				Ref:        "refs/heads/main",
				RunUrl:     "https://github.com/acme/web/actions/runs/103",
			},
		}))
		if err != nil {
			t.Fatal(err)
		}
		d5 := "sha256:" + strings.Repeat("5", 64)
		img5 := "ghcr.io/acme/web:5.0@" + d5
		e.Registry[img5] = dockertest.Image{Config: docker.Config{Cmd: []string{"web"}}, Digest: d5}
		plannedNew, err := tokServices.DeployService(t.Context(), connect.NewRequest(&v1.DeployServiceRequest{
			Id: svc.GetId(), Image: img5,
			DeployOrigin: &v1.DeployOrigin{
				Provider:   v1.DeployProvider_DEPLOY_PROVIDER_GITHUB_ACTIONS,
				Repository: "https://github.com/acme/web",
				Commit:     strings.Repeat("e", 40),
				Ref:        "refs/heads/main",
				RunUrl:     "https://github.com/acme/web/actions/runs/104",
			},
		}))
		if err != nil {
			t.Fatal(err)
		}
		doneChange(t, tokChanges, plannedNew.Msg.GetChange())
		if err := f.server.CIStatus().DeliverOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		gotOld, err := c.GetChange(t.Context(), connect.NewRequest(&v1.GetChangeRequest{Id: plannedOld.Msg.GetChange().GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		if gotOld.Msg.GetChange().GetDeployStatus().GetTransition() != v1.DeployStatusTransition_DEPLOY_STATUS_TRANSITION_DISCARDED ||
			gotOld.Msg.GetChange().GetDeployStatus().GetState() != v1.DeployStatusState_DEPLOY_STATUS_STATE_DELIVERED {
			t.Fatalf("expected superseded planned change to deliver discarded, got %+v", gotOld.Msg.GetChange().GetDeployStatus())
		}

		// L1: A repo-local 403 on an archived repository does NOT mark ChecksPermissionMissing
		// on the installation when GitHub's installation permissions still grant checks:write.
		gh.SetRepoCheckRunError("acme/archived", http.StatusForbidden, "Repository was archived so is read-only.")
		archTokSecret, _ := makeDeployToken(t, b, svc.GetId(), "https://github.com/acme/archived")
		archClient := bearer{token: archTokSecret, rt: b.client.Transport}
		archServices := dockwellv1connect.NewServiceServiceClient(archClient, b.origin, connect.WithProtoJSON())
		archChanges := changeClient(archClient, b.origin)
		d6 := "sha256:" + strings.Repeat("6", 64)
		img6 := "ghcr.io/acme/web:6.0@" + d6
		e.Registry[img6] = dockertest.Image{Config: docker.Config{Cmd: []string{"web"}}, Digest: d6}
		plannedArch, err := archServices.DeployService(t.Context(), connect.NewRequest(&v1.DeployServiceRequest{
			Id: svc.GetId(), Image: img6,
			DeployOrigin: &v1.DeployOrigin{
				Provider:   v1.DeployProvider_DEPLOY_PROVIDER_GITHUB_ACTIONS,
				Repository: "https://github.com/acme/archived",
				Commit:     strings.Repeat("f", 40),
				Ref:        "refs/heads/main",
				RunUrl:     "https://github.com/acme/archived/actions/runs/105",
			},
		}))
		if err != nil {
			t.Fatal(err)
		}
		archChg := doneChange(t, archChanges, plannedArch.Msg.GetChange())
		if err := f.server.CIStatus().DeliverOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		gotArch, err := c.GetChange(t.Context(), connect.NewRequest(&v1.GetChangeRequest{Id: archChg.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		if gotArch.Msg.GetChange().GetDeployStatus().GetReason() != v1.DeployStatusReason_DEPLOY_STATUS_REASON_GITHUB_AUTH_FAILED {
			t.Fatalf("archived repo 403 reason = %v, want GITHUB_AUTH_FAILED", gotArch.Msg.GetChange().GetDeployStatus().GetReason())
		}
		afterArch, err := gp.ListGitProviders(t.Context(), connect.NewRequest(&v1.ListGitProvidersRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		if afterArch.Msg.GetApps()[0].GetChecksPermissionMissing() {
			t.Fatalf("repo-local 403 wrongly flipped ChecksPermissionMissing on installation: %+v", afterArch.Msg.GetApps()[0])
		}
	})
}

// H1: A deploy token bound to acme/web cannot post status to acme/someone-elses-repo
// even when the GitHub App reaches both; slice-1 tokens without a repository report
// nothing until SetTokenDeployRepository sets it; token_repository is visible only
// to the token's owner and hidden from other project members.
func TestCIStatusH1TokenRepositoryBindingAndPrivacy(t *testing.T) {
	t.Parallel()
	eachStoreParallel(t, func(t *testing.T, backend store.Backend) {
		gh, err := ghtest.New()
		if err != nil {
			t.Fatal(err)
		}
		gh.SetRepos([]ghtest.Repository{
			{FullName: "acme/web", Path: "web.git", Private: true, DefaultBranch: "main"},
			{FullName: "acme/someone-elses-repo", Path: "other.git", Private: true, DefaultBranch: "main"},
		})
		gh.SeedCode("code-1", "dockwell-e2e")
		ghSrv := httptest.NewServer(gh)
		t.Cleanup(ghSrv.Close)
		f := startFullConfig(t, backend, nil, slog.New(slog.DiscardHandler), nil, logapi.Options{}, func(cfg *Config) {
			cfg.GitHub = GitHubConfig{API: ghSrv.URL, Web: ghSrv.URL, Client: ghSrv.Client()}
			cfg.PanelURL = "https://panel.example.com"
			cfg.ManualCIStatus = true
		})
		b := signedIn(t, f)
		e := f.addDockerServer(t)
		c := changeClient(b, b.origin)
		cc := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
		s := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
		gp := dockwellv1connect.NewGitProviderServiceClient(b, b.origin, connect.WithProtoJSON())
		tokens := dockwellv1connect.NewTokenServiceClient(b, b.origin, connect.WithProtoJSON())

		connectFakeGitHubApp(t, gp, c, b)
		svc, _ := setupStagingWeb(t, c, cc, s)

		// 1. Token bound to acme/web deploying with provenance naming acme/someone-elses-repo:
		// deploys normally, posts ZERO check runs to GitHub.
		tokSecret, _ := makeDeployToken(t, b, svc.GetId(), "https://github.com/acme/web")
		tokClient := bearer{token: tokSecret, rt: b.client.Transport}
		tokServices := dockwellv1connect.NewServiceServiceClient(tokClient, b.origin, connect.WithProtoJSON())
		tokChanges := changeClient(tokClient, b.origin)
		d1 := "sha256:" + strings.Repeat("a", 64)
		img1 := "ghcr.io/acme/web:1.0@" + d1
		e.Registry[img1] = dockertest.Image{Config: docker.Config{Cmd: []string{"web"}}, Digest: d1}
		plannedForeign, err := tokServices.DeployService(t.Context(), connect.NewRequest(&v1.DeployServiceRequest{
			Id: svc.GetId(), Image: img1,
			DeployOrigin: &v1.DeployOrigin{
				Provider:   v1.DeployProvider_DEPLOY_PROVIDER_GITHUB_ACTIONS,
				Repository: "https://github.com/acme/someone-elses-repo",
				Commit:     strings.Repeat("1", 40),
				Ref:        "refs/heads/main",
				RunUrl:     "https://github.com/acme/someone-elses-repo/actions/runs/1",
			},
		}))
		if err != nil {
			t.Fatal(err)
		}
		chgForeign := doneChange(t, tokChanges, plannedForeign.Msg.GetChange())
		if err := f.server.CIStatus().DeliverOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		if len(gh.CheckRuns()) != 0 {
			t.Fatalf("foreign repository posted check runs to GitHub: %+v", gh.CheckRuns())
		}
		// Token owner sees token_repository.
		ownerGot, err := c.GetChange(t.Context(), connect.NewRequest(&v1.GetChangeRequest{Id: chgForeign.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		stOwner := ownerGot.Msg.GetChange().GetDeployStatus()
		if stOwner.GetState() != v1.DeployStatusState_DEPLOY_STATUS_STATE_SKIPPED ||
			stOwner.GetReason() != v1.DeployStatusReason_DEPLOY_STATUS_REASON_REPOSITORY_MISMATCH ||
			stOwner.GetTokenRepository() != "https://github.com/acme/web" {
			t.Fatalf("owner view of mismatch = %+v", stOwner)
		}

		// Another user (a second member with install viewer access) must NOT see token_repository.
		otherUserID := id.New(id.User)
		const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
		body := strings.Repeat("a", 43)
		sum := new(big.Int).SetUint64(uint64(crc32.ChecksumIEEE([]byte(body))))
		chk := make([]byte, 6)
		b62, rem := big.NewInt(62), new(big.Int)
		for i := len(chk) - 1; i >= 0; i-- {
			sum.QuoRem(sum, b62, rem)
			chk[i] = base62[rem.Int64()]
		}
		otherTokenSecret := "dwt_" + body + string(chk)
		otherTokenHash := sha256.Sum256([]byte(otherTokenSecret))
		now := time.Now().UTC().Truncate(time.Millisecond)
		if err := f.store.Update(t.Context(), func(tx *store.Tx) error {
			if err := tx.CreateUser(t.Context(), store.User{
				ID: otherUserID, Email: "other@example.com", Name: "Other", WebAuthnHandle: []byte("other"), CreatedAt: now,
			}); err != nil {
				return err
			}
			if err := tx.SetGrant(t.Context(), store.Grant{UserID: otherUserID, ProjectID: "", Role: store.RoleViewer, CreatedAt: now}); err != nil {
				return err
			}
			return tx.CreateAPIToken(t.Context(), store.APIToken{
				ID:        id.New(id.Token),
				UserID:    otherUserID,
				Name:      "other read",
				TokenHash: otherTokenHash[:],
				Scope:     store.ScopeReadOnly,
				CreatedAt: now,
			})
		}); err != nil {
			t.Fatal(err)
		}
		otherChanges := changeClient(bearer{token: otherTokenSecret, rt: b.client.Transport}, b.origin)
		otherGot, err := otherChanges.GetChange(t.Context(), connect.NewRequest(&v1.GetChangeRequest{Id: chgForeign.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		if otherGot.Msg.GetChange().GetDeployStatus().GetTokenRepository() != "" {
			t.Fatalf("other user saw token_repository: %q", otherGot.Msg.GetChange().GetDeployStatus().GetTokenRepository())
		}

		// 2. Slice-1 token (empty deploy_repository) posts nothing until SetTokenDeployRepository sets it.
		s1Secret, s1Tok := makeDeployToken(t, b, svc.GetId(), "")
		s1Client := bearer{token: s1Secret, rt: b.client.Transport}
		s1Services := dockwellv1connect.NewServiceServiceClient(s1Client, b.origin, connect.WithProtoJSON())
		s1Changes := changeClient(s1Client, b.origin)
		d2 := "sha256:" + strings.Repeat("b", 64)
		img2 := "ghcr.io/acme/web:2.0@" + d2
		e.Registry[img2] = dockertest.Image{Config: docker.Config{Cmd: []string{"web"}}, Digest: d2}
		plannedS1, err := s1Services.DeployService(t.Context(), connect.NewRequest(&v1.DeployServiceRequest{
			Id: svc.GetId(), Image: img2,
			DeployOrigin: &v1.DeployOrigin{
				Provider:   v1.DeployProvider_DEPLOY_PROVIDER_GITHUB_ACTIONS,
				Repository: "https://github.com/acme/web",
				Commit:     strings.Repeat("2", 40),
				Ref:        "refs/heads/main",
				RunUrl:     "https://github.com/acme/web/actions/runs/2",
			},
		}))
		if err != nil {
			t.Fatal(err)
		}
		chgS1 := doneChange(t, s1Changes, plannedS1.Msg.GetChange())
		if err := f.server.CIStatus().DeliverOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		if len(gh.CheckRuns()) != 0 {
			t.Fatalf("slice-1 token without deploy_repository posted check runs: %+v", gh.CheckRuns())
		}
		gotS1, err := c.GetChange(t.Context(), connect.NewRequest(&v1.GetChangeRequest{Id: chgS1.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		if gotS1.Msg.GetChange().GetDeployStatus().GetReason() != v1.DeployStatusReason_DEPLOY_STATUS_REASON_NO_TOKEN_REPOSITORY {
			t.Fatalf("slice-1 token reason = %v, want NO_TOKEN_REPOSITORY", gotS1.Msg.GetChange().GetDeployStatus().GetReason())
		}

		// Set repository on the slice-1 token via SetTokenDeployRepository (with step-up, N7), then RetryDeployStatus -> delivers!
		updatedTok, err := tokens.SetTokenDeployRepository(t.Context(), connect.NewRequest(&v1.SetTokenDeployRepositoryRequest{
			Id: s1Tok.GetId(), DeployRepository: "https://github.com/acme/web",
			Proof: stepUpFor(t, b, v1.StepUpAction_STEP_UP_ACTION_CREATE_TOKEN, ""),
		}))
		if err != nil || updatedTok.Msg.GetToken().GetDeployRepository() != "https://github.com/acme/web" {
			t.Fatalf("SetTokenDeployRepository = %+v, %v", updatedTok, err)
		}
		if _, err := c.RetryDeployStatus(t.Context(), connect.NewRequest(&v1.RetryDeployStatusRequest{Id: chgS1.GetId()})); err != nil {
			t.Fatal(err)
		}
		if err := f.server.CIStatus().DeliverOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		if len(gh.CheckRuns()) != 1 || gh.CheckRuns()[0].Conclusion != "success" {
			t.Fatalf("expected 1 delivered check run after setting repository + retry, got %+v", gh.CheckRuns())
		}
	})
}

// End-to-end GitLab CI deploy status reporting via gitlab.connection resource,
// CheckGitLabConnection verification, ListGitLabProjects, protojson secret-leak
// checks (L8), foreign-repo block (H1), and commit status delivery.
func TestCIStatusEndToEndGitLabConnection(t *testing.T) {
	t.Parallel()
	eachStoreParallel(t, func(t *testing.T, backend store.Backend) {
		gl := gltest.New()
		gl.SetProjects([]gltest.Project{
			{PathWithNamespace: "acme/web", WebURL: "https://gitlab.example.com/acme/web"},
			{PathWithNamespace: "acme/someone-elses-repo", WebURL: "https://gitlab.example.com/acme/someone-elses-repo"},
			{PathWithNamespace: "other/app", WebURL: "https://gitlab.example.com/other/app"},
		})
		glSrv := httptest.NewServer(gl)
		t.Cleanup(glSrv.Close)
		f := startFullConfig(t, backend, nil, slog.New(slog.DiscardHandler), nil, logapi.Options{}, func(cfg *Config) {
			cfg.GitLab = GitLabConfig{Base: glSrv.URL, Client: glSrv.Client()}
			cfg.PanelURL = "https://panel.example.com"
			cfg.ManualCIStatus = true
		})
		b := signedIn(t, f)
		e := f.addDockerServer(t)
		c := changeClient(b, b.origin)
		cc := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
		s := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
		gp := dockwellv1connect.NewGitProviderServiceClient(b, b.origin, connect.WithProtoJSON())
		resClient := dockwellv1connect.NewResourceServiceClient(b, b.origin, connect.WithProtoJSON())
		evClient := dockwellv1connect.NewEventServiceClient(b, b.origin, connect.WithProtoJSON())

		// CheckGitLabConnection distinguishes valid api token, read_api token, and bad token.
		chkOK, err := gp.CheckGitLabConnection(t.Context(), connect.NewRequest(&v1.CheckGitLabConnectionRequest{
			Url: "https://gitlab.example.com", Token: gltest.TokenAPI,
		}))
		if err != nil || chkOK.Msg.GetState() != v1.GitLabConnectionState_GIT_LAB_CONNECTION_STATE_CONNECTED {
			t.Fatalf("CheckGitLabConnection(api) = %+v, %v", chkOK, err)
		}
		chkRead, err := gp.CheckGitLabConnection(t.Context(), connect.NewRequest(&v1.CheckGitLabConnectionRequest{
			Url: "https://gitlab.example.com", Token: gltest.TokenReadOnly,
		}))
		if err != nil || chkRead.Msg.GetState() != v1.GitLabConnectionState_GIT_LAB_CONNECTION_STATE_MISSING_SCOPE {
			t.Fatalf("CheckGitLabConnection(read_api) = %+v, %v", chkRead, err)
		}
		chkBad, err := gp.CheckGitLabConnection(t.Context(), connect.NewRequest(&v1.CheckGitLabConnectionRequest{
			Url: "https://gitlab.example.com", Token: "glpat-invalid",
		}))
		if err != nil || chkBad.Msg.GetState() != v1.GitLabConnectionState_GIT_LAB_CONNECTION_STATE_AUTH_FAILED {
			t.Fatalf("CheckGitLabConnection(bad) = %+v, %v", chkBad, err)
		}

		// Save gitlab.connection through ChangeService (normal plan & apply).
		prop, err := c.ProposeChange(t.Context(), connect.NewRequest(&v1.ProposeChangeRequest{
			Summary: "Connect GitLab",
			Edits: []*v1.Edit{{
				Resource: &v1.ResourceRef{Kind: "gitlab.connection", Name: "self-managed"},
				Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_GitlabConnection{GitlabConnection: &resourcev1.GitLabConnection{
					InstanceUrl: "https://gitlab.example.com",
					Token:       gltest.TokenAPI,
					PathPrefix:  "acme",
				}}},
			}},
		}))
		if err != nil {
			t.Fatal(err)
		}
		glConnChg := doneChange(t, c, prop.Msg.GetChange())

		// L8: assert the GitLab token never leaks in ProposeChange, GetChange, ListResources, or ListEvents.
		assertNoToken := func(label string, msg proto.Message) {
			t.Helper()
			raw, err := protojson.Marshal(msg)
			if err != nil {
				t.Fatalf("protojson.Marshal(%s): %v", label, err)
			}
			if strings.Contains(string(raw), gltest.TokenAPI) {
				t.Fatalf("%s leaked GitLab access token!", label)
			}
		}
		assertNoToken("ProposeChange", prop.Msg)
		gotGLChg, err := c.GetChange(t.Context(), connect.NewRequest(&v1.GetChangeRequest{Id: glConnChg.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		assertNoToken("GetChange", gotGLChg.Msg)
		allRes, err := resClient.ListResources(t.Context(), connect.NewRequest(&v1.ListResourcesRequest{Kind: "gitlab.connection"}))
		if err != nil {
			t.Fatal(err)
		}
		assertNoToken("ListResources", allRes.Msg)
		allEvs, err := evClient.ListEvents(t.Context(), connect.NewRequest(&v1.ListEventsRequest{PageSize: 50}))
		if err != nil {
			t.Fatal(err)
		}
		assertNoToken("ListEvents", allEvs.Msg)

		// ListGitProviders exposes the GitLab connection without its token.
		listed, err := gp.ListGitProviders(t.Context(), connect.NewRequest(&v1.ListGitProvidersRequest{}))
		if err != nil || len(listed.Msg.GetGitlabConnections()) != 1 || listed.Msg.GetGitlabConnections()[0].GetUrl() != "https://gitlab.example.com" {
			t.Fatalf("ListGitProviders = %+v, %v", listed, err)
		}
		assertNoToken("ListGitProviders", listed.Msg)

		// Re-check saved connection reusing its stored token (token omitted).
		recheck, err := gp.CheckGitLabConnection(t.Context(), connect.NewRequest(&v1.CheckGitLabConnectionRequest{
			Name: "self-managed", Url: "https://gitlab.example.com",
		}))
		if err != nil || recheck.Msg.GetState() != v1.GitLabConnectionState_GIT_LAB_CONNECTION_STATE_CONNECTED {
			t.Fatalf("saved recheck = %+v, %v", recheck, err)
		}

		// Import service into unprotected "staging" and verify ListGitLabProjects filters by path_prefix ("acme").
		svc, imp := setupStagingWeb(t, c, cc, s)

		glProjects, err := gp.ListGitLabProjects(t.Context(), connect.NewRequest(&v1.ListGitLabProjectsRequest{Project: "shop"}))
		if err != nil {
			t.Fatal(err)
		}
		if len(glProjects.Msg.GetProjects()) != 2 {
			t.Fatalf("ListGitLabProjects = %+v, want 2 acme/* projects (excluding other/app)", glProjects.Msg.GetProjects())
		}

		// Create deploy token bound to https://gitlab.example.com/acme/web.
		tokSecret, _ := makeDeployToken(t, b, svc.GetId(), "https://gitlab.example.com/acme/web")
		tokClient := bearer{token: tokSecret, rt: b.client.Transport}
		tokServices := dockwellv1connect.NewServiceServiceClient(tokClient, b.origin, connect.WithProtoJSON())
		tokChanges := changeClient(tokClient, b.origin)

		// H1: Foreign GitLab repo deploys cleanly but posts ZERO commit statuses!
		dForeign := "sha256:" + strings.Repeat("e", 64)
		imgForeign := "registry.gitlab.example.com/acme/web:0.9@" + dForeign
		e.Registry[imgForeign] = dockertest.Image{Config: docker.Config{Cmd: []string{"web"}}, Digest: dForeign}
		plannedForeign, err := tokServices.DeployService(t.Context(), connect.NewRequest(&v1.DeployServiceRequest{
			Id: svc.GetId(), Image: imgForeign,
			DeployOrigin: &v1.DeployOrigin{
				Provider:   v1.DeployProvider_DEPLOY_PROVIDER_GITLAB_CI,
				Repository: "https://gitlab.example.com/acme/someone-elses-repo",
				Commit:     strings.Repeat("9", 40),
				Ref:        "refs/heads/main",
				RunUrl:     "https://gitlab.example.com/acme/someone-elses-repo/-/jobs/1",
			},
		}))
		if err != nil {
			t.Fatal(err)
		}
		doneChange(t, tokChanges, plannedForeign.Msg.GetChange())
		if err := f.server.CIStatus().DeliverOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		if len(gl.Statuses()) != 0 {
			t.Fatalf("foreign GitLab repo posted statuses: %+v", gl.Statuses())
		}

		// Matching repo delivers pending -> running -> success with env-scoped name.
		d1 := "sha256:" + strings.Repeat("c", 64)
		img1 := "registry.gitlab.example.com/acme/web:1.0@" + d1
		e.Registry[img1] = dockertest.Image{Config: docker.Config{Cmd: []string{"web"}}, Digest: d1}
		planned, err := tokServices.DeployService(t.Context(), connect.NewRequest(&v1.DeployServiceRequest{
			Id: svc.GetId(), Image: img1,
			DeployOrigin: &v1.DeployOrigin{
				Provider:   v1.DeployProvider_DEPLOY_PROVIDER_GITLAB_CI,
				Repository: "https://gitlab.example.com/acme/web",
				Commit:     strings.Repeat("d", 40),
				Ref:        "refs/heads/main",
				RunUrl:     "https://gitlab.example.com/acme/web/-/jobs/77",
			},
		}))
		if err != nil {
			t.Fatal(err)
		}
		chg := doneChange(t, tokChanges, planned.Msg.GetChange())
		if err := f.server.CIStatus().DeliverOnce(t.Context()); err != nil {
			t.Fatal(err)
		}

		got, err := c.GetChange(t.Context(), connect.NewRequest(&v1.GetChangeRequest{Id: chg.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		st := got.Msg.GetChange().GetDeployStatus()
		if st.GetState() != v1.DeployStatusState_DEPLOY_STATUS_STATE_DELIVERED || st.GetTransition() != v1.DeployStatusTransition_DEPLOY_STATUS_TRANSITION_HEALTHY {
			t.Fatalf("expected delivered healthy GitLab status, got %+v", st)
		}
		statuses := gl.Statuses()
		if len(statuses) != 3 || statuses[0].State != "pending" || statuses[1].State != "running" || statuses[2].State != "success" {
			t.Fatalf("unexpected GitLab statuses sequence: %+v", statuses)
		}
		if statuses[2].Name != "Dockwell / staging / web" || statuses[2].TargetURL != "https://panel.example.com/changes/"+chg.GetId() || statuses[2].Project != "acme/web" {
			t.Fatalf("unexpected GitLab status target/project/name: %+v", statuses[2])
		}

		// Non-CI change gets no deploy_status and RetryDeployStatus refuses it cleanly.
		if _, err := c.RetryDeployStatus(t.Context(), connect.NewRequest(&v1.RetryDeployStatusRequest{Id: imp.GetId()})); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("RetryDeployStatus on non-CI change = %v, want InvalidArgument", err)
		}
	})
}

// L2: With no public panel domain set, outbound status links omit details_url
// (never leaking PanelListen). Once panel_domain is saved in settings, subsequent
// deploys pick up the public HTTPS domain live without restart.
func TestCIStatusL2DynamicPanelDomainAndNoListenLeak(t *testing.T) {
	t.Parallel()
	gh, err := ghtest.New()
	if err != nil {
		t.Fatal(err)
	}
	gh.SetRepos([]ghtest.Repository{{FullName: "acme/web", Path: "web.git", Private: true, DefaultBranch: "main"}})
	gh.SeedCode("code-1", "dockwell-e2e")
	ghSrv := httptest.NewServer(gh)
	t.Cleanup(ghSrv.Close)

	f := startFullConfig(t, store.SQLite, nil, slog.New(slog.DiscardHandler), nil, logapi.Options{}, func(cfg *Config) {
		cfg.GitHub = GitHubConfig{API: ghSrv.URL, Web: ghSrv.URL, Client: ghSrv.Client()}
		cfg.PanelListen = "127.0.0.1:8080" // Must NOT be leaked!
		cfg.PanelURL = ""
		cfg.ManualCIStatus = true
	})
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c := changeClient(b, b.origin)
	cc := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	s := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
	gp := dockwellv1connect.NewGitProviderServiceClient(b, b.origin, connect.WithProtoJSON())

	connectFakeGitHubApp(t, gp, c, b)
	svc, _ := setupStagingWeb(t, c, cc, s)
	tokSecret, _ := makeDeployToken(t, b, svc.GetId(), "https://github.com/acme/web")
	tokClient := bearer{token: tokSecret, rt: b.client.Transport}
	tokServices := dockwellv1connect.NewServiceServiceClient(tokClient, b.origin, connect.WithProtoJSON())
	tokChanges := changeClient(tokClient, b.origin)

	// Deploy 1: no panel_domain set -> DetailsURL MUST be empty (never 127.0.0.1:8080).
	d1 := "sha256:" + strings.Repeat("1", 64)
	img1 := "ghcr.io/acme/web:1.0@" + d1
	e.Registry[img1] = dockertest.Image{Config: docker.Config{Cmd: []string{"web"}}, Digest: d1}
	p1, err := tokServices.DeployService(t.Context(), connect.NewRequest(&v1.DeployServiceRequest{
		Id: svc.GetId(), Image: img1,
		DeployOrigin: &v1.DeployOrigin{
			Provider: v1.DeployProvider_DEPLOY_PROVIDER_GITHUB_ACTIONS, Repository: "https://github.com/acme/web",
			Commit: strings.Repeat("1", 40), Ref: "refs/heads/main", RunUrl: "https://github.com/acme/web/actions/runs/1",
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	doneChange(t, tokChanges, p1.Msg.GetChange())
	if err := f.server.CIStatus().DeliverOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if runs := gh.CheckRuns(); len(runs) != 1 || runs[0].DetailsURL != "" {
		t.Fatalf("expected empty details_url without public panel domain, got %+v", runs)
	}

	// Set panel_domain dynamically in settings -> Deploy 2 gets https://panel.live.example.com/changes/<id>.
	if err := f.store.Update(t.Context(), func(tx *store.Tx) error {
		return tx.PutSetting(t.Context(), settings.KeyPanelDomain, []byte("panel.live.example.com"))
	}); err != nil {
		t.Fatal(err)
	}
	d2 := "sha256:" + strings.Repeat("2", 64)
	img2 := "ghcr.io/acme/web:2.0@" + d2
	e.Registry[img2] = dockertest.Image{Config: docker.Config{Cmd: []string{"web"}}, Digest: d2}
	p2, err := tokServices.DeployService(t.Context(), connect.NewRequest(&v1.DeployServiceRequest{
		Id: svc.GetId(), Image: img2,
		DeployOrigin: &v1.DeployOrigin{
			Provider: v1.DeployProvider_DEPLOY_PROVIDER_GITHUB_ACTIONS, Repository: "https://github.com/acme/web",
			Commit: strings.Repeat("2", 40), Ref: "refs/heads/main", RunUrl: "https://github.com/acme/web/actions/runs/2",
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	chg2 := doneChange(t, tokChanges, p2.Msg.GetChange())
	if err := f.server.CIStatus().DeliverOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	runs := gh.CheckRuns()
	if len(runs) != 2 || runs[1].DetailsURL != "https://panel.live.example.com/changes/"+chg2.GetId() {
		t.Fatalf("expected dynamic panel_domain in details_url, got %+v", runs)
	}
}

func doneChange(t *testing.T, c dockwellv1connect.ChangeServiceClient, ch *v1.Change) *v1.Change {
	t.Helper()
	got := applied(t, c, ch)
	if got.GetState() != v1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("change %s ended %v: %s", got.GetId(), got.GetState(), got.GetError())
	}
	return got
}

// N6 & N7: A developer in project A cannot bind a deploy token to project B's
// repository (GitHub or GitLab, on create or change); an install admin can
// (with step-up, audited with the repository named). Reach is checked on create
// and change without revealing whether a repository exists. Changing a token's
// repository requires step-up and developer access on the token's app.
func TestCIStatusN6N7TokenRepoOwnershipReachAndStepUp(t *testing.T) {
	t.Parallel()
	eachStoreParallel(t, func(t *testing.T, backend store.Backend) {
		gh, err := ghtest.New()
		if err != nil {
			t.Fatal(err)
		}
		gh.SetRepos([]ghtest.Repository{
			{FullName: "acme/alpha", Path: "alpha.git", Private: true, DefaultBranch: "main"},
			{FullName: "acme/beta", Path: "beta.git", Private: true, DefaultBranch: "main"},
			{FullName: "acme/unclaimed", Path: "unclaimed.git", Private: true, DefaultBranch: "main"},
		})
		gh.SeedCode("code-1", "dockwell-e2e")
		ghSrv := httptest.NewServer(gh)
		t.Cleanup(ghSrv.Close)

		gl := gltest.New()
		gl.SetProjects([]gltest.Project{
			{PathWithNamespace: "acme/alpha", WebURL: "https://gitlab.example.com/acme/alpha"},
			{PathWithNamespace: "acme/beta", WebURL: "https://gitlab.example.com/acme/beta"},
			{PathWithNamespace: "acme/unclaimed", WebURL: "https://gitlab.example.com/acme/unclaimed"},
		})
		glSrv := httptest.NewServer(gl)
		t.Cleanup(glSrv.Close)

		cert, roots := selfSigned(t, "panel.example.com")
		f := startFullConfig(t, backend, &https.Static{Name: "panel.example.com", Cert: cert}, slog.New(slog.DiscardHandler), nil, logapi.Options{}, func(cfg *Config) {
			cfg.GitHub = GitHubConfig{API: ghSrv.URL, Web: ghSrv.URL, Client: ghSrv.Client()}
			cfg.GitLab = GitLabConfig{Base: glSrv.URL, Client: glSrv.Client()}
			cfg.PanelURL = "https://panel.example.com"
			cfg.ManualCIStatus = true
		})
		_, port, _ := net.SplitHostPort(f.https)
		origin := "https://panel.example.com:" + port
		owner := newBrowser(t, origin, f.https, roots)
		link, err := f.auth.CreateClaimLink(t.Context(), "test")
		if err != nil {
			t.Fatal(err)
		}
		owner.claim(t, link.Token)
		f.addDockerServer(t)

		c := changeClient(owner, origin)
		cc := dockwellv1connect.NewComposeServiceClient(owner, origin, connect.WithProtoJSON())
		s := dockwellv1connect.NewServiceServiceClient(owner, origin, connect.WithProtoJSON())
		gp := dockwellv1connect.NewGitProviderServiceClient(owner, origin, connect.WithProtoJSON())
		members := dockwellv1connect.NewMemberServiceClient(owner, origin, connect.WithProtoJSON())
		ownerTokens := dockwellv1connect.NewTokenServiceClient(owner, origin, connect.WithProtoJSON())
		if _, err := members.ConfirmProtections(t.Context(), connect.NewRequest(&v1.ConfirmProtectionsRequest{})); err != nil {
			t.Fatal(err)
		}

		// Connect GitHub App and GitLab connection reaching every project.
		connectFakeGitHubApp(t, gp, c, owner)
		glProp, err := c.ProposeChange(t.Context(), connect.NewRequest(&v1.ProposeChangeRequest{
			Summary: "Connect GitLab",
			Edits: []*v1.Edit{{
				Resource: &v1.ResourceRef{Kind: "gitlab.connection", Name: "corp-gitlab"},
				Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_GitlabConnection{GitlabConnection: &resourcev1.GitLabConnection{
					InstanceUrl: "https://gitlab.example.com",
					Token:       gltest.TokenAPI,
					PathPrefix:  "acme",
				}}},
			}},
		}))
		if err != nil {
			t.Fatal(err)
		}
		doneChange(t, c, glProp.Msg.GetChange())

		// Create project A ("alpha") and project B ("beta") with an app each.
		impA, err := cc.ImportCompose(t.Context(), connect.NewRequest(&v1.ImportComposeRequest{Project: "alpha", Compose: shopFile}))
		if err != nil {
			t.Fatal(err)
		}
		chgA := doneChange(t, c, impA.Msg.GetChange())
		projA := projectOf(chgA)
		scopeA := serviceScope(chgA)

		impB, err := cc.ImportCompose(t.Context(), connect.NewRequest(&v1.ImportComposeRequest{Project: "beta", Compose: shopFile}))
		if err != nil {
			t.Fatal(err)
		}
		chgB := doneChange(t, c, impB.Msg.GetChange())
		scopeB := serviceScope(chgB)

		listedSvcs, err := s.ListServices(t.Context(), connect.NewRequest(&v1.ListServicesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		var svcA, svcB string
		for _, svc := range listedSvcs.Msg.GetServices() {
			switch svc.GetResource().GetScope() {
			case scopeA:
				svcA = svc.GetId()
			case scopeB:
				svcB = svc.GetId()
			}
		}
		if svcA == "" || svcB == "" {
			t.Fatalf("missing svcA=%q or svcB=%q", svcA, svcB)
		}

		// Project B claims acme/beta on both GitHub and GitLab via its deploy tokens.
		makeDeployToken(t, owner, svcB, "https://github.com/acme/beta")
		makeDeployToken(t, owner, svcB, "https://gitlab.example.com/acme/beta")

		// Verify ListGitRepositories(alpha) and ListGitLabProjects(alpha) flag acme/beta as claimed by another project.
		ghList, err := gp.ListGitRepositories(t.Context(), connect.NewRequest(&v1.ListGitRepositoriesRequest{Project: projA}))
		if err != nil {
			t.Fatal(err)
		}
		claimedGH := map[string]bool{}
		for _, r := range ghList.Msg.GetRepositories() {
			claimedGH[r.GetFullName()] = r.GetClaimedByOtherProject()
		}
		if !claimedGH["acme/beta"] || claimedGH["acme/unclaimed"] {
			t.Fatalf("ListGitRepositories ClaimedByOtherProject = %+v, want beta=true unclaimed=false", claimedGH)
		}
		glList, err := gp.ListGitLabProjects(t.Context(), connect.NewRequest(&v1.ListGitLabProjectsRequest{Project: projA}))
		if err != nil {
			t.Fatal(err)
		}
		claimedGL := map[string]bool{}
		for _, p := range glList.Msg.GetProjects() {
			claimedGL[p.GetPathWithNamespace()] = p.GetClaimedByOtherProject()
		}
		if !claimedGL["acme/beta"] || claimedGL["acme/unclaimed"] {
			t.Fatalf("ListGitLabProjects ClaimedByOtherProject = %+v, want beta=true unclaimed=false", claimedGL)
		}

		// Invite a developer on project A alone.
		inviteProof := stepUpFor(t, owner, v1.StepUpAction_STEP_UP_ACTION_INVITE, auth.InviteTarget("dev@example.com", "developer", projA))
		inv, err := members.CreateInvite(t.Context(), connect.NewRequest(&v1.CreateInviteRequest{
			Email: "dev@example.com", Role: v1.Role_ROLE_DEVELOPER, Project: "alpha", Proof: inviteProof,
		}))
		if err != nil {
			t.Fatal(err)
		}
		dev := newBrowser(t, origin, f.https, roots)
		devJoined, err := accept(t, dev, secretOf(t, inv.Msg.GetLink()), "Dev Alpha")
		if err != nil {
			t.Fatal(err)
		}
		devTokens := dockwellv1connect.NewTokenServiceClient(dev, origin, connect.WithProtoJSON())

		// N7: Unreached repository is refused on CreateToken with one non-enumerating line.
		if _, err := devTokens.CreateToken(t.Context(), connect.NewRequest(&v1.CreateTokenRequest{
			Name: "unreached", Scope: v1.TokenScope_TOKEN_SCOPE_DEPLOY, DeployService: svcA,
			DeployRepository: "https://github.com/acme/does-not-exist",
			Proof:            stepUpFor(t, dev, v1.StepUpAction_STEP_UP_ACTION_CREATE_TOKEN, ""),
		})); connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "no GitHub App or GitLab connection connected to this project reaches that repository") {
			t.Fatalf("CreateToken(unreached) = %v, want InvalidArgument reach refusal", err)
		}

		// N6: Developer in project A CANNOT create a deploy token bound to project B's GitHub or GitLab repo.
		for _, forbidden := range []string{"https://github.com/acme/beta", "https://gitlab.example.com/acme/beta"} {
			if _, err := devTokens.CreateToken(t.Context(), connect.NewRequest(&v1.CreateTokenRequest{
				Name: "steal " + forbidden, Scope: v1.TokenScope_TOKEN_SCOPE_DEPLOY, DeployService: svcA,
				DeployRepository: forbidden,
				Proof:            stepUpFor(t, dev, v1.StepUpAction_STEP_UP_ACTION_CREATE_TOKEN, ""),
			})); connect.CodeOf(err) != connect.CodePermissionDenied {
				t.Fatalf("developer CreateToken(%s) = %v, want PermissionDenied", forbidden, err)
			}
		}

		// N6: Developer CAN create a deploy token bound to an unclaimed repo on an image-only app.
		devMade, err := devTokens.CreateToken(t.Context(), connect.NewRequest(&v1.CreateTokenRequest{
			Name: "dev unclaimed", Scope: v1.TokenScope_TOKEN_SCOPE_DEPLOY, DeployService: svcA,
			DeployRepository: "https://github.com/acme/unclaimed",
			Proof:            stepUpFor(t, dev, v1.StepUpAction_STEP_UP_ACTION_CREATE_TOKEN, ""),
		}))
		if err != nil || devMade.Msg.GetToken().GetDeployRepository() != "https://github.com/acme/unclaimed" {
			t.Fatalf("developer CreateToken(unclaimed) = %+v, %v", devMade, err)
		}
		devTokID := devMade.Msg.GetToken().GetId()

		// N7: SetTokenDeployRepository without step-up proof is refused.
		if _, err := devTokens.SetTokenDeployRepository(t.Context(), connect.NewRequest(&v1.SetTokenDeployRepositoryRequest{
			Id: devTokID, DeployRepository: "https://gitlab.example.com/acme/unclaimed",
		})); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("SetTokenDeployRepository without proof = %v, want PermissionDenied", err)
		}

		// N7: SetTokenDeployRepository to an unreached repo is refused.
		if _, err := devTokens.SetTokenDeployRepository(t.Context(), connect.NewRequest(&v1.SetTokenDeployRepositoryRequest{
			Id: devTokID, DeployRepository: "https://gitlab.example.com/other/unreached",
			Proof: stepUpFor(t, dev, v1.StepUpAction_STEP_UP_ACTION_CREATE_TOKEN, ""),
		})); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("SetTokenDeployRepository(unreached) = %v, want InvalidArgument", err)
		}

		// N6: Developer CANNOT rebind their token to project B's GitHub or GitLab repo.
		for _, forbidden := range []string{"https://github.com/acme/beta", "https://gitlab.example.com/acme/beta"} {
			if _, err := devTokens.SetTokenDeployRepository(t.Context(), connect.NewRequest(&v1.SetTokenDeployRepositoryRequest{
				Id: devTokID, DeployRepository: forbidden,
				Proof: stepUpFor(t, dev, v1.StepUpAction_STEP_UP_ACTION_CREATE_TOKEN, ""),
			})); connect.CodeOf(err) != connect.CodePermissionDenied {
				t.Fatalf("developer SetTokenDeployRepository(%s) = %v, want PermissionDenied", forbidden, err)
			}
		}

		// N6: Install admin CAN bind project A's token to project B's GitHub and GitLab repos (create + change).
		adminMade, err := ownerTokens.CreateToken(t.Context(), connect.NewRequest(&v1.CreateTokenRequest{
			Name: "admin override gh", Scope: v1.TokenScope_TOKEN_SCOPE_DEPLOY, DeployService: svcA,
			DeployRepository: "https://github.com/acme/beta",
			Proof:            stepUpFor(t, owner, v1.StepUpAction_STEP_UP_ACTION_CREATE_TOKEN, ""),
		}))
		if err != nil || adminMade.Msg.GetToken().GetDeployRepository() != "https://github.com/acme/beta" {
			t.Fatalf("admin CreateToken(beta GH) = %+v, %v", adminMade, err)
		}
		adminChanged, err := ownerTokens.SetTokenDeployRepository(t.Context(), connect.NewRequest(&v1.SetTokenDeployRepositoryRequest{
			Id: adminMade.Msg.GetToken().GetId(), DeployRepository: "https://gitlab.example.com/acme/beta",
			Proof: stepUpFor(t, owner, v1.StepUpAction_STEP_UP_ACTION_CREATE_TOKEN, ""),
		}))
		if err != nil || adminChanged.Msg.GetToken().GetDeployRepository() != "https://gitlab.example.com/acme/beta" {
			t.Fatalf("admin SetTokenDeployRepository(beta GL) = %+v, %v", adminChanged, err)
		}

		// Verify audit log recorded both repository bindings with the repository URL named.
		var sawCreateAudit, sawSetAudit bool
		if err := f.store.View(t.Context(), func(tx *store.Tx) error {
			evs, err := tx.ListEvents(t.Context(), 100)
			if err != nil {
				return err
			}
			for _, ev := range evs {
				if ev.Action == "token.create" && strings.Contains(ev.Detail, "https://github.com/acme/beta") {
					sawCreateAudit = true
				}
				if ev.Action == "token.deploy_repository" && strings.Contains(ev.Detail, "https://gitlab.example.com/acme/beta") {
					sawSetAudit = true
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if !sawCreateAudit || !sawSetAudit {
			t.Fatalf("missing repository audit details: create=%v set=%v", sawCreateAudit, sawSetAudit)
		}

		// N7: Demoting dev to viewer blocks SetTokenDeployRepository even on unclaimed repos.
		if err := f.store.Update(t.Context(), func(tx *store.Tx) error {
			return tx.SetGrant(t.Context(), store.Grant{
				UserID: devJoined.GetUser().GetId(), ProjectID: projA, Role: store.RoleViewer, CreatedAt: time.Now().UTC(),
			})
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := devTokens.SetTokenDeployRepository(t.Context(), connect.NewRequest(&v1.SetTokenDeployRepositoryRequest{
			Id: devTokID, DeployRepository: "https://gitlab.example.com/acme/unclaimed",
			Proof: stepUpFor(t, dev, v1.StepUpAction_STEP_UP_ACTION_CREATE_TOKEN, ""),
		})); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("demoted viewer SetTokenDeployRepository = %v, want PermissionDenied", err)
		}
	})
}

// N8: ListGitLabProjects caches per connection for 60 s, exposes per-connection
// failure state instead of dropping errors, and signals has_more beyond 100
// projects so server search finds any reachable project.
func TestCIStatusN8GitLabProjectCacheStateAndSearch(t *testing.T) {
	t.Parallel()
	gl := gltest.New()
	many := make([]gltest.Project, 105)
	for i := range many {
		p := fmt.Sprintf("acme/svc-%03d", i+1)
		many[i] = gltest.Project{PathWithNamespace: p, WebURL: "https://gitlab.example.com/" + p}
	}
	gl.SetProjects(many)
	glSrv := httptest.NewServer(gl)
	t.Cleanup(glSrv.Close)

	now := time.Date(2026, 11, 27, 1, 0, 0, 0, time.UTC)
	f := startFullConfig(t, store.SQLite, nil, slog.New(slog.DiscardHandler), nil, logapi.Options{}, func(cfg *Config) {
		cfg.GitHub = GitHubConfig{Now: func() time.Time { return now }}
		cfg.GitLab = GitLabConfig{Base: glSrv.URL, Client: glSrv.Client()}
		cfg.ManualCIStatus = true
	})
	b := signedIn(t, f)
	f.addDockerServer(t)
	c := changeClient(b, b.origin)
	cc := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	gp := dockwellv1connect.NewGitProviderServiceClient(b, b.origin, connect.WithProtoJSON())

	imp, err := cc.ImportCompose(t.Context(), connect.NewRequest(&v1.ImportComposeRequest{Compose: shopFile}))
	if err != nil {
		t.Fatal(err)
	}
	doneChange(t, c, imp.Msg.GetChange())

	glProp, err := c.ProposeChange(t.Context(), connect.NewRequest(&v1.ProposeChangeRequest{
		Summary: "Connect GitLab",
		Edits: []*v1.Edit{{
			Resource: &v1.ResourceRef{Kind: "gitlab.connection", Name: "big-gl"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_GitlabConnection{GitlabConnection: &resourcev1.GitLabConnection{
				InstanceUrl: "https://gitlab.example.com",
				Token:       gltest.TokenAPI,
				PathPrefix:  "acme",
			}}},
		}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	doneChange(t, c, glProp.Msg.GetChange())

	// First call hits GitLab once, returns 100 projects with has_more=true and state=CONNECTED.
	r1, err := gp.ListGitLabProjects(t.Context(), connect.NewRequest(&v1.ListGitLabProjectsRequest{Project: "shop"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(r1.Msg.GetProjects()) != 100 || len(r1.Msg.GetConnections()) != 1 ||
		!r1.Msg.GetConnections()[0].GetHasMore() || r1.Msg.GetConnections()[0].GetState() != v1.GitLabConnectionState_GIT_LAB_CONNECTION_STATE_CONNECTED {
		t.Fatalf("first ListGitLabProjects = len:%d conns:%+v", len(r1.Msg.GetProjects()), r1.Msg.GetConnections())
	}
	if calls := gl.ProjectsCalls(); calls != 1 {
		t.Fatalf("ProjectsCalls after first call = %d, want 1", calls)
	}

	// Second call within 60 s hits the cache without calling GitLab.
	now = now.Add(30 * time.Second)
	r2, err := gp.ListGitLabProjects(t.Context(), connect.NewRequest(&v1.ListGitLabProjectsRequest{Project: "shop"}))
	if err != nil || len(r2.Msg.GetProjects()) != 100 {
		t.Fatalf("cached ListGitLabProjects = %d, %v", len(r2.Msg.GetProjects()), err)
	}
	if calls := gl.ProjectsCalls(); calls != 1 {
		t.Fatalf("ProjectsCalls after cached call = %d, want 1", calls)
	}

	// Server-side search finds project #105 past the first 100.
	rSearch, err := gp.ListGitLabProjects(t.Context(), connect.NewRequest(&v1.ListGitLabProjectsRequest{Project: "shop", Query: "svc-105"}))
	if err != nil || len(rSearch.Msg.GetProjects()) != 1 || rSearch.Msg.GetProjects()[0].GetPathWithNamespace() != "acme/svc-105" {
		t.Fatalf("ListGitLabProjects(svc-105) = %+v, %v", rSearch.Msg.GetProjects(), err)
	}

	// After 61 s the cache expires; if the token is now revoked, ListGitLabProjects surfaces AUTH_FAILED.
	gl.SetToken(gltest.Token{Value: gltest.TokenAPI, Name: "dockwell-ci", Scopes: []string{"api"}, Active: false, Revoked: true})
	now = now.Add(61 * time.Second)
	rFail, err := gp.ListGitLabProjects(t.Context(), connect.NewRequest(&v1.ListGitLabProjectsRequest{Project: "shop"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(rFail.Msg.GetConnections()) != 1 || rFail.Msg.GetConnections()[0].GetState() != v1.GitLabConnectionState_GIT_LAB_CONNECTION_STATE_AUTH_FAILED {
		t.Fatalf("failed connection state = %+v, want AUTH_FAILED", rFail.Msg.GetConnections())
	}
}
