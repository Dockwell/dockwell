// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// applyAsRoot applies a plan that's as good as root, with a passkey for it.
func applyAsRoot(t *testing.T, b *browser, change *dockwellv1.Change) {
	t.Helper()
	c := changeClient(b, b.origin)
	proof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_APPLY_ROOT_CHANGE, change.GetId())
	w := watch(t, c, change.GetId())
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{
		Id: change.GetId(), Fingerprint: change.GetPlan().GetFingerprint(), Proof: proof,
	})); err != nil {
		t.Fatal(err)
	}
	waitWatch(t, w)
}

// A stolen session could apply a plan that's as good as root, and keep a way in to
// the server after the session is gone. So one takes a passkey, just now, for that
// change, as well as a person and a typed fingerprint.
func TestAPlanAsGoodAsRootTakesAPasskey(t *testing.T) {
	storetest.Each(t, testAPlanAsGoodAsRootTakesAPasskey)
}

func testAPlanAsGoodAsRootTakesAPasskey(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	f.addDockerServer(t)
	c := changeClient(b, b.origin)
	cc := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	root := importCompose(t, cc, "services:\n  app:\n    image: ghcr.io/acme/shop:1.4\n    privileged: true\n", nil, false)
	if !root.GetPlan().GetHostAccess() {
		t.Fatalf("privileged mode plans no host access: %v", root.GetPlan())
	}
	apply := func(fingerprint, proof string) error {
		t.Helper()
		_, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: root.GetId(), Fingerprint: fingerprint, Proof: proof}))
		return err
	}
	planned := func() {
		t.Helper()
		got, err := c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: root.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		if got.Msg.GetChange().GetState() != dockwellv1.ChangeState_CHANGE_STATE_PLANNED {
			t.Fatalf("the change is %v", got.Msg.GetChange().GetState())
		}
	}
	fingerprint := root.GetPlan().GetFingerprint()
	action := dockwellv1.StepUpAction_STEP_UP_ACTION_APPLY_ROOT_CHANGE

	// Without one it asks, having checked everything else, and nothing runs.
	if err := apply(fingerprint, ""); reasonOf(err) != "needs_step_up" || connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("with no proof = %v", err)
	}
	planned()
	// What else would refuse it comes first, and leaves a proof unspent.
	proof := stepUpFor(t, b, action, root.GetId())
	if err := apply("not-this-plan", proof); reasonOf(err) != "plan_changed" {
		t.Fatalf("with another fingerprint = %v", err)
	}
	if err := apply("", proof); reasonOf(err) != "needs_confirmation" {
		t.Fatalf("with no fingerprint = %v", err)
	}
	// One for another change, or for something else, doesn't do, and it's spent.
	for what, other := range map[string]string{
		"another change":    stepUpFor(t, b, action, "chg_another"),
		"revealing instead": stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_REVEAL_SECRET, root.GetId()),
	} {
		if err := apply(fingerprint, other); reasonOf(err) != "needs_step_up" {
			t.Fatalf("with a proof for %s = %v", what, err)
		}
		planned()
		if err := apply(fingerprint, other); reasonOf(err) != "needs_step_up" {
			t.Fatalf("with that proof again = %v", err)
		}
	}

	w := watch(t, c, root.GetId())
	if err := apply(fingerprint, proof); err != nil {
		t.Fatalf("with its proof = %v", err)
	}
	if got := waitWatch(t, w).last(); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the change = %v", got)
	}
}

// What stops short of root asks no more of a person than it did, or the prompt would
// stop meaning anything. Keeping what a service has asks nothing at all.
func TestAPlanShortOfRootTakesNoPasskey(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	f.addDockerServer(t)
	c := changeClient(b, b.origin)
	cc := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	folder := importCompose(t, cc, nasFile(":ro"), nil, false)
	if !folder.GetPlan().GetHostAccess() {
		t.Fatalf("a bind volume plans no host access: %v", folder.GetPlan())
	}
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: folder.GetId(), Fingerprint: folder.GetPlan().GetFingerprint()})); err != nil {
		t.Fatalf("a plan short of root = %v", err)
	}
}

