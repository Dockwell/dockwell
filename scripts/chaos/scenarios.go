// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dockwell/dockwell/internal/agent/desired"
)

// scenario breaks one thing at a bad moment, and puts it right; the invariants are
// checked after (docs/testing/chaos.md).
type scenario struct {
	name string
	// root needs sudo, and real a real Docker Engine.
	root, real bool
	run        func(ctx context.Context, s *stand, r *round) error
}

var scenarios = []scenario{
	{name: "change/kill-agent", run: changeKillAgent},
	{name: "change/kill-control", run: changeKillControl},
	{name: "change/kill-docker", root: true, real: true, run: changeKillDocker},
	{name: "change/cut", run: changeCut},
	{name: "change/full-disk", root: true, run: changeFullDisk},
	{name: "change/clock", root: true, real: true, run: changeClock},
	{name: "away/baseline", run: awayBaseline},
	{name: "away/kill-agent", run: awayKillAgent},
	{name: "away/kill-control", run: awayKillControl},
	{name: "away/silent-cut", run: awaySilentCut},
	{name: "away/kill-docker", root: true, real: true, run: awayKillDocker},
	{name: "away/full-disk", root: true, run: awayFullDisk},
	{name: "away/answer-unkept", root: true, run: awayAnswerUnkept},
	{name: "away/clock", root: true, real: true, run: awayClock},
	{name: "cache/corrupt", run: cacheCorrupt},
}

// cant says why the stand can't run sc, or nothing.
func (s *stand) cant(sc scenario) string {
	switch {
	case sc.real && !s.real:
		return "it needs a real Docker Engine"
	case sc.root && os.Getenv("CHAOS_HOST_FAULTS") != "1":
		return "it needs CHAOS_HOST_FAULTS=1 on a disposable host"
	case sc.root && !s.root:
		return "it needs root, through sudo -n"
	}
	return ""
}

// play runs one scenario and checks the invariants after it.
func (s *stand) play(ctx context.Context, sc scenario) error {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	r, err := s.begin(ctx)
	if err != nil {
		return fmt.Errorf("can't see how things stand before it: %w", err)
	}
	if err := sc.run(ctx, s, r); err != nil {
		return err
	}
	return s.settle(ctx, r)
}

// recover puts the stand back as well as it can after a scenario that failed, so
// the next starts from something whole.
func (s *stand) recover(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if err := s.clock.back(ctx); err != nil {
		return err
	}
	if err := s.free(); err != nil {
		return err
	}
	s.proxy.set(pass)
	if s.real && s.root {
		_ = s.startDocker(ctx)
	}
	if !s.control.alive() {
		if err := s.startControlAgain(ctx); err != nil {
			return err
		}
	}
	if !s.agent.alive() {
		if err := s.startAgent(); err != nil {
			return err
		}
	}
	if err := s.waitConnected(ctx); err != nil {
		return err
	}
	// Start what the failed scenario left stopped, so its failure isn't counted
	// again against each scenario after it. The scenario itself stays failed, and
	// this says what it did. held stays as a person left it.
	for _, name := range []string{"web", "slow", "spare"} {
		b, err := s.one(ctx, name)
		if err != nil || b.Running {
			continue
		}
		say("recovering: starting %s, which the failed scenario left stopped", name)
		if err := s.docker.StartContainer(ctx, b.ID); err != nil {
			say("recovering: can't start %s: %v", name, err)
		}
	}
	_ = s.probe.resume(ctx)
	s.probe.take()
	return nil
}

// Mid-change: a deploy of slow, broken while its step runs.

func changeKillAgent(ctx context.Context, s *stand, r *round) error {
	return midChange(ctx, s, r, func() error {
		s.agent.kill()
		if err := sleep(ctx, 3*time.Second); err != nil {
			return err
		}
		return s.restartAgent(ctx, true)
	})
}

func changeKillControl(ctx context.Context, s *stand, r *round) error {
	return midChange(ctx, s, r, func() error {
		s.killControl()
		if err := sleep(ctx, 3*time.Second); err != nil {
			return err
		}
		if err := s.startControlAgain(ctx); err != nil {
			return err
		}
		return s.waitConnected(ctx)
	})
}

