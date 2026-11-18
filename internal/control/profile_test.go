// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"strings"
	"testing"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A viewer changes their own name, and their own email with a passkey, through the
// API, and nobody else's: the request names no one, as it's always the caller's.
// An email that's taken or invited is refused by its reason, and the owner reads
// each change in the audit log, which the viewer can't (#801).
func TestAViewerChangesOnlyTheirOwnProfile(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newInviteStand(t, backend)
		viewer, viewerID := s.joins(t, "ada@example.com", "Ada Park", dockwellv1.Role_ROLE_VIEWER)
		if _, err := s.invite(t, "lee@example.com", dockwellv1.Role_ROLE_VIEWER, "", ""); err != nil {
			t.Fatal(err)
		}
		a := viewer.authClient()
		update := func(req *dockwellv1.UpdateProfileRequest) (*dockwellv1.User, error) {
			res, err := a.UpdateProfile(t.Context(), connect.NewRequest(req))
			if err != nil {
				return nil, err
			}
			return res.Msg.GetUser(), nil
		}
		ownerState := func() *dockwellv1.User {
			res, err := s.owner.authClient().GetAuthState(t.Context(), connect.NewRequest(&dockwellv1.GetAuthStateRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			return res.Msg.GetUser()
		}

		name := "Ada Okafor"
		u, err := update(&dockwellv1.UpdateProfileRequest{Name: &name})
		if err != nil || u.GetId() != viewerID || u.GetName() != "Ada Okafor" || u.GetEmail() != "ada@example.com" {
			t.Fatalf("a viewer changing their name: %v, %v", u, err)
		}
		email := "ada.park@example.com"
		if _, err := update(&dockwellv1.UpdateProfileRequest{Email: &email}); reasonOf(err) != authapi.ReasonNeedsStepUp {
			t.Fatalf("a new email with no passkey: %v, want needs_step_up", err)
		}
		proof := stepUpFor(t, viewer, dockwellv1.StepUpAction_STEP_UP_ACTION_CHANGE_EMAIL, email)
		if u, err = update(&dockwellv1.UpdateProfileRequest{Email: &email, Proof: proof}); err != nil || u.GetEmail() != email {
			t.Fatalf("a new email with the passkey: %v, %v", u, err)
		}
		for _, tt := range []struct{ email, reason string }{
			{"Mira@Acme.dev", "member_exists"},
			{"lee@example.com", "email_invited"},
		} {
			proof := stepUpFor(t, viewer, dockwellv1.StepUpAction_STEP_UP_ACTION_CHANGE_EMAIL, strings.ToLower(tt.email))
			_, err := update(&dockwellv1.UpdateProfileRequest{Email: &tt.email, Proof: proof})
			if connect.CodeOf(err) != connect.CodeAlreadyExists || reasonOf(err) != tt.reason {
				t.Errorf("the email %s: %v, want %s", tt.email, err, tt.reason)
			}
		}
		bad := "ada at example.com"
		if _, err := update(&dockwellv1.UpdateProfileRequest{Email: &bad}); reasonOf(err) != "invalid" {
			t.Errorf("what isn't an email: %v, want invalid", err)
		}

		if o := ownerState(); o.GetName() != "Mira Chen" || o.GetEmail() != "mira@acme.dev" {
			t.Errorf("the owner is %s <%s> after the viewer's changes", o.GetName(), o.GetEmail())
		}
		events, err := dockwellv1connect.NewAuditServiceClient(s.owner, s.origin).ListAuditEvents(t.Context(), connect.NewRequest(&dockwellv1.ListAuditEventsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, e := range events.Msg.GetEvents() {
			if e.GetAction() == "account.name" || e.GetAction() == "account.email" {
				if e.GetSubject() != viewerID {
					t.Errorf("%s is about %s, want the viewer", e.GetAction(), e.GetSubject())
				}
				got[e.GetAction()] = e.GetDetail()
			}
		}
		if got["account.name"] != `from "Ada Park" to "Ada Okafor"` || got["account.email"] != "from ada@example.com to ada.park@example.com" || len(got) != 2 {
			t.Errorf("the audit log's profile changes = %v", got)
		}
		if _, err := dockwellv1connect.NewAuditServiceClient(viewer, s.origin).ListAuditEvents(t.Context(), connect.NewRequest(&dockwellv1.ListAuditEventsRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("the viewer reading the audit log: %v, want it refused", err)
		}
	})
}
