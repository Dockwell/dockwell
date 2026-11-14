// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/store"
)

// together holds each call it gates until n of them are in, as servers whose steps
// go side by side all are; it gives up after a while, as steps that go one after
// another never get there.
type together struct {
	mu     sync.Mutex
	n, in  int
	all    chan struct{}
	failOn string
	order  []string
}

func newTogether(n int) *together { return &together{n: n, all: make(chan struct{})} }

func (g *together) arrive(server string) error {
	g.mu.Lock()
	g.in++
	g.order = append(g.order, server)
	if g.in == g.n {
		close(g.all)
	}
	g.mu.Unlock()
	select {
	case <-g.all:
	case <-time.After(5 * time.Second):
		return errors.New("the other server's step never started alongside this one")
	}
	if server == g.failOn {
		return errors.New("the step failed on " + server)
	}
	return nil
}

// gated is a server's agent whose calls to carry out a step go through together.
type gated struct {
	resource.Agent
	server string
	g      *together
}

func (a gated) Apply(ctx context.Context, step resource.Step, desired proto.Message) error {
	if err := a.g.arrive(a.server); err != nil {
		return err
	}
	return a.Agent.Apply(ctx, step, desired)
}

// runsAPI reports whether s runs onA's container.
func runsAPI(t *testing.T, s *server) bool {
	t.Helper()
	c, err := s.docker.Inspect(t.Context(), container.Target{Name: "api"})
	return err == nil && c != nil
}

// on is onA's container, on server.
func on(server string) Edit {
	e := onA()
	e.Ref.Scope = server
	return e
}

// A wave's servers go side by side (ADR 0094): their steps are under way at once.
func TestAWavesServersGoSideBySide(t *testing.T) {
	e, f, _ := setup(t, store.SQLite, "srv_A", "srv_B")
	g := newTogether(2)
	for _, s := range []string{"srv_A", "srv_B"} {
		f[s].agent = gated{Agent: f[s].agent, server: s, g: g}
	}
	p, err := e.Propose(t.Context(), Proposal{
		Summary: "both", Actor: "usr_test", Edits: []Edit{on("srv_A"), on("srv_B")},
		Rollout: &Rollout{Servers: []string{"srv_A", "srv_B"}, Batch: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Waves == nil || len(p.Waves.Order) != 1 {
		t.Fatalf("the waves are %+v, want one of both servers", p.Waves)
	}
	if err := e.Apply(t.Context(), p.Change); err != nil {
		t.Fatalf("applying = %v", err)
	}
	for _, s := range []string{"srv_A", "srv_B"} {
		if !runsAPI(t, f[s]) {
			t.Errorf("%s doesn't run api", s)
		}
	}
}

// A step that fails in a wave lets the steps under way beside it end, and starts no
// other: the next wave is skipped, and the change fails with that step.
func TestAFailureInAWaveSkipsWhatHasntStarted(t *testing.T) {
	e, f, db := setup(t, store.SQLite, "srv_A", "srv_B", "srv_C")
	g := newTogether(2)
	g.failOn = "srv_A"
	for _, s := range []string{"srv_A", "srv_B", "srv_C"} {
		f[s].agent = gated{Agent: f[s].agent, server: s, g: g}
	}
	p, err := e.Propose(t.Context(), Proposal{
		Summary: "three", Actor: "usr_test", Edits: []Edit{on("srv_A"), on("srv_B"), on("srv_C")},
		Rollout: &Rollout{Servers: []string{"srv_A", "srv_B", "srv_C"}, Batch: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	var failed *FailedError
	if err := e.Apply(t.Context(), p.Change); !errors.As(err, &failed) {
		t.Fatalf("applying = %v, want the change failed", err)
	}
	g.mu.Lock()
	order := slices.Clone(g.order)
	g.mu.Unlock()
	if slices.Contains(order, "srv_C") {
		t.Errorf("the next wave's server was called after a failure: %v", order)
	}
	// srv_B's step was under way beside the one that failed, and ended; what it made
	// goes as the change rolls back.
	if !slices.Contains(order, "srv_B") {
		t.Errorf("srv_B's step never started beside srv_A's: %v", order)
	}
	var c store.Change
	if err := db.View(t.Context(), func(tx *store.Tx) error {
		var err error
		c, err = tx.GetChange(t.Context(), p.Change)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	progress, err := decodeProgress(c.Progress)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range progress.Steps {
		want := map[string]StepState{"srv_A": StepFailed, "srv_B": StepDone, "srv_C": StepSkipped}[st.Server]
		if st.State != want {
			t.Errorf("%s's step is %v, want %v", st.Server, st.State, want)
		}
	}
}
