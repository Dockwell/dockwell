// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Stopping dockwell run while it starts is a stop, wherever it is: a write its stop
// ended under it, which database/sql rolls back, isn't a failure. So run exits 0, as
// a service systemd stops does, and says nothing of a commit (#758). Each run is
// stopped a little later than the one before, from at once to past the start-up
// line, and once on the start-up line itself, as the failure in CI was. The moment
// a write's rollback beats its commit is narrow; the store's own test makes it
// every time (TestAWriteItsStopEndsIsTheStop).
func TestStoppingRunWhileItStartsIsAStop(t *testing.T) {
	const runs = 25
	for i := range runs {
		after := time.Duration(i) * 50 * time.Millisecond
		dir := t.TempDir()
		ctx, cancel := context.WithCancel(context.Background())
		out, errOut := &syncBuffer{}, &syncBuffer{}
		done := make(chan int, 1)
		go func() {
			done <- Execute(ctx, []string{"run", "--listen", "127.0.0.1:0", "--agent-listen", "127.0.0.1:0", "--data-dir", dir, "--log-format", "json"},
				IO{Out: out, Err: errOut, Getenv: quiet(func(string) string { return "" }), OutIsTerminal: func() bool { return true }})
		}()
		if i == runs-1 {
			// On its start-up line, as #757's CI run stopped it.
			for deadline := time.Now().Add(startUp); !strings.Contains(out.String(), "/claim#") && time.Now().Before(deadline); {
				time.Sleep(time.Millisecond)
			}
		} else {
			time.Sleep(after)
		}
		cancel()
		select {
		case code := <-done:
			if code != exitOK || strings.Contains(errOut.String(), "can't commit") {
				t.Errorf("run stopped after %v exited %d:\n%s", after, code, errOut.String())
			}
		case <-time.After(startUp):
			t.Fatalf("run stopped after %v never exited", after)
		}
	}
}
