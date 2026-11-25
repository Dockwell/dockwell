// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/hosttool"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
)

const (
	toolboxFileLimit = 1 << 20
	toolboxHostsMark = " # Dockwell toolbox"
)

var (
	toolboxHostname = toolboxTool("hostnamectl")
	toolboxTime     = toolboxTool("timedatectl")
	toolboxSystem   = toolboxTool("systemctl")
	toolboxNetplan  = toolboxTool("netplan")
	toolboxLabel    = toolboxPattern(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)
)

type hostToolbox struct {
	tools    *hosttool.Runner
	dir      string
	probe    func(context.Context) error
	mu       sync.Mutex
	recovery atomic.Pointer[string]
}

type toolboxEdit struct {
	plan   *v1.HostToolboxPlan
	files  []toolboxWrite
	extra  []byte
	apply  func(context.Context) error
	reload func(context.Context) error
}

type toolboxWrite struct {
	path       string
	old, next  []byte
	mode       os.FileMode
	stamp      string
	wasMissing bool
	remove     bool
}

// Like the other agent readers, compile patterns on first use so they cost
// nothing on a server whose Toolbox nobody opens.
func toolboxPattern(pattern string) func() *regexp.Regexp {
	return sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(pattern) })
}

func toolboxTool(name string) hosttool.Tool {
	return hosttool.Tool{Name: name, Paths: []string{"/usr/bin/" + name, "/usr/sbin/" + name, "/bin/" + name, "/sbin/" + name}}
}

func (t *hostToolbox) path(name string) string { return filepath.Join(t.tools.Root, name) }

func (t *hostToolbox) root() string {
	if t.tools.Root != "" {
		return t.tools.Root
	}
	return "/"
}

func (t *hostToolbox) run(ctx context.Context, tool hosttool.Tool, args ...string) ([]byte, error) {
	result, err := t.tools.Run(ctx, tool, 30*time.Second, nil, args...)
	if err == nil && result.Code != 0 {
		err = hosttool.Fail(tool.Name, result)
	}
	return result.Out, err
}

func (t *hostToolbox) systemd() bool {
	if _, found := t.tools.Find(toolboxSystem); !found {
		return false
	}
	st, err := os.Stat(t.path("/run/systemd/system"))
	return err == nil && st.IsDir()
}

