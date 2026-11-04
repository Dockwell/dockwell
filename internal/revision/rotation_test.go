// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"fmt"
	"reflect"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// shape is everything a plan says, but for what tells one change from
// another: its ID, its fingerprint and the IDs of what it makes.
func shape(p *Plan) string {
	out := fmt.Sprintf("revisions %d, counts %+v, destructive %t\n", p.Revisions, p.Counts(), p.Destructive())
	for _, r := range p.Resources {
		out += fmt.Sprintf("resource %s: %s from %d, %+v, on %v from %v\n", r.Ref, r.Action, r.Base, r.Changes, r.Servers, r.Previous)
	}
	for _, s := range p.Servers {
		out += fmt.Sprintf("server %s offline %t downtime %s\n", s.Name, s.Offline, s.Downtime)
		for _, st := range s.Steps {
			out += fmt.Sprintf("  step %s: %+v %+v\n", st.Ref, st.Decision, st.Changes)
		}
	}
	return out
}

// Proposing a secret's value plans the same whether or not it is the value
// the secret has: a new revision, and a restart of every service that uses
// it. So whoever may propose, such as a read-write API token, can't tell a
// right guess from a wrong one by its plan. Nor by comparing fingerprints:
// a change's is its own, even beside an undo that puts the old value back.
func TestProposingAValuePlansTheSameWhateverItIs(t *testing.T) {
	storetest.Each(t, testProposingAValuePlansTheSameWhateverItIs)
}

func testProposingAValuePlansTheSameWhateverItIs(t *testing.T, backend store.Backend) {
	e, _, db := setup(t, backend, "srv_A")
	env := production(t, db)
	apply(t, e, propose(t, e, "Set up",
		secretEdit(env, "DB_PASSWORD", "hunter2"),
		user(env, "api", "srv_A", map[string]string{"DB_PASSWORD": ""}, nil),
	))
	rotation := propose(t, e, "Rotate", secretEdit(env, "DB_PASSWORD", "correct-horse"))
	apply(t, e, rotation)
	undo, err := e.Undo(t.Context(), rotation.Change, By{Actor: "usr_test", Via: ViaPanel})
	if err != nil {
		t.Fatal(err)
	}

	right := propose(t, e, "Guess", secretEdit(env, "DB_PASSWORD", "correct-horse"))
	wrong := propose(t, e, "Guess", secretEdit(env, "DB_PASSWORD", "letmein"))
	old := propose(t, e, "Guess", secretEdit(env, "DB_PASSWORD", "hunter2"))
	if a, b := shape(right), shape(wrong); a != b {
		t.Errorf("the value the secret has plans\n%s\nand another\n%s", a, b)
	}
	if right.Revisions != 2 || len(right.Steps()) != 1 || right.Steps()[0].Action != resource.ActionReplace {
		t.Errorf("setting the value the secret has: %s, want a revision of each, and api restarting", shape(right))
	}
	if r := planned(right, "DB_PASSWORD"); r.Action != resource.ActionUpdate || len(r.Changes) != 1 ||
		r.Changes[0] != (resource.Change{Path: "value", Op: resource.OpModified, Sensitive: true}) {
		t.Errorf("the secret's own plan: %+v", r)
	}
	if old.Fingerprint == undo.Fingerprint || right.Fingerprint == wrong.Fingerprint {
		t.Error("two changes that may set one value share a fingerprint")
	}
}

// A container's own environment is a rotation too: proposing it plans a
// revision and a replace, the same whether each value is the one it has or
// not, online and offline, and the plan never says which.
func TestAContainersEnvironmentPlansTheSameWhateverItHolds(t *testing.T) {
	storetest.Each(t, testAContainersEnvironmentPlansTheSameWhateverItHolds)
}

func testAContainersEnvironmentPlansTheSameWhateverItHolds(t *testing.T, backend store.Backend) {
	e, f, _ := setup(t, backend, "srv_A")
	withEnv := func(env map[string]string, restart resourcev1.RestartPolicy) Edit {
		ed := web("nginx:1.27")
		c := ed.Spec.(*resourcev1.Container)
		c.Env, c.Restart = env, restart
		return ed
	}
	apply(t, e, propose(t, e, "Run web", withEnv(map[string]string{"API_KEY": "s3cret-value", "REGION": "eu"}, resourcev1.RestartPolicy_RESTART_POLICY_ALWAYS)))
	for _, online := range []bool{true, false} {
		f["srv_A"].online.Store(online)
		for _, restart := range []resourcev1.RestartPolicy{resourcev1.RestartPolicy_RESTART_POLICY_ALWAYS, resourcev1.RestartPolicy_RESTART_POLICY_NO} {
			// Every value right, one wrong, all wrong: each plans alike.
			var first string
			for _, env := range []map[string]string{
				{"API_KEY": "s3cret-value", "REGION": "eu"},
				{"API_KEY": "guess", "REGION": "eu"},
				{"API_KEY": "guess", "REGION": "us"},
			} {
				p := propose(t, e, "Guess", withEnv(env, restart))
				got := shape(p)
				if first == "" {
					first = got
					if p.Revisions != 1 || len(p.Steps()) != 1 || p.Steps()[0].Action != resource.ActionReplace {
						t.Errorf("online %t, restart %s: %s, want a revision and a replace", online, restart, got)
					}
					continue
				}
				if got != first {
					t.Errorf("online %t, restart %s: one environment plans\n%s\nanother\n%s", online, restart, first, got)
				}
			}
		}
	}
}

// What Dockwell proposes of its own, from what a server already has or
// should have, isn't a rotation: keeping drift restarts nothing.
func TestKeepingDriftRotatesNothing(t *testing.T) { storetest.Each(t, testKeepingDriftRotatesNothing) }

func testKeepingDriftRotatesNothing(t *testing.T, backend store.Backend) {
	e, f, _ := setup(t, backend, "srv_A")
	apply(t, e, propose(t, e, "Run web", web("nginx:1.27")))
	f["srv_A"].docker.Edit("web", func(c *resourcev1.Container) { c.Labels = map[string]string{"by": "hand"} })
	drift, err := e.Drift(t.Context(), "srv_A")
	if err != nil || len(drift) != 1 {
		t.Fatalf("drift = %+v, %v", drift, err)
	}
	adopt, err := e.Adopt(t.Context(), drift[0], By{Actor: "usr_test", Via: ViaPanel})
	if err != nil {
		t.Fatal(err)
	}
	if adopt.Revisions != 1 || len(adopt.Steps()) != 0 {
		t.Errorf("keeping drift: %s, want a revision and nothing to do on the server", shape(adopt))
	}
	revert, err := e.Revert(t.Context(), drift[0], By{Actor: "usr_test", Via: ViaPanel})
	if err != nil {
		t.Fatal(err)
	}
	if want := []resource.Change{{Path: "labels[by]", Op: resource.OpRemoved, From: "hand"}}; revert.Revisions != 0 || len(revert.Steps()) != 1 ||
		!reflect.DeepEqual(revert.Steps()[0].Changes, want) {
		t.Errorf("reverting drift: %s, want only the label taken off", shape(revert))
	}
}
