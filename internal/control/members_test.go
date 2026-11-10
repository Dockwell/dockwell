// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"testing"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// Who can sign in is the owner alone today, as they signed in: their passkey and
// where it works, their sessions, and no authenticator app. No token reads it,
// as it names people and their emails (ADR 0048).
func TestMembersAreTheOwnerAlone(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		c := dockwellv1connect.NewMemberServiceClient(b, b.origin, connect.WithProtoJSON())
		res, err := c.ListMembers(t.Context(), connect.NewRequest(&dockwellv1.ListMembersRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		members := res.Msg.GetMembers()
		if len(members) != 1 {
			t.Fatalf("members = %v, want the owner alone", members)
		}
		m := members[0]
		if m.GetUser().GetName() != "Mira Chen" || m.GetUser().GetEmail() != "mira@acme.dev" || m.GetUser().GetRole() != dockwellv1.Role_ROLE_OWNER || !m.GetYou() {
			t.Errorf("the owner reads %v", m.GetUser())
		}
		if m.GetPasskeys() != 1 || !slices.Equal(m.GetPasskeyAddresses(), []string{"localhost"}) || m.GetAuthenticator() || m.GetRecoveryCodesLeft() != 0 {
			t.Errorf("the owner signs in with %d passkeys at %v, authenticator %v, %d codes", m.GetPasskeys(), m.GetPasskeyAddresses(), m.GetAuthenticator(), m.GetRecoveryCodesLeft())
		}
		if !slices.Equal(m.GetSignIn(), []dockwellv1.SignInKind{dockwellv1.SignInKind_SIGN_IN_KIND_PASSKEY}) || m.GetCodesLocked() {
			t.Errorf("the owner's kinds of sign-in are %v, codes locked %v, want a passkey", m.GetSignIn(), m.GetCodesLocked())
		}
		if m.GetSessions() != 1 || m.GetLastActiveTime() == nil {
			t.Errorf("the owner has %d sessions, last active %v", m.GetSessions(), m.GetLastActiveTime())
		}

		for _, scope := range []dockwellv1.TokenScope{dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE} {
			script := bearer{token: createToken(t, b, scope), rt: b.client.Transport}
			_, err := dockwellv1connect.NewMemberServiceClient(script, b.origin, connect.WithProtoJSON()).
				ListMembers(t.Context(), connect.NewRequest(&dockwellv1.ListMembersRequest{}))
			if connect.CodeOf(err) != connect.CodePermissionDenied || reasonOf(err) != "needs_session" {
				t.Errorf("a %v token listing members = %v, want needs_session", scope, err)
			}
		}
	})
}
