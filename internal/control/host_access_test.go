// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/store"
)

func deviceEdit(env, name, image string, devices ...string) *dockwellv1.Edit {
	app := &resourcev1.Container{Image: image}
	for _, d := range devices {
		app.Devices = append(app.Devices, &resourcev1.Device{HostPath: d})
	}
	return &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Scope: env, Name: name},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{Runs: &resourcev1.Service_App{App: app}}}},
	}
}

// Host access a person chose stays the app's own: a later change that keeps it
// needs nothing more, as a new image from a developer, and one that widens it goes
// through the same gate, so a developer can't apply it (ADR 0097).
func TestChosenHostAccessIsWidenedOnlyThroughTheGate(t *testing.T) {
	s := newReachStand(t, store.SQLite)
	ctx := t.Context()
	applyConfirmed(t, s.c, propose(t, s.c, "Give api the fuse device", deviceEdit(s.staging, "api", "ghcr.io/acme/shop:1.4", "/dev/fuse")))

	s.hold(in(s.blogID, store.RoleDeveloper))
	wider := propose(t, s.c, "Give api KVM too", deviceEdit(s.staging, "api", "ghcr.io/acme/shop:1.4", "/dev/fuse", "/dev/kvm"))
	if !wider.GetPlan().GetHostAccess() || !strings.Contains(wider.GetPlan().String(), "HOST_ACCESS_KIND_DEVICE") {
		t.Fatalf("a plan widening the devices: %v", wider.GetPlan())
	}
	_, err := s.c.ApplyChange(ctx, connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: wider.GetId(), Fingerprint: wider.GetPlan().GetFingerprint()}))
	needsRole(t, "a developer widening an app's devices", err)

	same := propose(t, s.c, "Deploy api", deviceEdit(s.staging, "api", "ghcr.io/acme/shop:1.5", "/dev/fuse"))
	if same.GetPlan().GetHostAccess() {
		t.Errorf("a new image that keeps the device asks for host access: %v", same.GetPlan())
	}
	if got := apply(t, s.c, same); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Errorf("a developer's deploy that keeps the device ended %v: %s", got.GetState(), got.GetError())
	}
}