// The words are for a person, whoever reads them first.
func TestAskingForAPasskeySaysSo(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	f.addDockerServer(t)
	c := changeClient(b, b.origin)
	cc := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	root := importCompose(t, cc, "services:\n  app:\n    image: ghcr.io/acme/shop:1.4\n    privileged: true\n", nil, false)
	_, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: root.GetId(), Fingerprint: root.GetPlan().GetFingerprint()}))
	if err == nil || !strings.Contains(err.Error(), "confirm with your passkey") {
		t.Fatalf("got %v", err)
	}
}

// registerImages has a server's Docker able to pull what these tests run.
func registerImages(e *dockertest.Engine) {
	for _, image := range []string{
		"ghcr.io/acme/ops:1.0", "ghcr.io/acme/ops:1.1", "ghcr.io/acme/ops:1.1@sha256:" + strings.Repeat("a", 64), "ghcr.io/stranger/ops:1.0", "evil.example/acme/ops:1.0",
		"ghcr.io/acme/web:1.0", "ghcr.io/acme/web:1.1", "ghcr.io/acme/web:1.2", "ghcr.io/acme/web:1.3", "ghcr.io/acme/web:1.4",
	} {
		e.Registry[image] = dockertest.Image{}
	}
}

// socketEdit is a service with Docker's socket, which is root.
func socketEdit(name string, change func(*resourcev1.Container)) *dockwellv1.Edit {
	edit := serviceEdit(name, "ghcr.io/acme/ops:1.0", nil)
	app := edit.GetSpec().GetService().GetApp()
	app.Mounts = []*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: "/var/run/docker.sock"}, Target: "/var/run/docker.sock"}}
	if change != nil {
		change(app)
	}
	return edit
}

func proposeEdit(t *testing.T, c dockwellv1connect.ChangeServiceClient, edit *dockwellv1.Edit) *dockwellv1.Change {
	t.Helper()
	made, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Edit it", Edits: []*dockwellv1.Edit{edit}}))
	if err != nil {
		t.Fatal(err)
	}
	return made.Msg.GetChange()
}

func askedBy(t *testing.T, c dockwellv1connect.ChangeServiceClient, change *dockwellv1.Change, fingerprint string) error {
	t.Helper()
	_, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: change.GetId(), Fingerprint: fingerprint}))
	return err
}

// Keeping root doesn't excuse what runs with it. Whatever else a stolen session or a
// token puts in a service with Docker's socket has root, so it's as much as reaching
// it anew: a person, the typed fingerprint and a passkey.
func TestWhatKeepsRootAsksAgainForAnythingElseThatRuns(t *testing.T) {
	storetest.Each(t, testWhatKeepsRootAsksAgainForAnythingElseThatRuns)
}

func testWhatKeepsRootAsksAgainForAnythingElseThatRuns(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	registerImages(f.addDockerServer(t))
	c := changeClient(b, b.origin)
	script := changeClient(bearer{token: createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE), rt: b.client.Transport}, b.origin)
	applyAsRoot(t, b, proposeEdit(t, c, socketEdit("ops", nil)))

	for what, change := range map[string]func(*resourcev1.Container){
		"another image":                    func(a *resourcev1.Container) { a.Image = "ghcr.io/stranger/ops:1.0" },
		"the same tag of another registry": func(a *resourcev1.Container) { a.Image = "evil.example/acme/ops:1.0" },
		"a command":                        func(a *resourcev1.Container) { a.Command = []string{"sh", "-c", "docker run --privileged x"} },
		"an entrypoint":                    func(a *resourcev1.Container) { a.Entrypoint = []string{"/bin/sh"} },
		"a user":                           func(a *resourcev1.Container) { a.User = "root" },
		"a health check": func(a *resourcev1.Container) {
			a.Healthcheck = &resourcev1.Healthcheck{Test: []string{"CMD", "sh", "-c", "x"}}
		},
		"its environment": func(a *resourcev1.Container) { a.Env = map[string]string{"LD_PRELOAD": "/x.so"} },
		"a new tag and something else": func(a *resourcev1.Container) {
			a.Image, a.Command = "ghcr.io/acme/ops:1.1", []string{"sh"}
		},
	} {
		t.Run(what, func(t *testing.T) {
			planned := proposeEdit(t, c, socketEdit("ops", change))
			if !planned.GetPlan().GetHostAccess() {
				t.Fatalf("it plans no host access: %v", planned.GetPlan())
			}
			fingerprint := planned.GetPlan().GetFingerprint()
			if err := askedBy(t, script, planned, fingerprint); reasonOf(err) != "needs_session" {
				t.Errorf("a token applying it = %v", err)
			}
			if err := askedBy(t, c, planned, ""); reasonOf(err) != "needs_confirmation" {
				t.Errorf("with no fingerprint = %v", err)
			}
			if err := askedBy(t, c, planned, fingerprint); reasonOf(err) != "needs_step_up" {
				t.Errorf("with no passkey = %v", err)
			}
			says := false
			for _, n := range planned.GetPlan().GetNotes() {
				says = says || n.GetReason() == "new_code_as_root"
			}
			if !says {
				t.Errorf("the plan doesn't say why it asks: %v", planned.GetPlan().GetNotes())
			}
			if _, err := c.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: planned.GetId()})); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The one thing that goes on as it did is how CI deploys: another tag or digest of
