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

// keeping is an agent that keeps what it's told to remember, over its server's own
// drivers, as a stream's session does for an agent from ADR 0054 on.
type keeping struct {
	resource.Agent
	keeps bool

	mu   sync.Mutex
	held map[resource.Ref]int64
	got  []remembered
}

type remembered struct {
	ref      resource.Ref
	revision int
	change   string
	spec     proto.Message
}

func (k *keeping) Holds() (map[resource.Ref]int64, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return maps.Clone(k.held), k.keeps
}

func (k *keeping) Remember(_ context.Context, ref resource.Ref, spec proto.Message, revision int, change string) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.got = append(k.got, remembered{ref: ref, revision: revision, change: change, spec: spec})
	if k.held[ref] > int64(revision) {
		return false, nil
	}
	k.held[ref] = int64(revision)
	return true, nil
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

// An agent is sent what its server runs, at each latest revision live there, where
// it doesn't keep that revision already, and nothing where it keeps a later one.
func TestRememberSendsWhatAnAgentLacks(t *testing.T) {
	storetest.Each(t, testRememberSendsWhatAnAgentLacks)
}

func testRememberSendsWhatAnAgentLacks(t *testing.T, backend store.Backend) {
	e, f, _ := setup(t, backend, "srv_A")
	first := propose(t, e, "Run web", web("nginx:1.27"), rule("srv_A", "http", 80))
	apply(t, e, first)
	k := &keeping{Agent: f["srv_A"].agent, keeps: true, held: map[resource.Ref]int64{}}
	f["srv_A"].agent = k

	remember := func(want int) {
		t.Helper()
		n, err := e.Remember(t.Context(), "srv_A")
		if err != nil || n != want {
			t.Fatalf("Remember = %d, %v; want %d kept", n, err, want)
		}
	}
	// An agent that keeps nothing yet is sent all of it.
	remember(2)
	got := k.sent()
	if names := slices.Sorted(maps.Keys(got)); !slices.Equal(names, []string{"http", "web"}) {
		t.Fatalf("sent %v, want http and web", names)
	}
	if w := got["web"]; w.revision != 1 || w.change != first.Change || w.spec.(*resourcev1.Container).GetImage() != "nginx:1.27" {
		t.Errorf("sent web at revision %d of %s, running %v", w.revision, w.change, w.spec)
	}
	// Then nothing, as it keeps all of it.
	remember(0)
	if got := k.sent(); len(got) != 0 {
		t.Errorf("sent %v again", slices.Sorted(maps.Keys(got)))
	}

	// A step it didn't keep, as an agent from before this one's: only that is sent.
	second := propose(t, e, "Upgrade web", web("nginx:1.28"))
	apply(t, e, second)
	remember(1)
	if w := k.sent()["web"]; w.revision != 2 || w.change != second.Change || w.spec.(*resourcev1.Container).GetImage() != "nginx:1.28" {
		t.Errorf("sent web at revision %d of %s, running %v; want revision 2", w.revision, w.change, w.spec)
	}

	// A later revision than the store has live there, as of a step that ended
	// after its stream did: that's for resuming its change to decide.
	k.mu.Lock()
	k.held[resource.Ref{Kind: container.Kind, Scope: "srv_A", Name: "web"}] = 7
	k.mu.Unlock()
	remember(0)
	if got := k.sent(); len(got) != 0 {
		t.Errorf("sent %v over a later revision", slices.Sorted(maps.Keys(got)))
	}

	// An agent that keeps nothing at all, and one that isn't connected, are sent nothing.
	k.mu.Lock()
	k.keeps, k.held = false, map[resource.Ref]int64{}
	k.mu.Unlock()
	remember(0)
	k.keeps = true
	f["srv_A"].online.Store(false)
	remember(0)
	if got := k.sent(); len(got) != 0 {
		t.Errorf("sent %v to an agent that keeps nothing, or isn't there", slices.Sorted(maps.Keys(got)))
	}
}
