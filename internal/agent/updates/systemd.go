// SPDX-License-Identifier: AGPL-3.0-only

package updates

import (
	"context"
	"errors"
	"strconv"
	"strings"
)

// systemd's programs.
const (
	systemdRun = "/usr/bin/systemd-run"
	systemctl  = "/usr/bin/systemctl"
)

// ErrNoUnits is a server whose init system can't run an install in a unit of its
// own: one without systemd, as Alpine's OpenRC.
var ErrNoUnits = errors.New("this server has no systemd to run an install in a unit of its own")

// Systemd runs installs as systemd's transient units: in a cgroup of their own,
// outside the agent's, so neither the agent's restart nor its unit's stop ends
// them, and with them every process a maintainer's script starts.
type Systemd struct {
	Reader *Reader
}

var _ Units = Systemd{}

// Has reports whether the server runs systemd, by what systemd itself leaves
// while it's the init system.
func (s Systemd) Has() bool { return s.Reader.exists("run/systemd/system") }

// Start runs argv as the transient service name. It isn't collected as it ends,
// so a failure's status can be read; Ended lets it go.
func (s Systemd) Start(ctx context.Context, name string, argv, env []string, log string) error {
	return s.StartAs(ctx, name, "Dockwell installs updates", argv, env, log)
}

// StartAs is Start, with what systemd says the unit is.
func (s Systemd) StartAs(ctx context.Context, name, description string, argv, env []string, log string) error {
	if !s.Has() {
		return ErrNoUnits
	}
	args := []string{
		"--unit=" + name, "--description=" + description, "--service-type=exec", "--quiet",
		"--property=StandardOutput=append:" + log, "--property=StandardError=append:" + log,
		// What it starts goes when it's stopped by hand, all of it, and nothing
		// else stops it: no timeout, no stop with the agent.
		"--property=KillMode=control-group", "--property=TimeoutStartSec=infinity",
	}
	for _, e := range env {
		args = append(args, "--setenv="+e)
	}
	args = append(append(args, "--"), argv...)
	res, err := s.Reader.run(ctx, "Starting the install", systemdRun, ReadTimeout, args...)
	if err != nil {
		return err
	}
	if res.code != 0 {
		return fail("Starting the install", res)
	}
	return nil
}

// Active reports whether the unit runs.
func (s Systemd) Active(ctx context.Context, name string) (bool, error) {
	if !s.Has() {
		return false, nil
	}
	state, err := s.show(ctx, name, "ActiveState")
	if err != nil {
		return false, err
	}
	return state == "active" || state == "activating" || state == "deactivating", nil
}

// Ended reads how the unit ended, then lets it go.
func (s Systemd) Ended(ctx context.Context, name string) (int, error) {
	if !s.Has() {
		return -1, nil
	}
	status, err := s.show(ctx, name, "ExecMainStatus")
	code := -1
	if err == nil {
		if n, err := strconv.Atoi(status); err == nil {
			code = n
		}
	}
	// A unit that ended well is gone already; one that failed waits to be let go.
	_, _ = s.Reader.run(ctx, "Letting the install's unit go", systemctl, ReadTimeout, "reset-failed", "--", name)
	return code, err
}

// show reads one property of the unit, or "" for one systemd doesn't know.
func (s Systemd) show(ctx context.Context, name, prop string) (string, error) {
	res, err := s.Reader.run(ctx, "Reading the install's unit", systemctl, ReadTimeout, "show", "--property="+prop, "--value", "--", name)
	if err != nil {
		return "", err
	}
	if res.code != 0 {
		return "", fail("Reading the install's unit", res)
	}
	return strings.TrimSpace(string(res.out)), nil
}
