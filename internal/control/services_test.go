// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"log/slog"
	"maps"
	"os"
	"path/filepath"
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

// One row that won't open used to fail the whole list, with "try again", so a
// damaged service took every app out of the panel, the ones to fix it from
// included. It costs that row, which says which it is and what to do, and the
// log has it too. A row in the clear goes the same way.
func TestARowItWontReadCostsOneServiceAndNotTheList(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		t.Run("sealed for another row", func(t *testing.T) {
			testARowItWontReadCostsOneService(t, backend, func(other string) string {
				return `(SELECT r.spec FROM revisions r WHERE r.resource_id = '` + other + `')`
			}, "Restore the database and the key file from the same backup")
		})
		t.Run("not sealed", func(t *testing.T) {
			spec := `x'0a0a'`
			if backend == store.Postgres {
				spec = `'\x0a0a'`
			}
			testARowItWontReadCostsOneService(t, backend, func(string) string { return spec },
				"Restore the database from a backup. On a development install, start again with an empty data directory")
		})
	})
}

func testARowItWontReadCostsOneService(t *testing.T, backend store.Backend, spec func(other string) string, fix string) {
	logs := &syncBuffer{}
	f := startFullLogging(t, backend, nil, slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelError})))
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	e.Registry["ghcr.io/acme/web:1.0"] = dockertest.Image{}
	e.Registry["ghcr.io/acme/api:1.0"] = dockertest.Image{}
	c := changeClient(b, b.origin)
	s := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
	applied(t, c, propose(t, c, "Run web", serviceEdit("web", "ghcr.io/acme/web:1.0", nil)))
	applied(t, c, propose(t, c, "Run api", serviceEdit("api", "ghcr.io/acme/api:1.0", nil)))
	list := func() map[string]*dockwellv1.ServiceInfo {
		t.Helper()
		res, err := s.ListServices(t.Context(), connect.NewRequest(&dockwellv1.ListServicesRequest{}))
		if err != nil {
			t.Fatalf("ListServices with a row it won't read = %v, want the others listed", err)
		}
		out := map[string]*dockwellv1.ServiceInfo{}
		for _, info := range res.Msg.GetServices() {
			out[info.GetResource().GetName()] = info
		}
		return out
	}
	before := list()
	web, api := before["web"], before["api"]
	if web == nil || api == nil || web.GetSpecUnreadable() != "" {
		t.Fatalf("before anything is damaged, ListServices = %v", before)
	}

	drop := `DROP TRIGGER revisions_never_change`
	if backend == store.Postgres {
		drop += ` ON revisions`
	}
	storetest.Exec(t, f.db, drop)
	storetest.Exec(t, f.db, `UPDATE revisions SET spec = `+spec(api.GetId())+` WHERE resource_id = '`+web.GetId()+`'`)

	after := list()
	got := after["web"]
	if got == nil || !strings.Contains(got.GetSpecUnreadable(), fix) || !strings.Contains(got.GetSpecUnreadable(), web.GetId()) {
		t.Fatalf("the damaged row = %v, want it saying which it is and what to do", got)
	}
	if got.GetSpec() != nil {
		t.Errorf("the damaged row has a spec: %v", got.GetSpec())
	}
	// What the row needs for a person to find it stands: it isn't read from the spec.
	if got.GetRevision() != web.GetRevision() || len(got.GetDeployments()) != 1 || got.GetChangeId() != web.GetChangeId() {
		t.Errorf("the damaged row lost what it knows: %v", got)
	}
	if other := after["api"]; other == nil || other.GetSpecUnreadable() != "" || other.GetSpec().GetApp().GetImage() != "ghcr.io/acme/api:1.0" {
		t.Errorf("the row beside it = %v", other)
	}
	if !strings.Contains(logs.String(), fix) {
		t.Errorf("after listing, the log doesn't say what to do: %s", logs.String())
	}
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
	ct := e.Serving(docker.Project(info.GetResource().GetScope()) + "-shop-1")
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
	if ct := e.Serving(docker.Project(info.GetResource().GetScope()) + "-shop-1"); ct.Config.Image != "ghcr.io/acme/shop:1.5" {
		t.Errorf("planning a rollback changed Docker: %v", ct.Config.Image)
	}
	if got := applied(t, c, back.Msg.GetChange()); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the rollback ended %v: %s", got.GetState(), got.GetError())
	}
	ct = e.Serving(docker.Project(info.GetResource().GetScope()) + "-shop-1")
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
		failedStep := got.GetSteps()[slices.IndexFunc(got.GetSteps(), func(s *dockwellv1.StepProgress) bool {
			return s.GetState() == dockwellv1.StepState_STEP_STATE_FAILED
		})]
		if failedStep.GetReason() != resource.ReasonExits || failedStep.GetLost() {
			t.Errorf("the step that failed comes down to %q, lost %v", failedStep.GetReason(), failedStep.GetLost())
		}
		waitFor(t, "the rollback to finish", func() bool {
			after := onlyService(t, s)
			return after.GetRevision() == 3 && len(after.GetDeployments()) == 1 && after.GetDeployments()[0].GetState() == dockwellv1.DeploymentState_DEPLOYMENT_STATE_LIVE
		})
		if after := onlyService(t, s); after.GetRolledBackChangeId() != deployed.Msg.GetChange().GetId() || after.GetSpec().GetApp().GetImage() != "ghcr.io/acme/shop:1.4" {
			t.Errorf("after the rollback the service is %v", after)
		}
		// The API says how it went, and what it put back, for a panel and a CLI that won't guess:
		// nothing, as this one rolled, so what ran never stopped.
		read, err := c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: deployed.Msg.GetChange().GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		status := read.Msg.GetChange().GetRollbackStatus()
		if status.GetId() != got.GetRolledBackBy() || status.GetState() != dockwellv1.RollbackState_ROLLBACK_STATE_APPLIED || status.GetSteps() != 1 || len(status.GetRestored()) != 0 {
			t.Errorf("the rollback's status is %v", status)
		}
		list, err := s.ListServiceRevisions(t.Context(), connect.NewRequest(&dockwellv1.ListServiceRevisionsRequest{Id: info.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		// A list has a plan in brief, which still says what it is for.
		if got := list.Msg.GetRevisions()[1].GetChange().GetPlan().GetIntent(); got.GetKind() != dockwellv1.IntentKind_INTENT_KIND_DEPLOY {
			t.Errorf("in a list, the deploy is for %v", got)
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

		res = check("services:\n  web:\n    user: a\n    build: .\n", nil)
		if got := res.GetRefusal(); !strings.HasPrefix(got.GetMessage(), "the service web builds an image") || got.GetLine() != 4 || len(res.GetServices()) != 0 {
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

// A change says what it is for when the method that proposed it knows, and that
// stays on its record. A plan of plain edits says nothing of the kind.
func TestAChangeSaysWhatItIsFor(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		f.addDockerServer(t)
		c := changeClient(b, b.origin)
		s := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
		made, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{
			Summary: "Run the shop", Edits: []*dockwellv1.Edit{serviceEdit("shop", "ghcr.io/acme/shop:1.4", nil)},
		}))
		if err != nil {
			t.Fatal(err)
		}
		if got := made.Msg.GetChange().GetPlan().GetIntent(); got != nil {
			t.Errorf("a plan of plain edits is for %v", got)
		}
		applied(t, c, made.Msg.GetChange())
		info := onlyService(t, s)

		deployed, err := s.DeployService(t.Context(), connect.NewRequest(&dockwellv1.DeployServiceRequest{Id: info.GetId(), Image: "ghcr.io/acme/shop:1.5"}))
		if err != nil {
			t.Fatal(err)
		}
		got := deployed.Msg.GetChange().GetPlan().GetIntent()
		if got.GetKind() != dockwellv1.IntentKind_INTENT_KIND_DEPLOY || got.GetImage() != "ghcr.io/acme/shop:1.5" || got.GetResourceId() != info.GetId() {
			t.Errorf("a deploy is for %v", got)
		}
		// It says so whoever reads it, and it comes through to applying it.
		read, err := c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: deployed.Msg.GetChange().GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		if read.Msg.GetChange().GetPlan().GetIntent().GetKind() != dockwellv1.IntentKind_INTENT_KIND_DEPLOY {
			t.Errorf("read back, a deploy is for %v", read.Msg.GetChange().GetPlan().GetIntent())
		}
		if done := applied(t, c, deployed.Msg.GetChange()); done.GetPlan().GetIntent().GetKind() != dockwellv1.IntentKind_INTENT_KIND_DEPLOY {
			t.Errorf("applied, a deploy is for %v", done.GetPlan().GetIntent())
		}

		// The same tag again is a pull, and may come to nothing.
		repull, err := s.DeployService(t.Context(), connect.NewRequest(&dockwellv1.DeployServiceRequest{Id: info.GetId(), Image: "ghcr.io/acme/shop:1.5"}))
		if err != nil {
			t.Fatal(err)
		}
		if got := repull.Msg.GetChange().GetPlan().GetIntent(); got.GetKind() != dockwellv1.IntentKind_INTENT_KIND_REPULL || got.GetImage() != "ghcr.io/acme/shop:1.5" {
			t.Errorf("deploying the same tag again is for %v", got)
		}
		if _, err := c.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: repull.Msg.GetChange().GetId()})); err != nil {
			t.Fatal(err)
		}

		port, err := s.AddServicePort(t.Context(), connect.NewRequest(&dockwellv1.AddServicePortRequest{Id: info.GetId(), HostPort: 8080, ContainerPort: 80}))
		if err != nil {
			t.Fatal(err)
		}
		got = port.Msg.GetChange().GetPlan().GetIntent()
		if got.GetKind() != dockwellv1.IntentKind_INTENT_KIND_PUBLISH_PORT || got.GetPort() != 8080 || got.GetResourceId() != info.GetId() {
			t.Errorf("adding a port is for %v", got)
		}
		if _, err := c.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: port.Msg.GetChange().GetId()})); err != nil {
			t.Fatal(err)
		}

		back, err := s.RollBackService(t.Context(), connect.NewRequest(&dockwellv1.RollBackServiceRequest{Id: info.GetId(), Revision: 1}))
		if err != nil {
			t.Fatal(err)
		}
		got = back.Msg.GetChange().GetPlan().GetIntent()
		if got.GetKind() != dockwellv1.IntentKind_INTENT_KIND_ROLL_BACK || got.GetRevision() != 1 || got.GetResourceId() != info.GetId() {
			t.Errorf("a rollback is for %v", got)
		}
	})
}

