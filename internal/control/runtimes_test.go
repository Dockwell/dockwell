// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// An app from a folder runs on a language's pinned image with the folder
// mounted read-write, which a person approves and a token can't. What it has
// beside its runtime, a port and its environment, stays through a change of
// version, which a token may apply, as it reaches nothing new. An image deploy is
// refused, as is a version Dockwell doesn't offer (ADR 0058).
func TestAnAppFromAFolderFromAPlanToAnotherVersion(t *testing.T) {
	storetest.Each(t, testAnAppFromAFolder)
}

func testAnAppFromAFolder(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	f.probe = connects
	e := f.addServerWith(t, nil)
	pullsAnything(e)
	c := changeClient(b, b.origin)
	services := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
	token := createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
	as := bearer{token: token, rt: b.client.Transport}
	scriptChanges := changeClient(as, b.origin)
	scriptServices := dockwellv1connect.NewServiceServiceClient(as, b.origin, connect.WithProtoJSON())
	root := t.TempDir()

	// The runtimes on offer, each with what it installs and starts with.
	listed, err := websiteClient(b, b.origin).ListWebsiteRuntimes(t.Context(), connect.NewRequest(&dockwellv1.ListWebsiteRuntimesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var node *dockwellv1.WebsiteRuntime
	for _, r := range listed.Msg.GetRuntimes() {
		if r.GetId() == "node" && node == nil {
			node = r
		}
	}
	if node == nil || node.GetLanguage() != "Node.js" || node.GetVersion() != "26" || node.GetPort() != 3000 || node.GetStart() != "npm start" {
		t.Fatalf("ListWebsiteRuntimes lists Node.js as %v", node)
	}
	if got := listed.Msg.GetRuntimes()[0]; got.GetId() != "php" {
		t.Errorf("ListWebsiteRuntimes lists %v first, want PHP as before", got)
	}

	made := propose(t, scriptChanges, "Run shop", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "service", Name: "shop"},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
			Runtime: &resourcev1.Runtime{Language: "node", Folder: root, User: "1000:1000"},
			Runs:    &resourcev1.Service_App{App: &resourcev1.Container{Env: map[string]string{"SESSION_KEY": "s3cret-value"}}},
		}}},
	})
	if !made.GetPlan().GetHostAccess() {
		t.Fatalf("an app from a folder plans no host access: %v", planned(made))
	}
	var named bool
	for _, r := range made.GetPlan().GetResources() {
		for _, a := range r.GetHostAccess() {
			named = named || (a.GetPath() == root && !a.GetReadOnly())
		}
	}
	if !named {
		t.Errorf("the plan doesn't name %s, read-write: %v", root, made.GetPlan().GetResources())
	}
	_, err = scriptChanges.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: made.GetId(), Fingerprint: made.GetPlan().GetFingerprint()}))
	if reasonOf(err) != authapi.ReasonNeedsSession {
		t.Errorf("a token applying an app from a folder = %v, want needs_session", err)
	}
	applyReviewed(t, c, made)

	ctr := runtimeContainer(t, e)
	newest, _ := service.RuntimeOf("node", "")
	if ctr.Config.Image != newest.Image || ctr.Config.User != "1000:1000" || ctr.Config.WorkingDir != "/app" {
		t.Errorf("the app runs %s as %q in %q", ctr.Config.Image, ctr.Config.User, ctr.Config.WorkingDir)
	}
	if want := root + ":/app"; !slices.ContainsFunc(ctr.HostConfig.Binds, func(b string) bool { return b == want || b == want+":rw" }) {
		t.Errorf("the app binds %v, want %s read-write", ctr.HostConfig.Binds, want)
	}
	if cmd := strings.Join(ctr.Config.Cmd, " "); !strings.Contains(cmd, "npm ci") || !strings.HasSuffix(cmd, "exec npm start") {
		t.Errorf("the app runs %q", ctr.Config.Cmd)
	}
	if !slices.Contains(ctr.Config.Env, "PORT=3000") || !slices.Contains(ctr.Config.Env, "SESSION_KEY=s3cret-value") {
		t.Errorf("the app's environment is %v", ctr.Config.Env)
	}
	info := onlyService(t, services)
	if rt := info.GetSpec().GetRuntime(); rt.GetLanguage() != "node" || rt.GetVersion() != "26" || rt.GetPort() != 3000 || rt.GetFolder() != root {
		t.Errorf("ListServices has its runtime as %v, want it as it was written", rt)
	}
	if v, ok := info.GetSpec().GetApp().GetEnv()["SESSION_KEY"]; !ok || v != "" {
		t.Errorf("ListServices shows SESSION_KEY as %q, want its name alone", v)
	}

	// A port of its own, which a change of version keeps.
	port, err := services.AddServicePort(t.Context(), connect.NewRequest(&dockwellv1.AddServicePortRequest{Id: "shop", HostPort: 8081, ContainerPort: 3000}))
	if err != nil {
		t.Fatalf("AddServicePort: %v", err)
	}
	applyReviewed(t, c, port.Msg.GetChange())

	// Its commands and who runs them are its code, as an image is an image app's:
	// a token's change to its start command plans host access, and only a person
	// applies it.
	listedSpec := proto.CloneOf(onlyService(t, services).GetSpec())
	listedSpec.GetRuntime().Start = "node other.js"
	started := propose(t, scriptChanges, "Start shop another way", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "service", Name: "shop"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: listedSpec}},
	})
	if !started.GetPlan().GetHostAccess() {
		t.Errorf("a token's new start command plans no host access: %v", started.GetPlan().GetResources())
	}
	_, err = scriptChanges.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: started.GetId(), Fingerprint: started.GetPlan().GetFingerprint()}))
	if reasonOf(err) != authapi.ReasonNeedsSession {
		t.Errorf("a token applying a new start command = %v, want needs_session", err)
	}
	if _, err := c.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: started.GetId()})); err != nil {
		t.Fatal(err)
	}

	// Another version, which a token may apply.
	moved, err := scriptServices.ChangeServiceRuntime(t.Context(), connect.NewRequest(&dockwellv1.ChangeServiceRuntimeRequest{Id: "shop", Version: "24"}))
	if err != nil {
		t.Fatalf("ChangeServiceRuntime: %v", err)
	}
	ch := moved.Msg.GetChange()
	if ch.GetSummary() != "Run shop on Node.js 24, from 26" {
		t.Errorf("the change is %q", ch.GetSummary())
	}
	if ch.GetPlan().GetHostAccess() {
		t.Errorf("another version of the same image plans host access: %v", ch.GetPlan().GetResources())
	}
	applyReviewed(t, scriptChanges, ch)
	ctr = runtimeContainer(t, e)
	on24, _ := service.RuntimeOf("node", "24")
	if ctr.Config.Image != on24.Image {
		t.Errorf("after the change the app runs %s, want %s", ctr.Config.Image, on24.Image)
	}
	if !slices.Contains(ctr.Config.Env, "SESSION_KEY=s3cret-value") {
		t.Errorf("after the change the app's environment is %v, want SESSION_KEY kept", ctr.Config.Env)
	}
	if len(ctr.HostConfig.PortBindings) == 0 {
		t.Errorf("after the change the app publishes nothing, want the port it had")
	}
	if rt := onlyService(t, services).GetSpec().GetRuntime(); rt.GetVersion() != "24" {
		t.Errorf("after the change its runtime is %v", rt)
	}

	// The newest build of the version it has: nothing to do.
	_, err = services.ChangeServiceRuntime(t.Context(), connect.NewRequest(&dockwellv1.ChangeServiceRuntimeRequest{Id: "shop"}))
	if reasonOf(err) != "invalid" || !strings.Contains(err.Error(), "shop already runs Node.js "+on24.Release+", the newest build of 24") {
		t.Errorf("updating to the build it has = %v", err)
	}
	_, err = services.ChangeServiceRuntime(t.Context(), connect.NewRequest(&dockwellv1.ChangeServiceRuntimeRequest{Id: "shop", Version: "18"}))
	if reasonOf(err) != "invalid" || !strings.Contains(err.Error(), "an app from a folder runs Node.js 26, 24 or 22, not 18") {
		t.Errorf("a version not on offer = %v", err)
	}
	_, err = services.DeployService(t.Context(), connect.NewRequest(&dockwellv1.DeployServiceRequest{Id: "shop", Image: "node:24"}))
	if reasonOf(err) != "invalid" || !strings.Contains(err.Error(), "runs from "+root+" on Node.js 24, so it has no image of its own to deploy") {
		t.Errorf("deploying an image to an app from a folder = %v", err)
	}

	// Its server never has the runtime, so it isn't drift. What's changed by hand
	// beside what the runtime decides is kept with the runtime; a hand-changed
	// image leaves an app that runs an image.
	drift := func() []*dockwellv1.Drift {
		t.Helper()
		res, err := c.ListDrift(t.Context(), connect.NewRequest(&dockwellv1.ListDriftRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.GetDrift()
	}
	if d := drift(); len(d) != 0 {
		t.Fatalf("an app from a folder as it was applied drifts: %v", d)
	}
	name := runtimeContainerName(t, e)
	keep := func(edit func(*docker.Container)) {
		t.Helper()
		e.Edit(name, edit)
		d := drift()
		if len(d) != 1 {
			t.Fatalf("after a change by hand, drift is %v", d)
		}
		adopt, err := c.AdoptDrift(t.Context(), connect.NewRequest(&dockwellv1.AdoptDriftRequest{ServerId: d[0].GetServerId(), ResourceId: d[0].GetResourceId()}))
		if err != nil {
			t.Fatal(err)
		}
		applyReviewed(t, c, adopt.Msg.GetChange())
		if d := drift(); len(d) != 0 {
			t.Errorf("after keeping it, drift is %v", d)
		}
	}
	keep(func(ctr *docker.Container) { ctr.Config.Env = append(ctr.Config.Env, "DEBUG=1") })
	if spec := onlyService(t, services).GetSpec(); spec.GetRuntime().GetVersion() != "24" {
		t.Errorf("keeping a variable added by hand made it %v, want it still on Node.js 24", spec)
	}
	keep(func(ctr *docker.Container) { ctr.Config.Image = "node:24" })
	if spec := onlyService(t, services).GetSpec(); spec.GetRuntime() != nil || spec.GetApp().GetImage() != "node:24" {
		t.Errorf("keeping an image changed by hand made it %v, want an app that runs node:24", spec)
	}
}

// runtimeContainer is the one container the fake engine runs.
func runtimeContainer(t *testing.T, e *dockertest.Engine) *docker.Container {
	t.Helper()
	return e.Container(runtimeContainerName(t, e))
}

func runtimeContainerName(t *testing.T, e *dockertest.Engine) string {
	t.Helper()
	var found string
	for _, name := range e.Containers() {
		if ctr := e.Container(name); ctr != nil && ctr.State.Running {
			if found != "" {
				t.Fatalf("Docker runs more than the app: %v", e.Containers())
			}
			found = name
		}
	}
	if found == "" {
		t.Fatalf("no app runs: %v", e.Containers())
	}
	return found
}
