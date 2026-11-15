// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent/onepanel/onepaneltest"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/store"
)

// deviceEdit gives api the image and devices.
func deviceEdit(env, image string, devices ...string) *dockwellv1.Edit {
	app := &resourcev1.Container{Image: image}
	for _, d := range devices {
		app.Devices = append(app.Devices, &resourcev1.Device{HostPath: d})
	}
	return &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Scope: env, Name: "api"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{Runs: &resourcev1.Service_App{App: app}}}},
	}
}

// Host access a person chose stays the app's own: a later change that keeps it
// needs nothing more, as a new image from a developer, and one that widens it goes
// through the same gate, so a developer can't apply it (ADR 0097).
func TestChosenHostAccessIsWidenedOnlyThroughTheGate(t *testing.T) {
	s := newReachStand(t, store.SQLite)
	ctx := t.Context()
	applyConfirmed(t, s.c, propose(t, s.c, "Give api the fuse device", deviceEdit(s.staging, "ghcr.io/acme/shop:1.4", "/dev/fuse")))

	s.hold(in(s.blogID, store.RoleDeveloper))
	wider := propose(t, s.c, "Give api KVM too", deviceEdit(s.staging, "ghcr.io/acme/shop:1.4", "/dev/fuse", "/dev/kvm"))
	if !wider.GetPlan().GetHostAccess() || !strings.Contains(wider.GetPlan().String(), "HOST_ACCESS_KIND_DEVICE") {
		t.Fatalf("a plan widening the devices: %v", wider.GetPlan())
	}
	_, err := s.c.ApplyChange(ctx, connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: wider.GetId(), Fingerprint: wider.GetPlan().GetFingerprint()}))
	needsRole(t, "a developer widening an app's devices", err)

	same := propose(t, s.c, "Deploy api", deviceEdit(s.staging, "ghcr.io/acme/shop:1.5", "/dev/fuse"))
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

// A device the agent finds is a disk is the same as root wherever a plan reads it
// (ADR 0097): a developer's deploy of another tag that keeps it is an admin's, as
// for anything as good as root (ADR 0048), and one that wasn't a disk when it was
// approved and is now counts as new access, however little else changes.
func TestADeviceThatIsADiskIsRootThroughout(t *testing.T) {
	root := map[string]bool{"/dev/sdb": true}
	s := newReachStandWith(t, store.SQLite, func(f *fullServer) {
		f.devices = func(p string) (bool, error) { return root[p], nil }
	})
	ctx := t.Context()
	disk := propose(t, s.c, "Give api the disk", deviceEdit(s.staging, "ghcr.io/acme/shop:1.4", "/dev/sdb", "/dev/serial/by-id/usb-stick"))
	rootIn := func(p *dockwellv1.Plan, path string) bool {
		return regexp.MustCompile(`path:"` + regexp.QuoteMeta(path) + `"\s+same_as_root:true`).MatchString(p.String())
	}
	if !rootIn(disk.GetPlan(), "/dev/sdb") {
		t.Fatalf("the plan doesn't read the disk as root: %v", disk.GetPlan())
	}
	applyIt := func(ch *dockwellv1.Change) error {
		proof := stepUpFor(t, s.b, dockwellv1.StepUpAction_STEP_UP_ACTION_APPLY_ROOT_CHANGE, ch.GetId())
		_, err := s.c.ApplyChange(ctx, connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint(), Proof: proof}))
		return err
	}
	w := watch(t, s.c, disk.GetId())
	if err := applyIt(disk); err != nil {
		t.Fatal(err)
	}
	if got := waitWatch(t, w).last(); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the change ended %v: %s", got.GetState(), got.GetError())
	}

	s.hold(in(s.blogID, store.RoleDeveloper))
	deploy := propose(t, s.c, "Deploy api", deviceEdit(s.staging, "ghcr.io/acme/shop:1.5", "/dev/sdb", "/dev/serial/by-id/usb-stick"))
	if deploy.GetPlan().GetHostAccess() {
		t.Errorf("a deploy that keeps what was approved gains host access: %v", deploy.GetPlan())
	}
	_, err := s.c.ApplyChange(ctx, connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: deploy.GetId(), Fingerprint: deploy.GetPlan().GetFingerprint()}))
	needsRole(t, "a developer deploying another tag of what keeps a disk", err)

	// The stick's link now leads to a disk.
	s.hold(onInstall(store.RoleAdmin))
	root["/dev/serial/by-id/usb-stick"] = true
	again := propose(t, s.c, "Deploy api", deviceEdit(s.staging, "ghcr.io/acme/shop:1.6", "/dev/sdb", "/dev/serial/by-id/usb-stick"))
	if !again.GetPlan().GetHostAccess() || !rootIn(again.GetPlan(), "/dev/serial/by-id/usb-stick") {
		t.Errorf("a device that came to be a disk isn't new: %v", again.GetPlan())
	}
}

