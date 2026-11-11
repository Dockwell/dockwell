// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/agent"
	"github.com/dockwell/dockwell/internal/datadir"
)

// Every kind an agent runs is either put back by the agent while the control
// plane is away, or left alone with a reason (ADR 0054). A kind added to the
// agent's drivers fails here until someone decides which.
func TestEveryAgentKindHasAReconcileDecision(t *testing.T) {
	stdio := IO{Out: io.Discard, Err: io.Discard, Getenv: func(string) string { return "" }}
	// agentDrivers starts what keeps a server's folders, which can write once
	// more as it stops. So it gets a context that has ended already, and a folder
	// of its own that's removed as well as it can be: t.TempDir fails a test
	// whose folder gains a file while it's being removed.
	root, err := os.MkdirTemp("", "dockwell-kinds-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// A writer on its way out can make the folder again, so look again.
		for range 40 {
			_ = os.RemoveAll(root)
			time.Sleep(25 * time.Millisecond)
			if _, err := os.Stat(root); os.IsNotExist(err) {
				return
			}
		}
		t.Logf("left %s behind: something still writes to it", root)
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cfg := agentDrivers(ctx, stdio, slog.New(slog.DiscardHandler), datadir.Dir{Root: root}, nil, nil, "")
	kinds := cfg.Drivers.All()
	if len(kinds) == 0 {
		t.Fatal("the agent's drivers name no kind")
	}
	for _, kind := range kinds {
		reconciled, why, decided := agent.ReconcileDecided(kind)
		switch {
		case !decided:
			t.Errorf("%s: the agent runs this kind, and nothing says what it does with it while the control plane is away. "+
				"Add it to reconciledKinds or leftAlone in internal/agent/reconcile.go, and to ADR 0054's list", kind)
		case !reconciled && why == "":
			t.Errorf("%s: left alone with no reason", kind)
		}
	}
}
