// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
)

const (
	dockerWatchLease     = 90 * time.Second
	dockerResyncInterval = 10 * time.Second
)

type inventoryWatch struct {
	ids     []string
	expires time.Time
}

type inventoryWatches struct {
	mu sync.Mutex

	held    map[string]inventoryWatch
	cancel  context.CancelFunc
	done    chan struct{}
	run     uint64
	samples []*v1.ContainerUsage
	at      *timestamppb.Timestamp
}

func (c *calls) watchInventory(ask *agentv1.DockerInventoryWatchCall) error {
	if c.cfg.Docker == nil {
		return errDockerInventory
	}
	id, ids := ask.GetSubscriptionId(), ask.GetVisibleIds()
	if id == "" || len(id) > 100 || len(ids) > 16 || slices.ContainsFunc(ids, func(id string) bool { return !docker.IsFullID(id) }) {
		return errors.New("a Docker watch needs a subscription ID and at most 16 full container IDs")
	}
	w := &c.inventory
	w.mu.Lock()
	defer w.mu.Unlock()
	if ask.GetStop() {
		delete(w.held, id)
		if len(w.held) == 0 && w.cancel != nil {
			w.end()
		}
		return nil
	}
	if _, exists := w.held[id]; !exists && len(w.held) >= 8 {
		return errors.New("this server has eight Docker inventory watches open. Close one and try again")
	}
	if w.held == nil {
		w.held = make(map[string]inventoryWatch)
	}
	w.held[id] = inventoryWatch{ids: slices.Clone(ids), expires: time.Now().Add(dockerWatchLease)}
	if w.cancel == nil {
		ctx, cancel := context.WithCancel(c.ctx)
		w.cancel = cancel
		w.run++
		w.done = make(chan struct{})
		go c.followInventory(ctx, w.run, w.done)
	}
	return nil
}

func (w *inventoryWatches) end() {
	w.cancel()
	w.cancel, w.samples, w.at = nil, nil, nil
}

func (c *calls) rememberInventorySamples(s *v1.ServerStats) {
	w := &c.inventory
	w.mu.Lock()
	if len(w.held) > 0 {
		w.samples, w.at = s.GetContainers(), s.GetTime()
	}
	w.mu.Unlock()
}

type inventoryChanges struct {
	mu     sync.Mutex
	wake   chan struct{}
	dirty  bool
	ready  bool
	health map[string]*v1.DockerHealthUpdate
}

func (changes *inventoryChanges) notify() {
	select {
	case changes.wake <- struct{}{}:
	default:
	}
}

func (changes *inventoryChanges) opened(ready bool) {
	changes.mu.Lock()
	changes.ready = ready
	changes.dirty = ready
	changes.mu.Unlock()
	changes.notify()
}

func (changes *inventoryChanges) event(e docker.Event) {
	changes.mu.Lock()
	defer changes.mu.Unlock()
	if health, ok := strings.CutPrefix(e.Action, "health_status: "); ok {
		if docker.IsFullID(e.ID) && (len(changes.health) < 256 || changes.health[e.ID] != nil) {
			changes.health[e.ID] = &v1.DockerHealthUpdate{
				Key: inventoryKey(e.ID), Health: inventoryHealth(&docker.Health{Status: health}), ObservedAt: timestamppb.Now(),
			}
		}
		return
	}
	changes.dirty = true
	changes.notify()
}

func (c *calls) followInventory(ctx context.Context, run uint64, done chan struct{}) {
	defer close(done)
	defer func() {
		w := &c.inventory
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.run == run && w.cancel != nil {
			clear(w.held)
			w.end()
		}
	}()
	changes := inventoryChanges{wake: make(chan struct{}, 1), health: make(map[string]*v1.DockerHealthUpdate)}
	ctx, cancel := context.WithCancel(ctx)
	var engine sync.WaitGroup
	defer func() {
		cancel()
		engine.Wait()
	}()
	engine.Go(func() {
		backoff := 5 * time.Second
		for ctx.Err() == nil {
			_ = c.cfg.Docker.InventoryEvents(ctx, func() {
				changes.opened(true)
				backoff = 5 * time.Second
			}, changes.event)
			changes.opened(false)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
				backoff = min(backoff*2, time.Minute)
			}
		}
	})
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	var lastResync time.Time
	lastMeasurement := c.cfg.disks.measurement()
	notified, previousReady := false, false
	for {
		sampling := false
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			sampling = true
		case <-changes.wake:
		}
		changes.mu.Lock()
		now := time.Now()
		measured := c.cfg.disks.measurement()
		resync := (changes.ready && changes.dirty || measured != lastMeasurement) && now.Sub(lastResync) >= dockerResyncInterval
		if resync {
			lastResync, lastMeasurement = now, measured
			changes.dirty = false
		}
		ready, health := changes.ready, changes.health
		if len(health) != 0 {
			changes.health = make(map[string]*v1.DockerHealthUpdate)
		}
		changes.mu.Unlock()
		statusChanged := !notified || ready != previousReady
		if !sampling && !resync && !statusChanged && len(health) == 0 {
			continue
		}
		notified, previousReady = true, ready
		if !c.sendInventory(ctx, run, resync, ready, sampling, health) {
			return
		}
	}
}

