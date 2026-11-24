// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/access"
	"github.com/dockwell/dockwell/internal/agent/jails"
)

const sshJailPath = jails.SSHPortFile

func (s *sshSettings) planSSHJail(ctx context.Context, old, next uint16) (*access.SSHJail, error) {
	r := s.reader
	if _, ok := r.tools.Find(fail2banClient); !ok {
		return nil, nil
	}
	configs := []string{"/etc/fail2ban/jail.conf"}
	for _, pat := range []string{"etc/fail2ban/jail.d/*.conf", "etc/fail2ban/jail.d/*.local"} {
		files, err := fs.Glob(r.root, pat)
		if err != nil {
			return nil, err
		}
		if strings.HasSuffix(pat, ".local") {
			configs = append(configs, "/etc/fail2ban/jail.local")
		}
		for _, file := range files {
			configs = append(configs, "/"+file)
		}
	}
	var before string
	var ports []uint16
	var found bool
	for _, file := range configs {
		data, err := readSSHFile(r.rootPath(file), maxSSHKeysFile)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("can't check %s for the SSH jail: %w", file, err)
		}
		values, exists := sshJailValues(string(data))
		found = found || exists
		managed := file == jails.File && jails.IsDockwells(string(data))
		override := file == sshJailPath && string(data) == sshJailBody(sshJailPorts(values["port"]))
		switch {
		case override:
			before = string(data)
		case file == sshJailPath:
			return nil, fmt.Errorf("%s wasn't written by Dockwell. Move it aside before changing the SSH port", file)
		case !managed && (values["action"] != "" || values["banaction"] != "" ||
			(values["port"] != "" && (file != configs[0] || values["port"] != "ssh"))):
			return nil, fmt.Errorf("%s pins the fail2ban sshd jail's ports or actions. Move SSH and this jail together on the server", file)
		}
		if value := values["port"]; value != "" {
			ports = sshJailPorts(value)
			if len(ports) == 0 {
				return nil, fmt.Errorf("%s uses an unsupported SSH jail port list. Use explicit ports before moving SSH", file)
			}
		}
	}
	if !found {
		return nil, nil
	}
	if len(ports) == 0 {
		ports = []uint16{old}
	}
	final := slices.DeleteFunc(slices.Clone(ports), func(p uint16) bool { return p == old })
	if !slices.Contains(final, next) {
		final = append(final, next)
	}
	candidate := slices.Clone(final)
	candidate = append(candidate, old)
	f := &fail2banReader{tools: r.tools}
	_, runningErr := f.client(ctx, "status", "sshd")
	return &access.SSHJail{Old: before, Candidate: sshJailBody(candidate), Final: sshJailBody(final), Active: runningErr == nil}, nil
}

func sshJailValues(text string) (map[string]string, bool) {
	values := map[string]string{}
	section := ""
	var exists bool
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			name, _, _ := strings.Cut(line, "]")
			section = strings.TrimPrefix(name, "[")
			exists = exists || section == "sshd"
		}
		if section != "sshd" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			key, value, ok = strings.Cut(line, ":")
		}
		if ok {
			values[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(strings.SplitN(value, "#", 2)[0])
		}
	}
	return values, exists
}

func sshJailPorts(value string) []uint16 {
	var ports []uint16
	for field := range strings.SplitSeq(value, ",") {
		field = strings.TrimSpace(field)
		if field == "ssh" {
			field = "22"
		}
		n, err := strconv.ParseUint(field, 10, 16)
		if err != nil || n == 0 {
			return nil
		}
		if !slices.Contains(ports, uint16(n)) {
			ports = append(ports, uint16(n))
		}
	}
	return ports
}

func sshJailBody(ports []uint16) string {
	values := make([]string, len(ports))
	for i, port := range ports {
		values[i] = strconv.Itoa(int(port))
	}
	return jails.SSHPortHeader + strings.Join(values, ",") + "\n"
}

func (s *sshSettings) installSSHJail(ctx context.Context, trial *access.SSHChange, body string) error {
	j := trial.Jail
	if j == nil {
		return nil
	}
	parent := s.reader.rootPath(path.Dir(sshJailPath))
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || !sshOwnership(info, s.reader.tools.Root) || info.Mode().Perm()&0o022 != 0 {
		return errors.New("fail2ban's jail directory needs to be a real directory writable only by its owner")
	}
	if err := writeSSHOwnedFile(s.reader.rootPath(sshJailPath), body); err != nil {
		return err
	}
	f := &fail2banReader{tools: s.reader.tools}
	if _, err := f.client(ctx, "-t"); err != nil {
		return fmt.Errorf("fail2ban refused the SSH jail on the new port: %w", err)
	}
	if _, err := f.client(ctx, "ping"); err != nil && !j.Active {
		return nil
	}
	// A plain reload can keep an existing action's input chain with its
	// original destination match. Restart this jail's actions so they rebuild
	// it. Fail2ban's persistent database restores its unexpired bans.
	if _, err := f.client(ctx, "reload", "--restart", "sshd"); err != nil {
		if !j.Active {
			if status, e := f.client(ctx, "status"); e == nil && !slices.Contains(jailList(status), "sshd") {
				return nil
			}
		}
		return fmt.Errorf("can't reload the fail2ban sshd jail: %w", err)
	}
	ports := []uint16{trial.Port}
	if body == j.Old {
		return nil
	}
	if body == j.Candidate {
		ports = append(ports, trial.OldPort)
	}
	return f.sshJailCovers(ctx, ports)
}

