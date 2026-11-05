// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func serviceEdit(name, image string, env map[string]string) *dockwellv1.Edit {
	return &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "service", Name: name},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
			Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: image, Env: env}},
		}}},
	}
}

// applied applies a planned change and waits for it to end.
func applied(t *testing.T, c dockwellv1connect.ChangeServiceClient, change *dockwellv1.Change) *dockwellv1.Change {
	t.Helper()
	w := watch(t, c, change.GetId())
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: change.GetId(), Fingerprint: change.GetPlan().GetFingerprint()})); err != nil {
		t.Fatal(err)
	}
	return waitWatch(t, w).last()
}

func onlyService(t *testing.T, s dockwellv1connect.ServiceServiceClient) *dockwellv1.ServiceInfo {
	t.Helper()
	res, err := s.ListServices(t.Context(), connect.NewRequest(&dockwellv1.ListServicesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Msg.GetServices()) != 1 {
		t.Fatalf("ListServices = %v, want one service", res.Msg.GetServices())
	}
	return res.Msg.GetServices()[0]
}

// A service says what it runs and how that went without its secrets, and
// deploys and rolls back as plans.
func TestServicesThroughTheAPI(t *testing.T) { storetest.Each(t, testServicesThroughTheAPI) }

func testServicesThroughTheAPI(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c := changeClient(b, b.origin)
	s := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
	const secret = "s3cret-pw"

	made, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{
		Summary: "Run the shop",
		Edits:   []*dockwellv1.Edit{serviceEdit("shop", "ghcr.io/acme/shop:1.4", map[string]string{"API_KEY": secret})},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := applied(t, c, made.Msg.GetChange()); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the first deploy ended %v: %s", got.GetState(), got.GetError())
	}

	info := onlyService(t, s)
	if strings.Contains(info.String(), secret) {
		t.Error("ListServices carries a secret")
	}
	if got := info.GetSpec().GetApp(); got.GetImage() != "ghcr.io/acme/shop:1.4" {
		t.Errorf("the service runs %q", got.GetImage())
	} else if value, named := got.GetEnv()["API_KEY"]; !named || value != "" {
		t.Errorf("API_KEY is %q, named %t; want it named, with no value", value, named)
	}
	if info.GetRevision() != 1 || info.GetChangeId() != made.Msg.GetChange().GetId() || info.GetRolledBackChangeId() != "" {
		t.Errorf("the service is %v", info)
	}
	if d := info.GetDeployments(); len(d) != 1 || d[0].GetServerName() != "hz-1" || d[0].GetState() != dockwellv1.DeploymentState_DEPLOYMENT_STATE_LIVE {
		t.Errorf("deployments = %v, want live on hz-1", d)
	}

	// Deploying another image keeps the rest, secrets included, though no
	// response ever shows them.
	if _, err := s.DeployService(t.Context(), connect.NewRequest(&dockwellv1.DeployServiceRequest{Id: "svc_nope", Image: "a"})); reasonOf(err) != "not_found" {
		t.Errorf("deploying no service = %v", err)
	}
	deployed, err := s.DeployService(t.Context(), connect.NewRequest(&dockwellv1.DeployServiceRequest{Id: info.GetId(), Image: "ghcr.io/acme/shop:1.5"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := deployed.Msg.GetChange().GetSummary(); got != "Deploy ghcr.io/acme/shop:1.5 to shop" {
		t.Errorf("the deploy's summary is %q", got)
	}
	if got := applied(t, c, deployed.Msg.GetChange()); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the deploy ended %v: %s", got.GetState(), got.GetError())
	}
	ct := e.Container(docker.Project(info.GetResource().GetScope()) + "-shop-1")
	if ct == nil || ct.Config.Image != "ghcr.io/acme/shop:1.5" {
		t.Fatalf("Docker runs %+v", ct)
	}
	if !hasEnv(ct.Config.Env, "API_KEY="+secret) {
		t.Errorf("deploying lost the secret: %v", ct.Config.Env)
	}

	// The history says what each revision changed, who made it and where
	// it went.
	list, err := s.ListServiceRevisions(t.Context(), connect.NewRequest(&dockwellv1.ListServiceRevisionsRequest{Id: info.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	revs := list.Msg.GetRevisions()
	if len(revs) != 2 || revs[0].GetNumber() != 2 || revs[1].GetNumber() != 1 {
		t.Fatalf("revisions = %v", revs)
	}
	if strings.Contains(list.Msg.String(), secret) {
		t.Error("ListServiceRevisions carries a secret")
	}
	if got := revs[0].GetChanges(); len(got) != 1 || got[0].GetPath() != "app.image" || got[0].GetFrom() != "ghcr.io/acme/shop:1.4" || got[0].GetTo() != "ghcr.io/acme/shop:1.5" {
		t.Errorf("r2 changed %v", got)
	}
	if revs[0].GetImage() != "ghcr.io/acme/shop:1.5" || revs[0].GetChange().GetAppliedBy().GetName() == "" || revs[0].GetChange().GetAppliedVia() != dockwellv1.Surface_SURFACE_PANEL {
		t.Errorf("r2 is %v", revs[0])
	}
	if d := revs[1].GetDeployments(); len(d) != 1 || d[0].GetState() != dockwellv1.DeploymentState_DEPLOYMENT_STATE_SUPERSEDED {
		t.Errorf("r1's deployments = %v, want superseded", d)
	}
	page, err := s.ListServiceRevisions(t.Context(), connect.NewRequest(&dockwellv1.ListServiceRevisionsRequest{Id: info.GetId(), PageSize: 1}))
	if err != nil || len(page.Msg.GetRevisions()) != 1 || page.Msg.GetNextPageToken() == "" {
		t.Fatalf("the first page = %v, %v", page, err)
	}
	next, err := s.ListServiceRevisions(t.Context(), connect.NewRequest(&dockwellv1.ListServiceRevisionsRequest{Id: info.GetId(), PageSize: 1, PageToken: page.Msg.GetNextPageToken()}))
	if err != nil || len(next.Msg.GetRevisions()) != 1 || next.Msg.GetRevisions()[0].GetNumber() != 1 || next.Msg.GetNextPageToken() != "" {
		t.Fatalf("the next page = %v, %v", next, err)
	}

	// A rollback is a plan, puts the older spec back with its secret, and
	// marks what it reverts.
	for revision, want := range map[int64]string{2: "on revision 2 already", 9: "no revision 9"} {
		if _, err := s.RollBackService(t.Context(), connect.NewRequest(&dockwellv1.RollBackServiceRequest{Id: info.GetId(), Revision: revision})); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("rolling back to %d = %v, want %q", revision, err, want)
		}
	}
	back, err := s.RollBackService(t.Context(), connect.NewRequest(&dockwellv1.RollBackServiceRequest{Id: info.GetId(), Revision: 1}))
	if err != nil {
		t.Fatal(err)
	}
	if got := back.Msg.GetChange(); got.GetSummary() != "Roll back shop to r1" || got.GetReverts() != deployed.Msg.GetChange().GetId() || got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_PLANNED {
		t.Errorf("the rollback is %v", got)
	}
	if ct := e.Container(docker.Project(info.GetResource().GetScope()) + "-shop-1"); ct.Config.Image != "ghcr.io/acme/shop:1.5" {
		t.Errorf("planning a rollback changed Docker: %v", ct.Config.Image)
	}
	if got := applied(t, c, back.Msg.GetChange()); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the rollback ended %v: %s", got.GetState(), got.GetError())
	}
	ct = e.Container(docker.Project(info.GetResource().GetScope()) + "-shop-1")
	if resource.BareImage(ct.Config.Image) != "ghcr.io/acme/shop:1.4" || !hasEnv(ct.Config.Env, "API_KEY="+secret) {
		t.Errorf("after the rollback Docker runs %v with %v", ct.Config.Image, ct.Config.Env)
	}
	if got := onlyService(t, s); got.GetRevision() != 3 || resource.BareImage(got.GetSpec().GetApp().GetImage()) != "ghcr.io/acme/shop:1.4" {
		t.Errorf("after the rollback the service is %v", got)
	}
}

func hasEnv(env []string, want string) bool { return slices.Contains(env, want) }

// waitFor waits for what it is told to hold, or fails the test saying what.
func waitFor(t *testing.T, what string, holds func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !holds() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Rolling back one service of a stack leaves the rest, and doesn't mark the
// change that made them all as undone.
func TestRollingBackOneServiceOfAStack(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		f.addDockerServer(t)
		c := changeClient(b, b.origin)
		cc := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
		s := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
		stack := func(tag string) string {
			return "services:\n  web:\n    image: ghcr.io/acme/shop:" + tag + "\n  worker:\n    image: ghcr.io/acme/worker:1.0\n"
		}
		applied(t, c, importCompose(t, cc, stack("1.4"), nil, false))
		second := importCompose(t, cc, "services:\n  web:\n    image: ghcr.io/acme/shop:1.5\n  worker:\n    image: postgres:17\n", nil, false)
		applied(t, c, second)
		res, err := s.ListServices(t.Context(), connect.NewRequest(&dockwellv1.ListServicesRequest{}))
		if err != nil || len(res.Msg.GetServices()) != 2 {
			t.Fatalf("ListServices = %v, %v", res, err)
		}
		web := res.Msg.GetServices()[0]
		if web.GetResource().GetName() != "web" {
			web = res.Msg.GetServices()[1]
		}
		back, err := s.RollBackService(t.Context(), connect.NewRequest(&dockwellv1.RollBackServiceRequest{Id: web.GetId(), Revision: 1}))
		if err != nil {
			t.Fatal(err)
		}
		if back.Msg.GetChange().GetReverts() != "" {
			t.Errorf("rolling back one service reverts %s whole", back.Msg.GetChange().GetReverts())
		}
		var names []string
		for _, r := range back.Msg.GetChange().GetPlan().GetResources() {
			names = append(names, r.GetResource().GetName())
		}
		if len(names) != 1 || names[0] != "web" {
			t.Errorf("the rollback touches %v", names)
		}
		if _, err := c.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: back.Msg.GetChange().GetId()})); err != nil {
			t.Fatal(err)
		}
	})
}

