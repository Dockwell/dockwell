// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
)

// Rebooting a server (ADR 0064): a step of a change for its host that asks the
// agent to reboot, then waits for the server, as a rollback waits for a server to
// reconnect. Resume plans it again as each agent connects: an agent on another
// boot than the one the reboot left has nothing left to plan, and the step passes,
// keeping when it started, so its time is the reboot's. FailLostReboots fails one
// whose server hasn't come back.

// RebootLimit is how long a reboot's step waits for its server to come back on
// another boot before it fails.
var RebootLimit = 15 * time.Minute

// ErrRebootLost is a server that hasn't come back after its reboot.
var ErrRebootLost = errors.New("the server hasn't come back after its reboot")

// rebootsScanned is how many of the newest changes a plan reads for the server's
// last reboot.
const rebootsScanned = 200

// SetControlServer names the server the control plane runs on, whose reboot stops
// the panel and the API with it.
func (e *Engine) SetControlServer(id string) {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	e.controlServer = id
}

// ControlServer is the server the control plane runs on, or "" before it's named.
func (e *Engine) ControlServer() string {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	return e.controlServer
}

// Connected reports whether server's agent is connected now.
func (e *Engine) Connected(server string) bool {
	_, online := e.agents.Agent(server)
	return online
}

// rebootAsked is the reboot an item asks of its server's host, or nil.
func (it *item) rebootAsked() *resource.RebootAsked {
	if it.reboot == "" {
		return nil
	}
	return &resource.RebootAsked{Change: it.reboot}
}

// rebootIn names the reboot change c asks of ref, as its plan says: c's ID, or "".
func rebootIn(c store.Change, ref resource.Ref) string {
	p, err := DecodePlan(c.Plan)
	if err != nil {
		return ""
	}
	for i := range p.Resources {
		if p.Resources[i].Ref == ref && p.Resources[i].Reboot != nil {
			return c.ID
		}
	}
	return ""
}

// rebootOf is what a plan says of a server's reboot: whether it's the control
// server, and how long its last reboot took, once Dockwell has seen one.
func (e *Engine) rebootOf(ctx context.Context, server string) *resource.Reboot {
	e.pmu.Lock()
	control := e.controlServer
	e.pmu.Unlock()
	return &resource.Reboot{ControlServer: server != "" && server == control, LastTook: e.lastRebootTook(ctx, server)}
}

// lastRebootTook is how long the server's newest reboot that passed took, from
// its step's start until it passed, of the newest changes; zero when none did.
func (e *Engine) lastRebootTook(ctx context.Context, server string) time.Duration {
	var changes []store.Change
	if err := e.db.View(ctx, func(tx *store.Tx) error {
		var err error
		changes, err = tx.ListChanges(ctx, store.ChangePage{}, rebootsScanned)
		return err
	}); err != nil {
		return 0
	}
	for i := range changes {
		if changes[i].State != store.ChangeApplied {
			continue
		}
		if took := rebootTook(changes[i], server); took > 0 {
			return took
		}
	}
	return 0
}

// rebootTook is how long change c's reboot of server took, or zero if it has none
// that passed.
func rebootTook(c store.Change, server string) time.Duration {
	p, err := DecodePlan(c.Plan)
	if err != nil {
		return 0
	}
	var host *resource.Ref
	for i := range p.Resources {
		if p.Resources[i].Reboot != nil && p.Resources[i].Ref.Scope == server {
			host = &p.Resources[i].Ref
		}
	}
	if host == nil {
		return 0
	}
	progress, err := decodeProgress(c.Progress)
	if err != nil {
		return 0
	}
	for i := range progress.Steps {
		s := &progress.Steps[i]
		if s.Server == server && s.Ref == *host && s.State == StepDone && !s.Started.IsZero() && s.Finished.After(s.Started) {
			return s.Finished.Sub(s.Started)
		}
	}
	return 0
}

// lostReboot is the step of change c that waits for its server to come back from a
// reboot past RebootLimit, by its index in the change's progress, or -1.
func (e *Engine) lostReboot(c store.Change) (int, resource.Ref, string) {
	p, err := DecodePlan(c.Plan)
	if err != nil {
		return -1, resource.Ref{}, ""
	}
	rebooting := map[resource.Ref]bool{}
	for i := range p.Resources {
		if p.Resources[i].waitsOn() {
			rebooting[p.Resources[i].Ref] = true
		}
	}
	if len(rebooting) == 0 {
		return -1, resource.Ref{}, ""
	}
	progress, err := decodeProgress(c.Progress)
	if err != nil {
		return -1, resource.Ref{}, ""
	}
	now := e.now()
	for i := range progress.Steps {
		s := &progress.Steps[i]
		if rebooting[s.Ref] && s.State == StepWaiting && !s.Started.IsZero() && now.Sub(s.Started) >= RebootLimit {
			name := s.Server
			for _, sp := range p.Servers {
				if sp.Server == s.Server && sp.Name != "" {
					name = sp.Name
				}
			}
			return i, s.Ref, name
		}
	}
	return -1, resource.Ref{}, ""
}

