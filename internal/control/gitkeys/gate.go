// SPDX-License-Identifier: AGPL-3.0-only

package gitkeys

import (
	"errors"
	"sync"

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
	// maxSSHPerProject bounds concurrent SSH operations for any single project
	// so one project cannot starve others.
	maxSSHPerProject = 2
	// maxSSHPerKeyList bounds concurrent ref listings/tests on the same key.
	maxSSHPerKeyList = 1
)

// sshGate bounds concurrent outbound SSH Git connections across the control
// plane, per project and per key so concurrent tests, branch listings, hook
// resolves and pack fetches stay comfortably inside the control-plane memory
// budget (ADR 0113).
type sshGate struct {
	mu      sync.Mutex
	total   int
	lists   int
	fetches int
	byProj  map[string]int
	keyList map[string]int
}

func (g *sshGate) acquire(projectID, keyID string, fetch bool) (func(), error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.byProj == nil {
		g.byProj = map[string]int{}
	}
	if g.keyList == nil {
		g.keyList = map[string]int{}
	}
	busy := func() error {
		return authapi.ReasonError(
			connect.CodeResourceExhausted,
			errors.New("too many SSH Git checks are running right now, so Dockwell didn't start another. Try again in a moment"),
			"too_many_readings",
		)
	}
	if g.total >= maxSSHTotal {
		return nil, busy()
	}
	if projectID != "" && g.byProj[projectID] >= maxSSHPerProject {
		return nil, busy()
	}
	if fetch {
		if g.fetches >= maxSSHFetch {
			return nil, busy()
		}
		g.fetches++
	} else {
		if g.lists >= maxSSHList {
			return nil, busy()
		}
		if keyID != "" && g.keyList[keyID] >= maxSSHPerKeyList {
			return nil, busy()
		}
		g.lists++
		if keyID != "" {
			g.keyList[keyID]++
		}
	}
	g.total++
	if projectID != "" {
		g.byProj[projectID]++
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.total--
			if projectID != "" {
				g.byProj[projectID]--
				if g.byProj[projectID] <= 0 {
					delete(g.byProj, projectID)
				}
			}
			if fetch {
				g.fetches--
			} else {
				g.lists--
				if keyID != "" {
					g.keyList[keyID]--
					if g.keyList[keyID] <= 0 {
						delete(g.keyList, keyID)
					}
				}
			}
		})
	}, nil
}
