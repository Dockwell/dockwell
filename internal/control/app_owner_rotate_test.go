// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"errors"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/owners"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// resetTop is ownerTop with reset steps that set a new password.
var resetTop = strings.Replace(ownerTop, "  done:\n", `  resetSteps:
    - request:
        method: POST
        path: /password
        contentType: application/json
        body: '{"password":"${OWNER_PASSWORD}"}'
        port: 80
  done:
`, 1)

// passwordApp is an agent for an app that keeps its owner's password: its steps set
// it, and its done reads whether the password sent signs in. How it answers a
// reset is the test's.
type passwordApp struct {
	mu       sync.Mutex
	password string
	// reset is how the app answers its reset steps: "" sets the password, "lost"
	// sets it and loses the answer, "refuse" says no and keeps it.
	reset  string
	resets []*resourcev1.OwnerRun
	sent   []map[string]string
}

func (a *passwordApp) MakeAppOwner(_ context.Context, _, _ string, run *resourcev1.OwnerRun, values map[string]string, checkFirst bool) (*agentv1.MakeAppOwnerResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if checkFirst && a.password != "" && values["OWNER_PASSWORD"] == a.password {
		return &agentv1.MakeAppOwnerResult{Made: true}, nil
	}
	if a.password == "" {
		a.password = values["OWNER_PASSWORD"]
		return &agentv1.MakeAppOwnerResult{Made: true}, nil
	}
	a.resets = append(a.resets, proto.CloneOf(run))
	a.sent = append(a.sent, maps.Clone(values))
	switch a.reset {
	case "lost":
		a.password = values["OWNER_PASSWORD"]
		return nil, errors.New("the agent went away")
	case "refuse":
		return &agentv1.MakeAppOwnerResult{Step: 1, Said: "no password " + values["OWNER_PASSWORD"] + " today"}, nil
	}
	a.password = values["OWNER_PASSWORD"]
	return &agentv1.MakeAppOwnerResult{Made: true}, nil
}

func (a *passwordApp) ReadAppOwner(_ context.Context, _, _ string, _ *resourcev1.OwnerRun, values map[string]string, _ bool) (*agentv1.ReadAppOwnerResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return &agentv1.ReadAppOwnerResult{Yes: a.password != "" && values["OWNER_PASSWORD"] == a.password}, nil
}

func (a *passwordApp) has() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.password
}

// madeWithPasswords is an install of resetTop's entry whose owner is made by an app
// that keeps its password.
func madeWithPasswords(t *testing.T, backend store.Backend, top string) (*owning, *passwordApp) {
	t.Helper()
	o := startOwning(t, backend, ownedEntry(1, "1.0", firstRelease, ownerInputs, top), &dockwellv1.InstallCatalogEntryRequest{Id: "thing"})
	app := &passwordApp{}
	o.f.server.owners.SetAgent(func(string) (owners.Agent, bool) { return app, true })
	o.look()
	if got := o.install().GetOwner().GetState(); got != dockwellv1.CatalogOwnerState_CATALOG_OWNER_STATE_MADE {
		t.Fatalf("the owner reads %v, want made", got)
	}
	if app.has() == "" || app.has() != o.ownerPassword() {
		t.Fatal("the app's password isn't the one Dockwell keeps")
	}
	return o, app
}

// ownerPassword is the owner's password as Dockwell keeps it.
func (o *owning) ownerPassword() string {
	o.t.Helper()
	s, _ := o.spec(variable.SecretKind, o.env, "OWNER_PASSWORD").(*resourcev1.Secret)
	return s.GetValue()
}

// rotateOwner asks for a new owner's password with a passkey.
func (o *owning) rotateOwner() *dockwellv1.CatalogOwner {
	o.t.Helper()
	proof := stepUpFor(o.t, o.b, dockwellv1.StepUpAction_STEP_UP_ACTION_RESET_CATALOG_OWNER_PASSWORD, o.install().GetProjectId())
	res, err := o.cat.ResetCatalogOwnerPassword(o.t.Context(), connect.NewRequest(&dockwellv1.ResetCatalogOwnerPasswordRequest{Id: "thing", Proof: proof}))
	if err != nil {
		o.t.Fatal(err)
	}
	return res.Msg.GetOwner()
}

