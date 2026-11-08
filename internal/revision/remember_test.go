// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"maps"
	"slices"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// keeping is an agent that keeps desired state, over its server's own drivers. It
// reports what the test says it holds, and nothing it was sent changes that, so
// what the engine sends rests on what an agent says alone.
type keeping struct {
	resource.Agent
	keeps bool
	cache string

	mu     sync.Mutex
	held   map[resource.Ref]int64
	got    []remembered
	forgot map[resource.Ref]int64
}

type remembered struct {
	ref      resource.Ref
	revision int
	change   string
	spec     proto.Message
}

func (k *keeping) Holds() (map[resource.Ref]int64, string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return maps.Clone(k.held), k.cache, k.keeps
}

func (k *keeping) Remember(_ context.Context, ref resource.Ref, spec proto.Message, revision int, change string) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.got = append(k.got, remembered{ref: ref, revision: revision, change: change, spec: spec})
	return true, nil
}

func (k *keeping) Forget(_ context.Context, ref resource.Ref, revision int64) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.forgot[ref] = revision
	return true, nil
}

func (k *keeping) hold(held map[resource.Ref]int64) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.held = held
}

// sent is what the agent was sent, by resource, and empties it.
func (k *keeping) sent() map[string]remembered {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := map[string]remembered{}
	for _, r := range k.got {
		out[r.ref.Name] = r
	}
	k.got = nil
	return out
}

func (k *keeping) forgotten() map[string]int64 {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := map[string]int64{}
	for ref, rev := range k.forgot {
		out[ref.Name] = rev
	}
	k.forgot = map[resource.Ref]int64{}
	return out
}

func fill(t *testing.T, e *Engine, want Filled) {
	t.Helper()
	n, err := e.Remember(t.Context(), "srv_A")
	if err != nil || n != want {
		t.Fatalf("Remember = %+v, %v; want %+v", n, err, want)
	}
}

var webRef = resource.Ref{Kind: container.Kind, Scope: "srv_A", Name: "web"}

// An agent is sent what its server runs, at each latest revision live there, where
// it doesn't keep that revision already, and nothing where it keeps a later one.
func TestRememberSendsWhatAnAgentLacks(t *testing.T) {
	storetest.Each(t, testRememberSendsWhatAnAgentLacks)
}

func testRememberSendsWhatAnAgentLacks(t *testing.T, backend store.Backend) {
	e, f, _ := setup(t, backend, "srv_A")
	first := propose(t, e, "Run web", web("nginx:1.27"), rule("srv_A", "http", 80))
	apply(t, e, first)
	k := &keeping{Agent: f["srv_A"].agent, keeps: true, cache: "c1", held: map[resource.Ref]int64{}, forgot: map[resource.Ref]int64{}}
	f["srv_A"].agent = k

	// An agent that keeps nothing yet is sent all of it.
	fill(t, e, Filled{Kept: 2, NewCache: true})
	got := k.sent()
	if names := slices.Sorted(maps.Keys(got)); !slices.Equal(names, []string{"http", "web"}) {
		t.Fatalf("sent %v, want http and web", names)
	}
	if w := got["web"]; w.revision != 1 || w.change != first.Change || w.spec.(*resourcev1.Container).GetImage() != "nginx:1.27" {
		t.Errorf("sent web at revision %d of %s, running %v", w.revision, w.change, w.spec)
	}
	// Sent again while it says it lacks them: what it holds is its word alone.
	fill(t, e, Filled{Kept: 2})
	k.sent()
	http := resource.Ref{Kind: "firewall.rule", Scope: "srv_A", Name: "http"}
	k.hold(map[resource.Ref]int64{webRef: 1, http: 1})
	fill(t, e, Filled{})
	if got := k.sent(); len(got) != 0 {
		t.Errorf("sent %v to an agent that keeps it all", slices.Sorted(maps.Keys(got)))
	}

	// A step it didn't keep, as an agent from before this one's: only that is sent.
	second := propose(t, e, "Upgrade web", web("nginx:1.28"))
	apply(t, e, second)
	fill(t, e, Filled{Kept: 1})
	if w := k.sent()["web"]; w.revision != 2 || w.change != second.Change || w.spec.(*resourcev1.Container).GetImage() != "nginx:1.28" {
		t.Errorf("sent web at revision %d of %s, running %v; want revision 2", w.revision, w.change, w.spec)
	}

	// A later revision than the store has live there, as of a step that ended
	// after its stream did: that's for resuming its change to decide.
	k.hold(map[resource.Ref]int64{webRef: 7, http: 1})
	fill(t, e, Filled{})
	if got := k.sent(); len(got) != 0 {
		t.Errorf("sent %v over a later revision", slices.Sorted(maps.Keys(got)))
	}

	// An agent that keeps nothing at all, and one that isn't connected, are sent nothing.
	k.mu.Lock()
	k.keeps = false
	k.mu.Unlock()
	fill(t, e, Filled{})
	k.mu.Lock()
	k.keeps = true
	k.mu.Unlock()
	f["srv_A"].online.Store(false)
	fill(t, e, Filled{})
	if got := k.sent(); len(got) != 0 {
		t.Errorf("sent %v to an agent that keeps nothing, or isn't there", slices.Sorted(maps.Keys(got)))
	}
}

