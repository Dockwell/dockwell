// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/dockwell/dockwell/internal/mounts"
)

// MaxStuckPaths is maxStuckPaths, for a test to fill.
const MaxStuckPaths = maxStuckPaths

// StopAnswering stands a mount that stopped answering in for the disk. stuck hears
// of each path asked, and says until when it won't answer, or nil for one that
// answers. A follow's wait is over the moment it asks a path that's stuck, and at no
// other time: the clock plays no part, so a follow that is only slow, on a busy
// machine, never passes for one that's stuck. wait is the time the product says it
// waited. restore puts things back.
func StopAnswering(wait time.Duration, stuck func(string) <-chan struct{}) (restore func()) {
	was, wasOver := pathWait, waitOver
	pathWait = wait
	waitsOver.Store(0)
	var mu sync.Mutex
	waiting := map[*follow]chan time.Time{}
	waitOver = func(f *follow) (<-chan time.Time, func()) {
		over := make(chan time.Time, 1)
		mu.Lock()
		defer mu.Unlock()
		waiting[f] = over
		return over, func() {
			mu.Lock()
			defer mu.Unlock()
			delete(waiting, f)
		}
	}
	hear := func(f *follow, p string) {
		until := stuck(p)
		if until == nil {
			return
		}
		mu.Lock()
		over := waiting[f]
		mu.Unlock()
		if over != nil {
			waitsOver.Add(1)
			over <- time.Now()
		}
		<-until
	}
	touching.Store(&hear)
	return func() {
		pathWait, waitOver = was, wasOver
		touching.Store(nil)
		stuckMu.Lock()
		defer stuckMu.Unlock()
		clear(leftFollows)
		readMounts = mounts.Read
	}
}

// waitsOver counts the waits that ran out since StopAnswering.
var waitsOver atomic.Int32

// WaitsOver is how many follows waited for a path until their wait was over, since
// StopAnswering.
func WaitsOver() int { return int(waitsOver.Load()) }

// FollowStuck follows a path whose disk doesn't answer until until, on the product's
// own clock with wait to answer in, and says what the follow was told.
func FollowStuck(wait time.Duration, until <-chan struct{}) error {
	was := pathWait
	pathWait = wait
	defer func() { pathWait = was }()
	_, err := followWithin("/stuck", func(string, func(string) error) (string, error) {
		<-until
		return "", nil
	})
	stuckMu.Lock()
	defer stuckMu.Unlock()
	clear(leftFollows)
	return err
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
