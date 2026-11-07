// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/mounts"
	"github.com/dockwell/dockwell/internal/resource"
)

// Following a server path means asking the disk, and a network mount whose far end
// is down holds that question for as long as it's down, with no context to end it.
// A plan for a service that binds such a path would stick there, so a follow runs on
// a goroutine of its own, and what waits for it stops at pathWait.
//
// Where it stopped stays marked until it comes back: the whole mount, for one that
// can stop answering, as everything on it is down together, and otherwise that path.
// So a dead mount costs one wait and one goroutine, however many paths are on it and
// however often they're planned, and a path anywhere else is followed as ever.

// maxStuckPaths bounds how many follows can be left behind, as each holds a thread.
// Past it no path is followed. Paths on one mount count once, so what fills it is a
// disk that stopped answering in many places, or many mounts.
const maxStuckPaths = 16

var (
	// pathWait is how long a path has to answer. A plan has a minute for all it does.
	pathWait = 10 * time.Second
	// touching hears of each path about to be asked of the disk. A test blocks in it
	// to stand in for a mount that stopped answering.
	touching atomic.Pointer[func(string)]
	// readMounts is what's mounted here, which a test stands in for.
	readMounts = mounts.Read

	stuckMu sync.Mutex
	// leftFollows is each follow left behind, and what to keep away from.
	leftFollows = map[*follow]string{}
)

// follow is one following of a path.
type follow struct {
	path string                 // what it was asked to follow
	at   atomic.Pointer[string] // what it's asking the disk now
	back bool                   // it returned, guarded by stuckMu
}

// touch says a follow is about to ask the disk for p, or refuses if that's known not
// to answer.
func (f *follow) touch(p string) error {
	if place := stalledAt(p); place != "" {
		return placeNotAnswering(f.path, place)
	}
	f.at.Store(&p)
	if hear := touching.Load(); hear != nil {
		(*hear)(p)
	}
	return nil
}

// stalledAt says which place left behind holds p, or nothing.
func stalledAt(p string) string {
	stuckMu.Lock()
	defer stuckMu.Unlock()
	for _, place := range leftFollows {
		if mounts.Holds(place, p) {
			return place
		}
	}
	return ""
}

// followWithin follows p with walk, for at most pathWait. walk says each path it's
// about to ask the disk for, and stops at what that refuses.
func followWithin(p string, walk func(string, func(string) error) (string, error)) (string, error) {
	stuckMu.Lock()
	n := len(leftFollows)
	stuckMu.Unlock()
	if n >= maxStuckPaths {
		return "", tooManyStuck(p, n)
	}
	type followed struct {
		to  string
		err error
	}
	f := &follow{path: p}
	f.at.Store(&p)
	done := make(chan followed, 1)
	go func() {
		to, err := walk(p, f.touch)
		stuckMu.Lock()
		f.back = true
		_, left := leftFollows[f]
		delete(leftFollows, f)
		stuckMu.Unlock()
		if left {
			resource.PathBack()
		}
		done <- followed{to, err}
	}()
	limit := time.NewTimer(pathWait)
	defer limit.Stop()
	select {
	case out := <-done:
		return out.to, out.err
	case <-limit.C:
	}
	place := mounts.PassOver(readMounts(), *f.at.Load())
	stuckMu.Lock()
	defer stuckMu.Unlock()
	if f.back {
		out := <-done
		return out.to, out.err
	}
	leftFollows[f] = place
	return "", notAnswering(p)
}

// notAnswering is a path that was asked, and didn't answer in time.
func notAnswering(p string) error {
	return fault.New(resource.ErrPathNotAnswering, "the server path %s isn't answering after %s, so Dockwell can't tell where it leads, and this can't go ahead. "+
		"It may be on a network mount whose far end is down. Check the mount on the server, then try again", p, human.Duration(pathWait))
}

// placeNotAnswering is a path that wasn't asked, as it leads through a place that
// didn't answer.
func placeNotAnswering(p, place string) error {
	return fault.New(resource.ErrPathNotAnswering, "%s isn't answering, so Dockwell can't tell where the server path %s leads, and this can't go ahead. "+
		"It may be a network mount whose far end is down. Check the mount on the server, then try again", place, p)
}

// tooManyStuck is every path, once as many as can be left behind have been.
func tooManyStuck(p string, n int) error {
	return fault.New(resource.ErrPathNotAnswering, "%d places on this server aren't answering, so Dockwell won't follow another path until one does, "+
		"can't tell where the server path %s leads, and this can't go ahead. Check the server's mounts and disks, then try again", n, p)
}