func changeKillDocker(ctx context.Context, s *stand, r *round) error {
	return midChange(ctx, s, r, func() error {
		s.probe.pause()
		r.restarted = true
		if err := s.killDocker(ctx); err != nil {
			return err
		}
		if err := sleep(ctx, 3*time.Second); err != nil {
			return err
		}
		if err := s.startDocker(ctx); err != nil {
			return err
		}
		return s.probe.resume(ctx)
	})
}

func changeCut(ctx context.Context, s *stand, r *round) error {
	return midChange(ctx, s, r, func() error {
		s.proxy.set(cut)
		if err := sleep(ctx, 40*time.Second); err != nil {
			return err
		}
		return s.heal(ctx)
	})
}

func changeFullDisk(ctx context.Context, s *stand, r *round) error {
	return midChange(ctx, s, r, func() error {
		if err := s.fill(); err != nil {
			return err
		}
		if err := sleep(ctx, 10*time.Second); err != nil {
			return err
		}
		return s.free()
	})
}

func changeClock(ctx context.Context, s *stand, r *round) error {
	return midChange(ctx, s, r, func() error {
		if err := s.clock.ahead(ctx, 2*time.Hour); err != nil {
			return err
		}
		if err := sleep(ctx, 10*time.Second); err != nil {
			return err
		}
		return s.clock.back(ctx)
	})
}

// midChange deploys slow, breaks it with fault as its step runs, and leaves the
// rest to the invariants.
func midChange(ctx context.Context, s *stand, r *round, fault func() error) error {
	id, applying, err := s.deploy(ctx)
	if err != nil {
		return err
	}
	r.changes["slow"] = id
	if err := s.inFlight(ctx, id, applying); err != nil {
		return fmt.Errorf("the deploy never got under way: %w", err)
	}
	return fault()
}

// Mid-reconcile: chaos-1 away, and web's container removed for its agent to put
// back, broken as it does.

func awayBaseline(ctx context.Context, s *stand, r *round) error {
	return midReconcile(ctx, s, r, refuse, nil)
}

func awaySilentCut(ctx context.Context, s *stand, r *round) error {
	return midReconcile(ctx, s, r, cut, nil)
}

func awayKillAgent(ctx context.Context, s *stand, r *round) error {
	return midReconcile(ctx, s, r, refuse, func(was string) error {
		// Killed as it makes web again: as soon as Docker has its new container.
		if err := wait(ctx, 30*time.Second, func() error {
			all, err := s.boxes(ctx)
			if err != nil {
				return err
			}
			for i := range all["web"] {
				b := &all["web"][i]
				if b.ID != was {
					return nil
				}
			}
			return errors.New("web isn't made again yet")
		}); err != nil {
			return err
		}
		s.agent.kill()
		if err := sleep(ctx, time.Second); err != nil {
			return err
		}
		return s.restartAgent(ctx, false)
	})
}

func awayKillControl(ctx context.Context, s *stand, r *round) error {
	before := s.lost()
	s.killControl()
	if err := wait(ctx, 15*time.Second, func() error {
		if s.lost() <= before {
			return errors.New("chaos-1's agent hasn't said it lost the control plane")
		}
		return nil
	}); err != nil {
		return err
	}
	if err := sleep(ctx, 3*time.Second); err != nil {
		return err
	}
	if err := removeWeb(ctx, s, r); err != nil {
		return err
	}
	if err := s.noteJournal(r); err != nil {
		return err
	}
	if err := s.startControlAgain(ctx); err != nil {
		return err
	}
	return s.waitConnected(ctx)
}