// A plan says which of what a change reaches is as good as root: all of it needs
// a signed-in person, and only that asks them for more than a click.
func TestAPlanSaysWhatIsAsGoodAsRoot(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		e := f.addDockerServer(t)
		e.Registry["nginx:1.27"] = dockertest.Image{Config: docker.Config{Cmd: []string{"nginx"}}}
		c := changeClient(b, b.origin)
		edit := serviceEdit("site", "nginx:1.27", nil)
		edit.GetSpec().GetService().GetApp().Mounts = []*resourcev1.Mount{
			{Source: &resourcev1.Mount_HostPath{HostPath: "/srv/site/html"}, Target: "/usr/share/nginx/html", ReadOnly: true},
			{Source: &resourcev1.Mount_HostPath{HostPath: "/var/run/docker.sock"}, Target: "/var/run/docker.sock"},
			{Source: &resourcev1.Mount_HostPath{HostPath: "/etc"}, Target: "/host-etc", ReadOnly: true},
		}
		made, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Run the site", Edits: []*dockwellv1.Edit{edit}}))
		if err != nil {
			t.Fatal(err)
		}
		plan := made.Msg.GetChange().GetPlan()
		if !plan.GetHostAccess() {
			t.Error("the plan doesn't say it reaches the server")
		}
		roots := map[string]bool{}
		for _, r := range plan.GetResources() {
			for _, a := range r.GetHostAccess() {
				roots[a.GetPath()] = a.GetSameAsRoot()
			}
		}
		if want := map[string]bool{"/srv/site/html": false, "/var/run/docker.sock": true, "/etc": true}; !maps.Equal(roots, want) {
			t.Errorf("what it reaches is %v, want %v", roots, want)
		}
	})
}

