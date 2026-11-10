// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/identity"
	"github.com/dockwell/dockwell/internal/pki"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
	"github.com/dockwell/dockwell/internal/ticket"
)

func TestAJoinTicketIsMadeInThePanel(t *testing.T) {
	storetest.Each(t, testAJoinTicketIsMadeInThePanel)
}

// A person makes a ticket from the panel with a passkey just now; it works once,
// says where it leads, and reads as joined, with the server's name, once a server
// uses it. It's on the audit log as dockwell token create's is.
func testAJoinTicketIsMadeInThePanel(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	servers := dockwellv1connect.NewServerServiceClient(b, b.origin)
	// As dockwell run records it on a server with only a private address.
	if err := f.ident.SetAgentURL(t.Context(), "https://10.0.0.4:7601"); err != nil {
		t.Fatal(err)
	}
	create := func(proof string, ttl uint32) (*dockwellv1.CreateJoinTicketResponse, error) {
		res, err := servers.CreateJoinTicket(t.Context(), connect.NewRequest(&dockwellv1.CreateJoinTicketRequest{Proof: proof, TtlSeconds: ttl}))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}
	get := func(id string) (*dockwellv1.JoinTicket, error) {
		res, err := servers.GetJoinTicket(t.Context(), connect.NewRequest(&dockwellv1.GetJoinTicketRequest{Id: id}))
		if err != nil {
			return nil, err
		}
		return res.Msg.GetTicket(), nil
	}

	if _, err := create("", 0); reasonOf(err) != authapi.ReasonNeedsStepUp {
		t.Fatalf("without a passkey: %v", err)
	}
	// A proof for something else makes no ticket.
	if _, err := create(stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_CREATE_TOKEN, ""), 0); reasonOf(err) != authapi.ReasonNeedsStepUp {
		t.Errorf("with a proof for a token: %v", err)
	}
	// Longer than a day is refused, and the proof it came with stays good for the next.
	proof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_ADD_SERVER, "")
	if _, err := create(proof, 2*24*3600); reasonOf(err) != "invalid" || !strings.Contains(err.Error(), "24 h at most") {
		t.Errorf("a ticket for two days: %v", err)
	}
	before := time.Now()
	made, err := create(proof, 0)
	if err != nil {
		t.Fatal(err)
	}
	// The proof works once.
	if _, err := create(proof, 0); reasonOf(err) != authapi.ReasonNeedsStepUp {
		t.Errorf("the same proof again: %v", err)
	}
	tk := made.GetTicket()
	if !strings.HasPrefix(tk.GetId(), "jtk_") || tk.GetState() != dockwellv1.JoinTicketState_JOIN_TICKET_STATE_WAITING {
		t.Errorf("the ticket reads %v", tk)
	}
	if left := tk.GetExpireTime().AsTime().Sub(before); left < 59*time.Minute || left > 61*time.Minute {
		t.Errorf("it works for %s, want an hour", left)
	}
	// A private address reaches only its own network.
	if len(tk.GetAddresses()) != 1 || tk.GetAddresses()[0].GetUrl() != "https://10.0.0.4:7601" ||
		tk.GetAddresses()[0].GetReach() != dockwellv1.JoinReach_JOIN_REACH_SAME_NETWORK {
		t.Errorf("it leads to %v", tk.GetAddresses())
	}
	decoded, err := ticket.Decode(made.GetValue())
	if err != nil || decoded.GetTokenId() != tk.GetId() || decoded.GetControlUrl() != tk.GetAddresses()[0].GetUrl() {
		t.Fatalf("the ticket's value decodes to %v, %v", decoded, err)
	}

	read, err := get(tk.GetId())
	if err != nil || read.GetState() != dockwellv1.JoinTicketState_JOIN_TICKET_STATE_WAITING || read.GetServerId() != "" {
		t.Errorf("before a server uses it, it reads %v, %v", read, err)
	}
	// Once a person sets the server's public address, the next ticket leads with it.
	if err := f.ident.SetPublicAgentURLs(t.Context(), []identity.JoinURL{{URL: "https://203.0.113.7:7601", WayIn: true}, {URL: "https://198.51.100.9:7601"}}); err != nil {
		t.Fatal(err)
	}
	next, err := create(stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_ADD_SERVER, ""), 900)
	want := []dockwellv1.JoinReach{dockwellv1.JoinReach_JOIN_REACH_ANYWHERE, dockwellv1.JoinReach_JOIN_REACH_WAY_OUT, dockwellv1.JoinReach_JOIN_REACH_SAME_NETWORK}
	if err != nil || len(next.GetTicket().GetAddresses()) != len(want) {
		t.Fatalf("with public addresses, a new ticket reads %v, %v", next, err)
	}
	for i, a := range next.GetTicket().GetAddresses() {
		if a.GetReach() != want[i] {
			t.Errorf("address %d, %s, reaches %v, want %v", i, a.GetUrl(), a.GetReach(), want[i])
		}
	}
	if left := time.Until(next.GetTicket().GetExpireTime().AsTime()); left > 15*time.Minute || left < 14*time.Minute {
		t.Errorf("a ticket for 900 s works for %s", left)
	}
	if _, err := get("srv_nobody"); reasonOf(err) != "invalid" {
		t.Errorf("a server's ID: %v", err)
	}
	if _, err := get("jtk_" + strings.Repeat("0", 26)); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("an unknown ticket: %v", err)
	}

	_, csr, err := pki.NewServerKey()
	if err != nil {
		t.Fatal(err)
	}
	joined, err := f.ident.Join(t.Context(), decoded.GetTokenId(), decoded.GetSecret(), csr, "web-2", "198.51.100.20")
	if err != nil {
		t.Fatal(err)
	}
	read, err = get(tk.GetId())
	if err != nil || read.GetState() != dockwellv1.JoinTicketState_JOIN_TICKET_STATE_JOINED ||
		read.GetServerId() != joined.Server.ID || read.GetServerName() != "web-2" {
		t.Errorf("once web-2 joined, it reads %v, %v", read, err)
	}

	audit := dockwellv1connect.NewAuditServiceClient(b, b.origin)
	events, err := audit.ListAuditEvents(t.Context(), connect.NewRequest(&dockwellv1.ListAuditEventsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var made1 bool
	for _, e := range events.Msg.GetEvents() {
		if e.GetAction() == "join_token.create" && e.GetSubject() == tk.GetId() {
			made1 = e.GetSurface() == dockwellv1.Surface_SURFACE_PANEL
		}
	}
	if !made1 {
		t.Errorf("the audit log has no join_token.create for %s from the panel", tk.GetId())
	}
}
