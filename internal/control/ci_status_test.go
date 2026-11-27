// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/github/ghtest"
	"github.com/dockwell/dockwell/internal/control/gitlab/gltest"
	logapi "github.com/dockwell/dockwell/internal/control/logs"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
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

// End-to-end GitHub CI deploy status reporting: missing Checks permission shows
// on Settings/Integrations and causes delivery failure; granting Checks and
// refreshing clears the warning; RetryDeployStatus reports the check run with
// panel change URL; a failing deploy reports failed + rolled_back.
func TestCIStatusEndToEndGitHubAndRollback(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		gh, err := ghtest.New()
		if err != nil {
			t.Fatal(err)
		}
		gh.SetPermissions(map[string]string{"contents": "read", "metadata": "read"})
		gh.SetRepos([]ghtest.Repository{{FullName: "acme/web", Path: "web.git", Private: true, DefaultBranch: "main"}})
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

		// Import a service ("web" -> repo acme/web in ghtest).
		imp, err := cc.ImportCompose(t.Context(), connect.NewRequest(&v1.ImportComposeRequest{Compose: shopFile}))
		if err != nil {
			t.Fatal(err)
		}
		doneChange(t, c, imp.Msg.GetChange())
		svc := onlyService(t, s)

		// Deploy via CI while checks:write is missing -> skipped/failed with reason.
		d1 := "sha256:" + strings.Repeat("1", 64)
		img1 := "ghcr.io/acme/web:1.0@" + d1
		e.Registry[img1] = dockertest.Image{Config: docker.Config{Cmd: []string{"web"}}, Digest: d1}
		planned, err := s.DeployService(t.Context(), connect.NewRequest(&v1.DeployServiceRequest{
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
		chg1 := doneChange(t, c, planned.Msg.GetChange())
		if err := f.server.CIStatus().DeliverOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		got1, err := c.GetChange(t.Context(), connect.NewRequest(&v1.GetChangeRequest{Id: chg1.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		st1 := got1.Msg.GetChange().GetDeployStatus()
		if st1 == nil || st1.GetState() != v1.DeployStatusState_DEPLOY_STATUS_STATE_SKIPPED || !st1.GetCanRetry() || !strings.Contains(st1.GetError(), "Checks permission") {
			t.Fatalf("expected skipped due to Checks permission, got %+v", st1)
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

		// RetryDeployStatus and deliver -> Check Run created with panel change URL.
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
		runs := gh.CheckRuns()
		if len(runs) != 1 || runs[0].Status != "completed" || runs[0].Conclusion != "success" ||
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
		planned2, err := s.DeployService(t.Context(), connect.NewRequest(&v1.DeployServiceRequest{
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
		failed := applied(t, c, planned2.Msg.GetChange())
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
	})
}

// End-to-end GitLab CI deploy status reporting via gitlab.connection resource,
// CheckGitLabConnection verification, path prefix matching, and commit status delivery.
func TestCIStatusEndToEndGitLabConnection(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		gl := gltest.New()
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
		doneChange(t, c, prop.Msg.GetChange())

		// ListGitProviders exposes the GitLab connection without its token.
		listed, err := gp.ListGitProviders(t.Context(), connect.NewRequest(&v1.ListGitProvidersRequest{}))
		if err != nil || len(listed.Msg.GetGitlabConnections()) != 1 || listed.Msg.GetGitlabConnections()[0].GetUrl() != "https://gitlab.example.com" {
			t.Fatalf("ListGitProviders = %+v, %v", listed, err)
		}

		// Re-check saved connection reusing its stored token (token omitted).
		recheck, err := gp.CheckGitLabConnection(t.Context(), connect.NewRequest(&v1.CheckGitLabConnectionRequest{
			Name: "self-managed", Url: "https://gitlab.example.com",
		}))
		if err != nil || recheck.Msg.GetState() != v1.GitLabConnectionState_GIT_LAB_CONNECTION_STATE_CONNECTED {
			t.Fatalf("saved recheck = %+v, %v", recheck, err)
		}

		// Deploy via GitLab CI and verify commit statuses delivered in order.
		imp, err := cc.ImportCompose(t.Context(), connect.NewRequest(&v1.ImportComposeRequest{Compose: shopFile}))
		if err != nil {
			t.Fatal(err)
		}
		doneChange(t, c, imp.Msg.GetChange())
		svc := onlyService(t, s)

		d1 := "sha256:" + strings.Repeat("c", 64)
		img1 := "registry.gitlab.example.com/acme/web:1.0@" + d1
		e.Registry[img1] = dockertest.Image{Config: docker.Config{Cmd: []string{"web"}}, Digest: d1}
		planned, err := s.DeployService(t.Context(), connect.NewRequest(&v1.DeployServiceRequest{
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
		chg := doneChange(t, c, planned.Msg.GetChange())
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
		if statuses[2].TargetURL != "https://panel.example.com/changes/"+chg.GetId() || statuses[2].Project != "acme/web" {
			t.Fatalf("unexpected GitLab status target/project: %+v", statuses[2])
		}

		// Non-CI change gets no deploy_status and RetryDeployStatus refuses it cleanly.
		if _, err := c.RetryDeployStatus(t.Context(), connect.NewRequest(&v1.RetryDeployStatusRequest{Id: imp.Msg.GetChange().GetId()})); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("RetryDeployStatus on non-CI change = %v, want InvalidArgument", err)
		}
	})
}

func doneChange(t *testing.T, c dockwellv1connect.ChangeServiceClient, ch *v1.Change) *v1.Change {
	t.Helper()
	got := applied(t, c, ch)
	if got.GetState() != v1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("change %s ended %v: %s", got.GetId(), got.GetState(), got.GetError())
	}
	return got
}

var _ = http.StatusOK
