// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent/enroll"
	firewallapi "github.com/dockwell/dockwell/internal/control/firewall"
	"github.com/dockwell/dockwell/internal/pki"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/fail2ban"
	"github.com/dockwell/dockwell/internal/store"
)

// addJailServer adds a server whose agent keeps fail2ban's jails in memory.
func (f *fullServer) addJailServer(t *testing.T, name string) (*testServer, *fail2ban.Fake) {
	t.Helper()
	key, csr, err := pki.NewServerKey()
	if err != nil {
		t.Fatal(err)
	}
	joined, err := f.ident.EnrollLocal(t.Context(), csr, name)
	if err != nil {
		t.Fatal(err)
	}
	jails := fail2ban.NewFake()
	s := &testServer{
		id:      enroll.New(joined.Server.ID, joined.Server.Name, "https://"+f.agentAddr, key, joined.Cert, f.ident.CA().Certificate()),
		drivers: resource.NewRegistry(&fail2ban.Driver{Backend: jails}),
		dataDir: t.TempDir(),
	}
	s.connect(t, f)
	return s, jails
}

func jailOn(server, name string, jail *resourcev1.Jail) *dockwellv1.Edit {
	return &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(fail2ban.Kind), Scope: server, Name: name},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Fail2BanJail{Fail2BanJail: jail}},
	}
}

// A jail decides who gets onto a server, so a change to one takes a signed-in
// person and their passkey, and no API token may apply it; it waits for no
// confirmation, as it can't lock anyone out of a new session (ADR 0052).
func TestAJailChangeTakesAPersonAndNoWait(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	srv, jails := f.addJailServer(t, "hz-1")
	c := changeClient(b, b.origin)
	edit := jailOn(srv.id.ServerID, "sshd", &resourcev1.Jail{Filter: "sshd", Backend: "systemd", Ports: []uint32{22}, MaxRetry: 3, BanTime: "1h"})

	byToken := propose(t, c, "Protect SSH", edit)
	token := createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
	script := changeClient(bearer{token: token, rt: b.client.Transport}, b.origin)
	if _, err := script.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: byToken.GetId()})); reasonOf(err) != "needs_session" {
		t.Errorf("a token applying a jail = %v, want needs_session", err)
	}
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: byToken.GetId()})); reasonOf(err) != "needs_step_up" {
		t.Errorf("applying a jail with no passkey = %v, want needs_step_up", err)
	}
	got := apply(t, c, byToken)
	if got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED || got.GetConfirmation() != nil {
		t.Errorf("the jail's change ends %v, waiting for %v", got.GetState(), got.GetConfirmation())
	}
	if j, _ := jails.Jail(t.Context(), "sshd"); j.GetMaxRetry() != 3 || j.GetBanTime() != "1h" {
		t.Errorf("the server holds %v", j)
	}
}

// A jail can't say more than its fields: fail2ban reads DEFAULT as every
// jail's settings, and a new line in a log file's path as the next setting,
// which can be a command it runs as root. Both are refused as they're planned.
func TestAJailThatSaysMoreThanAJailIsRefusedAtPlanning(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	srv, jails := f.addJailServer(t, "hz-1")
	c := changeClient(b, b.origin)
	for name, edit := range map[string]*dockwellv1.Edit{
		"every jail's section": jailOn(srv.id.ServerID, "DEFAULT", &resourcev1.Jail{Filter: "sshd", Backend: "systemd", MaxRetry: 1, BanForGood: true}),
		"an action in a log path": jailOn(srv.id.ServerID, "sshd", &resourcev1.Jail{
			Filter: "sshd", LogPaths: []string{"/var/log/auth.log\naction = dummy[actionstart=\"/usr/bin/touch /tmp/x\"]"},
		}),
		"a log outside the logs": jailOn(srv.id.ServerID, "sshd", &resourcev1.Jail{Filter: "sshd", LogPaths: []string{"/etc/shadow"}}),
	} {
		_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Protect SSH", Edits: []*dockwellv1.Edit{edit}}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: planned, %v", name, err)
		}
	}
	for _, name := range []string{"DEFAULT", "sshd"} {
		if j, _ := jails.Jail(t.Context(), name); j != nil {
			t.Errorf("the server holds %s: %v", name, j)
		}
	}
}