// A symlink on the server reads as what it leads to, from the agent, over its
// stream, in the plan the API gives: a person sees the alarm /etc would raise.
func TestAPlanReadsWhereASymlinkLeads(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		f.addDockerServer(t)
		c := changeClient(b, b.origin)
		dir := t.TempDir()
		link, site := filepath.Join(dir, "link"), filepath.Join(dir, "site")
		if err := os.Symlink("/etc", link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/run/docker.sock", site); err != nil {
			t.Fatal(err)
		}
		edit := serviceEdit("site", "ghcr.io/acme/shop:1.4", nil)
		edit.GetSpec().GetService().GetApp().Mounts = []*resourcev1.Mount{
			{Source: &resourcev1.Mount_HostPath{HostPath: link}, Target: "/a", ReadOnly: true},
			{Source: &resourcev1.Mount_HostPath{HostPath: site}, Target: "/b", ReadOnly: true},
		}
		made, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Run the site", Edits: []*dockwellv1.Edit{edit}}))
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]*dockwellv1.HostAccess{}
		for _, s := range made.Msg.GetChange().GetPlan().GetServers() {
			for _, st := range s.GetSteps() {
				for _, a := range st.GetHostAccess() {
					got[a.GetPath()] = a
				}
			}
		}
		if a := got[link]; a == nil || !a.GetSameAsRoot() || a.GetLeadsTo() != "/etc" || a.GetKind() != dockwellv1.HostAccessKind_HOST_ACCESS_KIND_SERVER_PATH {
			t.Errorf("a symlink to /etc reads %v", a)
		}
		if a := got[site]; a == nil || !a.GetSameAsRoot() || a.GetLeadsTo() != "/run/docker.sock" || a.GetKind() != dockwellv1.HostAccessKind_HOST_ACCESS_KIND_DOCKER_SOCKET {
			t.Errorf("a symlink to Docker's socket reads %v", a)
		}
	})
}
