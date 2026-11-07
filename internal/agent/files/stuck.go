// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/mounts"
)

// A network mount whose far end is down can hold a system call for as long as it
// stays down, and a context can't stop one. So the agent never waits on the disk
// directly: each call does its work on a goroutine of its own, and what waits for
// it stops at a bound. What's left behind can't be stopped, so it stays where it
// is until the mount comes back, but it holds nothing the agent needs: not its
// stream, its place, or anyone's time.
//
// The next call that comes to the same place is told at once, rather than left
// behind too, so a dead mount costs one wait. maxStuck bounds what can be left
// behind, by paths apart on mounts this can't tell are remote.

const (
	// answerWithin is how long a person waits on a path. It is under the 30 s the
	// control plane gives a call, so the answer is this agent's, not a timeout.
	answerWithin = 15 * time.Second
	// maxStuck is how many calls can be left behind. Each holds a goroutine and a
	// thread, a descriptor for each part of its path, and at most what it read.
	maxStuck = 8
)

// trail is one call's way across the disk: what it is touching now, so that a call
// left behind can say where.
type trail struct {
	s  *Server
	at atomic.Pointer[string]
	// done says the call came back. It's the Server's mu that guards it.
	done bool
}

// touch says a call is about to touch path, or refuses if that's known not to
// answer.
func (t *trail) touch(path string) *agentv1.FileRefusal {
	if at := t.s.stalledAt(path); at != "" {
		return notAnswering(at)
	}
	t.at.Store(&path)
	if t.s.Touching != nil {
		t.s.Touching(path)
	}
	return nil
}

// stalledAt says which place left behind holds path, or nothing.
func (s *Server) stalledAt(path string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, at := range s.stuck {
		if mounts.Holds(at, path) {
			return at
		}
	}
	return ""
}

// stuckCount says how many calls are left behind.
func (s *Server) stuckCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.stuck)
}

// outcome is what a call's work came to.
type outcome[T any] struct {
	value T
	err   error
}

// attempt does work on a goroutine of its own, and waits for it until its bound or
// until the caller goes, whichever is first. gaveUp says what a refusal comes to.
func attempt[T any](parent context.Context, s *Server, p string, work func(context.Context, *trail) (T, error), gaveUp func(*agentv1.FileRefusal) T) (T, error) {
	if n := s.stuckCount(); n >= maxStuck {
		return gaveUp(tooManyStuck(n)), nil
	}
	within := s.AnswerWithin
	if within <= 0 {
		within = answerWithin
	}
	ctx, cancel := context.WithTimeout(parent, within)
	defer cancel()
	t := &trail{s: s}
	t.at.Store(&p)
	done := make(chan outcome[T], 1)
	go func() {
		defer s.returned(t)
		value, err := work(ctx, t)
		done <- outcome[T]{value, err}
	}()
	select {
	case out := <-done:
		return out.value, out.err
	case <-ctx.Done():
	}
	// It may have finished as the time ran out, and an answer beats saying there
	// wasn't one.
	select {
	case out := <-done:
		return out.value, out.err
	default:
	}
	at := s.leave(t)
	if errors.Is(parent.Err(), context.Canceled) {
		var zero T
		return zero, parent.Err() // whoever asked went, as a stream does as it ends
	}
	return gaveUp(notAnswering(at)), nil
}

// leave notes a call left behind, and says where it stopped.
func (s *Server) leave(t *trail) string {
	at := *t.at.Load()
	place := mounts.PassOver(s.mounts(), at)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !t.done {
		s.stuck[t] = place
	}
	return place
}

// returned notes a call came back, however late, which frees where it was.
func (s *Server) returned(t *trail) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t.done = true
	delete(s.stuck, t)
}

// notAnswering is a path a call stopped waiting for.
func notAnswering(at string) *agentv1.FileRefusal {
	return &agentv1.FileRefusal{
		Kind: agentv1.FileRefusal_KIND_NOT_ANSWERING, Path: at,
		Message: fmt.Sprintf("%s isn't answering, so Dockwell stopped waiting for it. It may be on a network mount whose far end is down. "+
			"Check the mount on the server, then try again", at),
	}
}

// tooManyStuck is every call, once as many as can be left behind have been.
func tooManyStuck(n int) *agentv1.FileRefusal {
	return &agentv1.FileRefusal{
		Kind: agentv1.FileRefusal_KIND_NOT_ANSWERING, Path: "/",
		Message: fmt.Sprintf("%d paths on this server aren't answering, so Dockwell won't try another until they do, after %s each. "+
			"Check the server's network mounts", n, human.Duration(answerWithin)),
	}
}
