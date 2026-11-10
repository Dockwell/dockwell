// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
)

// A wait's deadline runs on the control plane's clock: an agent whose clock is
// an hour off says how long is left, and that's what the countdown takes.
func TestAWaitsDeadlineRunsOnTheControlPlanesClock(t *testing.T) {
	skewed := time.Now().Add(time.Hour + 45*time.Second)
	got := accessWait(&agentv1.AccessWait{Deadline: timestamppb.New(skewed), Left: durationpb.New(45 * time.Second), SshPorts: []uint32{22}})
	if until := time.Until(got.Deadline); until < 40*time.Second || until > 46*time.Second {
		t.Errorf("the deadline is %s away, want the 45 s the agent said were left", until)
	}
	// An agent that says only when: its deadline, as it is.
	if got := accessWait(&agentv1.AccessWait{Deadline: timestamppb.New(skewed)}); !got.Deadline.Equal(skewed) {
		t.Errorf("with no time left said, the deadline is %v, want %v", got.Deadline, skewed)
	}
}