// What an app needs of the server is a choice, app by app: an import leaves one
// that needs a device, saying what, as a state with no words of its own, and takes
// it on with that where it's chosen; a container taken on alone is refused the
// same way, and comes with it chosen. Either way it reads back as it runs.
func TestHostAccessIsAChoicePerApp(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c := changeClient(b, b.origin)
	cc := containerClient(b, b.origin)
	device := func(r *docker.CreateRequest) {
		r.HostConfig.PortBindings, r.HostConfig.Binds = nil, nil
		r.HostConfig.Devices = []docker.DeviceMapping{{PathOnHost: "/dev/dri/renderD128", PathInContainer: "/dev/dri/renderD128", CgroupPermissions: "rwm"}}
	}
	runByHand(t, e, "media", device)
	_, err := cc.AdoptContainer(t.Context(), connect.NewRequest(&dockwellv1.AdoptContainerRequest{Id: "media"}))
	if reasonOf(err) != "needs_host_access" || !strings.Contains(err.Error(), "needs host access, /dev/dri/renderD128") {
		t.Errorf("taking on a container with a device, unchosen = %v", err)
	}
	res, err := cc.AdoptContainer(t.Context(), connect.NewRequest(&dockwellv1.AdoptContainerRequest{Id: "media", HostAccess: true}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Msg.GetChange().GetPlan().GetHostAccess() || !strings.Contains(res.Msg.GetChange().GetPlan().String(), "/dev/dri/renderD128") {
		t.Errorf("the plan doesn't show the device: %v", res.Msg.GetChange().GetPlan())
	}
	applyConfirmed(t, c, res.Msg.GetChange())
	drift, err := c.ListDrift(t.Context(), connect.NewRequest(&dockwellv1.ListDriftRequest{}))
	if err != nil || len(drift.Msg.GetDrift()) != 0 {
		t.Errorf("drift after taking it on = %v, %v", drift.Msg.GetDrift(), err)
	}

	// A Compose project the same.
	imports := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	e.AddNetwork(docker.Network{ID: "jfnet", Name: "jf_default", Driver: "bridge"})
	composeContainer(t, e, "jf", "jellyfin", "jf-jellyfin-1", func(r *docker.CreateRequest) {
		r.HostConfig.Devices = []docker.DeviceMapping{{PathOnHost: "/dev/dri/card0", PathInContainer: "/dev/dri/card0", CgroupPermissions: "rwm"}}
		r.HostConfig.SecurityOpt = []string{"seccomp=unconfined"}
		r.HostConfig.Sysctls = map[string]string{"net.ipv4.ip_forward": "1"}
	})
	_, err = imports.AdoptCompose(t.Context(), connect.NewRequest(&dockwellv1.AdoptComposeRequest{Name: "jf", Project: "default"}))
	if err == nil || !strings.Contains(err.Error(), "needs host access, /dev/dri/card0 and seccomp=unconfined") {
		t.Errorf("taking on a project with a device, unchosen = %v", err)
	}
	took, err := imports.AdoptCompose(t.Context(), connect.NewRequest(&dockwellv1.AdoptComposeRequest{Name: "jf", Project: "default", HostAccess: true}))
	if err != nil {
		t.Fatal(err)
	}
	plan := took.Msg.GetChange().GetPlan()
	kinds := []dockwellv1.HostAccessKind{}
	for _, r := range plan.GetResources() {
		for _, a := range r.GetHostAccess() {
			kinds = append(kinds, a.GetKind())
		}
	}
	if !slices.Contains(kinds, dockwellv1.HostAccessKind_HOST_ACCESS_KIND_DEVICE) || !slices.Contains(kinds, dockwellv1.HostAccessKind_HOST_ACCESS_KIND_SECURITY_OPTION) {
		t.Errorf("the plan's host access is %v", kinds)
	}
	applyConfirmed(t, c, took.Msg.GetChange())
	drift, err = c.ListDrift(t.Context(), connect.NewRequest(&dockwellv1.ListDriftRequest{}))
	if err != nil || len(drift.Msg.GetDrift()) != 0 {
		t.Errorf("drift after taking the project on = %v, %v", drift.Msg.GetDrift(), err)
	}
}

// An import from Coolify leaves an app that needs a device, as a state and what it
// needs, with no words of its own under it, and takes it on with that once it's
// chosen, by its ID.
func TestAnImportTakesOnHostAccessWhereChosen(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	runByHand(t, e, "coolify", func(r *docker.CreateRequest) { r.HostConfig.PortBindings, r.HostConfig.Binds = nil, nil })
	e.AddNetwork(docker.Network{ID: "j6r2net", Name: "j6r2_default", Driver: "bridge"})
	composeContainer(t, e, "j6r2", "jellyfin", "jellyfin-j6r2", func(r *docker.CreateRequest) {
		r.Labels["coolify.managed"], r.Labels["coolify.type"], r.Labels["coolify.serviceUuid"], r.Labels["coolify.resourceName"] = "true", "service", "j6r2", "media"
		r.Labels[docker.LabelWorkingDir] = "/data/coolify/services/j6r2"
		r.HostConfig.Devices = []docker.DeviceMapping{{PathOnHost: "/dev/dri/renderD128", PathInContainer: "/dev/dri/renderD128", CgroupPermissions: "rwm"}}
	})
	ic := importClient(b)
	read := func(chosen ...string) *dockwellv1.ImportStep {
		t.Helper()
		res, err := ic.ImportCoolify(t.Context(), connect.NewRequest(&dockwellv1.ImportCoolifyRequest{ServerId: f.lastDocker.id.ServerID, SkipPlan: true, HostAccess: chosen}))
		if err != nil {
			t.Fatal(err)
		}
		return panelSteps(res.Msg)["IMPORT_KIND_APP media"]
	}
	step := read()
	if step.GetAction() != dockwellv1.ImportAction_IMPORT_ACTION_LEAVE || step.GetReason() != "" || step.GetAppId() != "j6r2" ||
		!slices.Equal(step.GetNeedsHostAccess(), []string{"/dev/dri/renderD128"}) {
		t.Errorf("unchosen, media reads %v", step)
	}
	if step = read("j6r2"); step.GetAction() != dockwellv1.ImportAction_IMPORT_ACTION_TAKE_ON || !slices.Equal(step.GetNeedsHostAccess(), []string{"/dev/dri/renderD128"}) {
		t.Errorf("chosen, media reads %v", step)
	}
}
