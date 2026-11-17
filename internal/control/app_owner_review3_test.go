// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/owners"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/appowner"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// downApp is a passwordApp that may stop answering, as a container that crash-loops.
type downApp struct {
	*passwordApp
	mu   sync.Mutex
	down bool
}

func (d *downApp) setDown(down bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.down = down
}

func (d *downApp) isDown() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.down
}

func (d *downApp) MakeAppOwner(ctx context.Context, e, s string, run *resourcev1.OwnerRun, v map[string]string, c bool) (*agentv1.MakeAppOwnerResult, error) {
	if d.isDown() {
		return nil, errors.New("connection refused")
	}
	return d.passwordApp.MakeAppOwner(ctx, e, s, run, v, c)
}

func (d *downApp) ReadAppOwner(ctx context.Context, e, s string, run *resourcev1.OwnerRun, v map[string]string, c bool) (*agentv1.ReadAppOwnerResult, error) {
	if d.isDown() {
		return nil, errors.New("connection refused")
	}
	return d.passwordApp.ReadAppOwner(ctx, e, s, run, v, c)
}

// lostThenDown is an owner whose app took a new password, lost the answer, and has
// been down for more than an hour since.
func lostThenDown(t *testing.T, backend store.Backend) (*owning, *downApp, *clock, string) {
	t.Helper()
	o, app := madeWithPasswords(t, backend, resetTop)
	d := &downApp{passwordApp: app}
	o.f.server.owners.SetAgent(func(string) (owners.Agent, bool) { return d, true })
	c := &clock{now: time.Now()}
	o.f.server.owners.SetNow(c.Now)
	before := o.ownerPassword()
	app.reset = "lost"
	o.rotateOwner()
	o.look()
	d.setDown(true)
	for range 20 {
		c.add(owners.Poll + time.Second)
		o.look()
	}
	if app.has() == before {
		t.Fatal("the test needs the app to hold the new password")
	}
	return o, d, c, before
}

// A rotation whose answer was lost, while the app is then down for an hour, keeps
// both passwords sealed: nothing but a password read signing in ends it. Once the
// app answers again, the one it took is kept (the review's H1).
func TestALostAnswerThenAnHourDownKeepsBothPasswords(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		o, d, c, before := lostThenDown(t, backend)
		owner := o.install().GetOwner()
		if !owner.GetRotating() || o.ownerPassword() != before {
			t.Fatalf("an hour without an answer ended the rotation: %v", owner)
		}
		if !strings.Contains(owner.GetError(), "can't read") {
			t.Errorf("past an hour, the owner says %q, want why neither reads", owner.GetError())
		}
		d.setDown(false)
		c.add(owners.RotateSlow + time.Second)
		o.look()
		if got := o.install().GetOwner(); got.GetRotating() || o.ownerPassword() != d.has() {
			t.Errorf("back up, the owner reads %v, keeps the app's password %v", got, o.ownerPassword() == d.has())
		}
	})
}

