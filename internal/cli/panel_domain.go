// SPDX-License-Identifier: AGPL-3.0-only

//go:build !agent

package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/settings"
	"github.com/dockwell/dockwell/internal/https"
	"github.com/dockwell/dockwell/internal/store"
)

// envTestDNS names what a test resolves, as "panel.example.com=192.0.2.10,...":
// a test asks no DNS server beyond the machine it runs on.
const envTestDNS = "DOCKWELL_TEST_DNS"

// errRestart is a control plane that stopped to start again, as a person asked
// when they saved the panel's domain (ADR 0063).
var errRestart = errors.New("the control plane restarts to serve the panel's domain")

// panelDomainAtStart is the panel's domain when dockwell run wasn't given one:
// the setting's, unless the panel can't take it, which never stops dockwell run.
// It says where the domain comes from, and why the setting's isn't served.
func panelDomainAtStart(ctx context.Context, logger *slog.Logger, db *store.DB, given string) (domain string, from dockwellv1.PanelDomainFrom, problem string) {
	if given != "" {
		return given, dockwellv1.PanelDomainFrom_PANEL_DOMAIN_FROM_RUN, ""
	}
	set, err := settings.PanelDomain(ctx, db)
	if err != nil {
		logger.Warn("can't read the panel's domain from the settings, so the panel answers on loopback alone. It reads it again at the next start", slog.String("error", err.Error()))
		return "", dockwellv1.PanelDomainFrom_PANEL_DOMAIN_FROM_NONE, ""
	}
	if set == "" {
		return "", dockwellv1.PanelDomainFrom_PANEL_DOMAIN_FROM_NONE, ""
	}
	var refusal string
	if err := db.View(ctx, func(tx *store.Tx) error {
		var err error
		refusal, err = settings.Takeable(ctx, tx, set)
		return err
	}); err != nil {
		refusal = err.Error()
	}
	if refusal != "" {
		logger.Warn("the panel can't take the domain its settings hold, so it answers on loopback alone. Stop using it under Settings, General, or run dockwell panel-domain --clear",
			slog.String("domain", set), slog.String("why", refusal))
		return "", dockwellv1.PanelDomainFrom_PANEL_DOMAIN_FROM_SETTING, refusal
	}
	return set, dockwellv1.PanelDomainFrom_PANEL_DOMAIN_FROM_SETTING, ""
}

// keepObtaining gets the domain's certificate as the control plane starts, and
// tries again with a growing wait, as DNS may come after the domain was set,
// until it has one or the control plane stops.
func keepObtaining(ctx context.Context, logger *slog.Logger, acme *https.ACME) {
	wait := 30 * time.Second
	for {
		err := acme.Obtain(ctx)
		if err == nil || ctx.Err() != nil {
			return
		}
		logger.Warn("can't get the panel's certificate yet, so its domain doesn't answer over HTTPS; Dockwell tries again", slog.String("domain", acme.Domain()),
			slog.Duration("again_in", wait), slog.String("error", err.Error()))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, 10*time.Minute)
	}
}

// resolverFor looks names up as the control plane does: the system's resolver,
// and in a test only the stand-ins DOCKWELL_TEST_DNS names.
func resolverFor(getenv func(string) string) func(context.Context, string) ([]netip.Addr, error) {
	if !isSet(getenv(envTest)) {
		return func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	names := map[string][]netip.Addr{}
	for _, pair := range strings.Split(getenv(envTestDNS), ",") {
		name, addr, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if a, err := netip.ParseAddr(addr); ok && err == nil {
			names[name] = append(names[name], a)
		}
	}
	return func(_ context.Context, host string) ([]netip.Addr, error) {
		if a, ok := names[host]; ok {
			return a, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
}

// portsHeld says what holds port 80 or 443 on this server other than Dockwell
// itself or its Caddy, read from /proc: the listening sockets, then the process
// that holds each. A test reads none.
func portsHeld(getenv func(string) string) func(context.Context) []*dockwellv1.PanelPortHolder {
	if isSet(getenv(envTest)) {
		return nil
	}
	return func(context.Context) []*dockwellv1.PanelPortHolder {
		return holders(os.DirFS("/proc"), os.Getpid())
	}
}

// holders reads proc for what listens on TCP port 80 or 443, by its process's
// name, leaving out self and Caddy.
func holders(proc fs.FS, self int) []*dockwellv1.PanelPortHolder {
	inodes := map[string]uint32{}
	for _, name := range []string{"net/tcp", "net/tcp6"} {
		b, err := fs.ReadFile(proc, name)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Scan() // the header
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) < 10 || f[3] != "0A" {
				continue
			}
			_, hexPort, _ := strings.Cut(f[1], ":")
			port, err := strconv.ParseUint(hexPort, 16, 16)
			if err != nil || port != 80 && port != 443 {
				continue
			}
			inodes["socket:["+f[9]+"]"] = uint32(port)
		}
	}
	if len(inodes) == 0 {
		return nil
	}
	var out []*dockwellv1.PanelPortHolder
	pids, _ := fs.ReadDir(proc, ".")
	for _, p := range pids {
		pid, err := strconv.Atoi(p.Name())
		if err != nil || pid == self {
			continue
		}
		fds, err := fs.ReadDir(proc, path.Join(p.Name(), "fd"))
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := fs.ReadLink(proc, path.Join(p.Name(), "fd", fd.Name()))
			port, ok := inodes[target]
			if err != nil || !ok {
				continue
			}
			comm, _ := fs.ReadFile(proc, path.Join(p.Name(), "comm"))
			program := strings.TrimSpace(string(comm))
			if program == "caddy" || slices.ContainsFunc(out, func(h *dockwellv1.PanelPortHolder) bool { return h.GetPort() == port && h.GetProgram() == program }) {
				continue
			}
			out = append(out, &dockwellv1.PanelPortHolder{Port: port, Program: program})
		}
	}
	slices.SortFunc(out, func(a, b *dockwellv1.PanelPortHolder) int { return int(a.GetPort()) - int(b.GetPort()) })
	return out
}

// reexec starts dockwell again in this process, with the arguments and
// environment it was started with, as a person asked: under systemd, OpenRC or a
// shell alike, as its PID stays the same.
func reexec() error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("can't find dockwell's own binary to start it again: %w. Start it again by hand", err)
	}
	// A binary replaced since it started reads as deleted; its path holds the new one.
	self = strings.TrimSuffix(self, " (deleted)")
	return syscall.Exec(self, os.Args, os.Environ()) //nolint:gosec // dockwell's own binary, with what it was started with
}
