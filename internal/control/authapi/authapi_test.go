// SPDX-License-Identifier: AGPL-3.0-only

package authapi

import (
	"errors"
	"log/slog"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/dockwell/dockwell/internal/auth"
)

// Waiting as long as Retry-After says is always enough.
func TestSlowDownRetryAfterRoundsUp(t *testing.T) {
	h := New(Config{Logger: slog.New(slog.DiscardHandler)})
	for wait, want := range map[time.Duration]string{
		300 * time.Millisecond:  "1",
		time.Second:             "1",
		1200 * time.Millisecond: "2",
		15 * time.Minute:        "900",
	} {
		var cerr *connect.Error
		if !errors.As(h.rpcError(t.Context(), &auth.SlowDown{Wait: wait}), &cerr) {
			t.Fatal("not a connect error")
		}
		if got := cerr.Meta().Get("Retry-After"); got != want || cerr.Meta().Get(ReasonKey) != "slow_down" {
			t.Errorf("a wait of %s: Retry-After %q, reason %q, want %q and slow_down", wait, got, cerr.Meta().Get(ReasonKey), want)
		}
	}
}
