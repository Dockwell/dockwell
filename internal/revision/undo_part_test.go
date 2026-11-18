// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"errors"
	"log/slog"
	"slices"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/dnsprovider"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// An undo that takes one project out of a DNS provider is planned from the
// provider as it was read, and that read is its base: a project another change
// adds between the read and the plan makes the plan stale, to be planned again,
// never dropped (#792's re-check, L9).
func TestAPartUndoPlannedFromAStaleReadIsRefused(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		db, _ := storetest.Open(t, backend)
		logger := slog.New(slog.DiscardHandler)
		if _, err := Setup(t.Context(), db, logger); err != nil {
			t.Fatal(err)
		}
		kinds := resource.NewRegistry(dnsprovider.Type{})
		e := New(db, kinds, fleet{}, secretstest.Keeper(db, kinds), logger)
		ref := resource.Ref{Kind: dnsprovider.Kind, Scope: tree(t, db).Organization.ID, Name: "cloudflare"}
		serving := func(projects ...string) Edit {
			return Edit{Ref: ref, Spec: &resourcev1.DnsProvider{
				Provider: dnsprovider.Cloudflare, Zones: []string{"example.com"}, ApiToken: "a-token", Projects: projects,
			}}
		}
		apply(t, e, propose(t, e, "Connect Cloudflare", serving("prj_a")))
		first := propose(t, e, "Serve b", serving("prj_a", "prj_b"))
		apply(t, e, first)
		apply(t, e, propose(t, e, "Serve c", serving("prj_a", "prj_b", "prj_c")))

		// The undo of the first is read now: it takes prj_b out of prj_a, prj_b, prj_c.
		edits, err := e.reverse(t.Context(), first.Change)
		if err != nil {
			t.Fatal(err)
		}
		if len(edits) != 1 || !slices.Equal(edits[0].Spec.(*resourcev1.DnsProvider).GetProjects(), []string{"prj_a", "prj_c"}) {
			t.Fatalf("the undo read is %+v", edits)
		}
		// Another install adds its project before the undo is planned.
		apply(t, e, propose(t, e, "Serve d", serving("prj_a", "prj_b", "prj_c", "prj_d")))
		_, err = e.Propose(t.Context(), Proposal{Summary: "Undo: Serve b", Actor: "usr_test", Edits: edits})
		if !errors.Is(err, ErrStale) {
			t.Fatalf("an undo planned from a read the provider moved past: %v, want it stale", err)
		}
		// Planned again from the latest, it keeps prj_d.
		again, err := e.reverse(t.Context(), first.Change)
		if err != nil {
			t.Fatal(err)
		}
		if got := again[0].Spec.(*resourcev1.DnsProvider).GetProjects(); !slices.Equal(got, []string{"prj_a", "prj_c", "prj_d"}) {
			t.Fatalf("planned again, the undo serves %v", got)
		}
	})
}
