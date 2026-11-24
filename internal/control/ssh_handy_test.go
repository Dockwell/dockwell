// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"golang.org/x/crypto/ssh"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/firewall"
	"github.com/dockwell/dockwell/internal/pki"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
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
