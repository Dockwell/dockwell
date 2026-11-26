// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
)

const (
	running = resourcev1.DesiredRunState_DESIRED_RUN_STATE_RUNNING
	stopped = resourcev1.DesiredRunState_DESIRED_RUN_STATE_STOPPED
	paused  = resourcev1.DesiredRunState_DESIRED_RUN_STATE_PAUSED
)

type runRecord struct {
	State   resourcev1.DesiredRunState
	Policy  resourcev1.RestartPolicy
	Retries uint32

	Restart   string    `json:",omitempty"`
	StartedAt time.Time `json:",omitzero"`
}

type runLock struct {
	mu   sync.Mutex
	refs int
}

func (d *Docker) lockRun(id string) func() {
	d.runMu.Lock()
	if d.runLocks == nil {
		d.runLocks = make(map[string]*runLock)
	}
	lock := d.runLocks[id]
	if lock == nil {
		lock = &runLock{}
		d.runLocks[id] = lock
	}
	lock.refs++
	d.runMu.Unlock()
	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		d.runMu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(d.runLocks, id)
		}
		d.runMu.Unlock()
	}
}

// WithRunIntent keeps a saved stop stable while an operation would start Docker.
func (d *Docker) WithRunIntent(id string, action func(stopped bool) error) error {
	return d.WithDesiredRunState(id, func(state resourcev1.DesiredRunState) error {
		return action(state == stopped)
	})
}

// WithDesiredRunState holds the saved intent stable by full Engine ID.
func (d *Docker) WithDesiredRunState(id string, action func(resourcev1.DesiredRunState) error) error {
	unlock := d.lockRun(id)
	defer unlock()
	record, err := d.runRecord(id)
	if err != nil {
		return err
	}
	return action(record.State)
}

// HasSavedPause reports whether this Engine ID has an explicit managed pause.
func (d *Docker) HasSavedPause(id string) bool {
	record, err := d.runRecord(id)
	return err == nil && record.State == paused
}

func (d *Docker) runRecord(id string) (runRecord, error) {
	if !docker.IsFullID(id) {
		return runRecord{}, nil
	}
	if d.RunStateDir == "" {
		if record, ok := d.runStates.Load(id); ok {
			return record.(runRecord), nil
		}
		return runRecord{}, nil
	}
	var record runRecord
	raw, err := os.ReadFile(filepath.Join(d.RunStateDir, id+".json")) //nolint:gosec // full hex ID under the agent data directory
	if errors.Is(err, os.ErrNotExist) {
		return record, nil
	}
	if err == nil {
		err = json.Unmarshal(raw, &record)
	}
	return record, err
}

