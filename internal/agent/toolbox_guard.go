// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
)

const (
	toolboxKeepTime      = 90 * time.Second
	toolboxNetplanSettle = 6 * time.Second
)

var toolboxChangeID = toolboxPattern(`^chg_[a-zA-Z0-9]{8,64}$`)

func (t *hostToolbox) probeControl(id *enroll.Identity) func(context.Context) error {
	return func(ctx context.Context) error {
		conf, err := id.ClientTLS()
		if err != nil {
			return err
		}
		return t.probeControlURL(ctx, id.ControlURL, conf)
	}
}

func (t *hostToolbox) probeControlURL(ctx context.Context, controlURL string, conf *tls.Config) error {
	probe, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	transport := enroll.ControlTransport(conf)
	transport.DialContext = t.dialFreshControl
	defer transport.CloseIdleConnections()
	var connected atomic.Bool
	// GotConn follows proxy negotiation (if any) and the control TLS handshake.
	// Only a new connection is needed, so stop before waiting for an HTTP reply.
	probe = httptrace.WithClientTrace(probe, &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) {
		connected.Store(true)
		cancel()
	}})
	req, err := http.NewRequestWithContext(probe, http.MethodHead, controlURL, http.NoBody)
	if err != nil {
		return err
	}
	res, err := transport.RoundTrip(req)
	if res != nil {
		_ = res.Body.Close()
	}
	if connected.Load() {
		return nil
	}
	return err
}

// Resolve the transport's first hop in a new process, bypassing Go's hosts
// and resolver caches. The transport still verifies the original TLS name.
func (t *hostToolbox) dialFreshControl(ctx context.Context, network, address string) (net.Conn, error) {
	name, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addresses, err := t.freshControlAddresses(ctx, name)
	if err != nil {
		return nil, err
	}
	dial := net.Dialer{Timeout: 2 * time.Second}
	var failure error
	for _, ip := range addresses {
		conn, err := dial.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		failure = errors.Join(failure, err)
	}
	return nil, failure
}

func (t *hostToolbox) freshControlAddresses(ctx context.Context, name string) ([]netip.Addr, error) {
	if ip, err := netip.ParseAddr(name); err == nil {
		return []netip.Addr{ip}, nil
	}
	if _, err := os.Stat(t.path("/run/systemd/resolve")); err == nil {
		tool, arg := toolboxTool("resolvectl"), "flush-caches"
		if _, ok := t.tools.Find(tool); !ok {
			tool, arg = toolboxTool("systemd-resolve"), "--flush-caches"
		}
		if _, err := t.run(ctx, tool, arg); err != nil {
			return nil, err
		}
	}
	var addresses []netip.Addr
	hosts, err := t.read("/etc/hosts")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for line := range strings.SplitSeq(string(hosts), "\n") {
		base, _, _ := strings.Cut(line, "#")
		fields := strings.Fields(base)
		if len(fields) < 2 {
			continue
		}
		for _, alias := range fields[1:] {
			if strings.EqualFold(strings.TrimSuffix(alias, "."), strings.TrimSuffix(name, ".")) {
				if ip, err := netip.ParseAddr(fields[0]); err == nil {
					addresses = append(addresses, ip)
				}
				break
			}
		}
	}
	if len(addresses) > 0 {
		return addresses, nil
	}
	raw, err := t.run(ctx, toolboxTool("getent"), "--service=dns", "ahosts", "--", name)
	var nslookup bool
	if err != nil {
		// Alpine's resolver tool is BusyBox nslookup. On a server where
		// getent supports a DNS-only lookup, keep its answer authoritative.
		if _, alpineErr := os.Stat(t.path("/etc/alpine-release")); alpineErr != nil {
			return nil, err
		}
		raw, err = t.run(ctx, toolboxTool("nslookup"), name)
		if err != nil {
			return nil, err
		}
		nslookup = true
	}
	seen := map[netip.Addr]bool{}
	answer := !nslookup
	for line := range strings.SplitSeq(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		value := fields[0]
		if nslookup {
			if value == "Name:" {
				answer = true
			}
			// Skip the resolver address printed before the answer.
			if !answer || value != "Address:" || len(fields) < 2 {
				continue
			}
			value = fields[1]
		}
		if ip, err := netip.ParseAddr(value); err == nil && !seen[ip] {
			addresses = append(addresses, ip)
			seen[ip] = true
		}
	}

	if len(addresses) == 0 {
		return nil, errors.New("the control server's name did not resolve")
	}
	return addresses, nil
}

