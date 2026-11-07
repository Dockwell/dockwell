// SPDX-License-Identifier: AGPL-3.0-only

package members

import (
	"slices"
	"testing"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/store"
)

// How someone signs in, in detail, is theirs: the list gives everyone the kinds of
// sign-in each member has and when they were last active, and only the person
// asking their own passkeys' addresses, codes and browsers (ADR 0048).
func TestOnlyYouSeeHowYouSignIn(t *testing.T) {
	u := &store.User{ID: "usr_1", Name: "Mira Chen", Email: "mira@example.com", Role: "owner"}
	in := auth.SignIn{Passkeys: 2, Addresses: []string{"localhost", "panel.example.com"}, Codes: true, RecoveryCodesLeft: 7}
	seen := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	sessions := []store.Session{{LastSeenAt: seen.Add(-time.Hour)}, {LastSeenAt: seen}}
	kinds := []dockwellv1.SignInKind{dockwellv1.SignInKind_SIGN_IN_KIND_PASSKEY, dockwellv1.SignInKind_SIGN_IN_KIND_AUTHENTICATOR_APP}

	you := member(u, in, sessions, true)
	if !slices.Equal(you.GetSignIn(), kinds) || you.GetPasskeys() != 2 || len(you.GetPasskeyAddresses()) != 2 ||
		!you.GetAuthenticator() || you.GetRecoveryCodesLeft() != 7 || you.GetSessions() != 2 || !you.GetLastActiveTime().AsTime().Equal(seen) {
		t.Errorf("you read as %v, want all of how you sign in", you)
	}

	other := member(u, in, sessions, false)
	if !slices.Equal(other.GetSignIn(), kinds) || !other.GetLastActiveTime().AsTime().Equal(seen) || other.GetUser().GetEmail() != "mira@example.com" {
		t.Errorf("someone else reads as %v, want who they are, their kinds of sign-in and when they were last active", other)
	}
	if other.GetPasskeys() != 0 || other.GetPasskeyAddresses() != nil || other.GetAuthenticator() || other.GetRecoveryCodesLeft() != 0 ||
		other.GetSessions() != 0 || other.GetCodesLocked() {
		t.Errorf("someone else reads as %v, want none of how they sign in in detail", other)
	}

	locked := member(u, auth.SignIn{Passkeys: 1, Addresses: []string{"localhost"}, CodesLocked: true}, nil, true)
	if !slices.Equal(locked.GetSignIn(), kinds[:1]) || locked.GetAuthenticator() || !locked.GetCodesLocked() || locked.GetLastActiveTime() != nil {
		t.Errorf("with codes locked, you read as %v, want a passkey alone, and the lock", locked)
	}
}
