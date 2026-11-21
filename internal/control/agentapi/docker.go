// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"crypto/rand"
	"errors"
	"slices"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

const dockerCallWait = 12 * time.Second

// DockerInventory reports whether this agent supports Docker inventory reads.
func (s *Session) DockerInventory() bool { return s.dockerCapabilities.GetInventory() }

// ExactTerminal reports whether this agent understands immutable IDs and argv.
func (s *Session) ExactTerminal() bool { return s.dockerCapabilities.GetExactTerminal() }

// DockerList reads a bounded inventory page or counters from this agent.
func (s *Session) DockerList(ctx context.Context, ask *agentv1.DockerInventoryListCall) (*agentv1.DockerInventoryListResult, error) {
	r, err := s.callOnceWithin(ctx, &agentv1.Call{Call: &agentv1.Call_DockerInventoryList{DockerInventoryList: ask}}, dockerCallWait)
	if err != nil {
		return nil, err
	}
	if out := r.GetDockerInventoryList(); out != nil {
		return out, nil
	}
	return nil, errors.New("the agent didn't return Docker inventory")
}

// DockerDetail inspects the exact instance through this agent.
func (s *Session) DockerDetail(ctx context.Context, ask *agentv1.DockerInventoryDetailCall) (*agentv1.DockerInventoryDetailResult, error) {
	r, err := s.callOnceWithin(ctx, &agentv1.Call{Call: &agentv1.Call_DockerInventoryDetail{DockerInventoryDetail: ask}}, dockerCallWait)
	if err != nil {
		return nil, err
	}
	if out := r.GetDockerInventoryDetail(); out != nil {
		return out, nil
	}
	return nil, errors.New("the agent didn't return Docker details")
}

func (s *Session) callOnceWithin(ctx context.Context, call *agentv1.Call, timeout time.Duration) (*agentv1.CallResult, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return s.callOnce(ctx, call)
}

// WatchDocker keeps a renewable subscription on this stream. Updates are
// coalesced here when a slow public client is unable to consume them.
func (s *Session) WatchDocker(ctx context.Context, visible []string, emit func(*agentv1.DockerInventoryChanged) error) error {
	id := rand.Text()
	inbox := make(chan *agentv1.DockerInventoryChanged, 1)

	s.mu.Lock()
	if len(s.dockerWatches) >= 8 {
		s.mu.Unlock()
		return errors.New("this server has eight Docker inventory watches open")
	}
	if s.dockerWatches == nil {
		s.dockerWatches = make(map[string]chan *agentv1.DockerInventoryChanged)
	}
	s.dockerWatches[id] = inbox
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.dockerWatches, id)
		s.mu.Unlock()
	}()

	renew := func(ctx context.Context, stop bool) error {
		ask := &agentv1.DockerInventoryWatchCall{SubscriptionId: id, VisibleIds: visible, Stop: stop}
		reply, err := s.callOnceWithin(ctx, &agentv1.Call{Call: &agentv1.Call_DockerInventoryWatch{DockerInventoryWatch: ask}}, 3*time.Second)
		if err == nil && reply.GetDockerInventoryWatch() == nil {
			return errors.New("the agent didn't confirm the Docker subscription")
		}
		return err
	}
	defer func() {
		if s.isDone() {
			return
		}
		_ = renew(context.WithoutCancel(ctx), true)
	}()
	if err := renew(ctx, false); err != nil {
		return err
	}
	tick := time.NewTicker(45 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.done:
			return errors.New("the agent disconnected")
		case <-tick.C:
			if err := renew(ctx, false); err != nil {
				return err
			}
		case msg := <-inbox:
			if err := emit(msg); err != nil {
				return err
			}
		}
	}
}

func (s *Session) isDone() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (s *Session) dockerChanged(msg *agentv1.DockerInventoryChanged) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inbox := s.dockerWatches[msg.GetSubscriptionId()]
	if inbox == nil {
		return
	}
	select {
	case inbox <- msg:
	default:
		select {
		case prev := <-inbox:
			msg.Resync = msg.GetResync() || prev.GetResync()
			for _, update := range prev.GetHealthUpdates() {
				if !slices.ContainsFunc(msg.GetHealthUpdates(), func(next *dockwellv1.DockerHealthUpdate) bool {
					return next.GetKey().GetEngineId() == update.GetKey().GetEngineId()
				}) && len(msg.GetHealthUpdates()) < 16 {
					msg.HealthUpdates = append(msg.HealthUpdates, update)
				}
			}
		default:
		}
		inbox <- msg
	}
}
