// SPDX-License-Identifier: AGPL-3.0-only

package agentapi_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/agent"
	"github.com/dockwell/dockwell/internal/agent/desired"
	"github.com/dockwell/dockwell/internal/buildinfo"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/resource"
)

// The agent and the control plane name a removed server's refusal the same way.
func TestTheRemovedReasonIsOneWord(t *testing.T) {
	if agent.ReasonRemoved != agentapi.ReasonServerRemoved {
		t.Errorf("the agent reads %q, and the control plane says %q", agent.ReasonRemoved, agentapi.ReasonServerRemoved)
	}
}

// heard collects what agents hand back.
type heard struct {
	mu   sync.Mutex
	got  []*agentv1.Reconciled
	fail bool
}

func (h *heard) hear(_ context.Context, _ string, r *agentv1.Reconciled) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.got = append(h.got, r)
	if h.fail {
		return context.DeadlineExceeded
	}
	return nil
}

func (h *heard) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.got)
}

// What an agent did on its own goes to the control plane as its stream opens, and
// once the control plane has it, the agent drops it. What the control plane can't
// keep is sent again on the next stream.
func TestWhatAnAgentDidOnItsOwnIsHandedBack(t *testing.T) {
	s := startStack(t, 5*time.Second)
	id := s.enrollLocal(t)
	dir := t.TempDir()
	store, err := desired.Open(dir, id.ServerID, nil)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := desired.OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	web := resource.Ref{Kind: "container", Scope: id.ServerID, Name: "web"}
	for _, what := range []string{desired.Made, desired.Waiting} {
		if err := journal.Add(web, what, "because", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	h := &heard{fail: true}
	s.agents.OnReconciled(h.hear)
	run := func() (stop func()) {
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			_ = agent.Run(ctx, agent.Config{
				Logger: slog.New(slog.DiscardHandler), Dir: dir, Identity: id, Build: buildinfo.Read(), Collector: fakeCollector(),
				Desired: store, Journal: journal, AwayAfter: time.Hour,
			})
			close(done)
		}()
		waitForAgent(t, s.agents, id.ServerID, true)
		return func() { cancel(); <-done }
	}
	until := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}
	stop := run()
	until("the hand-back", func() bool { return h.count() == 1 })
	got := h.got[0]
	if len(got.GetEntries()) != 2 || got.GetEntries()[0].GetAction() != agentv1.LocalAction_LOCAL_ACTION_MADE ||
		got.GetEntries()[1].GetReason() != "because" || got.GetEntries()[0].GetResource().GetName() != "web" {
		t.Fatalf("handed back %v", got)
	}
	time.Sleep(100 * time.Millisecond)
	if left, _, _ := journal.Pending(); len(left) != 2 {
		t.Fatalf("dropped %d entries the control plane couldn't keep", 2-len(left))
	}
	stop()
	waitForAgent(t, s.agents, id.ServerID, false)

	h.mu.Lock()
	h.fail = false
	h.mu.Unlock()
	stop = run()
	defer stop()
	until("the hand-back again", func() bool { return h.count() == 2 })
	until("the agent to drop what the control plane has", func() bool {
		left, dropped, _ := journal.Pending()
		return len(left) == 0 && dropped == 0
	})

	// What it writes down while the stream is up, as work that began while the
	// control plane was away ends, goes on that stream, not the next one.
	if err := journal.Add(web, desired.Made, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	until("what was written down while connected", func() bool { return h.count() == 3 })
	until("the agent to drop it", func() bool {
		left, _, _ := journal.Pending()
		return len(left) == 0
	})
}

// An agent whose server is removed from the fleet forgets what it kept of what its
// server ran, values and key, as the control plane refuses it as removed.
func TestARemovedServersAgentForgets(t *testing.T) {
	s := startStack(t, 5*time.Second)
	id := s.enrollLocal(t)
	dir := t.TempDir()
	store, err := desired.Open(dir, id.ServerID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Keep(desired.Record{Ref: resource.Ref{Kind: "container", Scope: id.ServerID, Name: "web"}, Revision: 1, Spec: []byte{}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		_ = agent.Run(ctx, agent.Config{Logger: slog.New(slog.DiscardHandler), Dir: dir, Identity: id, Build: buildinfo.Read(), Collector: fakeCollector(), Desired: store})
	}()
	waitForAgent(t, s.agents, id.ServerID, true)
	if err := s.auth.Revoke(t.Context(), id.ServerID, "cli:test"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		_, keyErr := os.Stat(filepath.Join(dir, desired.KeyFile))
		_, dirErr := os.Stat(filepath.Join(dir, desired.Folder))
		if os.IsNotExist(keyErr) && os.IsNotExist(dirErr) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the removed server's agent still keeps its records: %v, %v", keyErr, dirErr)
		}
	}
}