func (f *fail2banReader) sshJailCovers(ctx context.Context, ports []uint16) error {
	if len(ports) == 0 {
		return errors.New("can't verify which ports the fail2ban sshd jail must protect")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := f.client(ctx, "get", "sshd", "actions")
	if err != nil {
		return errors.New("the fail2ban sshd jail is unavailable. Check fail2ban on this server")
	}
	_, names, _ := strings.Cut(out, "\n")
	for name := range strings.SplitSeq(strings.TrimSpace(names), ",") {
		name = strings.TrimSpace(name)
		firewallAction := strings.HasPrefix(name, "iptables") || strings.HasPrefix(name, "nftables") || strings.HasPrefix(name, "firewallcmd")
		if !isJailArg(name) || !firewallAction {
			continue
		}
		protocol, err := f.client(ctx, "get", "sshd", "action", name, "protocol")
		if err != nil || !slices.Contains(strings.Split(strings.ReplaceAll(strings.TrimSpace(protocol), " ", ""), ","), "tcp") {
			continue
		}
		kind, _ := f.client(ctx, "get", "sshd", "action", name, "type")
		if strings.HasSuffix(name, "-allports") || strings.TrimSpace(kind) == "allports" {
			return nil
		}
		value, err := f.client(ctx, "get", "sshd", "action", name, "port")
		if err != nil {
			continue
		}
		allowed := sshJailPorts(strings.TrimSpace(value))
		if !slices.ContainsFunc(ports, func(p uint16) bool { return !slices.Contains(allowed, p) }) {
			return nil
		}
	}
	return errors.New("the fail2ban sshd jail's actions don't show coverage for the current SSH ports. Check the jail's configured ports")
}

func (r *sshReader) sshJailCanBan(ctx context.Context, f *fail2banReader) error {
	pins, err := r.sshPins().Read(ctx)
	if err != nil {
		return err
	}
	if pins.SSHByConfig {
		return errors.New("can't verify the live SSH listener ports against fail2ban")
	}
	return f.sshJailCovers(ctx, pins.SSHPorts)
}

// A port move can carry Dockwell's rules only. A foreign port-specific or
// opaque input rule may admit less traffic on the new listener.
func (r *sshReader) checkForeignSSHPort(ctx context.Context, from, to uint16, jail *access.SSHJail) error {
	f := &firewallReader{tools: r.tools}
	ufw := f.installed("ufw")
	movingJail := jail != nil && jail.Active
	kernel := map[*dockwellv1.FirewallTool]string{}
	tools, _ := f.kernelRules(ctx, ufw, func(tool *dockwellv1.FirewallTool, rules *ruleSet) {
		kernel[tool] = rules.sshPortConflict(from, to, movingJail)
	})
	if ufw {
		tools = append(tools, f.ufw(ctx))
	}
	if f.installed("firewall-cmd") {
		tools = append(tools, f.firewalld(ctx)...)
	}
	for _, tool := range tools {
		if tool.GetUnreadable() != "" {
			return fmt.Errorf("can't verify the SSH port change against %s. Check its input rules on the server", toolName(tool))
		}
		if !tool.GetActive() {
			continue
		}
		// The fail2ban actions are reloaded and checked with the trial.
		if jail != nil && jail.Active && strings.HasSuffix(tool.GetName(), " f2b-table") {
			continue
		}
		detail, inspected := kernel[tool]
		if !inspected {
			detail = foreignSSHPortConflict(tool, from, to)
		}
		if detail != "" {
			return fmt.Errorf("%s rule %q may change who can reach SSH on TCP port %d. Update the server's firewall before moving SSH", toolName(tool), detail, to)
		}
	}
	return nil
}

func foreignSSHPortConflict(tool *dockwellv1.FirewallTool, from, to uint16) string {
	for _, rule := range tool.GetOther() {
		return rule
	}
	var oldRules, nextRules []*dockwellv1.FirewallSeenRule
	for _, rule := range tool.GetRules() {
		spec := rule.GetPort()
		if known, ok := wellKnownServices[spec]; ok {
			spec = known
		}
		old, oldKnown := coversSpec(spec, &dockwellv1.HostListener{Port: uint32(from), Protocol: "tcp"}, ":-")
		next, nextKnown := coversSpec(spec, &dockwellv1.HostListener{Port: uint32(to), Protocol: "tcp"}, ":-")
		if !oldKnown || !nextKnown {
			return seenSSHRule(rule)
		}
		if old {
			oldRules = append(oldRules, rule)
		}
		if next {
			nextRules = append(nextRules, rule)
		}
	}
	for i := range min(len(oldRules), len(nextRules)) {
		a, b := oldRules[i], nextRules[i]
		if a.GetVerdict() != b.GetVerdict() || a.GetSource() != b.GetSource() || a.GetIpv6() != b.GetIpv6() ||
			a.GetInterface() != b.GetInterface() || a.GetDestination() != b.GetDestination() {
			return seenSSHRule(a)
		}
	}
	if len(oldRules) > len(nextRules) {
		return seenSSHRule(oldRules[len(nextRules)])
	}
	if len(nextRules) > len(oldRules) {
		return seenSSHRule(nextRules[len(oldRules)])
	}
	return ""
}
