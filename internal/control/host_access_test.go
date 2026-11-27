// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent/onepanel/onepaneltest"
	"github.com/dockwell/dockwell/internal/control/containers"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// Docker turns SELinux's labels off for a privileged app by itself, and a spec
// doesn't hold that beside privileged, so a plan that turns privileged off says the
// app runs under the labels again, in one line (the narrow check of ADR 0097, L-a).
func TestTurningPrivilegedOffSaysTheLabelsComeBack(t *testing.T) {
	t.Parallel()
	s := newReachStand(t, store.SQLite)
	edit := func(privileged bool, image string) *dockwellv1.Edit {
		e := deviceEdit(s.staging, image)
		e.GetSpec().GetService().GetApp().Privileged = privileged
		return e
	}
	confined := func(c *dockwellv1.Change) []string {
		var out []string
		for _, n := range c.GetPlan().GetNotes() {
			if n.GetReason() == "confined_again" {
				out = append(out, n.GetMessage())
			}
		}
		return out
	}
	on := propose(t, s.c, "Run api privileged", edit(true, "ghcr.io/acme/shop:1.4"))
	if got := confined(on); len(got) != 0 {
		t.Errorf("turning privileged on says %v", got)
	}
	w := watch(t, s.c, on.GetId())
	proof := stepUpFor(t, s.b, dockwellv1.StepUpAction_STEP_UP_ACTION_APPLY_ROOT_CHANGE, on.GetId())
	if _, err := s.c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: on.GetId(), Fingerprint: on.GetPlan().GetFingerprint(), Proof: proof})); err != nil {
		t.Fatal(err)
	}
	if got := waitWatch(t, w).last(); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the change ended %v: %s", got.GetState(), got.GetError())
	}
	if got := confined(propose(t, s.c, "Deploy api", edit(true, "ghcr.io/acme/shop:1.5"))); len(got) != 0 {
		t.Errorf("a deploy that keeps privileged says %v", got)
	}
	off := confined(propose(t, s.c, "Run api unprivileged", edit(false, "ghcr.io/acme/shop:1.4")))
	if len(off) != 1 || !strings.Contains(off[0], "api runs under SELinux's labels again") || !strings.Contains(off[0], "add label=disable to its security_opt") {
		t.Errorf("turning privileged off says %v", off)
	}
}

