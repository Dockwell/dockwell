// SPDX-License-Identifier: AGPL-3.0-only

package store_test

import (
	"cmp"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// fixture writes managed state the way the revision store does.
func revise(t *testing.T, db *store.DB, fn func(tx *store.RevisionTx) error) {
	t.Helper()
	if err := db.Revise(t.Context(), fn); err != nil {
		t.Fatal(err)
	}
}

func servers(t *testing.T, db *store.DB, ids ...string) {
	t.Helper()
	now := time.Now()
	if err := db.Update(t.Context(), func(tx *store.Tx) error {
		for _, id := range ids {
			if err := tx.CreateServer(t.Context(), store.Server{ID: id, Name: id, CreatedAt: now, CertSerial: id, CertNotAfter: now}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTheObjectModel(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		servers(t, db, "srv_A", "srv_B")
		now := time.Now().UTC().Truncate(time.Millisecond)

		_ = db.View(ctx, func(tx *store.Tx) error {
			if _, err := tx.Organization(ctx); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("Organization before setup = %v, want ErrNotFound", err)
			}
			return nil
		})
		// The organization, a project in it, an environment in that, and a
		// service in the environment placed on both servers: one change.
		revise(t, db, func(tx *store.RevisionTx) error {
			if err := tx.CreateOrganization(ctx, store.Organization{ID: "org_1", Name: "default", CreatedAt: now}); err != nil {
				return err
			}
			if err := tx.CreateChange(ctx, store.Change{
				ID: "chg_1", Summary: "set up", Actor: "dockwell", State: store.ChangeApplying,
				Fingerprint: "f", Plan: []byte("{}"), CreatedAt: now, Items: []store.ChangeItem{
					{Kind: "project", Scope: "org_1", Name: "default", Spec: []byte{1}},
					{Kind: "environment", Scope: "prj_1", Name: "production", Spec: []byte{2}},
					{Kind: "service", Scope: "env_1", Name: "web", Spec: []byte{3}, Servers: []string{"srv_A", "srv_B"}},
				},
			}); err != nil {
				return err
			}
			for i, n := range []store.NewRevision{
				{Kind: "project", Scope: "org_1", Name: "default", ID: "prj_1", Spec: []byte{1}},
				{Kind: "environment", Scope: "prj_1", Name: "production", ID: "env_1", Spec: []byte{2}},
				{Kind: "service", Scope: "env_1", Name: "web", ID: "svc_1", Spec: []byte{3}},
			} {
				n.Change, n.Position, n.At = "chg_1", i, now
				if _, err := tx.AddRevision(ctx, n); err != nil {
					return err
				}
			}
			return tx.PlaceRevision(ctx, "svc_1", 1, []string{"srv_A", "srv_B"}, now)
		})
		_ = db.View(ctx, func(tx *store.Tx) error {
			org, err := tx.Organization(ctx)
			if err != nil || org.ID != "org_1" || !org.CreatedAt.Equal(now) {
				t.Errorf("Organization = %+v, %v", org, err)
			}
			c, err := tx.GetChange(ctx, "chg_1")
			if err != nil || len(c.Items) != 3 || !slices.Equal(c.Items[2].Servers, []string{"srv_A", "srv_B"}) || c.Items[0].Servers != nil {
				t.Errorf("GetChange = %+v, %v", c, err)
			}
			envs, err := tx.ListResources(ctx, "prj_1", "environment")
			if err != nil || len(envs) != 1 || envs[0].ID != "env_1" || envs[0].Revision != 1 {
				t.Errorf("environments in prj_1 = %+v, %v", envs, err)
			}
			all, err := tx.ListResources(ctx, "env_1", "")
			if err != nil || len(all) != 1 || all[0].Name != "web" {
				t.Errorf("everything in env_1 = %+v, %v", all, err)
			}
			placed, err := tx.Placement(ctx, "svc_1", 1)
			if err != nil || !slices.Equal(placed, []string{"srv_A", "srv_B"}) {
				t.Errorf("Placement = %v, %v", placed, err)
			}
			for _, s := range []string{"srv_A", "srv_B"} {
				on, err := tx.ListPlaced(ctx, s)
				if err != nil || len(on) != 1 || on[0].ID != "svc_1" {
					t.Errorf("ListPlaced(%s) = %+v, %v", s, on, err)
				}
			}
			revs, err := tx.RevisionsOf(ctx, "chg_1")
			if err != nil || len(revs) != 3 || revs[0].ID != "prj_1" || revs[2].ID != "svc_1" {
				t.Errorf("RevisionsOf = %+v, %v", revs, err)
			}
			return nil
		})

		// Revision 2 moves web to srv_B alone. Live on srv_B, it supersedes
		// revision 1 there; srv_A keeps revision 1 until its delete runs.
		later := now.Add(time.Minute)
		revise(t, db, func(tx *store.RevisionTx) error {
			if err := tx.SetDeploymentState(ctx, store.Deployment{ResourceID: "svc_1", Revision: 1, Server: "srv_A", State: store.DeploymentLive, UpdatedAt: now}); err != nil {
				return err
			}
			if err := tx.SetDeploymentState(ctx, store.Deployment{ResourceID: "svc_1", Revision: 1, Server: "srv_B", State: store.DeploymentLive, UpdatedAt: now}); err != nil {
				return err
			}
			if err := tx.CreateChange(ctx, store.Change{
				ID: "chg_2", Summary: "move", Actor: "usr_1", State: store.ChangeApplying,
				Fingerprint: "g", Plan: []byte("{}"), CreatedAt: later,
			}); err != nil {
				return err
			}
			// A revision sealed for a number other than its own isn't written.
			if _, err := tx.AddRevision(ctx, store.NewRevision{Kind: "service", Scope: "env_1", Name: "web", Number: 3, Spec: []byte{4}, Change: "chg_2", At: later}); !errors.Is(err, store.ErrConflict) {
				t.Errorf("AddRevision as number 3 of a resource at 1 = %v, want ErrConflict", err)
			}
			rev, err := tx.AddRevision(ctx, store.NewRevision{Kind: "service", Scope: "env_1", Name: "web", Number: 2, Spec: []byte{4}, Change: "chg_2", At: later})
			if err != nil || rev.Revision != 2 || rev.ID != "svc_1" {
				t.Errorf("AddRevision = %+v, %v", rev, err)
			}
			if err := tx.PlaceRevision(ctx, "svc_1", 2, []string{"srv_B"}, later); err != nil {
				return err
			}
			return tx.SetDeploymentState(ctx, store.Deployment{ResourceID: "svc_1", Revision: 2, Server: "srv_B", State: store.DeploymentLive, UpdatedAt: later})
		})
		_ = db.View(ctx, func(tx *store.Tx) error {
			deps, err := tx.Deployments(ctx, "svc_1", 0)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, d := range deps {
				got = append(got, d.Server+"@"+string(rune('0'+d.Revision))+"="+d.State)
			}
			want := []string{"srv_B@2=live", "srv_A@1=live", "srv_B@1=superseded"}
			if !slices.Equal(got, want) {
				t.Errorf("deployments = %v, want %v", got, want)
			}
			on, err := tx.ListPlaced(ctx, "srv_A")
			if err != nil || len(on) != 0 {
				t.Errorf("srv_A should have nothing of the latest revision: %+v, %v", on, err)
			}
			return nil
		})
		err := db.Revise(ctx, func(tx *store.RevisionTx) error {
			return tx.SetDeploymentState(ctx, store.Deployment{ResourceID: "svc_1", Revision: 2, Server: "srv_A", State: store.DeploymentLive, UpdatedAt: later})
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("a deployment that was never placed: %v, want ErrNotFound", err)
		}
	})
}

// History is append-only in the database itself, so nothing that reaches
// it, inside dockwell or out, can rewrite it.
func TestHistoryIsAppendOnly(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, location := storetest.Open(t, b)
		ctx := t.Context()
		servers(t, db, "srv_A")
		now := time.Now()
		revise(t, db, func(tx *store.RevisionTx) error {
			if err := tx.CreateChange(ctx, store.Change{
				ID: "chg_1", Summary: "s", Actor: "a", State: store.ChangeApplying,
				Fingerprint: "f", Plan: []byte("{}"), CreatedAt: now, Items: []store.ChangeItem{{Kind: "firewall.rule", Scope: "srv_A", Name: "ssh", Spec: []byte{1}}},
				Servers: []string{"srv_A"},
			}); err != nil {
				return err
			}
			if _, err := tx.AddRevision(ctx, store.NewRevision{Kind: "firewall.rule", Scope: "srv_A", Name: "ssh", ID: "res_1", Spec: []byte{1}, Change: "chg_1", At: now}); err != nil {
				return err
			}
			if _, err := tx.AddRevision(ctx, store.NewRevision{
				Kind: "service", Scope: "env_1", Name: "api", ID: "svc_1", Spec: []byte{1}, Change: "chg_1", At: now,
				Uses: []store.Use{{As: "DB", Kind: "secret", Name: "DB", Revision: 3}},
			}); err != nil {
				return err
			}
			return tx.PlaceRevision(ctx, "res_1", 1, []string{"srv_A"}, now)
		})
		for _, q := range []string{
			`UPDATE revisions SET spec = NULL`,
			`DELETE FROM revisions`,
			`UPDATE change_items SET spec = NULL`,
			`DELETE FROM change_items`,
			`UPDATE changes SET plan = 'x'`,
			`UPDATE changes SET fingerprint = 'x'`,
			`UPDATE changes SET proposed_via = 'x'`,
			`UPDATE changes SET fingerprint_scheme = 1`,
			`UPDATE change_items SET rotates = 1`,
			`UPDATE revision_uses SET used_revision = 1`,
			`UPDATE revision_uses SET used_name = 'OTHER'`,
			`DELETE FROM revision_uses`,
			`UPDATE changes SET state = 'abandoned'`,
			`DELETE FROM changes`,
			`UPDATE resources SET revision = 5`,
			`UPDATE resources SET name = 'telnet'`,
			`DELETE FROM resources`,
			`UPDATE deployments SET server_id = 'srv_B'`,
			`UPDATE deployments SET revision = 2`,
			`DELETE FROM deployments`,
			`UPDATE change_servers SET server_id = 'srv_B'`,
			`DELETE FROM change_servers`,
		} {
			err := db.Update(ctx, func(tx *store.Tx) error { return store.ExecForTest(ctx, tx, q) })
			if err == nil {
				t.Errorf("%s succeeded; history must not be rewritten", q)
			}
		}
		// What is meant to move on still does.
		for _, q := range []string{
			`UPDATE changes SET state = 'applied', applied_at = 1`,
			`UPDATE changes SET applied_by = 'user:usr_1', applied_via = 'panel', progress = 'x'`,
			`UPDATE changes SET state = 'discarded'`,
			`UPDATE deployments SET state = 'live', updated_at = 1`,
		} {
			if err := db.Update(ctx, func(tx *store.Tx) error { return store.ExecForTest(ctx, tx, q) }); err != nil {
				t.Errorf("%s: %v", q, err)
			}
		}
		_ = db.View(ctx, func(tx *store.Tx) error {
			if got, err := tx.ChangeServers(ctx, "chg_1"); err != nil || !slices.Equal(got, []string{"srv_A"}) {
				t.Errorf("the change's servers = %v, %v, want srv_A", got, err)
			}
			return nil
		})
		_ = location
	})
}

// A database an M1 dockwell wrote keeps its revisions: resources take their
// server as their scope, and what each runs there becomes a deployment.
func TestUpgradeKeepsM1Revisions(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		location := storetest.New(t, b)
		old, err := store.OpenAt(t.Context(), location, 5)
		if err != nil {
			t.Fatal(err)
		}
		_ = old.Close()
		for _, q := range []string{
			`INSERT INTO servers (id, name, created_at, cert_serial, cert_not_after) VALUES ('srv_A', 'hz-1', 0, '1', 0)`,
			`INSERT INTO changes (id, summary, actor, state, fingerprint, plan, created_at) VALUES ('chg_1', 'web', 'usr_1', 'applied', 'f', '', 1)`,
			`INSERT INTO change_items (change_id, position, kind, server_id, name, base_revision, spec) VALUES ('chg_1', 0, 'firewall.rule', 'srv_A', 'ssh', 0, NULL)`,
			`INSERT INTO change_items (change_id, position, kind, server_id, name, base_revision, spec) VALUES ('chg_1', 1, 'container', 'srv_A', 'web', 0, NULL)`,
			`INSERT INTO resources (id, kind, server_id, name, revision) VALUES ('res_1', 'container', 'srv_A', 'web', 1)`,
			`INSERT INTO revisions (resource_id, number, spec, change_id, created_at) VALUES ('res_1', 1, NULL, 'chg_1', 1)`,
			// A change that failed, and one still applying.
			`INSERT INTO changes (id, summary, actor, state, fingerprint, plan, created_at) VALUES ('chg_2', 'api', 'usr_1', 'failed', 'f', '', 2)`,
			`INSERT INTO resources (id, kind, server_id, name, revision) VALUES ('res_2', 'container', 'srv_A', 'api', 1)`,
			`INSERT INTO revisions (resource_id, number, spec, change_id, created_at) VALUES ('res_2', 1, NULL, 'chg_2', 2)`,
			`INSERT INTO changes (id, summary, actor, state, fingerprint, plan, created_at) VALUES ('chg_3', 'db', 'usr_1', 'applying', 'f', '', 3)`,
			`INSERT INTO resources (id, kind, server_id, name, revision) VALUES ('res_3', 'container', 'srv_A', 'db', 1)`,
			`INSERT INTO revisions (resource_id, number, spec, change_id, created_at) VALUES ('res_3', 1, NULL, 'chg_3', 3)`,
		} {
			storetest.Exec(t, location, q)
		}
		if b == store.Postgres {
			storetest.Exec(t, location, `UPDATE revisions SET spec = '\x0102'`)
		} else {
			storetest.Exec(t, location, `UPDATE revisions SET spec = x'0102'`)
		}

		db, err := store.Open(t.Context(), location)
		if err != nil {
			t.Fatalf("upgrading an M1 database: %v", err)
		}
		defer db.Close()
		ctx := t.Context()
		_ = db.View(ctx, func(tx *store.Tx) error {
			r, err := tx.GetResource(ctx, "container", "srv_A", "web")
			if err != nil || r.ID != "res_1" || string(r.Spec) != "\x01\x02" || r.ChangeID != "chg_1" {
				t.Errorf("the M1 container = %+v, %v", r, err)
			}
			revs, err := tx.RevisionsOf(ctx, "chg_1")
			if err != nil || len(revs) != 1 {
				t.Errorf("RevisionsOf = %+v, %v", revs, err)
			}
			placed, err := tx.ListPlaced(ctx, "srv_A")
			if err != nil || len(placed) != 3 || placed[0].ID != "res_2" {
				t.Errorf("ListPlaced = %+v, %v", placed, err)
			}
			// Each deployment says how its change went.
			for res, want := range map[string]string{"res_1": store.DeploymentLive, "res_2": store.DeploymentFailed, "res_3": store.DeploymentPending} {
				deps, err := tx.Deployments(ctx, res, 1)
				if err != nil || len(deps) != 1 || deps[0].State != want {
					t.Errorf("Deployments of %s = %+v, %v; want one %s", res, deps, err, want)
				}
			}
			c, err := tx.GetChange(ctx, "chg_1")
			if err != nil || len(c.Items) != 2 || c.Items[1].Scope != "srv_A" || strings.Join(c.Items[1].Servers, " ") != "srv_A" ||
				c.Items[1].ResourceID != "res_1" || c.Items[0].ResourceID != "" {
				t.Errorf("GetChange = %+v, %v", c, err)
			}
			// The timeline finds each change on the servers it placed revisions on.
			for _, change := range []string{"chg_1", "chg_2", "chg_3"} {
				if got, err := tx.ChangeServers(ctx, change); err != nil || !slices.Equal(got, []string{"srv_A"}) {
					t.Errorf("the servers of %s = %v, %v, want srv_A", change, got, err)
				}
			}
			return nil
		})
	})
}