func (t *hostToolbox) canGuard() bool {
	for _, name := range []string{"sh", "flock", "cp", "mv", "mktemp", "rm"} {
		if _, ok := t.tools.Find(toolboxTool(name)); !ok {
			return false
		}
	}
	if t.systemd() {
		_, ok := t.tools.Find(toolboxTool("systemd-run"))
		return ok
	}
	return t.dir != ""
}

func (t *hostToolbox) changeDir(change string) string {
	return filepath.Join(t.dir, "host-toolbox", change)
}

func toolboxState(dir string) string {
	raw, _ := os.ReadFile(filepath.Join(dir, "state")) //nolint:gosec // private agent directory named by a checked change ID
	return strings.TrimSpace(string(raw))
}

func toolboxSetState(dir, state string) error {
	return toolboxAtomic(filepath.Join(dir, "state"), []byte(state+"\n"), 0o600)
}

func toolboxLock(dir string, nonblock bool) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0o600) //nolint:gosec // lock under private agent data
	if err != nil {
		return nil, err
	}
	flags := unix.LOCK_EX
	if nonblock {
		flags |= unix.LOCK_NB
	}
	if err := unix.Flock(int(f.Fd()), flags); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func toolboxShellWord(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func (t *hostToolbox) makeRevert(dir string, edit toolboxEdit) error {
	var script strings.Builder
	script.WriteString("#!/bin/sh\nset -eu\nPATH=/usr/sbin:/usr/bin:/sbin:/bin\nexport PATH\n")
	fmt.Fprintf(&script, "cd %s\nexec 9>lock\nflock -x 9\nread -r state <state\ncase \"$state\" in pending|ready) ;; *) exit 0 ;; esac\n", toolboxShellWord(dir))
	for i, w := range edit.files {
		target := toolboxShellWord(t.path(w.path))
		if w.wasMissing {
			fmt.Fprintf(&script, "rm -f -- %s\n", target)
			continue
		}
		backup := fmt.Sprintf("before-%d", i)
		if err := toolboxAtomic(filepath.Join(dir, backup), w.old, w.mode); err != nil {
			return err
		}
		template := toolboxShellWord(t.path(filepath.Dir(w.path)) + "/.dockwell-revert-XXXXXX")
		fmt.Fprintf(&script, "tmp=$(mktemp -- %s)\ntrap 'rm -f -- \"$tmp\"' EXIT\ncp -p -- %s \"$tmp\"\nmv -fT -- \"$tmp\" %s\ntrap - EXIT\n", template, backup, target)
	}
	switch edit.plan.GetOperation().GetOperation().(type) {
	case *v1.HostToolboxOperation_Hostname:
		old := edit.plan.GetDifferences()[0].GetBefore()
		tool := toolboxTool("hostname")
		args := toolboxShellWord(old)
		if t.usingSystemdTool(toolboxHostname) {
			tool, args = toolboxHostname, "set-hostname "+args
		}
		path, ok := t.tools.Find(tool)
		if !ok {
			return errors.New("the server has no tool to restore its hostname")
		}
		fmt.Fprintf(&script, "%s %s\n", toolboxShellWord(t.path(path)), args)
	case *v1.HostToolboxOperation_Dns:
		for _, w := range edit.files {
			switch w.path {
			case toolboxResolvedDropin:
				path, _ := t.tools.Find(toolboxSystem)
				fmt.Fprintf(&script, "%s restart systemd-resolved.service\n", toolboxShellWord(t.path(path)))
			case toolboxNetplanDropin:
				path, _ := t.tools.Find(toolboxNetplan)
				fmt.Fprintf(&script, "%s apply\n", toolboxShellWord(t.path(path)))
			}
		}
	default:
	}
	script.WriteString("printf 'rolled_back\\n' > state.new\nmv -fT -- state.new state\n")
	return toolboxAtomic(filepath.Join(dir, "restore"), []byte(script.String()), 0o700)
}

func (t *hostToolbox) armRevert(ctx context.Context, change, dir string) error {
	script := filepath.Join(dir, "restore")
	sh, _ := t.tools.Find(toolboxTool("sh"))
	sh = t.path(sh)
	if t.systemd() {
		_, err := t.run(ctx, toolboxTool("systemd-run"), "--quiet", "--unit=dockwell-toolbox-revert-"+change,
			"--on-active="+strconv.Itoa(int(toolboxKeepTime/time.Second)), "--timer-property=AccuracySec=1s", "--", sh, script)
		return err
	}
	const wait = "sleep \"$1\" && exec \"$2\" \"$3\""
	child := exec.CommandContext(context.WithoutCancel(ctx), sh, "-c", wait, "dockwell-toolbox", strconv.Itoa(int(toolboxKeepTime/time.Second)), sh, script) //nolint:gosec // fixed script and trusted arguments
	child.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := child.Start(); err != nil {
		return err
	}
	go func() { _ = child.Wait() }()
	return nil
}

func (t *hostToolbox) restore(ctx context.Context, dir string) error {
	_, err := t.run(ctx, toolboxTool("sh"), filepath.Join(dir, "restore"))
	return err
}

// recoverToolbox handles an agent or host restart before the timer could put
// the files back. Before connecting, every interrupted guarded edit is reverted.
func (t *hostToolbox) recoverToolbox(ctx context.Context) error {
	base := filepath.Join(t.dir, "host-toolbox")
	entries, err := os.ReadDir(base)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var failures error
	for _, e := range entries {
		if !e.IsDir() || !toolboxChangeID().MatchString(e.Name()) {
			continue
		}
		dir := filepath.Join(base, e.Name())
		switch toolboxState(dir) {
		case "pending", "ready":
			if err := t.restore(ctx, dir); err != nil {
				failures = errors.Join(failures, fmt.Errorf("%s: %w", e.Name(), err))
			}
		default:
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 7*24*time.Hour {
				_ = os.RemoveAll(dir)
			}
		}
	}
	return failures
}

func (t *hostToolbox) recoveryError() string {
	if err := t.recovery.Load(); err != nil {
		return *err
	}
	return ""
}

func (t *hostToolbox) retryRecovery(ctx context.Context, logger *slog.Logger, delay time.Duration) {
	for sleep(ctx, delay) {
		if err := t.recoverToolbox(ctx); err != nil {
			message := err.Error()
			t.recovery.Store(&message)
			logger.Warn("still can't restore the previous host settings", "error", err)
			delay = min(delay*2, time.Minute)
			continue
		}
		t.recovery.Store(nil)
		return
	}
}

func (t *hostToolbox) keep(ctx context.Context, change string) (resource.AccessVerdict, string) {
	if !toolboxChangeID().MatchString(change) {
		return resource.AccessNotWaiting, ""
	}
	dir := t.changeDir(change)
	switch toolboxState(dir) {
	case "ready":
		// The established stream can stay up after its resolver breaks.
		if time.Now().Before(toolboxProbeAfter(dir)) || t.probe == nil || t.probe(ctx) != nil {
			return resource.AccessNotProved, ""
		}
	case "kept":
		return resource.AccessKept, ""
	case "rolled_back":
		return resource.AccessRolledBack, ""
	default:
		return resource.AccessNotWaiting, ""
	}
	lock, err := toolboxLock(dir, true)
	if err != nil {
		return resource.AccessNotWaiting, ""
	}
	defer lock.Close()
	switch toolboxState(dir) {
	case "ready":
		if !time.Now().Before(toolboxDeadline(dir)) {
			return resource.AccessNotWaiting, ""
		}
		if err := toolboxSetState(dir, "kept"); err != nil {
			return resource.AccessNotKept, ""
		}
		return resource.AccessKept, ""
	case "kept":
		return resource.AccessKept, ""
	case "rolled_back":
		return resource.AccessRolledBack, ""
	default:
		return resource.AccessNotWaiting, ""
	}
}

func toolboxDeadline(dir string) time.Time { return toolboxTimeRecord(dir, "deadline") }

func toolboxProbeAfter(dir string) time.Time { return toolboxTimeRecord(dir, "probe-after") }

func toolboxTimeRecord(dir, name string) time.Time {
	raw, _ := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // private agent directory named by a checked change ID
	at, _ := time.Parse(time.RFC3339Nano, string(raw))
	return at
}

func (t *hostToolbox) awaitKeep(ctx context.Context, dir string) error {
	deadline := toolboxDeadline(dir)
	var offered bool
	nextProbe := toolboxProbeAfter(dir)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		switch toolboxState(dir) {
		case "kept":
			return nil
		case "rolled_back":
			return errors.New("the host settings were put back because the change wasn't kept")
		case "ready", "pending":
			if !time.Now().Before(deadline) {
				if err := t.restore(context.WithoutCancel(ctx), dir); err != nil {
					return fmt.Errorf("the rollback couldn't restore the host settings: %w", err)
				}
				continue
			}
			if !offered && t.probe != nil && toolboxState(dir) == "ready" && !time.Now().Before(nextProbe) {
				probe, cancel := context.WithDeadline(ctx, deadline)
				err := t.probe(probe)
				cancel()
				nextProbe = time.Now().Add(2 * time.Second)
				if left := time.Until(deadline); err == nil && left > 0 && toolboxState(dir) == "ready" {
					resource.Report(ctx, resource.Outcome{Waits: &resource.AccessWait{Deadline: deadline, Left: left}})
					offered = true
				}
			}
		default:
			return errors.New("the host settings rollback record is incomplete")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func (t *hostToolbox) guarded(ctx context.Context, dir string, edit toolboxEdit) error {
	if err := t.makeRevert(dir, edit); err != nil {
		return err
	}
	lock, err := toolboxLock(dir, false)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(toolboxKeepTime)
	if err = toolboxAtomic(filepath.Join(dir, "deadline"), []byte(deadline.Format(time.RFC3339Nano)), 0o600); err == nil {
		err = toolboxSetState(dir, "pending")
	}
	if err == nil {
		err = t.armRevert(ctx, edit.plan.GetChangeId(), dir)
	}
	if err != nil {
		// No configuration was written before the rollback was armed.
		_ = toolboxSetState(dir, "aborted")
		_ = lock.Close()
		return err
	}
	mutate, cancel := context.WithDeadline(ctx, deadline.Add(-5*time.Second))
	err = t.writeFiles(mutate, edit)
	cancel()
	if err == nil && slices.ContainsFunc(edit.files, func(w toolboxWrite) bool { return w.path == toolboxNetplanDropin }) {
		probeAfter := time.Now().Add(toolboxNetplanSettle).Format(time.RFC3339Nano)
		err = toolboxAtomic(filepath.Join(dir, "probe-after"), []byte(probeAfter), 0o600)
	}
	if err == nil {
		err = toolboxSetState(dir, "ready")
	}
	_ = lock.Close()
	if err != nil {
		return errors.Join(err, t.restore(context.WithoutCancel(ctx), dir))
	}
	return t.awaitKeep(ctx, dir)
}

func (t *hostToolbox) writeFiles(ctx context.Context, edit toolboxEdit) error {
	for _, w := range edit.files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if w.path == toolboxResolvedDropin || w.path == toolboxCloudConfig {
			err := os.Mkdir(t.path(filepath.Dir(w.path)), 0o755) //nolint:gosec // the resolver and cloud-init read these standard configuration directories
			if err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			if err := t.trustedParent(w.path); err != nil {
				return err
			}
		}
		if err := t.write(w); err != nil {
			return err
		}
	}
	if edit.apply != nil {
		if err := edit.apply(ctx); err != nil {
			return err
		}
	}
	if edit.reload != nil {
		return edit.reload(ctx)
	}
	return nil
}

func (t *hostToolbox) apply(ctx context.Context, reviewed *v1.HostToolboxPlan) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	change := reviewed.GetChangeId()
	if !toolboxChangeID().MatchString(change) {
		return errors.New("the toolbox change ID is invalid")
	}
	dir := t.changeDir(change)
	if evidence, err := os.ReadFile(filepath.Join(dir, "evidence")); err == nil { //nolint:gosec // private agent directory named by a checked change ID
		if string(evidence) != reviewed.GetEvidence() {
			return errors.New("the toolbox change differs from the one that started")
		}
		switch toolboxState(dir) {
		case "kept", "done":
			return nil
		case "pending", "ready", "rolled_back":
			return t.awaitKeep(ctx, dir)
		case "aborted":
			return errors.New("the host settings rollback couldn't start. Plan the operation again")
		default:
		}
	}
	unlock, err := t.lockToolboxAPT(reviewed)
	if err != nil {
		return err
	}
	defer unlock()
	edit, err := t.planLocked(ctx, reviewed)
	if err != nil {
		return err
	}
	if !proto.Equal(edit.plan, reviewed) {
		return errors.New("the host changed since you reviewed this plan. Plan the operation again")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := toolboxAtomic(filepath.Join(dir, "evidence"), []byte(reviewed.GetEvidence()), 0o600); err != nil {
		return err
	}
	if reviewed.GetKeep() {
		return t.guarded(ctx, dir, edit)
	}
	if op := reviewed.GetOperation(); op.GetSwap() != nil || op.GetCleanup() != nil || op.GetService() != nil {
		err = edit.apply(ctx)
	} else if err = t.writeFiles(ctx, edit); err != nil {
		for _, w := range edit.files {
			err = errors.Join(err, t.undo(w))
		}
	}
	if err != nil {
		return err
	}
	return toolboxSetState(dir, "done")
}

func (c *calls) applyHostToolbox(ctx context.Context, id uint64, apply *agentv1.ApplyCall, step resource.Step) *agentv1.CallResult {
	ask := apply.GetDesired().GetHostToolbox()
	ref := resourcepb.FromRef(apply.GetDesired().GetResource())
	if c.cfg.toolbox == nil || ask == nil || !proto.Equal(ask, step.Toolbox.Proto()) ||
		ref != step.Ref || ref != (resource.Ref{Kind: "server.host", Scope: c.server, Name: "host"}) ||
		ask.GetChangeId() != apply.GetDeploy().GetChangeId() {
		return callFailed(errors.New("the host toolbox operation differs from the reviewed step"))
	}
	if sealed := apply.GetValues(); len(sealed) > 0 {
		if c.delivery == nil {
			return callFailed(errors.New("the agent can't open the service's environment values"))
		}
		values, err := c.delivery.Open(c.server, ref, sealed)
		if err != nil {
			return callFailed(err)
		}
		defer clear(values)
		ctx = resource.WithValues(ctx, values)
	}
	ctx = resource.WithOutcome(ctx, func(o resource.Outcome) {
		_ = c.send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Progress{Progress: &agentv1.CallProgress{
			Id: id, Outcome: &agentv1.StepOutcome{Waits: accessWait(o.Waits)},
		}}})
	})
	if err := c.cfg.toolbox.apply(ctx, ask); err != nil {
		return callFailed(err)
	}
	return &agentv1.CallResult{Result: &agentv1.CallResult_Apply{Apply: &agentv1.ApplyResult{}}}
}
