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
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/firewall"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A database's external access is a plan of three edits that changes nothing until
// it's applied: its port on every address, public on purpose, and two rules that let
// the listed addresses in and keep everyone else out. The plan says the connection is
// unencrypted. An empty list, or anywhere, is refused, and turning it off takes all
// three away in one change (ADR 0038, "External access, as built").
func TestADatabasesExternalAccessIsThreeEditsThatGoTogether(t *testing.T) {
	t.Parallel()
	storetest.Each(t, testADatabasesExternalAccessIsThreeEditsThatGoTogether)
}

func testADatabasesExternalAccessIsThreeEditsThatGoTogether(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	f.addServerWith(t, func() []resource.Type { return []resource.Type{&firewall.Driver{Backend: firewall.NewFake()}} })
	ch := changeClient(b, b.origin)
	apply(t, ch, propose(t, ch, "Run the database", secretSpec("DB_PASSWORD", "first-password"),
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Name: "db"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
				Runs:    &resourcev1.Service_App{App: &resourcev1.Container{Image: "postgres:17"}},
				Secrets: map[string]string{"POSTGRES_PASSWORD": "DB_PASSWORD"},
			}}},
		}))
	dc := databaseClient(b, b.origin)
	set := func(req *dockwellv1.SetDatabaseExternalAccessRequest) (*dockwellv1.Change, error) {
		res, err := dc.SetDatabaseExternalAccess(t.Context(), connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		return res.Msg.GetChange(), nil
	}

	// Never anywhere: an empty list, or one that's everyone, is refused at its field.
	for _, allow := range [][]string{nil, {"0.0.0.0/0"}, {"::/0"}, {"not an address"}} {
		if _, err := set(&dockwellv1.SetDatabaseExternalAccessRequest{Id: "db", AllowFrom: allow}); reasonOf(err) != "invalid_allow_from" {
			t.Errorf("letting %v in = %v, want invalid_allow_from", allow, err)
		}
	}
	if _, err := set(&dockwellv1.SetDatabaseExternalAccessRequest{Id: "db", Off: true}); reasonOf(err) != "not_open" {
		t.Errorf("turning off what isn't on = %v, want not_open", err)
	}

	on, err := set(&dockwellv1.SetDatabaseExternalAccessRequest{Id: "db", AllowFrom: []string{"203.0.113.7", "203.0.113.0/24", "203.0.113.7"}})
	if err != nil {
		t.Fatal(err)
	}
	if on.GetState() != dockwellv1.ChangeState_CHANGE_STATE_PLANNED || on.GetSummary() != "Let 203.0.113.7/32 and 203.0.113.0/24 reach db on port 5432" {
		t.Errorf("turning it on plans %v %q", on.GetState(), on.GetSummary())
	}
	var kinds []string
	for _, r := range on.GetPlan().GetResources() {
		kinds = append(kinds, r.GetResource().GetKind()+" "+strings.SplitN(r.GetResource().GetName(), "-", 3)[0]+" "+r.GetAction().String())
	}
	slices.Sort(kinds)
	// Its container takes the port again, so the service is replaced.
	if !slices.Equal(kinds, []string{"firewall.rule external PLAN_ACTION_CREATE", "firewall.rule external PLAN_ACTION_CREATE", "service db PLAN_ACTION_REPLACE"}) {
		t.Errorf("the plan changes %v", kinds)
	}
	public, unencrypted := false, false
	for _, r := range on.GetPlan().GetResources() {
		for _, a := range r.GetHostAccess() {
			public = public || a.GetKind() == dockwellv1.HostAccessKind_HOST_ACCESS_KIND_PUBLIC_PORT && a.GetPath() == "5432"
		}
	}
	for _, s := range on.GetPlan().GetServers() {
		for _, step := range s.GetSteps() {
			for _, a := range step.GetHostAccess() {
				public = public || a.GetKind() == dockwellv1.HostAccessKind_HOST_ACCESS_KIND_PUBLIC_PORT
			}
		}
	}
	for _, n := range on.GetPlan().GetNotes() {
		unencrypted = unencrypted || n.GetReason() == "unencrypted"
	}
	if !public || !unencrypted {
		t.Errorf("the plan says its port is public %t, and that it's unencrypted %t", public, unencrypted)
	}
	// Nothing changes until it's applied.
	read, err := dc.GetDatabase(t.Context(), connect.NewRequest(&dockwellv1.GetDatabaseRequest{Id: "db"}))
	if err != nil {
		t.Fatal(err)
	}
	if read.Msg.GetDatabase().GetExternalAccess() != nil {
		t.Errorf("a planned change already gave db external access %v", read.Msg.GetDatabase().GetExternalAccess())
	}
	// No API token may apply it: its rules change who reaches the server.
	token := createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
	script := changeClient(bearer{token: token, rt: b.client.Transport}, b.origin)
	if _, err := script.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: on.GetId(), Fingerprint: on.GetPlan().GetFingerprint()})); reasonOf(err) != "needs_session" {
		t.Errorf("a token applying it = %v, want needs_session", err)
	}
	// A person applies it with the plan's fingerprint and their passkey.
	if _, err := ch.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: on.GetId(), Fingerprint: on.GetPlan().GetFingerprint()})); reasonOf(err) != "needs_step_up" {
		t.Errorf("applying it with no passkey = %v, want needs_step_up", err)
	}
	if done := applyWithPasskey(t, ch, on); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("turning it on ended %v: %v", done.GetState(), done.GetError())
	}
	read, err = dc.GetDatabase(t.Context(), connect.NewRequest(&dockwellv1.GetDatabaseRequest{Id: "db"}))
	if err != nil {
		t.Fatal(err)
	}
	got := read.Msg.GetDatabase().GetExternalAccess()
	if got.GetPort() != 5432 || !slices.Equal(got.GetAllowFrom(), []string{"203.0.113.0/24", "203.0.113.7/32"}) {
		t.Errorf("db's external access reads %v", got)
	}

	off, err := set(&dockwellv1.SetDatabaseExternalAccessRequest{Id: "db", Off: true})
	if err != nil {
		t.Fatal(err)
	}
	kinds = nil
	for _, r := range off.GetPlan().GetResources() {
		kinds = append(kinds, r.GetResource().GetKind()+" "+r.GetAction().String())
	}
	slices.Sort(kinds)
	if !slices.Equal(kinds, []string{"firewall.rule PLAN_ACTION_DELETE", "firewall.rule PLAN_ACTION_DELETE", "service PLAN_ACTION_REPLACE"}) {
		t.Errorf("turning it off changes %v", kinds)
	}
	if done := applyWithPasskey(t, ch, off); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("turning it off ended %v: %v", done.GetState(), done.GetError())
	}
	read, err = dc.GetDatabase(t.Context(), connect.NewRequest(&dockwellv1.GetDatabaseRequest{Id: "db"}))
	if err != nil {
		t.Fatal(err)
	}
	if read.Msg.GetDatabase().GetExternalAccess() != nil {
		t.Errorf("db still reads as reached from beyond its server: %v", read.Msg.GetDatabase().GetExternalAccess())
	}
}