// A person settles a rotation neither password reads signing in for with a
// passkey: they see the new one, and may keep the current one, forgetting the new
// one; a token does neither (the review's H1).
func TestAPersonSettlesARotationWithAPasskey(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		o, d, _, before := lostThenDown(t, backend)
		project := o.install().GetProjectId()
		token := createToken(t, o.b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
		tcat := dockwellv1connect.NewCatalogServiceClient(bearer{token: token, rt: o.b.client.Transport}, o.b.origin, connect.WithProtoJSON())
		if _, err := tcat.RevealCatalogOwnerPendingPassword(t.Context(), connect.NewRequest(&dockwellv1.RevealCatalogOwnerPendingPasswordRequest{Id: "thing"})); reasonOf(err) != "needs_session" {
			t.Errorf("a token's reveal: %v", err)
		}
		if _, err := o.cat.RevealCatalogOwnerPendingPassword(t.Context(), connect.NewRequest(&dockwellv1.RevealCatalogOwnerPendingPasswordRequest{Id: "thing"})); reasonOf(err) != "needs_step_up" {
			t.Errorf("a reveal without a passkey: %v", err)
		}
		proof := stepUpFor(t, o.b, dockwellv1.StepUpAction_STEP_UP_ACTION_REVEAL_CATALOG_OWNER_PENDING_PASSWORD, project)
		shown, err := o.cat.RevealCatalogOwnerPendingPassword(t.Context(), connect.NewRequest(&dockwellv1.RevealCatalogOwnerPendingPasswordRequest{Id: "thing", Proof: proof}))
		if err != nil || shown.Msg.GetPassword() != d.has() {
			t.Fatalf("the new password shown is the app's %v: %v", shown.Msg.GetPassword() == d.has(), err)
		}
		if _, err := o.cat.KeepCatalogOwnerPassword(t.Context(), connect.NewRequest(&dockwellv1.KeepCatalogOwnerPasswordRequest{Id: "thing"})); reasonOf(err) != "needs_step_up" {
			t.Errorf("keeping without a passkey: %v", err)
		}
		proof = stepUpFor(t, o.b, dockwellv1.StepUpAction_STEP_UP_ACTION_KEEP_CATALOG_OWNER_PASSWORD, project)
		kept, err := o.cat.KeepCatalogOwnerPassword(t.Context(), connect.NewRequest(&dockwellv1.KeepCatalogOwnerPasswordRequest{Id: "thing", Proof: proof}))
		if err != nil || kept.Msg.GetOwner().GetRotating() || o.ownerPassword() != before {
			t.Fatalf("keeping the current one: %v, %v", kept.Msg.GetOwner(), err)
		}
		proof = stepUpFor(t, o.b, dockwellv1.StepUpAction_STEP_UP_ACTION_KEEP_CATALOG_OWNER_PASSWORD, project)
		if _, err := o.cat.KeepCatalogOwnerPassword(t.Context(), connect.NewRequest(&dockwellv1.KeepCatalogOwnerPasswordRequest{Id: "thing", Proof: proof})); reasonOf(err) != "not_rotating" {
			t.Errorf("keeping with nothing being set: %v", err)
		}
	})
}

// An upgrade between a lost answer and the next look, to a release without reset
// steps, doesn't end the rotation: done is read from the release's run, and the new
// password the app took is kept (the review's H1).
func TestAnUpgradeMidRotationKeepsIt(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		o, app := madeWithPasswords(t, backend, resetTop)
		c := &clock{now: time.Now()}
		o.f.server.owners.SetNow(c.Now)
		before := o.ownerPassword()
		app.reset = "lost"
		o.rotateOwner()
		o.look()
		o.release(ownedEntry(2, "2.0", secondRelease, ownerInputs, ownerTop))
		o.applied(o.upgrade())
		c.add(owners.RotateAfter + time.Second)
		o.look()
		if got := o.install().GetOwner(); got.GetRotating() || o.ownerPassword() == before || o.ownerPassword() != app.has() {
			t.Errorf("after an upgrade mid-rotation, the owner reads %v, keeps the app's password %v", got, o.ownerPassword() == app.has())
		}
	})
}

// While an owner isn't made, the secrets its steps read can't be deleted. Where one
// is gone anyway, as an install from before this kept none, that owner alone fails,
// saying to undo the deletion, and every other owner is made in the same look (the
// review's H2).
func TestADeletedOwnerSecretFailsThatOwnerAlone(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		o := startOwning(t, backend, ownedEntry(1, "1.0", firstRelease, ownerInputs, ownerTop), &dockwellv1.InstallCatalogEntryRequest{Id: "thing"})
		_, err := o.c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Delete the owner's password", Edits: []*dockwellv1.Edit{{
			Resource: &dockwellv1.ResourceRef{Kind: string(variable.SecretKind), Scope: o.env, Name: "OWNER_PASSWORD"}, Delete: true,
		}}}))
		if err == nil || !strings.Contains(err.Error(), "OWNER_PASSWORD") {
			t.Fatalf("deleting the password of an owner not made: %v", err)
		}
		// An owner as one from before held them: none.
		spec, _ := o.spec(appowner.Kind, o.env, appowner.Name).(*resourcev1.AppOwner)
		ref := resource.Ref{Kind: appowner.Kind, Scope: o.env, Name: appowner.Name}
		by := revision.Proposal{Summary: "As before", Actor: revision.SystemActor, Via: revision.ViaAutomatic}
		del := by
		del.Edits = []revision.Edit{{Ref: ref}}
		plan, err := o.f.engine.Propose(t.Context(), del)
		if err != nil {
			t.Fatal(err)
		}
		if err := o.f.engine.Apply(t.Context(), plan.Change); err != nil {
			t.Fatal(err)
		}
		spec.HeldSecrets = nil
		add := by
		add.Edits = []revision.Edit{{Ref: ref, Spec: spec, Owner: true}}
		if plan, err = o.f.engine.Propose(t.Context(), add); err != nil {
			t.Fatal(err)
		}
		if err := o.f.engine.Apply(t.Context(), plan.Change); err != nil {
			t.Fatal(err)
		}
		o.applied(propose(t, o.c, "Delete the owner's password", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(variable.SecretKind), Scope: o.env, Name: "OWNER_PASSWORD"}, Delete: true,
		}))
		other := installApp(t, o.cat, &dockwellv1.InstallCatalogEntryRequest{Id: "thing", Project: "other"})
		o.applied(other)
		if err := o.f.server.owners.Tick(t.Context()); err != nil {
			t.Fatalf("the look stopped for every environment: %v", err)
		}
		o.f.server.owners.Wait()
		installs, err := o.cat.ListCatalogInstalls(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		for _, in := range installs.Msg.GetInstalls() {
			owner := in.GetOwner()
			switch in.GetProject() {
			case "thing":
				if owner.GetState() != dockwellv1.CatalogOwnerState_CATALOG_OWNER_STATE_FAILED || !strings.Contains(owner.GetError(), "OWNER_PASSWORD") ||
					!strings.Contains(owner.GetError(), "Undo that change") || strings.Contains(owner.GetError(), "backup") {
					t.Errorf("the owner whose secret is gone reads %v", owner)
				}
			case "other":
				if owner.GetState() != dockwellv1.CatalogOwnerState_CATALOG_OWNER_STATE_MADE {
					t.Errorf("another install's owner reads %v in the same look", owner)
				}
			}
		}
	})
}

