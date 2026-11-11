// SPDX-License-Identifier: AGPL-3.0-only

// Package system reads a server's system services (ADR 0067): its systemd units,
// or its OpenRC services, with the init system's own tools, run as ADR 0043 runs
// a package manager's, on a runner of the reader's own, so a firewall change
// waiting on its confirm timer holds up no list. It keeps nothing between reads.
package system

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/hosttool"
)

// Deadline bounds each run of a tool.
const Deadline = 10 * time.Second

// The init systems' tools. TZ=UTC makes systemctl print its times in UTC, so they
// read the same on every server.
var (
	systemctl = hosttool.Tool{Name: "systemctl", Paths: []string{"/usr/bin/systemctl", "/bin/systemctl"}, Env: []string{"TZ=UTC", "SYSTEMD_COLORS=0"}}
	rcStatus  = hosttool.Tool{Name: "rc-status", Paths: []string{"/sbin/rc-status", "/bin/rc-status", "/usr/sbin/rc-status", "/usr/bin/rc-status"}}
)

// Properties are all that systemctl show reads of a unit: what the list shows.
// Never Environment or ExecStart, which can hold a password; ExecMainStatus is
// a number.
var Properties = []string{
	"Id", "Description", "LoadState", "ActiveState", "SubState", "UnitFileState",
	"MainPID", "MemoryCurrent", "StateChangeTimestamp", "ExecMainStatus", "Result",
}

// Needed are the units Dockwell needs on a server, by systemd's names and
// OpenRC's: its agent, the access that guards it (ADR 0052), Docker and
// containerd, and the SSH server, which Ubuntu 22.10 and later start from
// ssh.socket, so the socket is listed beside the service.
var Needed = []string{
	"dockwell.service", "dockwell-access.service", "docker.service", "docker.socket", "containerd.service",
	"ssh.service", "sshd.service", "ssh.socket",
	"dockwell", "docker", "containerd", "sshd",
}

// sockets are the socket units read beside the services: the ones Dockwell needs.
var sockets = []string{"docker.socket", "ssh.socket"}

// unitName is a unit's name as systemctl knows it, instance escapes such as
// \x2d included. A name that isn't is never passed on.
var unitName = regexp.MustCompile(`^[A-Za-z0-9:_.@\\-]+\.(service|socket)$`)

// Reader reads a server's system services.
type Reader struct {
	// Tools runs the init system's tools: a runner of the reader's own.
	Tools *hosttool.Runner
	// Root, for a test, is where the server's files are, as the runner's Root.
	Root string
	// Unavailable, when set, is why nothing is read, as in a test without
	// stand-ins, which reads nothing of the machine it runs on.
	Unavailable string
}

// Services reads the server's system services now.
func (r *Reader) Services(ctx context.Context) (*dockwellv1.SystemServiceList, error) {
	if r.Unavailable != "" {
		return nil, errors.New(r.Unavailable)
	}
	list := &dockwellv1.SystemServiceList{Time: timestamppb.Now()}
	switch {
	case r.exists("run/systemd/system"):
		// systemd leaves this while it's the init system; a container with
		// systemd installed and another init doesn't have it.
		list.Init = dockwellv1.SystemInit_SYSTEM_INIT_SYSTEMD
		services, err := r.systemd(ctx)
		if err != nil {
			return nil, err
		}
		list.Services = services
	case r.exists("run/openrc"):
		list.Init = dockwellv1.SystemInit_SYSTEM_INIT_OPENRC
		services, err := r.openrc(ctx)
		if err != nil {
			return nil, err
		}
		list.Services = services
	default:
		list.Init = dockwellv1.SystemInit_SYSTEM_INIT_OTHER
	}
	sortServices(list.Services)
	return list, nil
}

func (r *Reader) exists(p string) bool {
	root := r.Root
	if root == "" {
		root = "/"
	}
	_, err := os.Stat(filepath.Join(root, p))
	return err == nil
}

func (r *Reader) run(ctx context.Context, t hosttool.Tool, args ...string) ([]byte, error) {
	res, err := r.Tools.Run(ctx, t, Deadline, nil, args...)
	if err != nil {
		return nil, fmt.Errorf("can't read the server's system services: %w", err)
	}
	if res.Code != 0 {
		return nil, fmt.Errorf("can't read the server's system services: %w", hosttool.Fail(t.Name, res))
	}
	return res.Out, nil
}

