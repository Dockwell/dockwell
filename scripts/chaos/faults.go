// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/dockwell/dockwell/internal/agent/desired"
)

// restartAgent starts chaos-1's agent again, and waits for it to connect, unless
// the proxy keeps it away.
func (s *stand) restartAgent(ctx context.Context, connect bool) error {
	if err := s.startAgent(); err != nil {
		return err
	}
	if !connect {
		return nil
	}
	return s.waitConnected(ctx)
}

// waitConnected waits for chaos-1 to be connected, which an agent that backed off
// takes up to 30 s to try again for.
func (s *stand) waitConnected(ctx context.Context) error {
	return wait(ctx, 75*time.Second, func() error {
		c, err := s.connection(ctx)
		if err != nil {
			return err
		}
		if c != connected {
			return fmt.Errorf("chaos-1 is %s", c)
		}
		return nil
	})
}

// lost counts the times chaos-1's agent said it lost the control plane, so a
// scenario can wait for the next.
func (s *stand) lost() int {
	b, _ := os.ReadFile(s.agent.log) //nolint:gosec // the process log in the stand's own temporary folder
	return bytes.Count(b, []byte(`"msg":"lost the control plane"`))
}

// away has chaos-1's agent lose the control plane, by the proxy's mode, and waits
// until it may act on its own: it has seen the stream go, and 2 s have passed.
func (s *stand) away(ctx context.Context, mode int) error {
	before := s.lost()
	s.proxy.set(mode)
	within := 15 * time.Second
	if mode == cut {
		// Nothing closes: the agent's ping and the control plane's silence take 30 s.
		within = 75 * time.Second
	}
	if err := wait(ctx, within, func() error {
		if s.lost() <= before {
			return errors.New("chaos-1's agent hasn't said it lost the control plane")
		}
		return nil
	}); err != nil {
		return err
	}
	return sleep(ctx, 3*time.Second)
}

// heal lets the agent reach the control plane again, and waits for it to.
func (s *stand) heal(ctx context.Context) error {
	s.proxy.set(pass)
	return s.waitConnected(ctx)
}

// killControl kills the control server, and its own agent with it.
func (s *stand) killControl() { s.control.kill() }

// killDocker kills the Docker daemon with no warning; systemd starts it again on
// its own a few seconds later, as it would after a crash.
func (s *stand) killDocker(ctx context.Context) error {
	return sudo(ctx, "systemctl", "kill", "--signal=SIGKILL", "docker.service")
}

// startDocker starts the Docker daemon if systemd hasn't yet, and waits for it.
func (s *stand) startDocker(ctx context.Context) error {
	_ = sudo(ctx, "systemctl", "reset-failed", "docker.service")
	if err := sudo(ctx, "systemctl", "start", "docker.service"); err != nil {
		return err
	}
	return s.waitEngine(ctx, 90*time.Second)
}

// fill takes all of chaos-1's data disk, and free gives it back.
func (s *stand) fill() error {
	f, err := os.OpenFile(s.filler(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // the filler in the stand's own mounted temporary folder
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 1<<20)
	for {
		if _, err := f.Write(buf); err != nil {
			if errors.Is(err, syscall.ENOSPC) {
				return nil
			}
			return err
		}
	}
}

func (s *stand) free() error {
	err := os.Remove(s.filler()) //nolint:gosec // the filler in the stand's own mounted temporary folder
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (s *stand) filler() string { return filepath.Join(s.agentData, ".chaos-filler") }

// corrupt changes one byte of one of chaos-1's records and cuts another short,
// with its agent stopped.
func (s *stand) corrupt() ([]string, error) {
	recs, err := filepath.Glob(filepath.Join(s.agentData, "agent", desired.Folder, "*.rec"))
	if err != nil {
		return nil, err
	}
	slices.Sort(recs)
	if len(recs) < 2 {
		return nil, fmt.Errorf("chaos-1 keeps %d records, want at least 2 to corrupt", len(recs))
	}
	b, err := os.ReadFile(recs[0]) //nolint:gosec // records globbed from the stand's own agent folder
	if err != nil {
		return nil, err
	}
	b[len(b)/2] ^= 0x40
	if err := os.WriteFile(recs[0], b, 0o600); err != nil { //nolint:gosec // the same record, corrupted on purpose
		return nil, err
	}
	if err := os.Truncate(recs[1], 5); err != nil {
		return nil, err
	}
	return recs[:2], nil
}

// holdPort listens on 127.0.0.1:port, so Docker can't publish a container there,
// until the returned function lets it go. It waits for the port, which a Docker
// that was killed lets go of a moment later.
func holdPort(ctx context.Context, port int) (func(), error) {
	var ln net.Listener
	err := wait(ctx, 15*time.Second, func() error {
		var err error
		ln, err = (&net.ListenConfig{}).Listen(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
		return err
	})
	if err != nil {
		return nil, err
	}
	return func() { _ = ln.Close() }, nil
}

// apply applies a planned change in the background, as a person would from the
// CLI, and stops waiting for it when ctx ends; what the change comes to is the
// store's to say.
func (s *stand) apply(ctx context.Context, id string) <-chan error {
	cmd := exec.CommandContext(ctx, s.bin, "changes", "apply", id, "--yes") //nolint:gosec // the binary under test, with its own change ID
	cmd.Env = s.env("DOCKWELL_DATA_DIR="+s.controlData, "DOCKWELL_LOG_FORMAT=text")
	done := make(chan error, 1)
	go func() {
		out, err := cmd.CombinedOutput()
		if err != nil {
			err = fmt.Errorf("can't apply %s: %w: %s", id, err, bytes.TrimSpace(out))
		}
		done <- err
	}()
	return done
}

// deploy plans a deploy of slow to its other image, applies it in the background,
// and says the change's ID.
func (s *stand) deploy(ctx context.Context) (string, <-chan error, error) {
	live, err := s.live(ctx)
	if err != nil {
		return "", nil, err
	}
	img := s.images[0]
	if live["slow"].Image == img {
		img = s.images[1]
	}
	var c changeJSON
	if err := s.cliJSON(ctx, &c, "services", "deploy", "slow", "--image", img, "--plan-only"); err != nil {
		return "", nil, err
	}
	return c.Change.ID, s.apply(ctx, c.Change.ID), nil
}

// inFlight waits for the change's step on slow to be under way: on real Docker,
// its new container running while the change still applies, which is the 8 s
// the agent's own check takes to pass; on the fake Engine, its image still
// being pulled. Docker has no health of its own to read here: an HTTP check is
// the agent's, kept in a label, so State.Health stays empty.
func (s *stand) inFlight(ctx context.Context, id string, applying <-chan error) error {
	return wait(ctx, 60*time.Second, func() error {
		select {
		case err := <-applying:
			if err != nil {
				return stop(err)
			}
			applying = nil
		default:
		}
		st, err := s.changeState(ctx, id)
		if err != nil {
			return err
		}
		switch {
		case st == "CHANGE_STATE_APPLYING":
		case applying == nil:
			// The apply returned and the change isn't applying, so it ended
			// before it was caught: nothing is left to break, and waiting can't help.
			return stop(fmt.Errorf("the change ended as %s before its step was caught under way", st))
		default:
			return fmt.Errorf("the change is %s", st)
		}
		if !s.real {
			return nil
		}
		all, err := s.boxes(ctx)
		if err != nil {
			return err
		}
		for i := range all["slow"] {
			b := &all["slow"][i]
			if b.Change == id && b.Running {
				return nil
			}
		}
		return errors.New("slow's new container isn't running yet")
	})
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