// What an agent keeps that its server no longer runs is forgotten, at the revision
// it said, as after a delete step that failed, or a record that came back, and
// nothing placed there is.
func TestRememberForgetsWhatTheServerNoLongerRuns(t *testing.T) {
	storetest.Each(t, testRememberForgetsWhatTheServerNoLongerRuns)
}

func testRememberForgetsWhatTheServerNoLongerRuns(t *testing.T, backend store.Backend) {
	e, f, _ := setup(t, backend, "srv_A")
	apply(t, e, propose(t, e, "Run web", web("nginx:1.27")))
	k := &keeping{Agent: f["srv_A"].agent, keeps: true, cache: "c1", forgot: map[resource.Ref]int64{}}
	f["srv_A"].agent = k
	stale := resource.Ref{Kind: container.Kind, Scope: "srv_A", Name: "gone"}
	k.hold(map[resource.Ref]int64{webRef: 1, stale: 4})
	fill(t, e, Filled{Forgot: 1, NewCache: true})
	if got := k.forgotten(); len(got) != 1 || got["gone"] != 4 {
		t.Errorf("forgot %v, want gone at 4, and not web", got)
	}
}

// holdingWeb holds the step for web until it's released, as a step under way does.
type holdingWeb struct {
	resource.Agent
	reached, release chan struct{}
}

func (h *holdingWeb) Apply(ctx context.Context, step resource.Step, desired proto.Message) error {
	if step.Ref == webRef {
		close(h.reached)
		<-h.release
	}
	return h.Agent.Apply(ctx, step, desired)
}

// What a change applying now placed on the server, but hasn't made live yet, is
// neither sent nor forgotten: its step has the say.
func TestRememberLeavesWhatAChangeIsApplying(t *testing.T) {
	storetest.Each(t, testRememberLeavesWhatAChangeIsApplying)
}

func testRememberLeavesWhatAChangeIsApplying(t *testing.T, backend store.Backend) {
	e, f, _ := setup(t, backend, "srv_A")
	apply(t, e, propose(t, e, "Run web", web("nginx:1.27")))
	h := &holdingWeb{Agent: f["srv_A"].agent, reached: make(chan struct{}), release: make(chan struct{})}
	k := &keeping{Agent: h, keeps: true, cache: "c1", held: map[resource.Ref]int64{webRef: 1}, forgot: map[resource.Ref]int64{}}
	f["srv_A"].agent = k
	p := propose(t, e, "Upgrade web", web("nginx:1.28"))
	done := make(chan error, 1)
	go func() { done <- e.Apply(t.Context(), p.Change) }()
	<-h.reached
	fill(t, e, Filled{NewCache: true})
	if got, gone := k.sent(), k.forgotten(); len(got) != 0 || len(gone) != 0 {
		t.Errorf("mid-apply, sent %v and forgot %v", slices.Sorted(maps.Keys(got)), gone)
	}
	close(h.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	fill(t, e, Filled{Kept: 1})
	if w := k.sent()["web"]; w.revision != 2 {
		t.Errorf("once live, sent web at revision %d, want 2", w.revision)
	}
}

// A fill that sends values goes on the server's timeline, saying whether the agent's
// cache was new: the first time, and when its ID changes.
func TestAFillWithValuesGoesOnTheTimeline(t *testing.T) {
	storetest.Each(t, testAFillWithValuesGoesOnTheTimeline)
}

func testAFillWithValuesGoesOnTheTimeline(t *testing.T, backend store.Backend) {
	e, f, db := setup(t, backend, "srv_A")
	env := production(t, db)
	apply(t, e, propose(t, e, "Run api", secretEdit(env, "TOKEN", "s3cret"), user(env, "api", "srv_A", map[string]string{"TOKEN": ""}, nil)))
	k := &keeping{Agent: f["srv_A"].agent, keeps: true, cache: "c1", held: map[resource.Ref]int64{}, forgot: map[resource.Ref]int64{}}
	f["srv_A"].agent = k
	events := func() []DesiredSent {
		t.Helper()
		var out []DesiredSent
		if err := db.View(t.Context(), func(tx *store.Tx) error {
			got, err := tx.Timeline(t.Context(), store.TimelineQuery{Filter: store.TimelineFilter{Actions: []string{ActionDesiredSent}}})
			for _, entry := range got {
				if sent, ok := ReadDesiredSent(entry.Event.Detail); ok && entry.Event.Target == "srv_A" {
					out = append(out, sent)
				}
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	fill(t, e, Filled{Kept: 1, WithValues: 1, NewCache: true})
	fill(t, e, Filled{Kept: 1, WithValues: 1})
	k.mu.Lock()
	k.cache = "c2"
	k.mu.Unlock()
	fill(t, e, Filled{Kept: 1, WithValues: 1, NewCache: true})
	got := events()
	news := 0
	for _, ev := range got {
		if ev.Resources != 1 || ev.WithValues != 1 || ev.Cache == "" {
			t.Errorf("an event says %+v", ev)
		}
		if ev.NewCache {
			news++
		}
	}
	if len(got) != 3 || news != 2 {
		t.Errorf("the timeline has %+v, want 3 fills, 2 of a new cache", got)
	}
}