// applyWithPasskey applies a change as a person who reviewed its plan does: with its
// fingerprint, and their passkey where it takes one.
func applyWithPasskey(t *testing.T, c dockwellv1connect.ChangeServiceClient, ch *dockwellv1.Change) *dockwellv1.Change {
	t.Helper()
	w := watch(t, c, ch.GetId())
	if _, err := applyChange(t, c, &dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint()}); err != nil {
		t.Fatalf("ApplyChange %s: %v", ch.GetSummary(), err)
	}
	return waitWatch(t, w).last()
}

// The rules are a server's, so turning external access on is an admin's: a developer
// in the project is refused, as the roles table says (#849's M1). And a list that
// reaches everyone, however it's written, is refused at its field (#849's M2).
func TestOnlyAnAdminGivesADatabaseExternalAccess(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		f.addServerWith(t, func() []resource.Type { return []resource.Type{&firewall.Driver{Backend: firewall.NewFake()}} })
		ch := changeClient(b, b.origin)
		apply(t, ch, propose(t, ch, "Run the database", secretSpec("DB_PASSWORD", "first-password"), postgresEdit()))
		dc := databaseClient(b, b.origin)
		set := func(allow ...string) error {
			_, err := dc.SetDatabaseExternalAccess(t.Context(), connect.NewRequest(&dockwellv1.SetDatabaseExternalAccessRequest{Id: "db", AllowFrom: allow}))
			return err
		}
		for _, allow := range [][]string{{"::ffff:0.0.0.0/96"}, {"0.0.0.0/1", "128.0.0.0/1"}, {"::/1", "8000::/1"}} {
			if err := set(allow...); reasonOf(err) != "invalid_allow_from" {
				t.Errorf("letting %v in = %v, want invalid_allow_from", allow, err)
			}
		}
		hold := aMember(t, f)
		hold(store.Grant{ProjectID: envProjectByName(t, f, scope.DefaultProject), Role: store.RoleDeveloper})
		if err := set("203.0.113.7"); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("a developer planning external access = %v, want it refused as an admin's", err)
		}
	})
}

