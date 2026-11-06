// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
)

// Following a server path means asking the disk, and a network mount whose far end
// is down holds that question for as long as it's down, with no context to end it.
// A plan for a service that binds such a path would stick there, so a follow runs on
// a goroutine of its own, and what waits for it stops at pathWait. The path stays
// marked until the follow comes back, so plans keep hearing at once, and each dead
// path holds one goroutine however often it is planned.

// maxStuckPaths bounds how many follows can be left behind. Past it no path is
// followed, as each one left behind holds a thread.
const maxStuckPaths = 16

var (
	// pathWait is how long a path has to answer. A plan has a minute for all it does.
	pathWait = 10 * time.Second
	// touching hears of each path about to be asked of the disk. A test blocks in it
	// to stand in for a mount that stopped answering.
	touching atomic.Pointer[func(string)]

	stuckMu    sync.Mutex
	stuckPaths = map[string]bool{}
)

// touchPath says a path is about to be asked of the disk.
func touchPath(p string) {
	if hear := touching.Load(); hear != nil {
		(*hear)(p)
	}
}

// followWithin follows p with walk, for at most pathWait.
func followWithin(p string, walk func(string) (string, error)) (string, error) {
	stuckMu.Lock()
	refused := stuckPaths[p] || len(stuckPaths) >= maxStuckPaths
	stuckMu.Unlock()
	if refused {
		return "", notAnswering(p)
	}
	type followed struct {
		to  string
		err error
	}
	var left, back bool // guarded by stuckMu
	done := make(chan followed, 1)
	go func() {
		to, err := walk(p)
		stuckMu.Lock()
		back = true
		if left {
			delete(stuckPaths, p)
		}
		stuckMu.Unlock()
		done <- followed{to, err}
	}()
	limit := time.NewTimer(pathWait)
	defer limit.Stop()
	select {
	case out := <-done:
		return out.to, out.err
	case <-limit.C:
	}
	stuckMu.Lock()
	defer stuckMu.Unlock()
	if back {
		out := <-done
		return out.to, out.err
	}
	left = true
	stuckPaths[p] = true
	return "", notAnswering(p)
}

func notAnswering(p string) error {
	return fault.New(resource.ErrPathNotAnswering, "the server path %s isn't answering after %s, so Dockwell can't tell where it leads, and this can't go ahead. "+
		"It may be on a network mount whose far end is down. Check the mount on the server, then try again", p, human.Duration(pathWait))
}
