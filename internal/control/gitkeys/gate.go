// SPDX-License-Identifier: AGPL-3.0-only

package gitkeys

import (
	"context"
	"errors"
	"sync"
	"time"

	"connectrpc.com/connect"

	"github.com/dockwell/dockwell/internal/control/authapi"
)

const (
	// maxSSHTotal bounds all concurrent outbound SSH Git operations on the
	// control plane (tests, branch listings, ref resolutions and pack fetches).
	maxSSHTotal = 4
	// maxSSHList bounds concurrent SSH ref listings and connection tests.
	maxSSHList = 2
	// maxSSHFetch bounds concurrent SSH pack fetches and SHA-1DC verifications.
	maxSSHFetch = 2
	// maxSSHListPerProject caps one project below maxSSHList so another project
	// always has a listing slot available.
	maxSSHListPerProject = 1
	// maxSSHFetchPerProject caps one project below maxSSHFetch so another
	// project always has a fetch slot available for builds and detections.
	maxSSHFetchPerProject = 1
	// maxSSHPerKeyList bounds concurrent ref listings/tests on the same key.
	maxSSHPerKeyList = 1
	// defaultListWait bounds how long interactive callers wait in FIFO order
	// for an SSH listing slot before receiving the busy state.
	defaultListWait = 2 * time.Second
)

type sshWaiter struct {
	projectID string
	keyID     string
	fetch     bool
	granted   chan struct{}
	done      bool
}

// sshGate bounds concurrent outbound SSH Git connections across the control
// plane, per project and per key so concurrent tests, branch listings, hook
// resolves and pack fetches stay comfortably inside the control-plane memory
// budget and no single project can starve others (ADR 0113).
type sshGate struct {
	mu        sync.Mutex
	total     int
	lists     int
	fetches   int
	projList  map[string]int
	projFetch map[string]int
	keyList   map[string]int
	waiters   []*sshWaiter
	listWait  time.Duration
}

func (g *sshGate) listWaitDuration() time.Duration {
	if g.listWait > 0 {
		return g.listWait
	}
	return defaultListWait
}

func sshBusyErr() error {
	return authapi.ReasonError(
		connect.CodeResourceExhausted,
		errors.New("too many SSH Git checks are running right now, so Dockwell didn't start another. Try again in a moment"),
		"too_many_readings",
	)
}

// acquireList acquires a listing/test slot, waiting up to listWaitDuration in
// FIFO order before returning ResourceExhausted ("too_many_readings").
func (g *sshGate) acquireList(ctx context.Context, projectID, keyID string) (func(), error) {
	return g.acquireWait(ctx, projectID, keyID, false, g.listWaitDuration())
}

// acquireFetch acquires a pack-fetch slot for a build or detection, waiting in
// FIFO order until a slot is free or ctx ends (bounded by the step's timeout).
func (g *sshGate) acquireFetch(ctx context.Context, projectID, keyID string) (func(), error) {
	return g.acquireWait(ctx, projectID, keyID, true, -1)
}

func (g *sshGate) acquireWait(ctx context.Context, projectID, keyID string, fetch bool, wait time.Duration) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g.mu.Lock()
	if g.canAcquireLocked(projectID, keyID, fetch) {
		g.takeLocked(projectID, keyID, fetch)
		g.mu.Unlock()
		return g.releaseFunc(projectID, keyID, fetch), nil
	}
	if wait == 0 {
		g.mu.Unlock()
		return nil, sshBusyErr()
	}
	w := &sshWaiter{
		projectID: projectID,
		keyID:     keyID,
		fetch:     fetch,
		granted:   make(chan struct{}),
	}
	g.waiters = append(g.waiters, w)
	g.mu.Unlock()

	var timerCh <-chan time.Time
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		timerCh = timer.C
	}

	select {
	case <-w.granted:
		return g.releaseFunc(projectID, keyID, fetch), nil
	case <-timerCh:
		g.mu.Lock()
		if w.done {
			g.mu.Unlock()
			return g.releaseFunc(projectID, keyID, fetch), nil
		}
		w.done = true
		g.removeWaiterLocked(w)
		g.mu.Unlock()
		return nil, sshBusyErr()
	case <-ctx.Done():
		g.mu.Lock()
		if w.done {
			g.releaseLocked(projectID, keyID, fetch)
			g.wakeLocked()
			g.mu.Unlock()
			return nil, ctx.Err()
		}
		w.done = true
		g.removeWaiterLocked(w)
		g.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (g *sshGate) canAcquireLocked(projectID, keyID string, fetch bool) bool {
	if g.total >= maxSSHTotal {
		return false
	}
	if fetch {
		if g.fetches >= maxSSHFetch {
			return false
		}
		if projectID != "" && g.projFetch[projectID] >= maxSSHFetchPerProject {
			return false
		}
		return true
	}
	if g.lists >= maxSSHList {
		return false
	}
	if projectID != "" && g.projList[projectID] >= maxSSHListPerProject {
		return false
	}
	if keyID != "" && g.keyList[keyID] >= maxSSHPerKeyList {
		return false
	}
	return true
}

func (g *sshGate) takeLocked(projectID, keyID string, fetch bool) {
	if g.projList == nil {
		g.projList = map[string]int{}
	}
	if g.projFetch == nil {
		g.projFetch = map[string]int{}
	}
	if g.keyList == nil {
		g.keyList = map[string]int{}
	}
	g.total++
	if fetch {
		g.fetches++
		if projectID != "" {
			g.projFetch[projectID]++
		}
		return
	}
	g.lists++
	if projectID != "" {
		g.projList[projectID]++
	}
	if keyID != "" {
		g.keyList[keyID]++
	}
}

func (g *sshGate) releaseLocked(projectID, keyID string, fetch bool) {
	g.total--
	if fetch {
		g.fetches--
		if projectID != "" {
			g.projFetch[projectID]--
			if g.projFetch[projectID] <= 0 {
				delete(g.projFetch, projectID)
			}
		}
		return
	}
	g.lists--
	if projectID != "" {
		g.projList[projectID]--
		if g.projList[projectID] <= 0 {
			delete(g.projList, projectID)
		}
	}
	if keyID != "" {
		g.keyList[keyID]--
		if g.keyList[keyID] <= 0 {
			delete(g.keyList, keyID)
		}
	}
}

func (g *sshGate) wakeLocked() {
	for i := 0; i < len(g.waiters); {
		w := g.waiters[i]
		if w.done || !g.canAcquireLocked(w.projectID, w.keyID, w.fetch) {
			i++
			continue
		}
		w.done = true
		copy(g.waiters[i:], g.waiters[i+1:])
		g.waiters[len(g.waiters)-1] = nil
		g.waiters = g.waiters[:len(g.waiters)-1]
		g.takeLocked(w.projectID, w.keyID, w.fetch)
		close(w.granted)
	}
}

func (g *sshGate) removeWaiterLocked(target *sshWaiter) {
	for i, w := range g.waiters {
		if w == target {
			copy(g.waiters[i:], g.waiters[i+1:])
			g.waiters[len(g.waiters)-1] = nil
			g.waiters = g.waiters[:len(g.waiters)-1]
			return
		}
	}
}

func (g *sshGate) releaseFunc(projectID, keyID string, fetch bool) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.releaseLocked(projectID, keyID, fetch)
			g.wakeLocked()
			g.mu.Unlock()
		})
	}
}