// flakyUnsets fails to read what an owner's entry takes out, a number of times, as a
// busy service or an unreadable catalog would.
type flakyUnsets struct {
	owners.Runs
	mu    sync.Mutex
	fails int
}

func (f *flakyUnsets) OwnerResetRun(ctx context.Context, env string) (*resourcev1.OwnerRun, map[string]string, []string, error) {
	return f.Runs.(owners.ResetRuns).OwnerResetRun(ctx, env)
}

func (f *flakyUnsets) OwnerUnset(ctx context.Context, env string) (string, []string, error) {
	f.mu.Lock()
	fail := f.fails > 0
	if fail {
		f.fails--
	}
	f.mu.Unlock()
	if fail {
		// What it would take out, with why it can't now, as a busy apply says.
		return "web", []string{"ADMIN_TOKEN"}, errors.New("another change is applying web")
	}
	return f.Runs.(owners.Unsets).OwnerUnset(ctx, env)
}

// adminToken says whether an install's web service still takes ADMIN_TOKEN.
func (o *owning) adminToken() bool {
	o.t.Helper()
	web, _ := o.spec(service.Kind, o.env, "web").(*resourcev1.Service)
	_, ok := web.GetSecrets()["ADMIN_TOKEN"]
	return ok
}

// What only making an owner needed goes on a later look where it failed, a minute,
// five, fifteen, then an hour on; after an hour of failing, Activity says so once,
// and the install says what's left, which a person takes out by a plan (the
// review's M1).
func TestTakingAdminTokenOutIsTriedAgainThenAPersonsPlan(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		o := startOwning(t, backend, inviteEntry(), &dockwellv1.InstallCatalogEntryRequest{Id: "thing"})
		app := &inviteApp{registered: true}
		o.f.server.owners.SetAgent(func(string) (owners.Agent, bool) { return app, true })
		c := &clock{now: time.Now().Add(-3 * time.Hour)}
		o.f.server.owners.SetNow(c.Now)
		flaky := &flakyUnsets{Runs: o.f.server.owners.RunsNow(), fails: 1}
		o.f.server.owners.SetRuns(flaky)
		o.look()
		if o.install().GetOwner().GetState() != dockwellv1.CatalogOwnerState_CATALOG_OWNER_STATE_MADE || !o.adminToken() {
			t.Fatalf("the test needs the owner made, and the first try failed: %v", o.install().GetOwner())
		}
		held, _ := o.spec(appowner.Kind, o.env, appowner.Name).(*resourcev1.AppOwner)
		if len(held.GetHeldSecrets()) == 0 {
			t.Error("the owner holds no secret its steps read")
		}
		o.look()
		if !o.adminToken() {
			t.Fatal("it was tried again sooner than a minute on")
		}
		c.add(time.Minute + time.Second)
		o.look()
		if o.adminToken() {
			t.Fatal("ADMIN_TOKEN wasn't taken out a minute on")
		}
		held, _ = o.spec(appowner.Kind, o.env, appowner.Name).(*resourcev1.AppOwner)
		if len(held.GetHeldSecrets()) != 0 {
			t.Errorf("once made, the owner still holds %v", held.GetHeldSecrets())
		}

		// A second install that fails for over an hour: Activity says so once, and the
		// install says what's left, which a person's plan takes out.
		o2 := startOwning(t, backend, inviteEntry(), &dockwellv1.InstallCatalogEntryRequest{Id: "thing"})
		o2.f.server.owners.SetAgent(func(string) (owners.Agent, bool) { return app, true })
		c2 := &clock{now: time.Now().Add(-3 * time.Hour)}
		o2.f.server.owners.SetNow(c2.Now)
		flaky2 := &flakyUnsets{Runs: o2.f.server.owners.RunsNow(), fails: 100}
		o2.f.server.owners.SetRuns(flaky2)
		o2.look()
		for _, d := range []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, time.Hour} {
			c2.add(d + time.Second)
			o2.look()
		}
		owner := o2.install().GetOwner()
		if !slices.Equal(owner.GetUnsetLeft(), []string{"ADMIN_TOKEN"}) || !o2.adminToken() {
			t.Fatalf("after an hour of failing, the owner reads %v", owner)
		}
		res, err := dockwellv1connect.NewEventServiceClient(o2.b, o2.b.origin, connect.WithProtoJSON()).ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{
			Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_APP_OWNER}, PageSize: 50,
		}))
		if err != nil {
			t.Fatal(err)
		}
		alerts := 0
		for _, e := range res.Msg.GetEvents() {
			if e.GetAction() == owners.ActionUnsetFailed {
				alerts++
				if !strings.Contains(e.GetAppOwner().GetError(), "ADMIN_TOKEN") {
					t.Errorf("the alert says %q", e.GetAppOwner().GetError())
				}
			}
		}
		if alerts != 1 {
			t.Errorf("Activity says it %d times, want once", alerts)
		}
		planned, err := o2.cat.SettleCatalogOwner(t.Context(), connect.NewRequest(&dockwellv1.SettleCatalogOwnerRequest{Id: "thing"}))
		if err != nil {
			t.Fatal(err)
		}
		if !o2.adminToken() {
			t.Fatal("planning took ADMIN_TOKEN out before it was applied")
		}
		o2.applied(planned.Msg.GetChange())
		// What failed now reads, and finds nothing left.
		flaky2.mu.Lock()
		flaky2.fails = 0
		flaky2.mu.Unlock()
		c2.add(time.Hour + time.Second)
		o2.look()
		if o2.adminToken() || len(o2.install().GetOwner().GetUnsetLeft()) != 0 {
			t.Errorf("the plan applied, and web takes ADMIN_TOKEN %v, the owner reads %v", o2.adminToken(), o2.install().GetOwner())
		}
		if _, err := o2.cat.SettleCatalogOwner(t.Context(), connect.NewRequest(&dockwellv1.SettleCatalogOwnerRequest{Id: "thing"})); reasonOf(err) != "settled" {
			t.Errorf("settling what's settled: %v", err)
		}
	})
}