// RebootCheckEvery is how often dockwell run looks at reboots that wait for their
// server: for one whose server is past RebootLimit, and for one whose server is
// still connected on the boot it left, whose agent may say its command failed.
const RebootCheckEvery = 30 * time.Second

// RebootsWaiting reports a change whose reboot's step waits for a server that's
// still connected, on the boot the reboot left or one it says it is: the server is
// on its way down, or its reboot command failed. Resuming such a change asks its
// agent again, which fails the step at once with what the command said, rather than
// at RebootLimit with the server up.
func (e *Engine) RebootsWaiting(ctx context.Context) (bool, error) {
	var ids []string
	if err := e.db.View(ctx, func(tx *store.Tx) error {
		var err error
		ids, err = tx.ChangesIn(ctx, store.ChangeApplying)
		return err
	}); err != nil {
		return false, err
	}
	for _, id := range ids {
		var c store.Change
		if err := e.db.View(ctx, func(tx *store.Tx) error {
			var err error
			c, err = tx.GetChange(ctx, id)
			return err
		}); err != nil {
			return false, err
		}
		for _, server := range waitingReboots(c) {
			if _, online := e.agents.Agent(server); online {
				return true, nil
			}
		}
	}
	return false, nil
}

// waitingReboots are the servers change c's reboot steps wait on.
func waitingReboots(c store.Change) []string {
	p, err := DecodePlan(c.Plan)
	if err != nil {
		return nil
	}
	rebooting := map[resource.Ref]bool{}
	for i := range p.Resources {
		if p.Resources[i].waitsOn() {
			rebooting[p.Resources[i].Ref] = true
		}
	}
	if len(rebooting) == 0 {
		return nil
	}
	progress, err := decodeProgress(c.Progress)
	if err != nil {
		return nil
	}
	var out []string
	for i := range progress.Steps {
		if s := &progress.Steps[i]; rebooting[s.Ref] && s.State == StepWaiting {
			out = append(out, s.Server)
		}
	}
	return out
}

// FailLostReboots fails each change whose reboot's step has waited past
// RebootLimit for its server to come back on another boot, as nothing else would:
// Resume waits for a server that's away for as long as it takes. Its time is the
// step's recorded start, so a control plane that starts again doesn't reset it. A
// change running here is left to its run.
func (e *Engine) FailLostReboots(ctx context.Context) error {
	if !e.enter() {
		return nil
	}
	defer e.exit()
	var ids []string
	if err := e.db.View(ctx, func(tx *store.Tx) error {
		var err error
		ids, err = tx.ChangesIn(ctx, store.ChangeApplying)
		return err
	}); err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		var c store.Change
		if err := e.db.View(ctx, func(tx *store.Tx) error {
			var err error
			c, err = tx.GetChange(ctx, id)
			return err
		}); err != nil {
			errs = append(errs, err)
			continue
		}
		i, ref, name := e.lostReboot(c)
		if i < 0 || !e.own(id) {
			continue
		}
		progress, err := decodeProgress(c.Progress)
		if err != nil {
			e.letGo(ctx, id)
			errs = append(errs, err)
			continue
		}
		e.track(id, progress)
		cause := fault.New(ErrRebootLost, "%s hasn't come back after its reboot, in %s. Check its console, or reach it over SSH", name, RebootLimit)
		if upgradeIn(c, ref) != nil {
			cause = fault.New(ErrUpgradeLost, "the agent on %s hasn't come back at its new version, in %s. Read its log with journalctl -u dockwell over SSH, and the script's in /var/lib/dockwell/upgrade", name, RebootLimit)
		}
		plain, _ := resource.Explain(cause)
		_, u := e.failed(id, i, cause)
		if e.recordProgress(ctx, id) {
			e.announce(id, u)
		}
		e.logger.WarnContext(ctx, "a server hasn't come back after its reboot or its agent's upgrade; its change failed", slog.String("change", id), slog.String("server", name))
		if err := e.fail(ctx, c, ref, cause, plain); err != nil {
			var failed *FailedError
			if !errors.As(err, &failed) {
				errs = append(errs, err)
			}
		}
		e.letGo(ctx, id)
	}
	return errors.Join(errs...)
}