// A database's password that's one of the app's own names is taken on only where a
// person chooses it, and then as it is: the plan names no value and says nothing
// of it, Activity says who chose it, and the app's page keeps the state until the
// password changes (ADR 0096, "A database password that's one of its own names").
func TestAGuessablePasswordIsAChoice(t *testing.T) {
	t.Parallel()
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c := changeClient(b, b.origin)
	cc := containerClient(b, b.origin)
	imports := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	e.AddNetwork(docker.Network{ID: "fjnet", Name: "forgejo_default", Driver: "bridge"})
	composeContainer(t, e, "forgejo", "db", "forgejo-db-1", func(r *docker.CreateRequest) {
		r.Env = append(r.Env, "POSTGRES_PASSWORD=forgejo")
	})

	_, err := imports.AdoptCompose(t.Context(), connect.NewRequest(&dockwellv1.AdoptComposeRequest{Name: "forgejo"}))
	if err == nil || !strings.Contains(err.Error(), "the container forgejo-db-1's database password is one of its own names, which anyone can guess") ||
		strings.Contains(err.Error(), "POSTGRES_PASSWORD") {
		t.Fatalf("taking it on unchosen = %v", err)
	}
	res, err := imports.AdoptCompose(t.Context(), connect.NewRequest(&dockwellv1.AdoptComposeRequest{Name: "forgejo", GuessablePassword: true}))
	if err != nil {
		t.Fatal(err)
	}
	plan := res.Msg.GetChange().GetPlan()
	for _, n := range plan.GetNotes() {
		if strings.Contains(strings.ToLower(n.GetMessage()), "password") {
			t.Errorf("the plan says of the password: %q", n.GetMessage())
		}
	}
	if strings.Contains(plan.String(), "POSTGRES_PASSWORD") && strings.Contains(plan.String(), `to:"forgejo"`) {
		t.Errorf("the plan names the value: %v", plan)
	}

	// Activity says who chose it, by the service the plan takes on.
	events := listEvents(t, eventClient(b, b.origin), &dockwellv1.ListEventsRequest{Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_ACCESS}})
	var chose []*dockwellv1.Event
	for _, ev := range events {
		if ev.GetAction() == containers.ActionGuessablePassword {
			chose = append(chose, ev)
		}
	}
	if len(chose) != 1 || chose[0].GetActor().GetKind() != dockwellv1.ActorKind_ACTOR_KIND_USER || !strings.HasPrefix(chose[0].GetSubject().GetId(), "svc_") {
		t.Fatalf("Activity = %v", chose)
	}
	applyConfirmed(t, c, res.Msg.GetChange())
	events = listEvents(t, eventClient(b, b.origin), &dockwellv1.ListEventsRequest{Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_ACCESS}})
	for _, ev := range events {
		if ev.GetAction() == containers.ActionGuessablePassword && ev.GetSubject().GetName() != "db" {
			t.Errorf("once applied, Activity names %q", ev.GetSubject().GetName())
		}
	}

	// The app's page keeps the state, read from the values as they are.
	svcs := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
	guessable := func() bool {
		t.Helper()
		list, err := svcs.ListServices(t.Context(), connect.NewRequest(&dockwellv1.ListServicesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range list.Msg.GetServices() {
			if s.GetResource().GetName() == "db" {
				return s.GetGuessablePassword()
			}
		}
		t.Fatal("no service db")
		return false
	}
	if !guessable() {
		t.Error("the app's page doesn't say its password is guessable")
	}

	// A container taken on alone asks the same.
	e.AddLocalImage("postgres:17", dockertest.Image{Config: docker.Config{Cmd: []string{"postgres"}}})
	runByHand(t, e, "wiki-db", func(r *docker.CreateRequest) {
		r.Image = "postgres:17"
		r.HostConfig.PortBindings, r.HostConfig.Binds = nil, nil
		r.Env = append(r.Env, "PASSWORD=wiki")
		r.Hostname = "wiki"
	})
	_, err = cc.AdoptContainer(t.Context(), connect.NewRequest(&dockwellv1.AdoptContainerRequest{Id: "wiki-db", Project: "default"}))
	if reasonOf(err) != "guessable_password" {
		t.Errorf("a container alone, unchosen = %v", err)
	}
	if _, err := cc.AdoptContainer(t.Context(), connect.NewRequest(&dockwellv1.AdoptContainerRequest{Id: "wiki-db", Project: "default", GuessablePassword: true})); err != nil {
		t.Errorf("a container alone, chosen = %v", err)
	}
}

// An import from Coolify leaves an app whose database password is one of its own
// names, as a state with no words of its own under it, until it's chosen by its ID,
// when it's taken on as it is, and Activity says who chose it (ADR 0096). With host
// access needed too, both are asked, and it comes once both are chosen.
func TestAnImportTakesOnAGuessablePasswordWhereChosen(t *testing.T) {
	t.Parallel()
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	runByHand(t, e, "coolify", func(r *docker.CreateRequest) { r.HostConfig.PortBindings, r.HostConfig.Binds = nil, nil })
	e.AddNetwork(docker.Network{ID: "k8q4net", Name: "k8q4_default", Driver: "bridge"})
	composeContainer(t, e, "k8q4", "listmonk", "listmonk-k8q4", func(r *docker.CreateRequest) {
		r.Labels["coolify.managed"], r.Labels["coolify.type"], r.Labels["coolify.serviceUuid"], r.Labels["coolify.resourceName"] = "true", "service", "k8q4", "newsletter"
		r.Labels[docker.LabelWorkingDir] = "/data/coolify/services/k8q4"
		r.Env = append(r.Env, "PGPASSWORD=listmonk")
		r.HostConfig.Devices = []docker.DeviceMapping{{PathOnHost: "/dev/dri/renderD128", PathInContainer: "/dev/dri/renderD128", CgroupPermissions: "rwm"}}
	})
	ic := importClient(b)
	read := func(skip bool, host, guess []string) *dockwellv1.ImportCoolifyResponse {
		t.Helper()
		res, err := ic.ImportCoolify(t.Context(), connect.NewRequest(&dockwellv1.ImportCoolifyRequest{ServerId: f.lastDocker.id.ServerID, SkipPlan: skip, HostAccess: host, GuessablePassword: guess}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg
	}
	step := panelSteps(read(true, nil, nil))["IMPORT_KIND_APP newsletter"]
	if step.GetAction() != dockwellv1.ImportAction_IMPORT_ACTION_LEAVE || step.GetReason() != "" || step.GetAppId() != "k8q4" || !step.GetGuessablePassword() {
		t.Errorf("unchosen, newsletter reads %v", step)
	}
	if step = panelSteps(read(true, []string{"k8q4"}, nil))["IMPORT_KIND_APP newsletter"]; step.GetAction() != dockwellv1.ImportAction_IMPORT_ACTION_LEAVE || step.GetReason() != "" {
		t.Errorf("with host access alone, newsletter reads %v", step)
	}
	res := read(false, []string{"k8q4"}, []string{"k8q4"})
	if step = panelSteps(res)["IMPORT_KIND_APP newsletter"]; step.GetAction() != dockwellv1.ImportAction_IMPORT_ACTION_TAKE_ON || !step.GetGuessablePassword() {
		t.Errorf("both chosen, newsletter reads %v", step)
	}
	events := listEvents(t, eventClient(b, b.origin), &dockwellv1.ListEventsRequest{Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_ACCESS}})
	n := 0
	for _, ev := range events {
		if ev.GetAction() == containers.ActionGuessablePassword {
			n++
		}
	}
	if res.GetChange() == nil || n != 1 {
		t.Errorf("the plan %v, with %d choices on Activity", res.GetChange().GetId(), n)
	}
}

// A server path that's the same as root to reach is taken on only where a person
// chooses it, as a device is, each one: /proc read-only, /sys read-only, Docker's
// folder of every container's log, /dev/disk and /. The kernel's random numbers read-only need no
// choice, as they're no host access. A Compose file a person imports asks for them in
// its plan, as a catalog app does (ADR 0097, "Server paths a person chooses").
func TestServerPathsAreAChoice(t *testing.T) {
	t.Parallel()
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	cc := containerClient(b, b.origin)
	for i, m := range []struct {
		path     string
		readOnly bool
	}{{"/proc", true}, {"/sys", true}, {"/var/lib/docker/containers", true}, {"/dev/disk", true}, {"/", false}} {
		name := fmt.Sprintf("watcher-%d", i)
		runByHand(t, e, name, func(r *docker.CreateRequest) {
			r.HostConfig.PortBindings = nil
			bind := m.path + ":/host"
			if m.readOnly {
				bind += ":ro"
			}
			r.HostConfig.Binds = []string{bind}
		})
		_, err := cc.AdoptContainer(t.Context(), connect.NewRequest(&dockwellv1.AdoptContainerRequest{Id: name, Project: "default"}))
		if reasonOf(err) != "needs_host_access" || !strings.Contains(err.Error(), "needs host access, "+m.path+",") {
			t.Errorf("%s unchosen = %v", m.path, err)
			continue
		}
		res, err := cc.AdoptContainer(t.Context(), connect.NewRequest(&dockwellv1.AdoptContainerRequest{Id: name, Project: "default", HostAccess: true}))
		if err != nil {
			t.Errorf("%s chosen = %v", m.path, err)
			continue
		}
		if !regexp.MustCompile(`path:"` + regexp.QuoteMeta(m.path) + `"[^}]*same_as_root:true|kind:HOST_ACCESS_KIND_DOCKER\s+path:"` + regexp.QuoteMeta(m.path) + `"`).MatchString(res.Msg.GetChange().GetPlan().String()) {
			t.Errorf("%s isn't the same as root in the plan: %v", m.path, res.Msg.GetChange().GetPlan())
		}
	}

	runByHand(t, e, "chart", func(r *docker.CreateRequest) {
		r.HostConfig.PortBindings = nil
		r.HostConfig.Binds = []string{"/dev/urandom:/dev/random:ro"}
	})
	res, err := cc.AdoptContainer(t.Context(), connect.NewRequest(&dockwellv1.AdoptContainerRequest{Id: "chart", Project: "default"}))
	if err != nil {
		t.Fatalf("reading the kernel's random numbers = %v", err)
	}
	for _, r := range res.Msg.GetChange().GetPlan().GetResources() {
		for _, a := range r.GetHostAccess() {
			if a.GetKind() != dockwellv1.HostAccessKind_HOST_ACCESS_KIND_TAKES_ON {
				t.Errorf("reading the kernel's random numbers reaches %v", a)
			}
		}
	}

	// A Compose file a person imports asks for it as any host access, in its plan:
	// the same as root, which only a signed-in admin applies. Only a take-on, whose
	// plan asks nothing, needs the choice made first.
	pullsAnything(e)
	compose := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	imported, err := compose.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{
		Project: "default", Compose: "services:\n  netdata:\n    image: netdata/netdata:v2\n    volumes:\n      - /proc:/host/proc:ro\n",
	}))
	if err != nil {
		t.Fatalf("a Compose file mounting /proc = %v", err)
	}
	if plan := imported.Msg.GetChange().GetPlan(); !regexp.MustCompile(`path:"/proc"[^}]*same_as_root:true`).MatchString(plan.String()) {
		t.Errorf("a Compose file mounting /proc plans %v", plan)
	}
}