// Resume lists the changes left applying on every start, every connect and
// every wakeup, so the list comes from an index on the state: not from
// reading every change there ever was.
func TestChangesInAStateComeFromAnIndex(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		var plan string
		if err := db.View(t.Context(), func(tx *store.Tx) (err error) {
			plan, err = store.ChangesInPlanForTest(t.Context(), tx, store.ChangeApplying)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		// The state is a condition of the index, not a filter on each row.
		want := map[store.Backend]string{store.SQLite: "changes_by_state (state=?)", store.Postgres: "Index Cond: (state = "}[b]
		if !strings.Contains(plan, want) {
			t.Errorf("listing the changes left applying runs as\n%s\nwant it to look the state up in an index (%q)", plan, want)
		}
	})
}

// What a revision uses is kept beside it, so the store finds what uses a
// value without reading a spec: only each resource's latest revision
// counts, and a deleted resource uses nothing. An environment's values
// list by name, with who made each, a page at a time.
func TestUsesAndValuesByName(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		db, _ := storetest.Open(t, b)
		ctx := t.Context()
		now := time.Now().UTC().Truncate(time.Millisecond)
		change := func(tx *store.RevisionTx, id, actor string) error {
			return tx.CreateChange(ctx, store.Change{ID: id, Summary: id, Actor: actor, State: store.ChangeApplying, Fingerprint: "f", Plan: []byte("{}"), CreatedAt: now})
		}
		revise(t, db, func(tx *store.RevisionTx) error {
			if err := change(tx, "chg_1", "user:usr_mira"); err != nil {
				return err
			}
			for _, r := range []store.NewRevision{
				{Kind: "secret", Name: "DB_PASSWORD", ID: "sec_1"},
				{Kind: "variable", Name: "LOG_LEVEL", ID: "var_1"},
				{Kind: "variable", Name: "API_URL", ID: "var_2"},
				{Kind: "secret", Name: "OTHER", Scope: "env_2", ID: "sec_2"},
				{Kind: "service", Name: "web", ID: "svc_1", Uses: []store.Use{
					{As: "DATABASE_PASSWORD", Kind: "secret", Name: "DB_PASSWORD", Revision: 1},
					{As: "LOG_LEVEL", Kind: "variable", Name: "LOG_LEVEL", Revision: 1},
				}},
				{Kind: "service", Name: "worker", ID: "svc_2", Uses: []store.Use{{As: "DB_PASSWORD", Kind: "secret", Name: "DB_PASSWORD", Revision: 1}}},
				{Kind: "service", Name: "gone", ID: "svc_3", Uses: []store.Use{{As: "DB_PASSWORD", Kind: "secret", Name: "DB_PASSWORD", Revision: 1}}},
			} {
				r.Scope = cmp.Or(r.Scope, "env_1")
				r.Spec, r.Change, r.At = []byte{1}, "chg_1", now
				if _, err := tx.AddRevision(ctx, r); err != nil {
					return err
				}
			}
			return nil
		})
		// worker stops using the password; gone is deleted.
		revise(t, db, func(tx *store.RevisionTx) error {
			if err := change(tx, "chg_2", "token:tok_ci"); err != nil {
				return err
			}
			if _, err := tx.AddRevision(ctx, store.NewRevision{Kind: "service", Scope: "env_1", Name: "worker", Spec: []byte{2}, Change: "chg_2", At: now}); err != nil {
				return err
			}
			_, err := tx.AddRevision(ctx, store.NewRevision{Kind: "service", Scope: "env_1", Name: "gone", Change: "chg_2", At: now})
			return err
		})
		_ = db.View(ctx, func(tx *store.Tx) error {
			users, err := tx.UsersOf(ctx, "env_1", "secret", "DB_PASSWORD")
			if err != nil || len(users) != 1 || users[0].ID != "svc_1" || users[0].Uses.As != "DATABASE_PASSWORD" || users[0].Uses.Revision != 1 {
				t.Errorf("UsersOf(DB_PASSWORD) = %+v, %v; want web alone, as DATABASE_PASSWORD", users, err)
			}
			all, err := tx.UsersOf(ctx, "env_1", "")
			if err != nil || len(all) != 2 || all[0].Uses.As != "DATABASE_PASSWORD" || all[1].Uses.As != "LOG_LEVEL" {
				t.Errorf("UsersOf(env_1) = %+v, %v; want web's two uses", all, err)
			}
			if none, err := tx.UsersOf(ctx, "env_2", ""); err != nil || len(none) != 0 {
				t.Errorf("UsersOf(env_2) = %+v, %v; want nothing", none, err)
			}

			kinds := []string{"variable", "secret"}
			var names []string
			var after store.NamedResource
			for {
				page, err := tx.PageByName(ctx, "env_1", kinds, after, 2)
				if err != nil {
					t.Fatal(err)
				}
				for _, r := range page {
					names = append(names, r.Name+" by "+r.Actor)
				}
				if len(page) < 2 {
					break
				}
				after = page[len(page)-1]
			}
			if want := []string{"API_URL by user:usr_mira", "DB_PASSWORD by user:usr_mira", "LOG_LEVEL by user:usr_mira"}; !slices.Equal(names, want) {
				t.Errorf("PageByName, two at a time = %q, want %q", names, want)
			}
			return nil
		})
	})
}
