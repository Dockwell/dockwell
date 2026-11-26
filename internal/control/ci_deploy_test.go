// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func TestCIDeployKeySurvivesAnApplyingControlPlaneRestart(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		b := newBench(t, backend)
		first := b.file(t, &v1.ImportComposeRequest{Compose: shopFile})
		b.done(t, first)
		b.addEnvironment(t, projectOf(first), "staging")
		stage := b.file(t, &v1.ImportComposeRequest{Project: "shop", Environment: "staging", Compose: shopFile})
		b.done(t, stage)

		staging, production := serviceScope(stage), serviceScope(first)
		listed, err := b.s.ListServices(t.Context(), connect.NewRequest(&v1.ListServicesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		apps := map[string]string{}
		for _, app := range listed.Msg.GetServices() {
			apps[app.GetResource().GetScope()] = app.GetId()
		}
		app := apps[staging]
		if app == "" || apps[production] == "" {
			t.Fatalf("expected one app in each environment, got %v", apps)
		}

		tokens := dockwellv1connect.NewTokenServiceClient(b.b, b.b.origin)
		made, err := tokens.CreateToken(t.Context(), connect.NewRequest(&v1.CreateTokenRequest{
			Name: "CI deploy", Scope: v1.TokenScope_TOKEN_SCOPE_DEPLOY, DeployService: app,
			Proof: stepUpFor(t, b.b, v1.StepUpAction_STEP_UP_ACTION_CREATE_TOKEN, ""),
		}))
		if err != nil {
			t.Fatal(err)
		}
		token := made.Msg.GetToken()
		if token.GetDeployEnvironment() != staging || token.GetDeployService() != app ||
			token.GetRole() != v1.TokenRole_TOKEN_ROLE_DEVELOPER || len(token.GetProjects()) != 1 {
			t.Fatalf("unexpected deploy token: %v", token)
		}
		client := bearer{token: made.Msg.GetSecret(), rt: b.b.client.Transport}
		deploys := dockwellv1connect.NewServiceServiceClient(client, b.b.origin)
		changes := dockwellv1connect.NewChangeServiceClient(client, b.b.origin)
		if _, err := deploys.ListServices(t.Context(), connect.NewRequest(&v1.ListServicesRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("deploy token listing services: %v", err)
		}
		if _, err := changes.GetChange(t.Context(), connect.NewRequest(&v1.GetChangeRequest{Id: stage.GetId()})); connect.CodeOf(err) != connect.CodeNotFound {
			t.Fatalf("deploy token reading another change: %v", err)
		}

		digest := "sha256:" + strings.Repeat("a", 64)
		image := "ghcr.io/acme/shop:1.5@" + digest
		b.e.Registry[image] = dockertest.Image{Config: docker.Config{Cmd: []string{"shop"}}, Digest: digest}
		request := func(svc string) *connect.Request[v1.DeployServiceRequest] {
			return connect.NewRequest(&v1.DeployServiceRequest{Id: svc, Image: image, DeployOrigin: &v1.DeployOrigin{
				Repository: "https://github.com/acme/shop", Commit: strings.Repeat("b", 40), Ref: "refs/heads/main",
				RunUrl: "https://github.com/acme/shop/actions/runs/42", Provider: v1.DeployProvider_DEPLOY_PROVIDER_GITHUB_ACTIONS,
			}})
		}
		if _, err := deploys.DeployService(t.Context(), request(apps[production])); connect.CodeOf(err) != connect.CodeNotFound {
			t.Fatalf("deploy token reaching another environment: %v", err)
		}

		// The first two requests may plan concurrently. The transaction that
		// records the plan claims the key, and both callers read that same plan.
		var planned [2]*v1.Change
		var errs [2]error
		var group sync.WaitGroup
		for i := range planned {
			group.Go(func() {
				res, err := deploys.DeployService(t.Context(), request(app))
				errs[i] = err
				if err == nil {
					planned[i] = res.Msg.GetChange()
				}
			})
		}
		group.Wait()
		if errs[0] != nil || errs[1] != nil || planned[0].GetId() == "" || planned[0].GetId() != planned[1].GetId() ||
			planned[0].GetDeployOrigin().GetDigest() != digest || planned[0].GetDeployOrigin().GetImage() != image {
			t.Fatalf("concurrent requests returned %v, %v, with errors %v", planned[0], planned[1], errs)
		}

		started := make(chan struct{}, 1)
		var once sync.Once
		b.e.Intercept(func(r *http.Request) error {
			if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/images/create") {
				block := false
				once.Do(func() { block = true })
				if block {
					started <- struct{}{}
					<-r.Context().Done()
					return r.Context().Err()
				}
			}
			return nil
		})
		changeID := planned[0].GetId()
		if _, err := changes.ApplyChange(t.Context(), connect.NewRequest(&v1.ApplyChangeRequest{Id: changeID, Fingerprint: planned[0].GetPlan().GetFingerprint()})); err != nil {
			t.Fatal(err)
		}
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatal("CI deploy never began pulling the image")
		}
		_, letIn := b.f.restart(t, func(*Server) {})
		waitFor(t, "CI deploy retry after the control plane restarted", func() bool {
			res, err := deploys.DeployService(t.Context(), request(app))
			return err == nil && res.Msg.GetChange().GetId() == changeID &&
				res.Msg.GetChange().GetState() == v1.ChangeState_CHANGE_STATE_APPLYING
		})
		letIn()
		waitFor(t, "the app's agent to reconnect", func() bool {
			_, ok := b.f.agents.Agent(b.f.lastDocker.id.ServerID)
			return ok
		})
		if err := b.f.engine.Resume(t.Context()); err != nil {
			t.Fatal(err)
		}
		res, err := changes.GetChange(t.Context(), connect.NewRequest(&v1.GetChangeRequest{Id: changeID}))
		if err != nil || res.Msg.GetChange().GetState() != v1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("the resumed CI deploy: %v, %v", res, err)
		}

		history, err := b.s.ListServiceRevisions(t.Context(), connect.NewRequest(&v1.ListServiceRevisionsRequest{Id: app}))
		if err != nil {
			t.Fatal(err)
		}
		revisions := history.Msg.GetRevisions()
		if len(revisions) != 2 || revisions[0].GetChange().GetId() != changeID || len(revisions[0].GetDeployments()) != 1 ||
			revisions[0].GetDeployments()[0].GetDeployOrigin().GetDigest() != digest {
			t.Fatalf("unexpected deployment history: %v", revisions)
		}
	})
}