// A database that publishes its engine's port beyond its server already has that port
// held to the list, not a second one opened beside it (#849's M5). And external access
// from before the tag, the port with rules but no tag, still reads as on, and turns off
// whole: the port and both rules (#849's H1, "turning it off from any state").
func TestExternalAccessTakesOnThePortThereAndTurnsOffFromAnyState(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		srv := f.addServerWith(t, func() []resource.Type { return []resource.Type{&firewall.Driver{Backend: firewall.NewFake()}} })
		_ = srv
		ch := changeClient(b, b.origin)
		open := postgresEdit()
		open.Spec.GetService().GetApp().Ports = []*resourcev1.PortMapping{{HostPort: 5432, ContainerPort: 5432}}
		applyWithPasskey(t, ch, propose(t, ch, "Run the database on every address", secretSpec("DB_PASSWORD", "first-password"), open))
		dc := databaseClient(b, b.origin)
		on, err := dc.SetDatabaseExternalAccess(t.Context(), connect.NewRequest(&dockwellv1.SetDatabaseExternalAccessRequest{Id: "db", AllowFrom: []string{"203.0.113.0/24"}}))
		if err != nil {
			t.Fatal(err)
		}
		if got := on.Msg.GetChange().GetSummary(); got != "Let 203.0.113.0/24 reach db on port 5432" {
			t.Errorf("taking on the port it publishes plans %q, want port 5432", got)
		}
		for _, r := range on.Msg.GetChange().GetPlan().GetResources() {
			for _, c := range r.GetChanges() {
				if strings.Contains(c.GetTo(), "5433") {
					t.Errorf("turning it on opens another port: %v", c)
				}
			}
		}
		if done := applyWithPasskey(t, ch, on.Msg.GetChange()); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("turning it on ended %v: %v", done.GetState(), done.GetError())
		}

		// The tag taken away by hand, as a version from before it would leave things:
		// the port with its rules reads as on, and Off takes the port and both rules.
		untag := postgresEdit()
		untag.Spec.GetService().GetApp().Ports = []*resourcev1.PortMapping{{HostPort: 5432, ContainerPort: 5432}}
		applyWithPasskey(t, ch, propose(t, ch, "Take the tag away", untag))
		read, err := dc.GetDatabase(t.Context(), connect.NewRequest(&dockwellv1.GetDatabaseRequest{Id: "db"}))
		if err != nil {
			t.Fatal(err)
		}
		if got := read.Msg.GetDatabase().GetExternalAccess(); got.GetPort() != 5432 {
			t.Errorf("a port with its rules and no tag reads as %v, want it on, so it can be turned off", got)
		}
		off, err := dc.SetDatabaseExternalAccess(t.Context(), connect.NewRequest(&dockwellv1.SetDatabaseExternalAccessRequest{Id: "db", Off: true}))
		if err != nil {
			t.Fatal(err)
		}
		if done := applyWithPasskey(t, ch, off.Msg.GetChange()); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("turning it off ended %v: %v", done.GetState(), done.GetError())
		}
		read, err = dc.GetDatabase(t.Context(), connect.NewRequest(&dockwellv1.GetDatabaseRequest{Id: "db"}))
		if err != nil {
			t.Fatal(err)
		}
		if got := read.Msg.GetDatabase().GetExternalAccess(); got != nil {
			t.Errorf("after Off, db still reads as reached from beyond its server: %v", got)
		}
	})
}

// postgresEdit runs db on postgres:17 with its password in DB_PASSWORD.
func postgresEdit() *dockwellv1.Edit {
	return &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Name: "db"},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
			Runs:    &resourcev1.Service_App{App: &resourcev1.Container{Image: "postgres:17"}},
			Secrets: map[string]string{"POSTGRES_PASSWORD": "DB_PASSWORD"},
		}}},
	}
}