// A deploy that fails rolls back on its own, and the list says so.
func TestAFailedDeployShowsAsRolledBack(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		e := f.addDockerServer(t)
		e.Registry["ghcr.io/acme/shop:broken"] = dockertest.Image{Exits: true, Config: docker.Config{Cmd: []string{"shop"}}}
		c := changeClient(b, b.origin)
		s := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
		made, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{
			Summary: "Run the shop", Edits: []*dockwellv1.Edit{serviceEdit("shop", "ghcr.io/acme/shop:1.4", nil)},
		}))
		if err != nil {
			t.Fatal(err)
		}
		applied(t, c, made.Msg.GetChange())
		info := onlyService(t, s)
		deployed, err := s.DeployService(t.Context(), connect.NewRequest(&dockwellv1.DeployServiceRequest{Id: info.GetId(), Image: "ghcr.io/acme/shop:broken"}))
		if err != nil {
			t.Fatal(err)
		}
		got := applied(t, c, deployed.Msg.GetChange())
		if got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_FAILED || got.GetRolledBackBy() == "" {
			t.Fatalf("the deploy ended %v, rolled back by %q", got.GetState(), got.GetRolledBackBy())
		}
		waitFor(t, "the rollback to finish", func() bool {
			after := onlyService(t, s)
			return after.GetRevision() == 3 && len(after.GetDeployments()) == 1 && after.GetDeployments()[0].GetState() == dockwellv1.DeploymentState_DEPLOYMENT_STATE_LIVE
		})
		if after := onlyService(t, s); after.GetRolledBackChangeId() != deployed.Msg.GetChange().GetId() || after.GetSpec().GetApp().GetImage() != "ghcr.io/acme/shop:1.4" {
			t.Errorf("after the rollback the service is %v", after)
		}
		list, err := s.ListServiceRevisions(t.Context(), connect.NewRequest(&dockwellv1.ListServiceRevisionsRequest{Id: info.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		failed := list.Msg.GetRevisions()[1]
		if failed.GetNumber() != 2 || failed.GetDeployments()[0].GetState() != dockwellv1.DeploymentState_DEPLOYMENT_STATE_FAILED || failed.GetDeployments()[0].GetError() == "" {
			t.Errorf("the failed revision is %v", failed)
		}
	})
}