// A Swarm-run app made again is a choice the same as a take-on: what Swarm gave its
// task's container of the server is left, as a state, until the app is chosen, by
// its ID (the review's L1).
func TestASwarmAppsHostAccessIsAChoiceToo(t *testing.T) {
	d := dokployShop(t)
	d.e.Edit(d.task, func(c *docker.Container) { c.HostConfig.SecurityOpt = []string{"seccomp=unconfined"} })
	read := func(chosen ...string) *dockwellv1.ImportStep {
		t.Helper()
		res, err := d.ic.ImportDokploy(t.Context(), connect.NewRequest(&dockwellv1.ImportDokployRequest{ServerId: d.id, SkipPlan: true, HostAccess: chosen}))
		if err != nil {
			t.Fatal(err)
		}
		return panelSteps(res.Msg)["IMPORT_KIND_APP shop"]
	}
	step := read()
	if step.GetAction() != dockwellv1.ImportAction_IMPORT_ACTION_LEAVE || step.GetReason() != "" || step.GetAppId() != "shop-k2m4" ||
		!slices.Equal(step.GetNeedsHostAccess(), []string{"seccomp=unconfined"}) {
		t.Errorf("unchosen, shop reads %v", step)
	}
	if step = read("shop-k2m4"); step.GetAction() != dockwellv1.ImportAction_IMPORT_ACTION_MAKE {
		t.Errorf("chosen, shop reads %v", step)
	}
}

// 1Panel's import has no host access to choose, so an app that needs some is left
// with the take-on that chooses it (the review of ADR 0097, L9).
func TestA1PanelAppWithHostAccessIsLeftWithTheFix(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	f.onePanelRoot = onepaneltest.Make(t, "/opt", false)
	e := f.addDockerServer(t)
	e.AddNetwork(docker.Network{ID: "1pnet", Name: "1panel-network", Driver: "bridge"})
	composeContainer(t, e, "mysql", "mysql", "1Panel-mysql-AbCd", func(r *docker.CreateRequest) {
		r.HostConfig.NetworkMode = "1panel-network"
		r.NetworkingConfig.EndpointsConfig = map[string]*docker.EndpointSettings{"1panel-network": {Aliases: []string{"mysql", "1Panel-mysql-AbCd"}}}
		r.HostConfig.Binds = []string{"/opt/1panel/apps/mysql/mysql/data:/data"}
		r.HostConfig.Devices = []docker.DeviceMapping{{PathOnHost: "/dev/dri/renderD128", PathInContainer: "/dev/dri/renderD128", CgroupPermissions: "rwm"}}
	})
	res, err := importClient(b).ImportOnePanel(t.Context(), connect.NewRequest(&dockwellv1.ImportOnePanelRequest{ServerId: f.lastDocker.id.ServerID}))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range res.Msg.GetSteps() {
		if s.GetKind() != dockwellv1.ImportKind_IMPORT_KIND_APP || s.GetName() != "mysql" {
			continue
		}
		if s.GetAction() != dockwellv1.ImportAction_IMPORT_ACTION_LEAVE || !strings.Contains(s.GetReason(), "needs host access, /dev/dri/renderD128") ||
			!strings.Contains(s.GetReason(), "dockwell compose adopt --name mysql --host-access") {
			t.Errorf("mysql = %v %q", s.GetAction(), s.GetReason())
		}
		return
	}
	t.Error("no step for mysql")
}
