// SPDX-License-Identifier: AGPL-3.0-only

package health

import (
	"testing"

	"github.com/dockwell/dockwell/internal/docker"
)

// A container that exited 0, which Docker won't start again, ran to its end: a
// one-shot job. One that failed, or that Docker restarts, didn't.
func TestAOneShotJobFinishes(t *testing.T) {
	exited := func(code int, policy string) *docker.Container {
		return &docker.Container{State: docker.State{Status: "exited", ExitCode: code}, HostConfig: docker.HostConfig{RestartPolicy: docker.RestartPolicy{Name: policy}}}
	}
	for c, want := range map[*docker.Container]bool{
		exited(0, ""):                            true,
		exited(0, "no"):                          true,
		exited(0, "on-failure"):                  true,
		exited(1, "on-failure"):                  false,
		exited(0, "always"):                      false,
		exited(0, "unless-stopped"):              false,
		{State: docker.State{Status: "created"}}: false,
	} {
		if got := finished(c); got != want {
			t.Errorf("finished(%+v, %q) = %v, want %v", c.State, c.HostConfig.RestartPolicy.Name, got, want)
		}
	}
}