func awayKillDocker(ctx context.Context, s *stand, r *round) error {
	if err := s.away(ctx, refuse); err != nil {
		return err
	}
	s.probe.pause()
	r.restarted = true
	spare, err := s.one(ctx, "spare")
	if err != nil {
		return err
	}
	// A SIGKILL can leave containers running under containerd. Stop spare first:
	// its always policy makes Docker try it on restart, with its port occupied.
	if err := s.docker.StopContainer(ctx, spare.ID, 2*time.Second); err != nil {
		return err
	}
	release, err := holdPort(ctx, s.sparePort)
	if err != nil {
		return err
	}
	if err := s.killDocker(ctx); err != nil {
		release()
		return err
	}
	if err := s.startDocker(ctx); err != nil {
		release()
		return err
	}
	if err := wait(ctx, 20*time.Second, func() error {
		spare, err := s.one(ctx, "spare")
		if err != nil {
			return err
		}
		if spare.Error == "" || spare.Running {
			return fmt.Errorf("the Docker engine didn't leave a failed start to reconcile: running=%t, State.Error=%q", spare.Running, spare.Error)
		}
		return nil
	}); err != nil {
		release()
		return err
	}
	r.expect["started spare"] = 1
	if err := sleep(ctx, 3*time.Second); err != nil {
		release()
		return err
	}
	release()
	if err := s.probe.resume(ctx); err != nil {
		return err
	}
	if r.expect["started spare"] == 1 {
		// The agent tries again 10 s after its first try.
		if err := wait(ctx, 45*time.Second, func() error {
			b, err := s.one(ctx, "spare")
			if err != nil {
				return err
			}
			if !b.Running {
				return fmt.Errorf("spare isn't running: %s", b.Error)
			}
			return nil
		}); err != nil {
			return fmt.Errorf("the agent didn't start what Docker couldn't: %w", err)
		}
	}
	if err := s.noteJournal(r); err != nil {
		return err
	}
	return s.heal(ctx)
}

func awayFullDisk(ctx context.Context, s *stand, r *round) error {
	if err := s.away(ctx, refuse); err != nil {
		return err
	}
	if err := s.fill(); err != nil {
		return err
	}
	web, err := s.one(ctx, "web")
	if err != nil {
		return err
	}
	s.probe.pause()
	r.removed["web"], r.expect["made web"] = true, 1
	if err := s.docker.RemoveContainer(ctx, web.ID, true); err != nil {
		return err
	}
	// A full journal must hold Docker back until the intent can be kept. Allow
	// several looks while full and verify no unaccounted-for creation happens.
	until := time.Now().Add(6 * time.Second)
	for time.Now().Before(until) {
		all, err := s.boxes(ctx)
		if err != nil {
			return err
		}
		if len(all["web"]) != 0 {
			return errors.New("the agent made web without room for its durable pending receipt")
		}
		if err := sleep(ctx, 200*time.Millisecond); err != nil {
			return err
		}
	}
	if err := s.free(); err != nil {
		return err
	}
	if err := waitMade(ctx, s, web.ID); err != nil {
		return err
	}
	if err := s.probe.resume(ctx); err != nil {
		return err
	}
	if err := s.noteJournal(r); err != nil {
		return err
	}
	return s.heal(ctx)
}

// awayAnswerUnkept has the hand-back answered while the disk is full, so the agent
// can't keep that it was heard; killed and started again, it sends it again.
func awayAnswerUnkept(ctx context.Context, s *stand, r *round) error {
	return handBackTwice(ctx, s, r, nil)
}

// awayClock jumps the clock ahead while away, so what the agent writes down is from
// ahead of the control plane's clock once it's back, and has it sent twice.
func awayClock(ctx context.Context, s *stand, r *round) error {
	return handBackTwice(ctx, s, r, func() error { return s.clock.ahead(ctx, 2*time.Hour) })
}

func handBackTwice(ctx context.Context, s *stand, r *round, before func() error) error {
	if err := s.away(ctx, refuse); err != nil {
		return err
	}
	if before != nil {
		if err := before(); err != nil {
			return err
		}
	}
	if err := removeWeb(ctx, s, r); err != nil {
		return err
	}
	if err := s.clock.back(ctx); err != nil {
		return err
	}
	if err := s.noteJournal(r); err != nil {
		return err
	}
	if err := s.fill(); err != nil {
		return err
	}
	if err := s.heal(ctx); err != nil {
		return err
	}
	// Once the control plane has it, the agent hears so and can't write it down.
	if err := wait(ctx, 30*time.Second, func() error {
		events, err := s.reconciled(ctx)
		if err != nil {
			return err
		}
		for _, e := range events {
			if !r.seen[e.ID] && e.Action == "server.reconciled_made" {
				return nil
			}
		}
		return errors.New("the hand-back isn't on Activity yet")
	}); err != nil {
		return err
	}
	if err := sleep(ctx, time.Second); err != nil {
		return err
	}
	s.agent.kill()
	if err := s.free(); err != nil {
		return err
	}
	return s.restartAgent(ctx, true)
}

