// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const (
	toolboxResolvedDropin = "/etc/systemd/resolved.conf.d/99-dockwell-toolbox.conf"
	toolboxNetplanDropin  = "/etc/netplan/99-dockwell-toolbox.yaml"
)

type toolboxDNSOwner struct {
	name, target, iface string
	servers, links      []string
	dhcpConfig          string
	dhcp4, dhcp6        bool
}

func toolboxNameservers(raw string) []string {
	var ips []string
	for line := range strings.SplitSeq(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "nameserver" {
			if ip, err := netip.ParseAddr(fields[1]); err == nil {
				ips = append(ips, ip.String())
			}
		}
	}
	return ips
}

var toolboxInterface = toolboxPattern(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,14}$`)

func (t *hostToolbox) defaultInterface() string {
	raw, _ := t.read("/proc/net/route")
	var name string
	best := uint64(1 << 32)
	for line := range strings.SplitSeq(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) < 8 || f[1] != "00000000" || f[7] != "00000000" || !toolboxInterface().MatchString(f[0]) {
			continue
		}
		flags, _ := strconv.ParseUint(f[3], 16, 32)
		metric, err := strconv.ParseUint(f[6], 10, 32)
		if flags&1 == 0 || err != nil {
			continue
		}
		if metric < best {
			name, best = f[0], metric
		} else if metric == best && name != f[0] {
			name = ""
		}
	}
	return name
}

func (t *hostToolbox) dnsOwner(ctx context.Context) toolboxDNSOwner {
	raw, _ := t.read("/etc/resolv.conf")
	owner := toolboxDNSOwner{name: "/etc/resolv.conf", servers: toolboxNameservers(string(raw))}
	path, err := os.Readlink(t.path("/etc/resolv.conf"))
	if err == nil && !filepath.IsAbs(path) {
		path = filepath.Join(string(filepath.Separator), "etc", path)
	}
	if len(raw) == 0 && strings.HasPrefix(path, "/run/") {
		raw, _ = t.read(path)
		owner.servers = toolboxNameservers(string(raw))
	}
	if strings.HasPrefix(path, "/run/systemd/resolve/") {
		owner.name, owner.target = "systemd-resolved", toolboxResolvedDropin
		owner.servers = nil
		if listed, err := t.run(ctx, toolboxTool("resolvectl"), "dns"); err == nil {
			for line := range strings.SplitSeq(string(listed), "\n") {
				scope, rest, _ := strings.Cut(line, ":")
				servers := &owner.links
				if scope == "Global" {
					servers = &owner.servers
				}
				for _, f := range strings.Fields(rest) {
					if ip, err := netip.ParseAddr(f); err == nil && !slices.Contains(*servers, ip.String()) {
						*servers = append(*servers, ip.String())
					}
				}
			}
		}
		netplan, _ := filepath.Glob(t.path("/etc/netplan/*.yaml"))
		if len(netplan) != 0 {
			owner.name, owner.target = "netplan", ""
			iface := t.defaultInterface()
			if iface != "" {
				for _, kind := range []string{"ethernets", "bonds", "bridges", "vlans", "wifis"} {
					raw, err := t.run(ctx, toolboxNetplan, "get", kind+"."+iface)
					config := strings.TrimSpace(string(raw))
					if err != nil || config == "" || config == "null" || config == "{}" {
						continue
					}
					// An overlay keyed by an exact Netplan ID preserves the rest of the link's setup.
					owner.target, owner.iface = toolboxNetplanDropin, kind+"."+iface
					for line := range strings.SplitSeq(config, "\n") {
						switch strings.TrimSpace(line) {
						case "dhcp4: true":
							owner.dhcp4 = true
						case "dhcp6: true":
							owner.dhcp6 = true
						}
					}
					break
				}
			}
		}
		return owner
	}
	lower := strings.ToLower(string(raw))
	switch {
	case strings.Contains(path, "NetworkManager"), strings.Contains(lower, "networkmanager"):
		owner.name = "NetworkManager"
	case strings.Contains(path, "resolvconf"), strings.Contains(lower, "resolvconf"):
		owner.name = "resolvconf"
	case strings.Contains(lower, "dhcpcd"), strings.Contains(lower, "dhclient"):
		owner = t.dhcpDNSOwner(owner, lower)
	case path != "", strings.Contains(lower, "generated"), strings.Contains(lower, "do not edit"),
		strings.Contains(lower, "docker"), strings.Contains(lower, "netplan"):
		owner.name = "Managed resolver"
	default:
		owner.target = "/etc/resolv.conf"
		owner = t.dhcpDNSOwner(owner, lower)
	}
	return owner
}

func (t *hostToolbox) dhcpDNSOwner(owner toolboxDNSOwner, header string) toolboxDNSOwner {
	hasFiles := func(patterns ...string) bool {
		for _, pattern := range patterns {
			if paths, _ := filepath.Glob(t.path(pattern)); len(paths) > 0 {
				return true
			}
		}
		return false
	}
	dhclient := strings.Contains(header, "dhclient") || hasFiles("/var/lib/dhcp/dhclient*.leases", "/run/dhclient*.pid", "/run/dhclient/*.pid")
	dhcpcd := strings.Contains(header, "dhcpcd") || hasFiles("/var/lib/dhcpcd/*.lease*", "/var/lib/dhcpcd5/*.lease*", "/var/db/dhcpcd*.lease*", "/run/dhcpcd*.pid", "/run/dhcpcd/*.pid")
	if !dhclient && !dhcpcd {
		paths, _ := filepath.Glob(t.path("/etc/network/interfaces.d/*"))
		for _, name := range append([]string{t.path("/etc/network/interfaces")}, paths...) {
			raw, _ := t.read(strings.TrimPrefix(name, t.tools.Root))
			for line := range strings.SplitSeq(string(raw), "\n") {
				fields := strings.Fields(line)
				if len(fields) >= 4 && fields[0] == "iface" && (fields[2] == "inet" || fields[2] == "inet6") && fields[3] == "dhcp" {
					owner.name, owner.target = "ifupdown (DHCP)", ""
				}
			}
		}
		if owner.name == "ifupdown (DHCP)" {
			_, dhclient = t.tools.Find(toolboxTool("dhclient"))
			_, dhcpcd = t.tools.Find(toolboxTool("dhcpcd"))
		}
	}
	switch {
	case dhclient && dhcpcd:
		owner.name, owner.target = "dhclient and dhcpcd", ""
	case dhclient:
		owner.name, owner.target, owner.dhcpConfig = "dhclient", "/etc/resolv.conf", "/etc/dhcp/dhclient.conf"
	case dhcpcd:
		owner.name, owner.target, owner.dhcpConfig = "dhcpcd", "/etc/resolv.conf", "/etc/dhcpcd.conf"
	}
	return owner
}

const toolboxDHCPMarker = " # Managed by Dockwell toolbox"

var toolboxDHClientDNS = toolboxPattern(`(?m)(?:^|[;{}])[[:space:]]*(?:supersede|prepend|append|default|option)[[:space:]]+domain-name-servers\b`)

func (t *hostToolbox) planDHCPDNS(edit *toolboxEdit, owner toolboxDNSOwner, ips []string) error {
	w, err := t.file(owner.dhcpConfig, 0o644)
	if err != nil {
		return err
	}
	directive := "static domain_name_servers=" + strings.Join(ips, " ")
	if owner.name == "dhclient" {
		for _, s := range ips {
			if ip, _ := netip.ParseAddr(s); !ip.Is4() {
				return errors.New("dhclient's persistent DNS override requires IPv4 resolver addresses")
			}
		}
		directive = "supersede domain-name-servers " + strings.Join(ips, ", ") + ";"
	}
	var out strings.Builder
	out.WriteString(directive + toolboxDHCPMarker + "\n")
	var config strings.Builder
	for line := range strings.SplitAfterSeq(string(w.old), "\n") {
		if strings.HasSuffix(strings.TrimSpace(line), strings.TrimSpace(toolboxDHCPMarker)) {
			continue
		}
		body, _, _ := strings.Cut(line, "#")
		config.WriteString(body)
		if !strings.HasSuffix(body, "\n") {
			config.WriteByte('\n')
		}
		out.WriteString(line)
	}
	if owner.name == "dhclient" && toolboxDHClientDNS().MatchString(config.String()) ||
		owner.name == "dhcpcd" && strings.Contains(config.String(), "domain_name_servers") {
		return fmt.Errorf("%s already sets DNS in %s. Remove the existing override before planning", owner.name, w.path)
	}
	w.next = []byte(out.String())
	edit.files = append(edit.files, w)
	toolboxDifference(edit.plan, owner.name+" DNS override", "", w.path)
	return nil
}

func (t *hostToolbox) planDNS(ctx context.Context, edit *toolboxEdit, addresses []string) error {
	ips, err := toolboxIPs(addresses)
	if err != nil {
		return err
	}
	owner := t.dnsOwner(ctx)
	if owner.target == "" {
		return fmt.Errorf("%s manages this server's resolver. Dockwell can't safely edit its settings here", owner.name)
	}
	if owner.target == toolboxResolvedDropin {
		// The directory is standard but may not exist on a fresh install.
		if err := t.trustedParent("/etc/systemd/resolved.conf"); err != nil {
			return err
		}
	}
	w, err := t.optionalConfig(owner.target)
	if err != nil {
		return err
	}
	if owner.target != "/etc/resolv.conf" && !w.wasMissing && !strings.HasPrefix(string(w.old), "# Managed by Dockwell toolbox\n") {
		return fmt.Errorf("%s is already in use. Move the existing configuration before planning", owner.target)
	}
	switch owner.target {
	case toolboxResolvedDropin:
		links := strings.Join(owner.links, ", ")
		if links == "" {
			links = "Configured by each link"
		}
		toolboxDifference(edit.plan, "Link DNS servers stay in use", links, links)
		w.next = fmt.Appendf(nil, "# Managed by Dockwell toolbox\n[Resolve]\nDNS=\nDNS=%s\nDomains=~.\n", strings.Join(ips, " "))
		edit.reload = func(ctx context.Context) error {
			_, err := t.run(ctx, toolboxSystem, "restart", "systemd-resolved.service")
			return err
		}
	case toolboxNetplanDropin:
		// Netplan merges sequences across files. Its !override tag isn't available,
		// so refuse a static list owned by any other file instead of stacking DNS.
		kind, iface, _ := strings.Cut(owner.iface, ".")
		config, err := t.run(ctx, toolboxNetplan, "get", owner.iface+".nameservers.addresses")
		if err == nil && len(strings.Fields(string(config))) > 0 && strings.TrimSpace(string(config)) != "[]" &&
			strings.TrimSpace(string(config)) != "null" && w.wasMissing {
			return errors.New("netplan already defines static DNS servers for this interface. Dockwell can't replace the list safely")
		}
		w.next = fmt.Appendf(nil, "# Managed by Dockwell toolbox\nnetwork:\n  version: 2\n  %s:\n    %s:\n      nameservers:\n        addresses: [%s]\n",
			kind, iface, strings.Join(ips, ", "))
		if owner.dhcp4 {
			w.next = append(w.next, "      dhcp4-overrides:\n        use-dns: false\n"...)
		}
		if owner.dhcp6 {
			w.next = append(w.next, "      dhcp6-overrides:\n        use-dns: false\n"...)
		}
		edit.reload = func(ctx context.Context) error {
			if _, err := t.run(ctx, toolboxNetplan, "generate"); err != nil {
				return err
			}
			_, err := t.run(ctx, toolboxNetplan, "apply")
			return err
		}
	default:
		if owner.dhcpConfig != "" {
			if err := t.planDHCPDNS(edit, owner, ips); err != nil {
				return err
			}
		}
		var body strings.Builder
		for _, ip := range ips {
			fmt.Fprintf(&body, "nameserver %s\n", ip)
		}
		for line := range strings.SplitAfterSeq(string(w.old), "\n") {
			if f := strings.Fields(line); len(f) == 0 || f[0] != "nameserver" {
				body.WriteString(line)
			}
		}
		w.next = []byte(body.String())
	}
	edit.files = append(edit.files, w)
	label, current := owner.name+" DNS", owner.servers
	switch owner.target {
	case toolboxResolvedDropin:
		label = "Global DNS"
	case toolboxNetplanDropin:
		label, current = "netplan DNS on "+owner.iface, owner.links
	}
	toolboxDifference(edit.plan, label, strings.Join(current, ", "), strings.Join(ips, ", "))
	toolboxDifference(edit.plan, "Resolver configuration", "", owner.target)
	return nil
}

func (t *hostToolbox) optionalConfig(path string) (toolboxWrite, error) {
	if path == toolboxResolvedDropin || path == toolboxCloudConfig {
		if _, err := os.Lstat(t.path(filepath.Dir(path))); errors.Is(err, os.ErrNotExist) {
			return toolboxWrite{path: path, mode: 0o644, wasMissing: true}, t.trustedParent(filepath.Dir(path))
		}
	}
	mode := os.FileMode(0o644)
	if path == toolboxNetplanDropin {
		mode = 0o600
	}
	return t.file(path, mode)
}