// systemd reads every service unit, loaded or installed, and the sockets
// Dockwell needs, then only the properties shown of each.
func (r *Reader) systemd(ctx context.Context) ([]*dockwellv1.SystemServiceInfo, error) {
	units, err := r.run(ctx, systemctl, "list-units", "--type=service", "--all", "--plain", "--no-legend", "--no-pager")
	if err != nil {
		return nil, err
	}
	files, err := r.run(ctx, systemctl, "list-unit-files", "--type=service", "--plain", "--no-legend", "--no-pager")
	if err != nil {
		return nil, err
	}
	names := firstColumn(units)
	for _, n := range unitFiles(files) {
		if !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	names = append(names, sockets...)
	names = slices.DeleteFunc(names, func(n string) bool {
		// A template, such as getty@.service, isn't a unit that runs.
		return !unitName.MatchString(n) || strings.Contains(n, "@.")
	})
	slices.Sort(names)
	names = slices.Compact(names)
	if len(names) == 0 {
		return nil, nil
	}
	args := append([]string{"show", "--property=" + strings.Join(Properties, ","), "--no-pager", "--"}, names...)
	out, err := r.run(ctx, systemctl, args...)
	if err != nil {
		return nil, err
	}
	return parseShow(out), nil
}

// firstColumn is the first word of each line, which a plain list without a
// legend starts with the unit's name.
func firstColumn(out []byte) []string {
	var names []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), hosttool.MaxOutput)
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) > 0 {
			names = append(names, f[0])
		}
	}
	return names
}

// unitFiles is each installed unit's name, leaving out an alias, such as Debian's
// sshd.service for ssh.service: show reads it as the unit it names, which is
// listed already.
func unitFiles(out []byte) []string {
	var names []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), hosttool.MaxOutput)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 || len(f) > 1 && f[1] == "alias" {
			continue
		}
		names = append(names, f[0])
	}
	return names
}

// noMemory is what systemctl says of memory a unit doesn't account: [not set],
// or the largest number, as systemd 239 prints it.
const noMemory = "18446744073709551615"

// parseShow reads systemctl show's blocks, one a unit, KEY=VALUE lines apart by a
// blank line. A unit systemd doesn't know isn't listed, nor one listed already,
// as an alias reads as the unit it names.
func parseShow(out []byte) []*dockwellv1.SystemServiceInfo {
	var services []*dockwellv1.SystemServiceInfo
	seen := map[string]bool{}
	for _, block := range bytes.Split(out, []byte("\n\n")) {
		props := map[string]string{}
		for _, line := range strings.Split(string(block), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				props[k] = v
			}
		}
		if props["Id"] == "" || props["LoadState"] == "not-found" || seen[props["Id"]] {
			continue
		}
		seen[props["Id"]] = true
		s := &dockwellv1.SystemServiceInfo{
			Name:        props["Id"],
			Description: props["Description"],
			State:       stateOf(props["ActiveState"], props["SubState"]),
			Detail:      props["SubState"],
			AtBoot:      props["UnitFileState"],
			Needed:      slices.Contains(Needed, props["Id"]),
		}
		if t, err := time.Parse("Mon 2006-01-02 15:04:05 MST", props["StateChangeTimestamp"]); err == nil {
			s.Since = timestamppb.New(t)
		}
		running := s.State == dockwellv1.SystemServiceState_SYSTEM_SERVICE_STATE_RUNNING
		if pid, err := strconv.ParseUint(props["MainPID"], 10, 32); err == nil && running {
			s.MainPid = uint32(pid)
		}
		if m := props["MemoryCurrent"]; running && m != noMemory {
			if n, err := strconv.ParseUint(m, 10, 64); err == nil {
				s.MemoryBytes = &n
			}
		}
		if s.State == dockwellv1.SystemServiceState_SYSTEM_SERVICE_STATE_FAILED {
			if res := props["Result"]; res != "success" {
				s.Result = res
			}
		}
		if n, err := strconv.ParseInt(props["ExecMainStatus"], 10, 32); err == nil {
			s.ExitStatus = int32(n)
		}
		services = append(services, s)
	}
	return services
}

