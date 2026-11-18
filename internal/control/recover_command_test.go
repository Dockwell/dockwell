// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"testing"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// Profile's Last resort is the signed-in person's own way back in, as the server
// says it (#801): the owner's is the control server's recover command, which signs
// in as the owner, and anyone else's names them with --user. Nobody is told
// another person's, and an install with no owner says none, as claiming applies.
func TestEachPersonIsToldTheirOwnRecoverCommand(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		bare := startFull(t, backend, nil)
		before, err := newBrowser(t, "http://"+bare.api, bare.api, nil).authClient().GetAuthState(t.Context(), connect.NewRequest(&dockwellv1.GetAuthStateRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		if got := before.Msg.GetRecoverCommand(); got != "" || before.Msg.GetClaimed() {
			t.Errorf("an install with no owner says recover with %q", got)
		}

		s := newInviteStand(t, backend)
		viewer, _ := s.joins(t, "ada@example.com", "Ada Park", dockwellv1.Role_ROLE_VIEWER)
		state := func(b *browser) *dockwellv1.GetAuthStateResponse {
			res, err := b.authClient().GetAuthState(t.Context(), connect.NewRequest(&dockwellv1.GetAuthStateRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			return res.Msg
		}
		if got := state(s.owner).GetRecoverCommand(); got != "sudo dockwell recover --data-dir /srv/dockwell" {
			t.Errorf("the owner is told %q", got)
		}
		if got := state(viewer).GetRecoverCommand(); got != "sudo dockwell recover --data-dir /srv/dockwell --user ada@example.com" {
			t.Errorf("a viewer is told %q", got)
		}
		// Someone not signed in is told nothing.
		if got := state(s.newcomer()).GetRecoverCommand(); got != "" {
			t.Errorf("a browser signed in as nobody is told %q", got)
		}
	})
}