func (c *calls) sendInventory(ctx context.Context, run uint64, resync, ready, sampling bool, health map[string]*v1.DockerHealthUpdate) bool {
	w := &c.inventory
	w.mu.Lock()
	if w.run != run || w.cancel == nil {
		w.mu.Unlock()
		return false
	}
	now := time.Now()
	viewers := make(map[string][]string, len(w.held))
	detail := make(map[string]bool)
	for id, watch := range w.held {
		if now.After(watch.expires) {
			delete(w.held, id)
			continue
		}
		viewers[id] = watch.ids
		if sampling && len(watch.ids) == 1 {
			detail[watch.ids[0]] = true
		}
	}
	if len(viewers) == 0 {
		w.end()
		w.mu.Unlock()
		return false
	}
	held, at := w.samples, w.at
	w.mu.Unlock()

	samples := make(map[string]*v1.DockerSample, len(detail))
	wanted := make(map[string]bool)
	if sampling {
		for _, ids := range viewers {
			for _, id := range ids {
				wanted[id] = true
			}
		}
	}
	if at != nil && now.Sub(at.AsTime()) < 20*time.Second {
		for _, sample := range held {
			id := sample.GetContainerId()
			if wanted[id] {
				cpu, memory, limit := sample.GetCpuPercent(), sample.GetMemoryBytes(), sample.GetMemoryLimitBytes()
				samples[id] = &v1.DockerSample{Key: inventoryKey(id), ObservedAt: at, CpuPercent: &cpu, MemoryBytes: &memory}
				if limit > 0 {
					samples[id].MemoryLimitBytes = &limit
				}
			}
		}
	}

	// Give detail reads precedence. Bound the fallback to a small number
	// of Engine calls per tick, and rotate through the remaining visible IDs.
	fallback := make([]string, 0, len(wanted))
	details := make([]string, 0, len(detail))
	for id := range wanted {
		if detail[id] {
			details = append(details, id)
		} else if samples[id] == nil {
			fallback = append(fallback, id)
		}
	}
	slices.Sort(details)
	slices.Sort(fallback)
	start := int(now.Unix()/3) % max(1, len(fallback))
	fallback = append(append(details, fallback[start:]...), fallback[:start]...)
	var group sync.WaitGroup
	var mu sync.Mutex
	for _, id := range fallback[:min(8, len(fallback))] {
		group.Go(func() {
			ask, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			s, err := c.cfg.Docker.ContainerCounters(ask, id)
			if err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if s.CPU == nil && s.Memory == nil && s.Rx == nil && s.Read == nil {
				return
			}
			out := samples[id]
			if out == nil {
				out = &v1.DockerSample{Key: inventoryKey(id), ObservedAt: timestamppb.Now(), CpuPercent: s.CPU, MemoryBytes: s.Memory, MemoryLimitBytes: s.Limit}
				samples[id] = out
			}
			out.NetworkRxBytes, out.NetworkTxBytes = s.Rx, s.Tx
			out.BlockReadBytes, out.BlockWriteBytes = s.Read, s.Written
		})
	}
	group.Wait()
	for id, keys := range viewers {
		msg := &agentv1.DockerInventoryChanged{SubscriptionId: id, Resync: resync, EventStreamReady: &ready}
		if len(keys) == 0 {
			// Summary viewers need changes in health without per-container sampling.
			for _, update := range health {
				msg.HealthUpdates = append(msg.HealthUpdates, update)
			}
		}
		for _, key := range keys {
			if sample := samples[key]; sample != nil {
				msg.Samples = append(msg.Samples, sample)
			}
			if update := health[key]; update != nil {
				msg.HealthUpdates = append(msg.HealthUpdates, update)
			}
		}
		if ctx.Err() != nil {
			return false
		}
		if c.send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_DockerInventoryChanged{DockerInventoryChanged: msg}}) != nil {
			return false
		}
	}
	return true
}
