// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/link"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
)

// backs stands in for what backs up a volume on the server, as this agent does, or
// one from before: ignores has it back up the whole volume whatever it's asked,
// ignoresAlso drop the more databases a step asks for, and unnamed say nothing of
// the database it dumped.
type backs struct {
	mu                            sync.Mutex
	ignores, ignoresAlso, unnamed bool
}

func (b *backs) JudgeBackup(_ context.Context, _ resource.Ref, asked resource.BackupAsked) (resource.Decision, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	first := &resource.BackupFirst{ID: asked.ID, Method: resource.BackupDump, Database: "PostgreSQL"}
	if !b.ignores {
		first.One = asked.Database
		if !b.ignoresAlso {
			first.Also = asked.Also
		}
	}
	return resource.Decision{Action: resource.ActionUpdate, BackupFirst: first}, true, nil
}

func (b *backs) ApplyBackup(ctx context.Context, step resource.Step) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	first := step.BackupFirst
	for _, d := range append([]resource.DatabaseDump{{ID: first.ID, Database: first.One}}, first.Also...) {
		taken := resource.BackupTaken{ID: d.ID, Method: resource.BackupDump, Database: "PostgreSQL", One: d.Database}
		if b.unnamed {
			taken.One = ""
		}
		resource.Report(ctx, resource.Outcome{Backup: &taken})
	}
	return nil
}

// A backup of one database alone is what the agent says it took, never what it was
// asked: an agent from before, which backs up the whole volume whatever it's asked,
// or drops the more databases it's asked for, is refused as the plan is made; one
// whose report doesn't name the database fails its step, and isn't recorded as the
// app's. A restore made for an app's link brings back that link's database alone,
// and is refused for a backup of the whole volume or of another database (ADR 0084).
func TestOneDatabaseIsWhatTheAgentSaysItTook(t *testing.T) {
	x := withVolume(t, store.SQLite)
	e, ref, env := x.e, x.ref, x.env
	withData(t, e, env)
	b := &backs{}
	client := x.engine.Client()
	x.agents["srv_A"] = twoAgents{Local: x.agents["srv_A"].(twoAgents).Local, volumes: resource.Local{Drivers: resource.NewRegistry(&volume.Driver{
		Runtime: &volume.Docker{Client: client, Kept: &volume.Kept{}, Paths: &container.Paths{Client: client}}, Restores: x.r, BacksUp: b,
	})}}
	var recorded []BackupStep
	var mu sync.Mutex
	e.OnBackup(func(_ context.Context, s BackupStep) {
		mu.Lock()
		defer mu.Unlock()
		recorded = append(recorded, s)
	})
	dump := func(databases ...string) (*Plan, error) {
		var ones []OneBackup
		for _, d := range databases {
			ones = append(ones, OneBackup{Volume: ref, Database: d})
		}
		// As before an update of what reads them: the service is the change's edit.
		return e.Propose(t.Context(), Proposal{Summary: "Dump first", Actor: "dockwell", Edits: []Edit{uses2(env)}, BackUpOne: ones})
	}

	// An agent from before plans a backup of the whole volume: refused as too old.
	b.ignores = true
	if _, err := dump("shop"); err == nil || !strings.Contains(err.Error(), "too old") {
		t.Errorf("planning with an agent that backs up the whole volume = %v", err)
	}
	b.ignores, b.ignoresAlso = false, true
	if _, err := dump("shop", "blog"); err == nil || !strings.Contains(err.Error(), "too old") {
		t.Errorf("planning with an agent that drops the more databases = %v", err)
	}
	b.ignoresAlso = false

	// One whose report doesn't name what it dumped fails, recorded as no one's.
	b.unnamed = true
	p, err := dump("shop")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Apply(t.Context(), p.Change); err == nil || !strings.Contains(err.Error(), "without saying it dumped shop alone") {
		t.Errorf("a report that doesn't name the database = %v", err)
	}
	if len(recorded) != 1 || recorded[0].Taken.One != "" {
		t.Errorf("recorded %+v", recorded)
	}
	b.unnamed, recorded = false, nil
	p, err = dump("shop", "blog")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Apply(t.Context(), p.Change); err != nil {
		t.Fatal(err)
	}
	if len(recorded) != 2 || recorded[0].Taken.One != "shop" || recorded[1].Taken.One != "blog" {
		t.Errorf("recorded %+v", recorded)
	}

	// An agent downgraded between the plan and applying it isn't sent the step: it
	// would back up, or restore, the whole volume.
	planned, err := dump("shop")
	if err != nil {
		t.Fatal(err)
	}
	older := x.agents["srv_A"].(twoAgents)
	older.old = true
	x.agents["srv_A"] = older
	if err := e.Apply(t.Context(), planned.Change); err == nil || !strings.Contains(err.Error(), "backs up and restores a whole volume, not one database alone") {
		t.Errorf("applying on an agent downgraded since the plan = %v", err)
	}
	older.old = false
	x.agents["srv_A"] = older

	// A restore for an app's link: of that link's database alone, and nothing else.
	e.kinds = e.kinds.With(link.Type{})
	e.sealer = secretstest.Keeper(x.db, e.kinds)
	apply(t, e, propose(t, e, "Link shop", Edit{Ref: resource.Ref{Kind: link.Kind, Scope: env, Name: "db"}, Spec: &resourcev1.Link{
		Label: "Database", Service: "web", Engine: resourcev1.LinkEngine_LINK_ENGINE_POSTGRESQL, DatabaseName: "shop",
		Accepts: []*resourcev1.LinkAccept{{Engine: resourcev1.LinkEngine_LINK_ENGINE_POSTGRESQL}},
		Sets:    []*resourcev1.LinkSet{{Name: "DB_HOST", Part: resourcev1.LinkPart_LINK_PART_HOST}},
	}}))
	forShop := RestoreAs{For: resource.Ref{Kind: link.Kind, Scope: env, Name: "db"}}
	source := resource.RestoreSource{Backup: "bkp_1", Server: "srv_A"}
	for one, want := range map[string]string{"": "holds the whole of data", "blog": "holds the database blog, and link db reads shop"} {
		x.r.one = one
		if _, err := e.RestoreWith(t.Context(), ref, source, forShop, restoreBy(), ""); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("restoring a backup of %q for shop's link = %v", one, err)
		}
	}
	x.r.one = "shop"
	restore, err := e.RestoreWith(t.Context(), ref, source, forShop, restoreBy(), "")
	if err != nil {
		t.Fatalf("restoring shop's own database for its link = %v", err)
	}
	older.old = true
	x.agents["srv_A"] = older
	if err := e.Apply(t.Context(), restore.Change); err == nil || !strings.Contains(err.Error(), "backs up and restores a whole volume") || len(x.r.ran) != 0 {
		t.Errorf("restoring on an agent downgraded since the plan = %v, ran %v", err, x.r.ran)
	}
	if errors.Is(err, ErrNeedsRole) {
		t.Error("it wanted a role of Dockwell's own")
	}
}
