// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A person signed in reads the audit log as it was recorded: what the timeline
// leaves out, such as a refused sign-in, with the address it came from, and who
// acted on what by name, narrowed and paged. No API token reads it, as its words
// name where people signed in from (ADR 0047).
func TestTheAuditLogIsForAPerson(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		readOnly := createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY)
		writes := createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
		// A stranger's wrong code, for the owner's email.
		stranger := newBrowser(t, b.origin, f.api, nil)
		if _, err := stranger.authClient().SignInWithCode(t.Context(), connect.NewRequest(&dockwellv1.SignInWithCodeRequest{Email: "mira@acme.dev", Code: "123456"})); err == nil {
			t.Fatal("a wrong code signed in")
		}

		c := dockwellv1connect.NewAuditServiceClient(b, b.origin, connect.WithProtoJSON())
		list := func(req *dockwellv1.ListAuditEventsRequest) *dockwellv1.ListAuditEventsResponse {
			t.Helper()
			res, err := c.ListAuditEvents(t.Context(), connect.NewRequest(req))
			if err != nil {
				t.Fatal(err)
			}
			return res.Msg
		}
		actions := func(events []*dockwellv1.AuditEvent) []string {
			out := make([]string, len(events))
			for i, e := range events {
				out[i] = e.GetAction()
			}
			return out
		}

		all := list(&dockwellv1.ListAuditEventsRequest{}).GetEvents()
		for _, want := range []string{"owner.claim", "auth.sign_in", "token.create", "auth.sign_in_failed"} {
			if !slices.Contains(actions(all), want) {
				t.Errorf("the audit log has %v, want %s among them", actions(all), want)
			}
		}
		for i := 1; i < len(all); i++ {
			if all[i].GetTime().AsTime().After(all[i-1].GetTime().AsTime()) {
				t.Fatalf("the audit log isn't newest first: %v", actions(all))
			}
		}

		strangers := list(&dockwellv1.ListAuditEventsRequest{Actor: dockwellv1.AuditActor_AUDIT_ACTOR_STRANGER}).GetEvents()
		if len(strangers) == 0 || strangers[0].GetAction() != "auth.sign_in_failed" || strangers[0].GetActor() != nil || strangers[0].GetAddress() != "127.0.0.1" {
			t.Errorf("a stranger's wrong code reads %v, want it from 127.0.0.1 with no actor", strangers)
		}

		made := list(&dockwellv1.ListAuditEventsRequest{Query: "token.create"}).GetEvents()
		if len(made) != 2 {
			t.Fatalf("token.create finds %v, want the two tokens", actions(made))
		}
		for _, e := range made {
			if e.GetActor().GetName() != "Mira Chen" || e.GetActor().GetKind() != dockwellv1.ActorKind_ACTOR_KIND_USER ||
				e.GetSurface() != dockwellv1.Surface_SURFACE_PANEL || e.GetTarget().GetId() == "" || !strings.HasPrefix(e.GetTarget().GetName(), "CI TOKEN_SCOPE_") {
				t.Errorf("a token made reads %v, want Mira Chen from the panel, naming the token", e)
			}
		}
		if got := list(&dockwellv1.ListAuditEventsRequest{Actor: dockwellv1.AuditActor_AUDIT_ACTOR_PERSON, Surface: dockwellv1.Surface_SURFACE_CLI}).GetEvents(); len(got) != 0 {
			t.Errorf("a person from the CLI finds %v, want nothing", actions(got))
		}
		if got := list(&dockwellv1.ListAuditEventsRequest{Since: all[0].GetTime()}).GetEvents(); len(got) == 0 || len(got) >= len(all) {
			t.Errorf("since the newest finds %d of %d", len(got), len(all))
		}

		// Pages carry on where the one before ended, and the last says so.
		var paged []string
		token := ""
		for range all {
			page := list(&dockwellv1.ListAuditEventsRequest{PageSize: 2, PageToken: token})
			paged = append(paged, actions(page.GetEvents())...)
			if token = page.GetNextPageToken(); token == "" {
				break
			}
		}
		if !slices.Equal(paged, actions(all)) {
			t.Errorf("two at a time reads %v, want %v", paged, actions(all))
		}
		if _, err := c.ListAuditEvents(t.Context(), connect.NewRequest(&dockwellv1.ListAuditEventsRequest{PageToken: "nonsense"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("a made-up page token = %v, want InvalidArgument", err)
		}

		for _, secret := range []string{readOnly, writes} {
			script := bearer{token: secret, rt: b.client.Transport}
			_, err := dockwellv1connect.NewAuditServiceClient(script, b.origin, connect.WithProtoJSON()).
				ListAuditEvents(t.Context(), connect.NewRequest(&dockwellv1.ListAuditEventsRequest{}))
			if connect.CodeOf(err) != connect.CodePermissionDenied || reasonOf(err) != "needs_session" {
				t.Errorf("a token reading the audit log = %v, want needs_session", err)
			}
		}
	})
}