// stateOf is how a unit stands, from systemd's ActiveState and SubState.
func stateOf(active, sub string) dockwellv1.SystemServiceState {
	switch active {
	case "active", "reloading", "refreshing":
		if sub == "exited" {
			return dockwellv1.SystemServiceState_SYSTEM_SERVICE_STATE_EXITED
		}
		return dockwellv1.SystemServiceState_SYSTEM_SERVICE_STATE_RUNNING
	case "failed":
		return dockwellv1.SystemServiceState_SYSTEM_SERVICE_STATE_FAILED
	case "activating":
		return dockwellv1.SystemServiceState_SYSTEM_SERVICE_STATE_STARTING
	case "deactivating":
		return dockwellv1.SystemServiceState_SYSTEM_SERVICE_STATE_STOPPING
	}
	return dockwellv1.SystemServiceState_SYSTEM_SERVICE_STATE_INACTIVE
}

// openrc reads rc-status's sections: a runlevel's, which start at boot in it, then
// the dynamic ones, which don't.
func (r *Reader) openrc(ctx context.Context) ([]*dockwellv1.SystemServiceInfo, error) {
	out, err := r.run(ctx, rcStatus, "--all", "--nocolor")
	if err != nil {
		return nil, err
	}
	return parseRCStatus(out), nil
}

// rcLine is one service in rc-status's list: its name, then its state in brackets.
var rcLine = regexp.MustCompile(`^\s+(\S+)\s+\[\s*([a-z]+)`)

func parseRCStatus(out []byte) []*dockwellv1.SystemServiceInfo {
	seen := map[string]*dockwellv1.SystemServiceInfo{}
	var services []*dockwellv1.SystemServiceInfo
	runlevel := ""
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "Runlevel: "):
			runlevel = strings.TrimSpace(strings.TrimPrefix(line, "Runlevel: "))
			continue
		case strings.HasPrefix(line, "Dynamic Runlevel: "):
			runlevel = ""
			continue
		}
		m := rcLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if s, ok := seen[m[1]]; ok {
			if s.AtBoot == "" {
				s.AtBoot = runlevel
			}
			continue
		}
		s := &dockwellv1.SystemServiceInfo{Name: m[1], Detail: m[2], State: rcState(m[2]), AtBoot: runlevel, Needed: slices.Contains(Needed, m[1])}
		seen[m[1]] = s
		services = append(services, s)
	}
	return services
}

func rcState(state string) dockwellv1.SystemServiceState {
	switch state {
	case "started":
		return dockwellv1.SystemServiceState_SYSTEM_SERVICE_STATE_RUNNING
	case "crashed", "failed":
		return dockwellv1.SystemServiceState_SYSTEM_SERVICE_STATE_FAILED
	case "starting":
		return dockwellv1.SystemServiceState_SYSTEM_SERVICE_STATE_STARTING
	case "stopping":
		return dockwellv1.SystemServiceState_SYSTEM_SERVICE_STATE_STOPPING
	}
	return dockwellv1.SystemServiceState_SYSTEM_SERVICE_STATE_INACTIVE
}

// sortServices puts failed ones first, then running ones, then the rest, each by
// name.
func sortServices(services []*dockwellv1.SystemServiceInfo) {
	rank := func(s *dockwellv1.SystemServiceInfo) int {
		switch s.GetState() {
		case dockwellv1.SystemServiceState_SYSTEM_SERVICE_STATE_FAILED:
			return 0
		case dockwellv1.SystemServiceState_SYSTEM_SERVICE_STATE_RUNNING:
			return 1
		case dockwellv1.SystemServiceState_SYSTEM_SERVICE_STATE_UNSPECIFIED, dockwellv1.SystemServiceState_SYSTEM_SERVICE_STATE_EXITED,
			dockwellv1.SystemServiceState_SYSTEM_SERVICE_STATE_INACTIVE, dockwellv1.SystemServiceState_SYSTEM_SERVICE_STATE_STARTING,
			dockwellv1.SystemServiceState_SYSTEM_SERVICE_STATE_STOPPING:
		}
		return 2
	}
	slices.SortStableFunc(services, func(a, b *dockwellv1.SystemServiceInfo) int {
		if d := rank(a) - rank(b); d != 0 {
			return d
		}
		return strings.Compare(a.GetName(), b.GetName())
	})
}