// A person who opens an invited install to register through it, before its owner
// registered, still has ADMIN_TOKEN taken out once it's opened (the review's M1).
func TestAnOpenedInvitesAdminTokenGoesToo(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		o := startOwning(t, backend, inviteEntry(), &dockwellv1.InstallCatalogEntryRequest{Id: "thing"})
		app := &inviteApp{}
		o.f.server.owners.SetAgent(func(string) (owners.Agent, bool) { return app, true })
		o.look()
		o.open()
		o.look()
		if got := o.install().GetOwner().GetState(); got != dockwellv1.CatalogOwnerState_CATALOG_OWNER_STATE_OPENED {
			t.Fatalf("the owner reads %v, want opened", got)
		}
		if o.adminToken() {
			t.Error("opened, web still takes ADMIN_TOKEN")
		}
	})
}

// A password a person forgot, once made, stays forgotten through an upgrade: the
// upgrade generates none the app never had (the review's L1).
func TestAForgottenPasswordStaysForgottenThroughAnUpgrade(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		o, _ := madeWithPasswords(t, backend, ownerTop)
		o.look()
		o.applied(propose(t, o.c, "Forget Thing's owner password", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(variable.SecretKind), Scope: o.env, Name: "OWNER_PASSWORD"}, Delete: true,
		}))
		o.release(ownedEntry(2, "2.0", secondRelease, ownerInputs, ownerTop))
		o.applied(o.upgrade())
		if o.spec(variable.SecretKind, o.env, "OWNER_PASSWORD") != nil {
			t.Error("the upgrade made a password the app never had")
		}
	})
}