// midReconcile has chaos-1 go away by mode, removes web's container, and lets
// fault break the agent as it puts it back, given the removed container's ID.
func midReconcile(ctx context.Context, s *stand, r *round, mode int, fault func(was string) error) error {
	if err := s.away(ctx, mode); err != nil {
		return err
	}
	if fault == nil {
		if err := removeWeb(ctx, s, r); err != nil {
			return err
		}
	} else {
		web, err := s.one(ctx, "web")
		if err != nil {
			return err
		}
		s.probe.pause()
		r.removed["web"], r.expect["made web"] = true, 1
		if err := s.docker.RemoveContainer(ctx, web.ID, true); err != nil {
			return err
		}
		if err := fault(web.ID); err != nil {
			return err
		}
		if err := waitMade(ctx, s, web.ID); err != nil {
			return err
		}
		if err := s.probe.resume(ctx); err != nil {
			return err
		}
	}
	if err := s.noteJournal(r); err != nil {
		return err
	}
	return s.heal(ctx)
}

// removeWeb removes web's container, as docker rm -f would, and waits for the
// agent to make it again on its own.
func removeWeb(ctx context.Context, s *stand, r *round) error {
	web, err := s.one(ctx, "web")
	if err != nil {
		return err
	}
	s.probe.pause()
	r.removed["web"], r.expect["made web"] = true, 1
	if err := s.docker.RemoveContainer(ctx, web.ID, true); err != nil {
		return err
	}
	say("removed web's container %s", short(web.ID))
	if err := waitMade(ctx, s, web.ID); err != nil {
		return err
	}
	return s.probe.resume(ctx)
}

// waitMade waits for web to run again in a container other than was.
func waitMade(ctx context.Context, s *stand, was string) error {
	err := wait(ctx, 45*time.Second, func() error {
		b, err := s.one(ctx, "web")
		if err != nil {
			return err
		}
		if b.ID == was || !b.Running {
			return errors.New("web isn't running again yet")
		}
		say("web runs again in %s, made by the agent while away (it was %s)", short(b.ID), short(was))
		return nil
	})
	if err != nil {
		return fmt.Errorf("the agent didn't put web back while away: %w", err)
	}
	return nil
}

// cacheCorrupt corrupts two of chaos-1's records with its agent stopped: they go
// aside, the control plane fills them again, and nothing on the server changes.
func cacheCorrupt(ctx context.Context, s *stand, _ *round) error {
	s.agent.stop()
	if _, err := s.records(); err != nil {
		return err
	}
	paths, err := s.corrupt()
	if err != nil {
		return err
	}
	broken := make(map[string][]byte, len(paths))
	for _, path := range paths {
		b, err := os.ReadFile(path) //nolint:gosec // the records just corrupted in the stand's own folder
		if err != nil {
			return err
		}
		broken[filepath.Base(path)] = b
	}
	if err := s.restartAgent(ctx, true); err != nil {
		return err
	}
	// Read the files directly while the agent runs. Opening its cache here would
	// move damaged files itself and race the agent. Each record replaces its
	// previous quarantined copy, so check the current broken bytes, not a count.
	return wait(ctx, 30*time.Second, func() error {
		for name, want := range broken {
			path := filepath.Join(s.agentData, "agent", desired.Folder, desired.Damaged, name)
			got, err := os.ReadFile(path) //nolint:gosec // the quarantine in the stand's own agent folder
			if err != nil {
				return fmt.Errorf("can't read the quarantined record %s: %w", name, err)
			}
			if !bytes.Equal(got, want) {
				return fmt.Errorf("the current damaged record %s hasn't been put aside", name)
			}
		}
		records, err := filepath.Glob(filepath.Join(s.agentData, "agent", desired.Folder, "*.rec"))
		if err != nil {
			return err
		}
		if len(records) < len(services) {
			return fmt.Errorf("chaos-1 has %d records after putting the damaged ones aside, want %d", len(records), len(services))
		}
		return nil
	})
}
