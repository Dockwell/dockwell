// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/crypto/ssh"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent/access"
	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/firewall"
	"github.com/dockwell/dockwell/internal/pki"
	"github.com/dockwell/dockwell/internal/resource"
	rulekind "github.com/dockwell/dockwell/internal/resource/firewall"
	"github.com/dockwell/dockwell/internal/resource/sshconfig"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func TestSSHOperationsTakeAProofForTheReviewedTarget(t *testing.T) {
	t.Parallel()
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	key, csr, err := pki.NewServerKey()
	if err != nil {
		t.Fatal(err)
	}
	joined, err := f.ident.EnrollLocal(t.Context(), csr, "ssh-fixture")
	if err != nil {
		t.Fatal(err)
	}
	server := joined.Server.ID
	identity := enroll.New(server, joined.Server.Name, "https://"+f.agentAddr, key, joined.Cert, f.ident.CA().Certificate())
	client, release, err := identity.Client()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	stream := client.Connect(t.Context())
	defer func() { _ = stream.CloseRequest(); _ = stream.CloseResponse() }()
	if err := stream.Send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Hello{Hello: &agentv1.Hello{ManagesSsh: true}}}); err != nil {
		t.Fatal(err)
	}
	if msg, err := stream.Receive(); err != nil || msg.GetWelcome() == nil {
		t.Fatalf("agent welcome: %v, %v", msg, err)
	}
	waitConnected(t, f, server, true)

	testKey, err := ssh.NewPublicKey(ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)).Public())
	if err != nil {
		t.Fatal(err)
	}
	fp := ssh.FingerprintSHA256(testKey)
	public := string(ssh.MarshalAuthorizedKey(testKey))
	calls := make(chan string, 8)
	go func() {
		for {
			msg, err := stream.Receive()
			if err != nil {
				return
			}
			call := msg.GetCall()
			if call == nil {
				continue
			}
			result := &agentv1.CallResult{Id: call.GetId()}
			switch {
			case call.GetEditSshKey() != nil:
				request := call.GetEditSshKey()
				operation := "remove"
				if request.GetPublicKey() != "" {
					operation = "add"
				}
				calls <- operation + ":" + request.GetAccount() + ":" + request.GetProof()
				result.Result = &agentv1.CallResult_EditSshKey{EditSshKey: &dockwellv1.EditServerSshKeyResponse{Key: &dockwellv1.SshKey{Fingerprint: fp}}}
			case call.GetDisconnectSshSession() != nil:
				request := call.GetDisconnectSshSession()
				calls <- request.GetSessionId() + ":" + request.GetProof()
				switch request.GetSessionId() {
				case "protected":
					result.Result = &agentv1.CallResult_Error{Error: &agentv1.CallError{Message: "protected session", Plain: "protected session", Reason: resource.ReasonSSHRefused}}
				case "uncertain":
					result.Result = &agentv1.CallResult_Error{Error: &agentv1.CallError{Message: "deadline after dispatch", OutOfTime: true}}
				default:
					result.Result = &agentv1.CallResult_DisconnectSshSession{DisconnectSshSession: &dockwellv1.DisconnectServerSshSessionResponse{}}
				}
			case call.GetBanSshAddress() != nil:
				request := call.GetBanSshAddress()
				calls <- request.GetAddress() + ":" + request.GetProof()
				result.Result = &agentv1.CallResult_BanSshAddress{BanSshAddress: &dockwellv1.BanServerSshAddressResponse{}}
			default:
				result.Result = &agentv1.CallResult_Error{Error: &agentv1.CallError{Message: "unexpected agent call"}}
			}
			if err := stream.Send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Result{Result: result}}); err != nil {
				return
			}
		}
	}()

	firewalls := dockwellv1connect.NewFirewallServiceClient(b, b.origin)
	edit := func(account, proof string, add bool) error {
		r := &dockwellv1.EditServerSshKeyRequest{ServerId: server, Account: account, Proof: proof}
		if add {
			r.Edit = &dockwellv1.EditServerSshKeyRequest_PublicKey{PublicKey: public}
		} else {
			r.Edit = &dockwellv1.EditServerSshKeyRequest_RemoveFingerprint{RemoveFingerprint: fp}
		}
		_, err := firewalls.EditServerSshKey(t.Context(), connect.NewRequest(r))
		return err
	}
	disconnect := func(id, proof string) error {
		_, err := firewalls.DisconnectServerSshSession(t.Context(), connect.NewRequest(&dockwellv1.DisconnectServerSshSessionRequest{ServerId: server, SessionId: id, Proof: proof}))
		return err
	}
	ban := func(proof string) error {
		_, err := firewalls.BanServerSshAddress(t.Context(), connect.NewRequest(&dockwellv1.BanServerSshAddressRequest{ServerId: server, Address: "203.0.113.9", Proof: proof}))
		return err
	}
	for _, tc := range []struct {
		call   func(string) error
		want   string
		prefix string
		code   connect.Code
	}{
		{func(proof string) error { return edit("deploy", proof, true) }, "add:deploy:", server + ":deploy:add:" + fp, 0},
		{func(proof string) error { return edit("deploy", proof, false) }, "remove:deploy:", server + ":deploy:remove:" + fp, 0},
		{func(proof string) error { return disconnect("session-1", proof) }, "session-1:", server + ":session-1", 0},
		{ban, "203.0.113.9:", server + ":203.0.113.9", 0},
		{func(proof string) error { return disconnect("protected", proof) }, "protected:", server + ":protected", connect.CodeFailedPrecondition},
		{func(proof string) error { return disconnect("uncertain", proof) }, "uncertain:", server + ":uncertain", connect.CodeUnavailable},
	} {
		needed := sshStepUpNeeded(t, tc.call(""))
		if !strings.HasPrefix(needed.GetTarget(), tc.prefix) {
			t.Fatalf("target %q doesn't bind %q", needed.GetTarget(), tc.prefix)
		}
		wrong := stepUpFor(t, b, needed.GetAction(), needed.GetTarget()+"-elsewhere")
		sshStepUpNeeded(t, tc.call(wrong))
		proof := stepUpFor(t, b, needed.GetAction(), needed.GetTarget())
		if err := tc.call(proof); (tc.code == 0 && err != nil) || (tc.code != 0 && connect.CodeOf(err) != tc.code) {
			t.Fatalf("%s returned %v, want code %v", tc.want, err, tc.code)
		}
		select {
		case got := <-calls:
			if got != tc.want {
				t.Fatalf("agent received %q, want %q", got, tc.want)
			}
		default:
			t.Fatalf("%s never reached the agent", tc.want)
		}
	}

	events := map[string][]store.Event{}
	if err := f.store.View(t.Context(), func(tx *store.Tx) error {
		rows, err := tx.ListEvents(t.Context(), 100)
		for _, row := range rows {
			events[row.Action] = append(events[row.Action], row)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{firewall.EventSSHKeyAdded, firewall.EventSSHKeyRemoved, firewall.EventSSHDisconnected, firewall.EventSSHAddressBanned, firewall.EventSSHUnconfirmed} {
		if len(events[action]) != 1 {
			t.Errorf("Activity has %d %s rows, want 1", len(events[action]), action)
		}
	}
	if rows := events[firewall.EventSSHUnconfirmed]; len(rows) == 1 && rows[0].Subject != "uncertain" {
		t.Errorf("unconfirmed activity recorded %q", rows[0].Subject)
	}
}

func sshStepUpNeeded(t *testing.T, err error) *dockwellv1.StepUpNeeded {
	t.Helper()
	var wire *connect.Error
	if !errors.As(err, &wire) || reasonOf(err) != authapi.ReasonNeedsStepUp {
		t.Fatalf("want SSH step-up: %v", err)
	}
	for _, detail := range wire.Details() {
		if value, err := detail.Value(); err == nil {
			if needed, ok := value.(*dockwellv1.StepUpNeeded); ok {
				return needed
			}
		}
	}
	t.Fatal("SSH step-up response had no target")
	return nil
}

type sshPortFixture struct{ port atomic.Uint32 }

func (*sshPortFixture) Plan(_ context.Context, before, next *resourcev1.SshConfig) (*access.SSHChange, error) {
	old, port := uint16(22), uint16(22)
	if before.GetPort() != 0 {
		old = uint16(before.GetPort())
	}
	if next.GetPort() != 0 {
		port = uint16(next.GetPort())
	}
	return &access.SSHChange{
		Account: "admin", OldPort: old, Port: port, Candidate: "trial", Final: "final",
		ProofPeer: netip.MustParseAddr("203.0.113.7"),
	}, nil
}

func (*sshPortFixture) Arm(context.Context, string, string, time.Duration, *access.SSHChange) error {
	return nil
}

func (*sshPortFixture) Rearm(context.Context, string, string, time.Duration, *access.SSHChange) error {
	return nil
}

func (s *sshPortFixture) Install(_ context.Context, trial *access.SSHChange, _ string) error {
	s.port.Store(uint32(trial.Port))
	return nil
}

func (s *sshPortFixture) Restore(_ context.Context, trial *access.SSHChange) error {
	s.port.Store(uint32(trial.OldPort))
	return nil
}

func (*sshPortFixture) Started() (uint64, error) { return 1, nil }

func (*sshPortFixture) CandidateLive(*access.SSHChange) bool { return true }

func (*sshPortFixture) Proved(_ context.Context, trial *access.SSHChange) (*access.SSHProof, netip.Addr) {
	return &access.SSHProof{Address: trial.ProofPeer.String()}, netip.Addr{}
}

func (*sshPortFixture) Disarm(context.Context, string) error { return nil }

func TestSSHMoveKeepsThePortWhenEditingTheStoredFirewallRule(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		listener := &sshPortFixture{}
		listener.port.Store(22)
		srv, tbl := f.addFirewallServer(t, "ssh-fixture", time.Minute, func(s *testServer) {
			m := s.access.(*access.Manager)
			m.SSH = listener
			m.Pins = func(context.Context) (access.Pins, error) {
				return access.Pins{SSHPorts: []uint16{uint16(listener.port.Load())}, SSHFrom: []netip.Prefix{
					netip.MustParsePrefix("203.0.113.7/32"),
				}}, nil
			}
			s.drivers = s.drivers.With(&sshconfig.Driver{Backend: m})
		})
		server := srv.id.ServerID
		c := changeClient(b, b.origin)
		keep := func(ch *dockwellv1.Change) {
			t.Helper()
			watch := watchConfirm(t, c, ch.GetId())
			req := &dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint()}
			_, err := c.ApplyChange(t.Context(), connect.NewRequest(req))
			if reasonOf(err) == authapi.ReasonNeedsStepUp {
				needed := sshStepUpNeeded(t, err)
				req.Proof = stepUpFor(t, b, needed.GetAction(), needed.GetTarget())
				_, err = c.ApplyChange(t.Context(), connect.NewRequest(req))
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.ConfirmChange(t.Context(), confirmation(ch.GetId(), watch.next())); err != nil {
				t.Fatal(err)
			}
			if got := watch.end().last(); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
				t.Fatalf("change ended with %v, %s", got.GetState(), got.GetError())
			}
		}
		rule := ruleOn(server, "allow-ssh", 22)
		policy := &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(rulekind.PolicyKind), Scope: server, Name: "default"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_FirewallPolicy{FirewallPolicy: &resourcev1.FirewallPolicy{
				DenyEverythingElse: true,
			}}},
		}
		keep(propose(t, c, "Open SSH and deny other ports", rule, policy))
		move := propose(t, c, "Move SSH to port 2222", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(sshconfig.Kind), Scope: server, Name: "default"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_SshConfig{SshConfig: &resourcev1.SshConfig{
				Account: "admin", Port: 2222,
			}}},
		})
		plan := move.GetPlan().GetServers()[0].GetSteps()[0].GetSshPortMove()
		if plan.GetFromPort() != 22 || plan.GetToPort() != 2222 || !slices.Equal(plan.GetFirewallRules(), []string{"allow-ssh"}) {
			t.Fatalf("SSH move plan: %v", plan)
		}
		keep(move)

		var stored *resourcev1.FirewallRule
		if err := f.store.View(t.Context(), func(tx *store.Tx) error {
			row, err := tx.GetResource(t.Context(), string(rulekind.Kind), server, "allow-ssh")
			if err != nil {
				return err
			}
			spec, err := f.kinds.Decode(rulekind.Kind, row.Spec)
			if err != nil {
				return err
			}
			stored = spec.(*resourcev1.FirewallRule)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if stored.GetPort() != 2222 {
			t.Fatalf("stored rule kept TCP port %d", stored.GetPort())
		}
		stored.Sources = []string{"198.51.100.0/24"}
		rule.Spec = &resourcev1.Spec{Kind: &resourcev1.Spec_FirewallRule{FirewallRule: stored}}
		keep(propose(t, c, "Limit SSH to the office", rule))
		partial := ruleOn(server, "allow-ssh", 0)
		partial.Spec.GetFirewallRule().Sources = []string{"198.51.100.128/25"}
		keep(propose(t, c, "Narrow the office range without specifying a port", partial))
		partial.Spec.GetFirewallRule().Port = 22
		portEdit := propose(t, c, "Explicitly change the firewall rule to port 22", partial)
		changes := portEdit.GetPlan().GetServers()[0].GetSteps()[0].GetChanges()
		if !slices.ContainsFunc(changes, func(field *dockwellv1.FieldChange) bool {
			return field.GetPath() == "port" && field.GetFrom() == "2222" && field.GetTo() == "22"
		}) {
			t.Fatalf("plan did not show the explicit port change: %v", changes)
		}

		tbl.mu.Lock()
		defer tbl.mu.Unlock()
		for _, live := range tbl.loaded.Allows {
			if live.Name == "rule allow-ssh" && slices.Equal(live.Ports, []uint16{2222}) &&
				slices.Equal(live.Sources, []netip.Prefix{netip.MustParsePrefix("198.51.100.128/25")}) {
				return
			}
		}
		t.Fatalf("source edit did not keep SSH on 2222: %v", tbl.loaded.Allows)
	})
}