// A jail is the whole server's: below an admin nobody plans one by name,
// applies one an admin planned, or undoes one, as themselves or with an API
// token, whatever projects they hold. An admin's token may plan one, as any
// plan is only a plan, and can't apply it (ADR 0048, 0049, 0052).
func TestAJailIsAnAdminsHoweverTheChangeComes(t *testing.T) {
	s := newReachStand(t, store.SQLite)
	srv, jails := s.f.addJailServer(t, "hz-2")
	ctx := t.Context()
	jail := func(tries uint32) *dockwellv1.ProposeChangeRequest {
		return &dockwellv1.ProposeChangeRequest{Summary: "Protect SSH", Edits: []*dockwellv1.Edit{
			jailOn(srv.id.ServerID, "sshd", &resourcev1.Jail{Filter: "sshd", Backend: "systemd", Ports: []uint32{22}, MaxRetry: tries}),
		}}
	}
	token := createToken(t, s.b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
	script := changeClient(bearer{token: token, rt: s.b.client.Transport}, s.b.origin)

	// The owner's token plans one, and can't apply it; the owner applies it.
	byToken, err := script.ProposeChange(ctx, connect.NewRequest(jail(3)))
	if err != nil {
		t.Fatalf("an owner's token planning a jail: %v", err)
	}
	if _, err := script.ApplyChange(ctx, connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: byToken.Msg.GetChange().GetId()})); reasonOf(err) != "needs_session" {
		t.Errorf("an owner's token applying a jail = %v, want needs_session", err)
	}
	applied := apply(t, s.c, byToken.Msg.GetChange())
	if applied.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the owner applying it ends %v", applied.GetState())
	}
	waiting := propose(t, s.c, "Protect SSH harder", jail(2).GetEdits()...)

	// Now a developer in every project, and their token.
	s.hold(in(s.blogID, store.RoleDeveloper), in(s.defaultID, store.RoleDeveloper))
	for who, c := range map[string]dockwellv1connect.ChangeServiceClient{"a developer": s.c, "a developer's token": script} {
		// A jail that isn't there yet is an admin's to make, and says so.
		_, err := c.ProposeChange(ctx, connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Jail repeat offenders", Edits: []*dockwellv1.Edit{
			jailOn(srv.id.ServerID, "recidive", &resourcev1.Jail{Filter: "recidive", LogPaths: []string{"/var/log/fail2ban.log"}}),
		}}))
		needsRole(t, who+" planning a new jail", err)
		// One that's there is in no place they hold, so it answers as missing does,
		// which tells them nothing of it: changing it, undoing it, applying a plan of it.
		_, err = c.ProposeChange(ctx, connect.NewRequest(jail(1)))
		notFound(t, who+" changing the jail", err)
		_, err = c.UndoChange(ctx, connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: applied.GetId()}))
		notFound(t, who+" undoing the jail", err)
		_, err = c.ApplyChange(ctx, connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: waiting.GetId()}))
		notFound(t, who+" applying a plan of the jail", err)
	}
	if j, _ := jails.Jail(ctx, "sshd"); j.GetMaxRetry() != 3 {
		t.Errorf("the server holds %v, want the owner's jail untouched", j)
	}
}

// The read says the address the request came from, as the control plane sees
// it, for a jail's form to put in what it never bans; the server's own
// loopback is nobody's.
func TestTheCallersAddressIsTheirsOrNone(t *testing.T) {
	for addr, want := range map[string]string{"203.0.113.7:51234": "203.0.113.7", "[2001:db8::7]:51234": "2001:db8::7", "127.0.0.1:51234": "", "[::1]:4": "", "": "", "[::ffff:203.0.113.7]:9": "203.0.113.7"} {
		if got := firewallapi.CallersAddress(addr); got != want {
			t.Errorf("%q reads %q, want %q", addr, got, want)
		}
	}
}