// A new owner's password is a person's, with a passkey: Dockwell generates it as the
// entry's input does, keeps it sealed beside the current one, sets it with the
// entry's reset steps, and only then puts it in place of the current one, as a
// change of its own that holds no password (ADR 0105).
func TestANewOwnersPasswordIsSetByItsResetStepsThenKept(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		o, app := madeWithPasswords(t, backend, resetTop)
		before := o.ownerPassword()
		token := createToken(t, o.b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
		tcat := dockwellv1connect.NewCatalogServiceClient(bearer{token: token, rt: o.b.client.Transport}, o.b.origin, connect.WithProtoJSON())
		if _, err := tcat.ResetCatalogOwnerPassword(t.Context(), connect.NewRequest(&dockwellv1.ResetCatalogOwnerPasswordRequest{Id: "thing"})); reasonOf(err) != "needs_session" {
			t.Errorf("a token's new password: %v", err)
		}
		if _, err := o.cat.ResetCatalogOwnerPassword(t.Context(), connect.NewRequest(&dockwellv1.ResetCatalogOwnerPasswordRequest{Id: "thing"})); reasonOf(err) != "needs_step_up" {
			t.Errorf("without a passkey: %v", err)
		}
		if owner := o.rotateOwner(); !owner.GetRotating() || owner.GetState() != dockwellv1.CatalogOwnerState_CATALOG_OWNER_STATE_MADE {
			t.Fatalf("the owner reads %v, want made and rotating", owner)
		}
		if o.ownerPassword() != before {
			t.Fatal("the new password took the current one's place before the app had it")
		}
		proof := stepUpFor(t, o.b, dockwellv1.StepUpAction_STEP_UP_ACTION_RESET_CATALOG_OWNER_PASSWORD, o.install().GetProjectId())
		if _, err := o.cat.ResetCatalogOwnerPassword(t.Context(), connect.NewRequest(&dockwellv1.ResetCatalogOwnerPasswordRequest{Id: "thing", Proof: proof})); reasonOf(err) != "rotating" {
			t.Errorf("a second new password while one is set: %v", err)
		}
		o.look()
		if len(app.resets) != 1 || len(app.resets[0].GetSteps()) != 1 || app.resets[0].GetSteps()[0].GetRequest().GetPath() != "/password" {
			t.Fatalf("the app was sent %v, want the reset steps once", app.resets)
		}
		after := o.ownerPassword()
		if after == before || len(after) != 24 || app.has() != after {
			t.Fatalf("Dockwell keeps a password of %d characters, changed %v, the app's %v", len(after), after != before, app.has() == after)
		}
		if owner := o.install().GetOwner(); owner.GetRotating() || owner.GetError() != "" {
			t.Errorf("once set, the owner reads %v", owner)
		}
		changes, err := o.c.ListChanges(t.Context(), connect.NewRequest(&dockwellv1.ListChangesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		text := prototext.Format(changes.Msg)
		if strings.Contains(text, after) || strings.Contains(text, before) {
			t.Error("a change holds the owner's password")
		}
		if !strings.Contains(text, "Keep thing's new owner password") {
			t.Error("no change says the new password was kept")
		}
		var st store.AppOwnerState
		_ = o.f.store.View(t.Context(), func(tx *store.Tx) error {
			list, err := tx.AppOwnersRotating(t.Context())
			if len(list) > 0 {
				st = list[0]
			}
			return err
		})
		if st.Rotation != "" || len(st.Pending) > 0 {
			t.Errorf("a set password is still kept as pending: %q", st.Rotation)
		}
	})
}

// Where the reset's answer is lost, Dockwell keeps both passwords, and reads which
// one signs in before it tries again; where the app says no, the current one
// stays, with what the app said, scrubbed (ADR 0105, L5).
func TestANewOwnersPasswordKeepsBothUntilOneSignsIn(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		o, app := madeWithPasswords(t, backend, resetTop)
		var mu sync.Mutex
		now := time.Now()
		o.f.server.owners.SetNow(func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		})
		later := func() {
			mu.Lock()
			defer mu.Unlock()
			now = now.Add(owners.RotateAfter + time.Second)
		}
		before := o.ownerPassword()
		app.reset = "lost"
		o.rotateOwner()
		o.look()
		if !o.install().GetOwner().GetRotating() || o.ownerPassword() != before {
			t.Fatal("a lost answer ended the new password, or put it in place unread")
		}
		set := app.has()
		later()
		o.look()
		if o.install().GetOwner().GetRotating() || o.ownerPassword() != set || set == before {
			t.Fatal("the new password signs in, and Dockwell didn't keep it")
		}

		app.reset = "refuse"
		o.rotateOwner()
		o.look()
		owner := o.install().GetOwner()
		if owner.GetRotating() || o.ownerPassword() != set || app.has() != set {
			t.Fatalf("the app said no, and the owner reads %v, its password changed %v", owner, o.ownerPassword() != set)
		}
		sent := app.sent[len(app.sent)-1]["OWNER_PASSWORD"]
		if !strings.Contains(owner.GetError(), "no password") || strings.Contains(owner.GetError(), sent) {
			t.Errorf("the owner says %q, want what the app said without the password", owner.GetError())
		}
	})
}

// An entry without reset steps sets no new password: the person changes it in the
// app (ADR 0105).
func TestAnEntryWithoutResetStepsSetsNoNewPassword(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		o, _ := madeWithPasswords(t, backend, ownerTop)
		proof := stepUpFor(t, o.b, dockwellv1.StepUpAction_STEP_UP_ACTION_RESET_CATALOG_OWNER_PASSWORD, o.install().GetProjectId())
		if _, err := o.cat.ResetCatalogOwnerPassword(t.Context(), connect.NewRequest(&dockwellv1.ResetCatalogOwnerPasswordRequest{Id: "thing", Proof: proof})); reasonOf(err) != "no_reset" {
			t.Errorf("without reset steps: %v", err)
		}
	})
}
