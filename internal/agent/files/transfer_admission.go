// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"cmp"
	"context"
	"errors"
	"os"
	"runtime/debug"
	"sync"

	"github.com/dockwell/dockwell/internal/agent/pageout"
)

// Only an exclusive standalone fetch needs this margin for SSH packet garbage.
const backupFetchGC = 3

// ErrRestoring lets a panel transfer retry after an exclusive backup fetch.
var ErrRestoring = errors.New("the server is fetching a backup, so this transfer didn't start. Try again when the fetch finishes")

type transferAdmission struct {
	mu                sync.Mutex
	changed           chan struct{}
	fetchLane         chan struct{}
	active            int
	waiting, fetching bool
	// One refill of the places free when the fetch arrived lets a small
	// transfer use those places. Once that batch drains, the fetch goes next.
	spare int
}

// TakeTransfer waits for an upload or download place for at most transferWait.
func (s *Server) TakeTransfer(ctx context.Context) (func(), error) {
	ctx, cancel := context.WithTimeoutCause(ctx, cmp.Or(s.transferWait, transferWait), ErrBusy)
	defer cancel()
	return s.waitTransfer(ctx, true)
}

// WaitTransfer waits for a place through the caller's deadline.
func (s *Server) WaitTransfer(ctx context.Context) (func(), error) {
	return s.waitTransfer(ctx, false)
}

func (s *Server) waitTransfer(ctx context.Context, reportFetch bool) (func(), error) {
	a := &s.transfers
	for {
		a.mu.Lock()
		switch {
		case ctx.Err() != nil:
			a.mu.Unlock()
			return nil, context.Cause(ctx)
		case reportFetch && a.fetching:
			a.mu.Unlock()
			return nil, ErrRestoring
		case !a.fetching && a.active < transfers && (!a.waiting || a.active > 0 && a.spare > 0):
			a.active++
			if a.waiting {
				a.spare--
			}
			a.mu.Unlock()
			release := s.holdTransferMemory()
			return func() {
				release()
				a.mu.Lock()
				a.active--
				a.wake()
				a.mu.Unlock()
			}, nil
		default:
			changed := a.changedLocked()
			a.mu.Unlock()
			select {
			case <-changed:
			case <-ctx.Done():
			}
		}
	}
}

// WaitBackupFetch admits a standalone fetch only when the shared places are
// empty. It waits outside the ordinary transfer queue.
func (s *Server) WaitBackupFetch(ctx context.Context) (func(), error) {
	if s.Embedded {
		return s.WaitTransfer(ctx)
	}
	a := &s.transfers
	select {
	case a.fetchLane <- struct{}{}:
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
	a.mu.Lock()
	a.waiting = true
	a.spare = max(1, transfers-a.active)
	for ctx.Err() == nil && a.active != 0 {
		changed := a.changedLocked()
		a.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
		}
		a.mu.Lock()
	}
	a.waiting = false
	if ctx.Err() != nil {
		a.wake()
		a.mu.Unlock()
		<-a.fetchLane
		return nil, context.Cause(ctx)
	}
	a.fetching = true
	a.wake()
	a.mu.Unlock()

	release := s.holdTransferMemory()
	tuneGC := os.Getenv("GOGC") == ""
	var previousGC int
	if tuneGC {
		previousGC = debug.SetGCPercent(backupFetchGC)
		if previousGC < backupFetchGC {
			debug.SetGCPercent(previousGC)
		}
	}
	// Give back idle transfer buffers before opening the SSH connection.
	debug.FreeOSMemory()
	return func() {
		if tuneGC {
			debug.SetGCPercent(previousGC)
		}
		// Give back the closed SSH client's buffers before admitting panel work.
		debug.FreeOSMemory()
		pageout.Executable()
		release()
		a.mu.Lock()
		a.fetching = false
		a.wake()
		a.mu.Unlock()
		<-a.fetchLane
	}, nil
}

// changedLocked waits on the next state change. Callers hold mu.
func (a *transferAdmission) changedLocked() <-chan struct{} {
	if a.changed == nil {
		a.changed = make(chan struct{})
	}
	return a.changed
}

// wake releases current waiters. Callers hold mu.
func (a *transferAdmission) wake() {
	if a.changed != nil {
		close(a.changed)
		a.changed = nil
	}
}