// the same repository, with nothing else changed, which a token applies.
func TestANewTagOfTheSameImageKeepsDeployingWithRoot(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	registerImages(f.addDockerServer(t))
	c := changeClient(b, b.origin)
	script := changeClient(bearer{token: createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE), rt: b.client.Transport}, b.origin)
	applyAsRoot(t, b, proposeEdit(t, c, socketEdit("ops", nil)))
	for _, image := range []string{"ghcr.io/acme/ops:1.1", "ghcr.io/acme/ops:1.1@sha256:" + strings.Repeat("a", 64)} {
		planned := proposeEdit(t, script, socketEdit("ops", func(a *resourcev1.Container) { a.Image = image }))
		if planned.GetPlan().GetHostAccess() {
			t.Errorf("%s plans host access: %v", image, planned.GetPlan())
		}
		if got := apply(t, script, planned); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("a token's new tag %s: %v", image, got)
		}
	}
}

// Taking root out and undoing that brings it back, so the undo asks for a passkey.
func TestAnUndoThatBringsRootBackTakesAPasskey(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	registerImages(f.addDockerServer(t))
	c := changeClient(b, b.origin)
	applyAsRoot(t, b, proposeEdit(t, c, socketEdit("ops", nil)))
	plain := proto.Clone(socketEdit("ops", func(a *resourcev1.Container) { a.Mounts = nil })).(*dockwellv1.Edit)
	dropped := proposeEdit(t, c, plain)
	if got := applied(t, c, dropped); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("dropping it: %v", got)
	}
	undo, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: dropped.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	planned := undo.Msg.GetChange()
	if err := askedBy(t, c, planned, planned.GetPlan().GetFingerprint()); reasonOf(err) != "needs_step_up" {
		t.Fatalf("the undo = %v", err)
	}
	applyAsRoot(t, b, planned)
}

// A folder approved in amber can become a link to /etc. What a person approved was
// where it led then, so the next plan asks as if for the first time.
func TestAKeptPathThatLeadsElsewhereAsksAgain(t *testing.T) {
	storetest.Each(t, testAKeptPathThatLeadsElsewhereAsksAgain)
}

