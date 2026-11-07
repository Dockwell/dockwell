// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"time"

	"github.com/dockwell/dockwell/internal/mounts"
)

// MaxStuckPaths is maxStuckPaths, for a test to fill.
const MaxStuckPaths = maxStuckPaths

// StopAnswering stands a mount that stopped answering in for the disk: a path has
// wait to answer, and touch hears of each one asked. restore puts things back.
func StopAnswering(wait time.Duration, touch func(string)) (restore func()) {
	was := pathWait
	pathWait = wait
	touching.Store(&touch)
	return func() {
		pathWait = was
		touching.Store(nil)
		stuckMu.Lock()
		defer stuckMu.Unlock()
		clear(leftFollows)
		readMounts = mounts.Read
	}
}

// PretendMounts stands info in for what's mounted here, until StopAnswering restores.
func PretendMounts(info string) { readMounts = func() string { return info } }

// ResolveForRollout is how a rollout follows a server path.
func ResolveForRollout(d *Docker, p string) (string, error) { return d.resolvePath(p) }

// SilentRegistries is how many registries d holds as silent.
func (d *Docker) SilentRegistries() int {
	d.registries.mu.Lock()
	defer d.registries.mu.Unlock()
	return len(d.registries.silent)
}