// Checking a Compose file says what it would make, or why it can't, and
// records nothing.
func TestCheckCompose(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		c := changeClient(b, b.origin)
		cc := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
		check := func(file string, vars map[string]string) *dockwellv1.CheckComposeResponse {
			t.Helper()
			res, err := cc.CheckCompose(t.Context(), connect.NewRequest(&dockwellv1.CheckComposeRequest{Compose: file, Variables: vars}))
			if err != nil {
				t.Fatal(err)
			}
			return res.Msg
		}

		if got := check(" \n", nil); got.GetRefusal() != nil || len(got.GetServices()) != 0 {
			t.Errorf("checking nothing = %v", got)
		}

		res := check(shopStack, nil)
		if got := res.GetRefusal().GetMessage(); !strings.Contains(got, "DB_PASSWORD") {
			t.Errorf("checking without DB_PASSWORD = %q", got)
		}
		var required []string
		for _, v := range res.GetVariables() {
			if v.GetRequired() {
				required = append(required, v.GetName())
			}
		}
		if len(required) != 1 || required[0] != "DB_PASSWORD" {
			t.Errorf("the file needs %v, want DB_PASSWORD", required)
		}

		res = check(shopStack, map[string]string{"DB_PASSWORD": "s3cret-pw"})
		if res.GetRefusal() != nil {
			t.Fatalf("checking the stack = %v", res.GetRefusal())
		}
		if strings.Contains(res.String(), "s3cret-pw") {
			t.Error("the check carries a secret")
		}
		var names []string
		for _, s := range res.GetServices() {
			names = append(names, s.GetResource().GetName())
		}
		if len(names) != 3 || len(res.GetVolumes()) != 1 || len(res.GetNetworks()) != 1 {
			t.Errorf("it would make services %v, volumes %v, networks %v", names, res.GetVolumes(), res.GetNetworks())
		}

		res = check("services:\n  web:\n    image: nginx\n    build: .\n", nil)
		if got := res.GetRefusal(); !strings.HasPrefix(got.GetMessage(), "the service web uses `build`") || got.GetLine() != 4 || len(res.GetServices()) != 0 {
			t.Errorf("checking a build = %v", res)
		}
		res = check("services:\n  sh:\n    image: alpine\n    volumes: [\"/:/host\"]\n", nil)
		if got := res.GetRefusal(); !strings.Contains(got.GetMessage(), "the server's whole file system") || got.GetLine() == 0 {
			t.Errorf("checking a mount of / = %v", res)
		}

		all, err := c.ListChanges(t.Context(), connect.NewRequest(&dockwellv1.ListChangesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		for _, ch := range all.Msg.GetChanges() {
			if strings.Contains(ch.GetSummary(), "Compose") {
				t.Errorf("checking recorded a change: %v", ch)
			}
		}
	})
}
