// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
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
			// The review's: a Cyrillic і, which reads as the owner's (#801's review, M1).
			{"mіra@acme.dev", "email_lookalike"},
			{"Іee@example.com", "email_lookalike"},
		} {
			// The email as typed: the server writes the target as it keeps emails (#801's review, L1).
			proof := stepUpFor(t, viewer, dockwellv1.StepUpAction_STEP_UP_ACTION_CHANGE_EMAIL, tt.email)
			_, err := update(&dockwellv1.UpdateProfileRequest{Email: &tt.email, Proof: proof})
			if connect.CodeOf(err) != connect.CodeAlreadyExists || reasonOf(err) != tt.reason {
				t.Errorf("the email %s: %v, want %s", tt.email, err, tt.reason)
			}
		}
		// The owner's name, by look, would read as the owner in Activity and an approval.
		copied := "Mіra Chen"
		if _, err := update(&dockwellv1.UpdateProfileRequest{Name: &copied}); connect.CodeOf(err) != connect.CodeAlreadyExists || reasonOf(err) != "name_taken" {
			t.Errorf("the owner's name: %v, want name_taken", err)
		}
		bad := "ada at example.com"
		if _, err := update(&dockwellv1.UpdateProfileRequest{Email: &bad}); reasonOf(err) != "invalid" {
			t.Errorf("what isn't an email: %v, want invalid", err)
		}
		// Their own time zone changes with no passkey, shows on GetAuthState, and
		// refuses anything that isn't an IANA zone (#834).
		tz := "Asia/Tokyo"
		if u, err = update(&dockwellv1.UpdateProfileRequest{TimeZone: &tz}); err != nil || u.GetTimeZone() != tz {
			t.Fatalf("a viewer setting their time zone: %v, %v", u, err)
		}
		if res, err := a.GetAuthState(t.Context(), connect.NewRequest(&dockwellv1.GetAuthStateRequest{})); err != nil || res.Msg.GetUser().GetTimeZone() != tz {
			t.Errorf("GetAuthState time zone = %q, %v; want %q", res.Msg.GetUser().GetTimeZone(), err, tz)
		}
		badZone := "Mars/Olympus"
		if _, err := update(&dockwellv1.UpdateProfileRequest{TimeZone: &badZone}); connect.CodeOf(err) != connect.CodeInvalidArgument || reasonOf(err) != "invalid" {
			t.Errorf("invalid time zone: %v, want InvalidArgument/invalid", err)
		}

		if o := ownerState(); o.GetName() != "Mira Chen" || o.GetEmail() != "mira@acme.dev" || o.GetTimeZone() != "" {
			t.Errorf("the owner is %s <%s> (%q) after the viewer's changes", o.GetName(), o.GetEmail(), o.GetTimeZone())
		}
		events, err := dockwellv1connect.NewAuditServiceClient(s.owner, s.origin).ListAuditEvents(t.Context(), connect.NewRequest(&dockwellv1.ListAuditEventsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, e := range events.Msg.GetEvents() {
			if e.GetAction() == "account.name" || e.GetAction() == "account.email" || e.GetAction() == "account.time_zone" {
				if e.GetSubject() != viewerID {
					t.Errorf("%s is about %s, want the viewer", e.GetAction(), e.GetSubject())
				}
				got[e.GetAction()] = e.GetDetail()
			}
		}
		if got["account.name"] != `from "Ada Park" to "Ada Okafor"` || got["account.email"] != `from "ada@example.com" to "ada.park@example.com"` || got["account.time_zone"] != `from "" to "Asia/Tokyo"` || len(got) != 3 {
			t.Errorf("the audit log's profile changes = %v", got)
		}
		if _, err := dockwellv1connect.NewAuditServiceClient(viewer, s.origin).ListAuditEvents(t.Context(), connect.NewRequest(&dockwellv1.ListAuditEventsRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("the viewer reading the audit log: %v, want it refused", err)
		}

		// Activity has each for admins, with what it was and is, where people look (#801's review, M1; #834).
		access := []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_ACCESS}
		timeline, err := eventClient(s.owner, s.origin).ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{Kinds: access}))
		if err != nil {
			t.Fatal(err)
		}
		lines := map[string]*dockwellv1.EventSetting{}
		for _, e := range timeline.Msg.GetEvents() {
			if e.GetAction() == "account.name" || e.GetAction() == "account.email" || e.GetAction() == "account.time_zone" {
				lines[e.GetAction()] = e.GetSetting()
				if e.GetSubject().GetId() != viewerID || e.GetSubject().GetName() != "Ada Okafor" {
					t.Errorf("Activity's %s is about %v", e.GetAction(), e.GetSubject())
				}
			}
		}
		if n, m, z := lines["account.name"], lines["account.email"], lines["account.time_zone"]; n.GetPrevious() != "Ada Park" || n.GetValue() != "Ada Okafor" ||
			m.GetPrevious() != "ada@example.com" || m.GetValue() != "ada.park@example.com" ||
			z.GetPrevious() != "" || z.GetValue() != "Asia/Tokyo" {
			t.Errorf("Activity's profile lines read %v, %v, and %v", n, m, z)
		}
		counted, err := eventClient(s.owner, s.origin).CountEvents(t.Context(), connect.NewRequest(&dockwellv1.CountEventsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		// Below an admin, none of them is listed, nor counted, even for an install-wide viewer.
		theirs, err := eventClient(viewer, s.origin).ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{Kinds: access}))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range theirs.Msg.GetEvents() {
			if e.GetAction() == "account.name" || e.GetAction() == "account.email" || e.GetAction() == "account.time_zone" {
				t.Errorf("a viewer's Activity has %s", e.GetAction())
			}
		}
		theirCount, err := eventClient(viewer, s.origin).CountEvents(t.Context(), connect.NewRequest(&dockwellv1.CountEventsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		if counted.Msg.GetTotal() <= theirCount.Msg.GetTotal() {
			t.Errorf("an admin counts %d events and a viewer %d, want the viewer fewer", counted.Msg.GetTotal(), theirCount.Msg.GetTotal())
		}
	})
}
