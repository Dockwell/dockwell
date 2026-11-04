// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// shape is everything a plan says, but for what tells one change from
// another: its ID, its fingerprint and the IDs of what it makes.
func shape(p *Plan) string {
	var out strings.Builder
	fmt.Fprintf(&out, "revisions %d, counts %+v, destructive %t\n", p.Revisions, p.Counts(), p.Destructive())
	for i := range p.Resources {
		r := &p.Resources[i]
		fmt.Fprintf(&out, "resource %s: %s from %d, %+v, on %v from %v\n", r.Ref, r.Action, r.Base, r.Changes, r.Servers, r.Previous)
	}
	for _, s := range p.Servers {
		fmt.Fprintf(&out, "server %s offline %t downtime %s\n", s.Name, s.Offline, s.Downtime)
		for i := range s.Steps {
			st := &s.Steps[i]
			fmt.Fprintf(&out, "  step %s: %+v %+v\n", st.Ref, st.Decision, st.Changes)
		}
	}
	return out.String()
}

// rotating names the items of a change that rotate, as the store keeps
// them.
func rotating(t *testing.T, db *store.DB, changeID string) []string {
	t.Helper()
	var out []string
	c := stored(t, db, changeID)
	for i := range c.Items {
		if c.Items[i].Rotates {
			out = append(out, c.Items[i].Name)
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

	// Undoing it rotates too, so an undo can't say that it set the same
	// value either.
	apply(t, e, right)
	back, err := e.Undo(t.Context(), right.Change, By{Actor: "usr_test", Via: ViaPanel})
	if err != nil {
		t.Fatal(err)
	}
	if r := planned(back, "DB_PASSWORD"); back.Revisions != 2 || r.Action != resource.ActionUpdate || len(back.Steps()) != 1 {
		t.Errorf("undoing a change that set the value it had plans %s, want a rotation", shape(back))
	}
	if got := rotating(t, db, back.Change); !slices.Equal(got, []string{"DB_PASSWORD"}) {
		t.Errorf("in the undo, %v rotate, want the secret alone", got)
	}
}

// Edits carry whole specs, so applying the same thing twice changes
// nothing, a workload's own environment included: online and offline,
// proposing the same services and container again plans no change. A new
// image of one leaves another alone, as deploying through Compose import
// does, though it holds a password.
func TestProposingAWorkloadAgainPlansNothing(t *testing.T) {
	storetest.Each(t, testProposingAWorkloadAgainPlansNothing)
}

func testProposingAWorkloadAgainPlansNothing(t *testing.T, backend store.Backend) {
	e, f, db := setup(t, backend, "srv_A")
	env := production(t, db)
	apply(t, e, propose(t, e, "Set up", secretEdit(env, "API_TOKEN", "s3cret-token")))
	service := func(name, image string, secrets map[string]string) Edit {
		ed := user(env, name, "srv_A", secrets, nil)
		app := ed.Spec.(*resourcev1.Service).GetApp()
		app.Image, app.Env = image, map[string]string{"POSTGRES_PASSWORD": "s3cret-" + name}
		return ed
	}
	stack := func(webImage string) []Edit {
		return []Edit{
			service("db", "postgres:17", nil),
			service("api", webImage, map[string]string{"API_TOKEN": ""}),
			web("nginx:1.27"),
		}
	}
	apply(t, e, propose(t, e, "Deploy", stack("acme/api:1")...))
	for _, online := range []bool{true, false} {
		f["srv_A"].online.Store(online)
		if p := propose(t, e, "Deploy again", stack("acme/api:1")...); !p.Empty() {
			t.Errorf("online %t: proposing the same workloads again plans %s, want nothing", online, shape(p))
		}
	}
	f["srv_A"].online.Store(true)

	before := f["srv_A"].docker.Runs
	next := propose(t, e, "Deploy api 2", stack("acme/api:2")...)
	if c := next.Counts(); next.Revisions != 1 || c.Replace != 1 || len(next.Steps()) != 1 || next.Steps()[0].Ref.Name != "api" {
		t.Errorf("a new image of api plans %s, want api alone", shape(next))
	}
	if r := planned(next, "db"); r.Action != resource.ActionNone || len(r.Changes) != 0 {
		t.Errorf("db's own plan = %+v, want nothing", r)
	}
	apply(t, e, next)
	if got := f["srv_A"].docker.Runs - before; got != 1 {
		t.Errorf("deploying api ran %d containers, want 1", got)
	}
	if got := rotating(t, db, next.Change); len(got) != 0 {
		t.Errorf("%v rotate, want no workload to", got)
	}
}

// A workload's environment compares as any field does, and stays masked: a
// value that differs is a change, of that key alone, that shows no value.
// With it the same, a change of the restart policy alone is done in place.
func TestAWorkloadsEnvironmentComparesAndStaysMasked(t *testing.T) {
	storetest.Each(t, testAWorkloadsEnvironmentComparesAndStaysMasked)
}

func testAWorkloadsEnvironmentComparesAndStaysMasked(t *testing.T, backend store.Backend) {
	e, _, _ := setup(t, backend, "srv_A")
	withEnv := func(env map[string]string, restart resourcev1.RestartPolicy) Edit {
		ed := web("nginx:1.27")
		c := ed.Spec.(*resourcev1.Container)
		c.Env, c.Restart = env, restart
		return ed
	}
	always := resourcev1.RestartPolicy_RESTART_POLICY_ALWAYS
	apply(t, e, propose(t, e, "Run web", withEnv(map[string]string{"API_KEY": "s3cret-value", "REGION": "eu"}, always)))

	p := propose(t, e, "Change a key", withEnv(map[string]string{"API_KEY": "another-value", "REGION": "eu"}, always))
	want := []resource.Change{{Path: "env[API_KEY]", Op: resource.OpModified, Sensitive: true}}
	if !reflect.DeepEqual(p.Resources[0].Changes, want) || p.Revisions != 1 || len(p.Steps()) != 1 ||
		p.Steps()[0].Action != resource.ActionReplace || !reflect.DeepEqual(p.Steps()[0].Changes, want) {
		t.Errorf("changing one key plans %s, want a masked change of that key and a replace", shape(p))
	}
	p = propose(t, e, "Change the policy", withEnv(map[string]string{"API_KEY": "s3cret-value", "REGION": "eu"}, resourcev1.RestartPolicy_RESTART_POLICY_NO))
	if len(p.Steps()) != 1 || p.Steps()[0].Action != resource.ActionUpdate || len(p.Steps()[0].Changes) != 1 {
		t.Errorf("changing the restart policy alone plans %s, want it done in place", shape(p))
	}
}

// noted is a sealed kind that holds a value, as a secret does, with
// something else to edit: Dockwell's secret has only its value, so an edit
// of one that sends none is refused. Its value is the Container's
// environment, which is sensitive, and its note the image, which isn't.
type noted struct{ variable.SecretType }

const notedKind resource.Kind = "test.noted"

func (noted) Kind() resource.Kind          { return notedKind }
func (noted) New() proto.Message           { return &resourcev1.Container{} }
func (noted) Validate(proto.Message) error { return nil }
func (noted) Value(spec proto.Message) string {
	return spec.(*resourcev1.Container).GetEnv()["VALUE"]
}

// Only sending a value rotates: an edit of a secret's other fields compares
// as any edit does, and one with a value rotates each time.
func TestOnlySendingAValueRotates(t *testing.T) { storetest.Each(t, testOnlySendingAValueRotates) }

func testOnlySendingAValueRotates(t *testing.T, backend store.Backend) {
	_, f, db := setup(t, backend, "srv_A")
	env := production(t, db)
	kinds := testKinds().With(noted{})
	e := New(db, kinds, f, secretstest.Keeper(db, kinds.With(scope.Types()...)), slog.New(slog.DiscardHandler))
	edit := func(note string, value ...string) Edit {
		spec := &resourcev1.Container{Image: note}
		if len(value) > 0 {
			spec.Env = map[string]string{"VALUE": value[0]}
		}
		return Edit{Ref: resource.Ref{Kind: notedKind, Scope: env, Name: "API_KEY"}, Spec: spec}
	}
	apply(t, e, propose(t, e, "Add it", edit("first")))
	if p := propose(t, e, "The same", edit("first")); !p.Empty() {
		t.Errorf("the same note with no value plans %s, want nothing", shape(p))
	}
	p := propose(t, e, "Change the note", edit("second"))
	want := []resource.Change{{Path: "image", Op: resource.OpModified, From: "first", To: "second"}}
	if !reflect.DeepEqual(p.Resources[0].Changes, want) || stored(t, db, p.Change).Items[0].Rotates {
		t.Errorf("changing the note alone plans %s, rotating; want the note's change alone", shape(p))
	}
	apply(t, e, p)
	apply(t, e, propose(t, e, "Set the value", edit("second", "s3cret")))
	p = propose(t, e, "The same value", edit("second", "s3cret"))
	if p.Revisions != 1 || !stored(t, db, p.Change).Items[0].Rotates {
		t.Errorf("setting the value it has plans %s, want a rotation", shape(p))
	}

	// Dockwell's secret refuses an edit with no value, so it never gets as
	// far as a plan.
	if _, err := e.Propose(t.Context(), Proposal{Summary: "No value", Actor: "usr_test", Edits: []Edit{secretEdit(env, "DB_PASSWORD", "")}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a secret with no value = %v, want ErrInvalid", err)
	}
}

// A rollback rotates a secret as an undo does: a failed change that set a
// secret to the value it had puts it back as a change too, so what it did
// says nothing of the value. The services it takes in don't rotate.
func TestARollbackRotatesASecret(t *testing.T) { storetest.Each(t, testARollbackRotatesASecret) }

func testARollbackRotatesASecret(t *testing.T, backend store.Backend) {
	e, f, db := setup(t, backend, "srv_A")
	env := production(t, db)
	apply(t, e, propose(t, e, "Set up", secretEdit(env, "DB_PASSWORD", "hunter2"), user(env, "api", "srv_A", map[string]string{"DB_PASSWORD": ""}, nil)))
	f["srv_A"].docker.Broken["acme/api:broken"] = true
	broken := user(env, "api", "srv_A", map[string]string{"DB_PASSWORD": ""}, nil)
	broken.Spec.(*resourcev1.Service).GetApp().Image = "acme/api:broken"
	p := propose(t, e, "Deploy a broken build", secretEdit(env, "DB_PASSWORD", "hunter2"), broken)
	err := e.Apply(t.Context(), p.Change)
	var failed *FailedError
	if !errors.As(err, &failed) || failed.Rollback == "" {
		t.Fatalf("apply = %v, want it to fail and roll back", err)
	}
	if got := rotating(t, db, failed.Rollback); !slices.Equal(got, []string{"DB_PASSWORD"}) {
		t.Errorf("in the rollback, %v rotate, want the secret alone", got)
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