func (t *hostToolbox) read(name string) ([]byte, error) {
	root, err := os.OpenRoot(t.root())
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(strings.TrimPrefix(name, "/"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > toolboxFileLimit {
		return nil, fmt.Errorf("%s isn't a small regular file", name)
	}
	raw, err := io.ReadAll(io.LimitReader(f, toolboxFileLimit+1))
	if err == nil && len(raw) > toolboxFileLimit {
		err = fmt.Errorf("%s is too large", name)
	}
	return raw, err
}

// Verify every parent of files this feature writes. Root-owned config
// directories may have other writers running as root, but no unprivileged writer.
func (t *hostToolbox) trustedParent(name string) error {
	dir := filepath.Dir(name)
	for dir != "/" && dir != "." {
		info, err := os.Lstat(t.path(dir))
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode().Perm()&0o022 != 0 || !t.owns(info) {
			return fmt.Errorf("%s isn't a trusted host configuration directory", dir)
		}
		dir = filepath.Dir(dir)
	}
	return nil
}

func (t *hostToolbox) file(name string, mode os.FileMode) (toolboxWrite, error) {
	w := toolboxWrite{path: name, mode: mode}
	if err := t.trustedParent(name); err != nil {
		return w, err
	}
	info, err := os.Lstat(t.path(name))
	if errors.Is(err, os.ErrNotExist) {
		w.wasMissing = true
		return w, nil
	}
	if err != nil {
		return w, err
	}
	if !toolboxRegular(info) || info.Mode().Perm()&0o022 != 0 || !t.owns(info) {
		return w, fmt.Errorf("%s isn't a trusted regular file", name)
	}
	if !toolboxSameMount(t.path(name), t.path(filepath.Dir(name))) {
		return w, fmt.Errorf("%s is mounted separately and can't be replaced safely", name)
	}
	w.stamp = toolboxCandidateInfo(info)
	w.mode = info.Mode().Perm()
	w.old, err = t.read(name)
	return w, err
}

func toolboxAtomic(name string, body []byte, mode os.FileMode) error {
	dir := filepath.Dir(name)
	temp, err := os.CreateTemp(dir, ".dockwell-toolbox-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if err = temp.Chmod(mode); err == nil {
		_, err = temp.Write(body)
	}
	if err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(temp.Name(), name); err != nil {
		return err
	}
	d, err := os.Open(dir) //nolint:gosec // callers supply a checked host path or a private agent data directory
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (t *hostToolbox) write(w toolboxWrite) error {
	if w.remove {
		return os.Remove(t.path(w.path))
	}
	return toolboxAtomic(t.path(w.path), w.next, w.mode)
}

func (t *hostToolbox) undo(w toolboxWrite) error {
	if w.wasMissing {
		err := os.Remove(t.path(w.path))
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return toolboxAtomic(t.path(w.path), w.old, w.mode)
}

func toolboxValidHostname(name string) bool {
	if len(name) > 253 || name == "" {
		return false
	}
	for label := range strings.SplitSeq(name, ".") {
		if !toolboxLabel().MatchString(label) {
			return false
		}
	}
	return true
}

func toolboxDifference(p *v1.HostToolboxPlan, subject, before, after string) {
	p.Differences = append(p.Differences, &v1.ToolboxDifference{Subject: subject, Before: before, After: after})
}

func (t *hostToolbox) plan(ctx context.Context, ask *v1.HostToolboxPlan) (toolboxEdit, error) {
	unlock, err := t.lockToolboxAPT(ask)
	if err != nil {
		return toolboxEdit{}, err
	}
	defer unlock()
	return t.planLocked(ctx, ask)
}

func (t *hostToolbox) lockToolboxAPT(ask *v1.HostToolboxPlan) (func(), error) {
	if ask.GetOperation().GetCleanup().GetKind() == v1.ToolboxCleanupKind_TOOLBOX_CLEANUP_KIND_APT {
		return t.lockAPTCleanup()
	}
	return func() {}, nil
}

func (t *hostToolbox) planLocked(ctx context.Context, ask *v1.HostToolboxPlan) (toolboxEdit, error) {
	if err := t.recoveryError(); err != "" {
		return toolboxEdit{}, fmt.Errorf("couldn't restore the previous host settings: %s", err)
	}
	p := &v1.HostToolboxPlan{Operation: ask.GetOperation(), ChangeId: ask.GetChangeId(), Keep: resource.ToolboxNeedsKeep(ask.GetOperation())}
	edit := toolboxEdit{plan: p}
	var err error
	switch op := p.GetOperation().GetOperation().(type) {
	case *v1.HostToolboxOperation_Hostname:
		err = t.planHostname(ctx, &edit, op.Hostname)
	case *v1.HostToolboxOperation_TimeZone:
		err = t.planTimezone(ctx, &edit, op.TimeZone)
	case *v1.HostToolboxOperation_Ntp:
		err = t.planNTP(ctx, &edit, op.Ntp)
	case *v1.HostToolboxOperation_Dns:
		err = t.planDNS(ctx, &edit, op.Dns.GetServers())
	case *v1.HostToolboxOperation_Hosts:
		err = t.planHosts(&edit, op.Hosts)
	case *v1.HostToolboxOperation_Swappiness:
		err = t.planSwappiness(&edit, op.Swappiness)
	case *v1.HostToolboxOperation_Swap:
		err = t.planSwap(&edit, op.Swap.GetSizeMib())
	case *v1.HostToolboxOperation_Cleanup:
		err = t.planCleanup(ctx, &edit, op.Cleanup)
	case *v1.HostToolboxOperation_Service:
		err = t.planService(ctx, &edit, op.Service)
	default:
		err = errors.New("choose a host toolbox operation")
	}
	if err != nil {
		return edit, err
	}
	if p.Keep && !t.canGuard() {
		return edit, errors.New("this server can't schedule a host settings rollback. Check systemd-run or the host's rollback tools")
	}
	digest := sha256.New()
	data, err := (proto.MarshalOptions{Deterministic: true}).Marshal(p)
	if err != nil {
		return edit, err
	}
	_, _ = digest.Write(data)
	for _, w := range edit.files {
		fmt.Fprintf(digest, "%q %t %t %o %s\n", w.path, w.wasMissing, w.remove, w.mode, w.stamp)
		if !strings.HasPrefix(w.path, toolboxEnvDir+"/") {
			fmt.Fprintf(digest, "%x %x\n", sha256.Sum256(w.old), sha256.Sum256(w.next))
		}
	}
	_, _ = digest.Write(edit.extra)
	p.Evidence = hex.EncodeToString(digest.Sum(nil))
	return edit, nil
}

func (c *calls) planHostToolbox(ctx context.Context, asked *agentv1.PlanCall) (*agentv1.CallResult, bool) {
	all := asked.GetDesired()
	index := slices.IndexFunc(all, func(d *agentv1.DesiredResource) bool { return d.GetHostToolbox() != nil })
	if index < 0 {
		return nil, false
	}
	d := all[index]
	ref := resourcepb.FromRef(d.GetResource())
	t := c.cfg.toolbox
	if t == nil || len(all) != 1 || ref != (resource.Ref{Kind: "server.host", Scope: c.server, Name: "host"}) {
		return callFailed(errors.New("the host toolbox is unavailable for this resource")), true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	edit, err := t.plan(ctx, d.GetHostToolbox())
	if err != nil {
		return callFailed(err), true
	}
	p := edit.plan
	step := resource.Step{Ref: ref, Decision: resource.Decision{Toolbox: (*resource.ToolboxPlan)(p), Action: resource.ActionUpdate}}
	if p.GetKeep() {
		step.Warnings = append(step.Warnings, resource.WarningMayLockOut)
	}
	step.Destroys = p.GetOperation().GetCleanup() != nil
	return &agentv1.CallResult{Result: &agentv1.CallResult_Plan{Plan: &agentv1.PlanResult{
		Steps: []*v1.PlanStep{resourcepb.Step(step, "")},
	}}}, true
}

func (t *hostToolbox) status(ctx context.Context) *v1.HostToolboxStatus {
	if err := t.recoveryError(); err != "" {
		return &v1.HostToolboxStatus{RecoveryError: err}
	}
	s := &v1.HostToolboxStatus{Hostname: t.hostname(ctx), TimeZone: t.timezone(ctx), Systemd: t.systemd()}
	s.NtpAvailable, s.NtpEnabled, s.NtpSynced = t.ntp(ctx)
	if raw, err := t.read("/proc/sys/vm/swappiness"); err == nil {
		n, _ := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 32)
		s.Swappiness = uint32(n)
	}
	s.ActiveSwap, s.ManagedSwapMib, s.RecommendedSwapMib, s.SwapFreeMib, s.SwapUnavailable = t.swaps()
	owner := t.dnsOwner(ctx)
	s.DnsOwner, s.DnsServers, s.LinkDnsServers, s.CanEditDns = owner.name, owner.servers, owner.links, owner.target != "" && t.canGuard()
	if raw, err := t.read("/etc/hosts"); err == nil {
		for line := range strings.SplitSeq(string(raw), "\n") {
			if name, addr := toolboxOwnHost(line); name != "" {
				s.Hosts = append(s.Hosts, &v1.ToolboxHosts{Hostname: name, Address: addr})
			}
		}
	}
	s.Services = t.serviceNames()
	s.TimeZones = t.timezones(ctx)
	return s
}

func toolboxIPs(values []string) ([]string, error) {
	if len(values) == 0 || len(values) > 3 {
		return nil, errors.New("enter between one and three DNS server addresses")
	}
	seen := make(map[netip.Addr]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		ip, err := netip.ParseAddr(value)
		if err != nil || ip.Zone() != "" || ip.IsUnspecified() || ip.IsMulticast() || seen[ip] {
			return nil, errors.New("enter distinct IP addresses for the DNS servers")
		}
		seen[ip] = true
		out = append(out, ip.String())
	}
	return out, nil
}
