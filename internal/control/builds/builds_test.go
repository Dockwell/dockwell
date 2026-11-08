// SPDX-License-Identifier: AGPL-3.0-only

package builds

import (
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// An agent speaks for its own server's steps: a build it tells of is kept only when
// the change has a step for that service on that server.
func TestABuildIsKeptOnlyForAStepOnItsServer(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		db, _ := storetest.Open(t, backend)
		ctx := t.Context()
		web := resource.Ref{Kind: "service", Scope: "env_1", Name: "web"}
		other := resource.Ref{Kind: "service", Scope: "env_1", Name: "other"}
		plan, err := json.Marshal(revision.Plan{Servers: []revision.ServerPlan{{Server: "srv_A", Steps: []resource.Step{
			{Ref: web, Decision: resource.Decision{Action: resource.ActionReplace, Build: &resource.BuildPlan{URL: "https://example.com/acme/web", Commit: "abc", Image: "dockwell.invalid/svc_1:abc-chg"}}},
		}}}})
		if err != nil {
			t.Fatal(err)
		}
		at := time.UnixMilli(1_800_000_000_000).UTC()
		if err := db.Revise(ctx, func(tx *store.RevisionTx) error {
			if err := tx.CreateChange(ctx, store.Change{
				ID: "chg_1", Summary: "s", Actor: "a", State: store.ChangeApplying, Fingerprint: "f", Plan: plan, CreatedAt: at,
				Items: []store.ChangeItem{{Kind: "service", Scope: "env_1", Name: "web", Spec: []byte("x")}}, Servers: []string{"srv_A"},
			}); err != nil {
				return err
			}
			for _, r := range []store.NewRevision{
				{Kind: "service", Scope: "env_1", Name: "web", ID: "svc_1", Spec: []byte("x"), Change: "chg_1", At: at},
				{Kind: "service", Scope: "env_1", Name: "other", ID: "svc_2", Spec: []byte("x"), Change: "chg_1", Position: 1, At: at},
			} {
				if _, err := tx.AddRevision(ctx, r); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		h := &Handler{db: db, logger: slog.New(slog.DiscardHandler), now: time.Now, watchers: map[string]map[chan []store.BuildLine]struct{}{}}
		say := func(server string, ref resource.Ref) {
			h.record(server, &agentv1.BuildUpdate{ChangeId: "chg_1", Resource: resourcepb.Ref(ref), State: dockwellv1.BuildState_BUILD_STATE_QUEUED})
		}
		say("srv_B", web)   // another server, which has no step for it
		say("srv_A", other) // a service the change has no step for there
		say("srv_A", web)   // its own step
		var kept []store.Build
		if err := db.View(ctx, func(tx *store.Tx) error {
			for _, id := range []string{"svc_1", "svc_2"} {
				got, err := tx.ListBuilds(ctx, store.BuildFilter{ResourceID: id, Limit: 10})
				if err != nil {
					return err
				}
				kept = append(kept, got...)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if len(kept) != 1 || kept[0].ServerID != "srv_A" || kept[0].ResourceID != "svc_1" || kept[0].Commit != "abc" {
			t.Errorf("kept %+v, not the one build of web on srv_A", kept)
		}
	})
}