// Any secret that's one of the app's own names is a choice, not only a
// database's: the refusal and the app's state say what kind it is, a password or
// a key, and never which (ADR 0096, "A secret that's one of its own names").
func TestAGuessablePasswordOrKeyIsAChoice(t *testing.T) {
	t.Parallel()
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c := changeClient(b, b.origin)
	imports := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	svcs := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
	for _, tc := range []struct {
		project, env, says string
		want               dockwellv1.GuessableSecret
	}{
		{"odoo", "PASSWORD=odoo", "the container odoo-web-1's password is one of its own names", dockwellv1.GuessableSecret_GUESSABLE_SECRET_PASSWORD},
		{"louez", "S3_ACCESS_KEY_ID=louez", "the container louez-web-1's key is one of its own names", dockwellv1.GuessableSecret_GUESSABLE_SECRET_KEY},
	} {
		e.AddNetwork(docker.Network{ID: tc.project + "net", Name: tc.project + "_default", Driver: "bridge"})
		composeContainer(t, e, tc.project, "web", tc.project+"-web-1", func(r *docker.CreateRequest) { r.Env = append(r.Env, tc.env) })
		_, err := imports.AdoptCompose(t.Context(), connect.NewRequest(&dockwellv1.AdoptComposeRequest{Name: tc.project}))
		if err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s unchosen = %v", tc.project, err)
			continue
		}
		res, err := imports.AdoptCompose(t.Context(), connect.NewRequest(&dockwellv1.AdoptComposeRequest{Name: tc.project, GuessablePassword: true}))
		if err != nil {
			t.Errorf("%s chosen = %v", tc.project, err)
			continue
		}
		applyConfirmed(t, c, res.Msg.GetChange())
		list, err := svcs.ListServices(t.Context(), connect.NewRequest(&dockwellv1.ListServicesRequest{Project: tc.project}))
		if err != nil {
			t.Fatal(err)
		}
		if got := list.Msg.GetServices(); len(got) != 1 || !got[0].GetGuessablePassword() || got[0].GetGuessable() != tc.want {
			t.Errorf("%s's state = %v", tc.project, got)
		}
	}
}