func testAKeptPathThatLeadsElsewhereAsksAgain(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	registerImages(f.addDockerServer(t))
	c := changeClient(b, b.origin)
	script := changeClient(bearer{token: createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE), rt: b.client.Transport}, b.origin)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	site, other := filepath.Join(dir, "site"), filepath.Join(dir, "other")
	for _, d := range []string{site, other} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	edit := func(image string) *dockwellv1.Edit {
		e := serviceEdit("web", image, nil)
		e.GetSpec().GetService().GetApp().Mounts = []*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: site}, Target: "/site"}}
		return e
	}
	if got := applied(t, c, proposeEdit(t, c, edit("ghcr.io/acme/web:1.0"))); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the first apply: %v", got)
	}
	// Left as it is, a new tag is a token's to deploy.
	tag := proposeEdit(t, script, edit("ghcr.io/acme/web:1.1"))
	if tag.GetPlan().GetHostAccess() {
		t.Fatalf("a path that leads where it did plans host access: %v", tag.GetPlan())
	}
	if got := apply(t, script, tag); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the token's new tag: %v", got)
	}

	if err := os.Remove(site); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, site); err != nil {
		t.Fatal(err)
	}
	elsewhere := proposeEdit(t, script, edit("ghcr.io/acme/web:1.2"))
	if !elsewhere.GetPlan().GetHostAccess() {
		t.Fatalf("a path that leads elsewhere now plans no host access: %v", elsewhere.GetPlan())
	}
	if err := askedBy(t, script, elsewhere, elsewhere.GetPlan().GetFingerprint()); reasonOf(err) != "needs_session" {
		t.Errorf("a token applying it = %v", err)
	}
	// A person approves where it leads now, which it goes on to be asked by.
	if got := applied(t, c, elsewhere); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("a person's apply: %v", got)
	}
	again := proposeEdit(t, script, edit("ghcr.io/acme/web:1.3"))
	if again.GetPlan().GetHostAccess() {
		t.Fatalf("a path that leads where a person approved plans host access: %v", again.GetPlan())
	}
	if got := apply(t, script, again); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the token's next tag: %v", got)
	}

	if err := os.Remove(site); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", site); err != nil {
		t.Fatal(err)
	}
	etc := proposeEdit(t, script, edit("ghcr.io/acme/web:1.4"))
	if err := askedBy(t, script, etc, etc.GetPlan().GetFingerprint()); reasonOf(err) != "needs_session" {
		t.Errorf("a token applying a link to /etc = %v", err)
	}
	if err := askedBy(t, c, etc, etc.GetPlan().GetFingerprint()); reasonOf(err) != "needs_step_up" {
		t.Errorf("a person applying a link to /etc with no passkey = %v", err)
	}
}

// A proof goes where the action does. One for a change that can't start, as its
// server is away, isn't used up, and works once the server's back.
func TestARefusedApplyLeavesItsProofToUse(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	registerImages(f.addDockerServer(t))
	c := changeClient(b, b.origin)
	planned := proposeEdit(t, c, socketEdit("ops", nil))
	proof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_APPLY_ROOT_CHANGE, planned.GetId())
	server := f.lastDocker
	server.stop()
	waitConnected(t, f, server.id.ServerID, false)
	req := &dockwellv1.ApplyChangeRequest{Id: planned.GetId(), Fingerprint: planned.GetPlan().GetFingerprint(), Proof: proof}
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(req)); err == nil || reasonOf(err) == "needs_step_up" {
		t.Fatalf("applying with its server away = %v", err)
	}
	server.connect(t, f)
	w := watch(t, c, planned.GetId())
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(req)); err != nil {
		t.Fatalf("with that proof, once it's back = %v", err)
	}
	if got := waitWatch(t, w).last(); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the change = %v", got)
	}
}

// Trying to apply what's as good as root, with no passkey following, is what a stolen
// session looks like, so it's on Activity. One that's confirmed isn't.
func TestAPlanNobodyConfirmsIsOnActivity(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	registerImages(f.addDockerServer(t))
	c := changeClient(b, b.origin)
	planned := proposeEdit(t, c, socketEdit("ops", nil))
	if err := askedBy(t, c, planned, planned.GetPlan().GetFingerprint()); reasonOf(err) != "needs_step_up" {
		t.Fatal(err)
	}
	warned := func(id string) bool {
		list, err := dockwellv1connect.NewEventServiceClient(b, b.origin, connect.WithProtoJSON()).ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range list.Msg.GetEvents() {
			if e.GetAction() == "change.apply_unconfirmed" && e.GetSubject().GetId() == id {
				return true
			}
		}
		return false
	}
	deadline := time.Now().Add(5 * time.Second)
	for !warned(planned.GetId()) {
		if time.Now().After(deadline) {
			t.Fatal("nothing on Activity says a passkey was asked for and none came")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := c.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: planned.GetId()})); err != nil {
		t.Fatal(err)
	}
	confirmed := proposeEdit(t, c, socketEdit("ops", nil))
	if err := askedBy(t, c, confirmed, confirmed.GetPlan().GetFingerprint()); reasonOf(err) != "needs_step_up" {
		t.Fatal(err)
	}
	applyAsRoot(t, b, confirmed)
	time.Sleep(3 * askWait)
	if warned(confirmed.GetId()) {
		t.Error("a plan someone confirmed reads as one nobody did")
	}
}