func (d *Docker) rememberRun(id string, record runRecord) error {
	if !docker.IsFullID(id) {
		return errors.New("run record needs a full Docker container ID")
	}
	if d.RunStateDir == "" {
		if record == (runRecord{}) {
			d.runStates.Delete(id)
		} else {
			d.runStates.Store(id, record)
		}
		return nil
	}
	path := filepath.Join(d.RunStateDir, id+".json")
	if record == (runRecord{}) {
		err := os.Remove(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
	} else {
		if err := os.MkdirAll(d.RunStateDir, 0o700); err != nil {
			return err
		}
		tmp, err := os.CreateTemp(d.RunStateDir, ".run-")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		err = json.NewEncoder(tmp).Encode(record)
		if err == nil {
			err = tmp.Sync()
		}
		if err := errors.Join(err, tmp.Close()); err != nil {
			return err
		}
		if err := os.Rename(tmp.Name(), path); err != nil {
			return err
		}
	}
	dir, err := os.Open(d.RunStateDir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (d *Docker) forgetRun(id string) {
	if err := d.rememberRun(id, runRecord{}); err != nil {
		d.logger().Warn("can't remove the run record of a removed container", "error", err)
	}
}

// PruneRunState forgets run records for containers that no longer exist.
func (d *Docker) PruneRunState(ctx context.Context) error {
	if d.RunStateDir == "" {
		return nil
	}
	entries, err := os.ReadDir(d.RunStateDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	gone := make(map[string]bool)
	for _, entry := range entries {
		if id, ok := strings.CutSuffix(entry.Name(), ".json"); ok && !entry.IsDir() && docker.IsFullID(id) {
			gone[id] = true
		}
	}
	if len(gone) == 0 {
		return nil
	}
	if err := d.Client.EachContainer(ctx, nil, func(c *docker.ContainerSummary) error {
		delete(gone, c.ID)
		return nil
	}); err != nil {
		return err
	}
	for id := range gone {
		d.forgetRun(id)
	}
	return nil
}

func (d *Docker) projectRun(c *docker.Container, out *resourcev1.Container) error {
	record, err := d.runRecord(c.ID)
	if err != nil {
		return err
	}
	switch record.State {
	case resourcev1.DesiredRunState_DESIRED_RUN_STATE_UNSPECIFIED:
	case stopped:
		// Only a completed stop projects the configured policy. A running
		// container with Docker's policy disabled has a change left to undo.
		if out.GetRestart() == resourcev1.RestartPolicy_RESTART_POLICY_NO && !c.State.Running && !c.State.Restarting {
			out.Restart, out.RestartRetries = record.Policy, record.Retries
			out.DesiredRunState = stopped
		}
	case running:
		if c.State.Running && !c.State.Restarting && !c.State.Paused {
			out.DesiredRunState = running
		}
	case paused:
		if c.State.Paused {
			out.DesiredRunState = paused
		}
	}
	return nil
}

// updateRun changes the effective policy before stopping. Until the stop is
// complete the record reads as unapplied, so a retry can finish the same step.
func (d *Docker) updateRun(ctx context.Context, t Target, c *instance, spec *resourcev1.Container, dec resource.Decision) (err error) {
	unlock := d.lockRun(c.ID)
	defer unlock()
	record, err := d.runRecord(c.ID)
	if err != nil {
		return err
	}
	want := spec.GetDesiredRunState()
	if want != 0 || record.State == stopped || record.State == paused {
		// A backup may have restarted it since the rollout read its instances.
		c.Container, err = d.Client.InspectContainer(ctx, c.ID)
		if err != nil {
			return err
		}
	}
	if want == paused && record.State != paused && (!c.State.Running || c.State.Restarting) {
		return errors.New("only a running workload can be paused. Start it before reviewing Pause")
	}
	if want == stopped {
		next := runRecord{State: want, Policy: spec.GetRestart(), Retries: spec.GetRestartRetries()}
		if record != next {
			if err := d.rememberRun(c.ID, next); err != nil {
				return err
			}
		}
		if record.State != stopped {
			defer func() {
				if err != nil {
					err = errors.Join(err, d.undoFailedStop(ctx, c.Container, record))
				}
			}()
		}
	}
	start := want != stopped && (want != 0 || record.State == stopped || record.State == paused)
	if start && !c.State.Running {
		if err := d.startsAsPlanned(ctx, t, spec, dec, c.ID); err != nil {
			return err
		}
	}
	if err := d.Client.UpdateContainer(ctx, c.ID, docker.ContainerUpdate{
		RestartPolicy:     restartOf(spec),
		NanoCPUs:          cpuNanos(spec.GetResources().GetCpus()),
		Memory:            memoryBytes(spec.GetResources().GetMemory()),
		MemoryReservation: memoryBytes(spec.GetResources().GetMemoryReservation()),
	}); err != nil {
		return hideIn(err, valuesInContainer(c.Container))
	}
	if want == stopped {
		if c.State.Paused {
			if err := d.Client.UnpauseContainer(ctx, c.ID); err != nil {
				return hideIn(err, valuesInContainer(c.Container))
			}
		}
		if c.State.Running || c.State.Restarting || c.State.Paused {
			if err := d.Client.StopContainer(ctx, c.ID, stopGrace(c.Container)); err != nil {
				return hideIn(err, valuesInContainer(c.Container))
			}
		}
		if d.Ingress != nil && t.Scoped {
			return d.Ingress.RemoveUpstream(ctx, t.instance(c.number))
		}
		return nil
	}
	if want == paused {
		if record.State != paused {
			if err := d.rememberRun(c.ID, runRecord{State: paused}); err != nil {
				return err
			}
		}
		return d.pauseInstance(ctx, t, c)
	}
	if start {
		if c.State.Paused {
			if err := d.Client.UnpauseContainer(ctx, c.ID); err != nil {
				return hideIn(err, valuesInContainer(c.Container))
			}
		}
		if err := d.startInstance(ctx, t, c); err != nil {
			return err
		}
	}
	if want != record.State {
		return d.rememberRun(c.ID, runRecord{State: want})
	}
	return nil
}

// undoFailedStop restores the effective policy when the container never stopped.
// A completed stop keeps its record so a retry or rollback can see it.
func (d *Docker) undoFailedStop(ctx context.Context, before *docker.Container, record runRecord) error {
	ctx, cancel := resource.Finishing(ctx, finishWait)
	defer cancel()
	after, err := d.Client.InspectContainer(ctx, before.ID)
	if err != nil || !after.State.Running && !after.State.Restarting {
		return err
	}
	if err := d.Client.UpdateRestartPolicy(ctx, before.ID, before.HostConfig.RestartPolicy); err != nil {
		return hideIn(err, valuesInContainer(before))
	}
	if before.State.Paused && !after.State.Paused {
		if err := d.Client.PauseContainer(ctx, before.ID); err != nil {
			return hideIn(err, valuesInContainer(before))
		}
	}
	return d.rememberRun(before.ID, record)
}

func (d *Docker) pauseInstance(ctx context.Context, t Target, c *instance) error {
	if !c.State.Running {
		if err := d.Client.StartContainer(ctx, c.ID); err != nil {
			return hideIn(err, valuesInContainer(c.Container))
		}
	}
	if !c.State.Paused {
		if err := d.Client.PauseContainer(ctx, c.ID); err != nil {
			return hideIn(err, valuesInContainer(c.Container))
		}
	}
	if d.Ingress != nil && t.Scoped {
		return d.Ingress.PauseUpstream(ctx, t.instance(c.number))
	}
	return nil
}

func (d *Docker) startInstance(ctx context.Context, t Target, c *instance) error {
	if !c.State.Running {
		if err := d.Client.StartContainer(ctx, c.ID); err != nil {
			return hideIn(err, valuesInContainer(c.Container))
		}
	}
	if d.Ingress != nil && t.Scoped {
		return d.Ingress.AddUpstream(ctx, t.instance(c.number))
	}
	return nil
}

// IsStopped reports explicit stop intent.
func IsStopped(spec proto.Message) bool { return resource.RunIntent(spec) == stopped }

// verifyRun verifies a start against the usual health gate and a stop
// against Docker's effective policy and process state.
func (d *Docker) verifyRun(ctx context.Context, t Target, spec proto.Message) error {
	if resource.RunIntent(spec) == paused {
		all, err := d.instances(ctx, t)
		if err != nil {
			return err
		}
		if len(all) == 0 {
			return fmt.Errorf("%s is missing. Deploy it again", t.Service)
		}
		for _, i := range all {
			if !i.State.Paused {
				return fmt.Errorf("%s hasn't settled in the paused state", t.Service)
			}
		}
		d.reportRan(ctx, all[0].Container)
		return nil
	}
	if !IsStopped(spec) {
		return d.Healthy(ctx, t)
	}
	st, err := d.RunStateAt(ctx, t)
	if err != nil {
		return err
	}
	if !st.Exists || st.Running || st.Wants {
		return fmt.Errorf("%s hasn't settled in the stopped state", t.Service)
	}
	return nil
}

// PlanRestart checks membership in the service on this server against the exact
// identity reviewed in Docker inventory.
func (d *Docker) PlanRestart(ctx context.Context, t Target, spec proto.Message, change, id string) (resource.Decision, error) {
	i, done, err := d.restartInstance(ctx, t, spec, change, id)
	if err != nil || done {
		return resource.Decision{}, err
	}
	wait := stopGrace(i.Container)
	return resource.Decision{Action: resource.ActionUpdate, Effect: resource.EffectRestart, Downtime: wait}, nil
}

func (d *Docker) restartInstance(ctx context.Context, t Target, spec proto.Message, change, id string) (*instance, bool, error) {
	if !docker.IsFullID(id) || change == "" {
		return nil, false, errors.New("restart needs the exact container identity and reviewed change")
	}
	if IsStopped(spec) {
		return nil, false, errors.New("this workload is stopped by its revision. Use Start")
	}
	all, err := d.instances(ctx, t)
	if err != nil {
		return nil, false, err
	}
	for _, i := range all {
		if i.ID != id || i.temp || i.stopping {
			continue
		}
		if moved, err := d.Paths.movedSince(ctx, i.Container, nil); err != nil {
			return nil, false, err
		} else if moved != nil {
			return nil, false, fmt.Errorf("the reviewed container's mounts changed: %w", moved)
		}
		record, err := d.runRecord(id)
		if err != nil {
			return nil, false, err
		}
		if record.State == stopped {
			return nil, false, errors.New("this container is stopped by Dockwell. Use Start")
		}
		if record.Restart == change && i.State.StartedAt.After(record.StartedAt) {
			return i, true, nil
		}
		if record.Restart != change && (i.State.Paused || !i.State.Running && !i.State.Restarting) {
			return nil, false, errors.New("only a running container can be restarted. Use Start for the workload")
		}
		return i, false, nil
	}
	return nil, false, errors.New("the reviewed container is no longer an instance of this workload on this server")
}

// ApplyRestart sends Docker the full ID it checked and holds the prior start
// time so a resumed apply can recognize a restart Docker already performed.
func (d *Docker) ApplyRestart(ctx context.Context, t Target, spec proto.Message, change, id string) error {
	unlock := d.lockRun(id)
	defer unlock()
	i, done, err := d.restartInstance(ctx, t, spec, change, id)
	if err != nil {
		return err
	}
	if !done {
		record, err := d.runRecord(id)
		if err != nil {
			return err
		}
		if record.Restart != change {
			record.Restart, record.StartedAt = change, i.State.StartedAt
			if err := d.rememberRun(id, record); err != nil {
				return err
			}
		}
		if err := d.Client.RestartContainer(ctx, id, stopGrace(i.Container)); err != nil {
			return hideIn(err, valuesInContainer(i.Container))
		}
	}
	return d.waitHealthy(ctx, t, i, 0)
}
