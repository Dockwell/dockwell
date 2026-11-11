// SPDX-License-Identifier: AGPL-3.0-only

// Package jails keeps fail2ban's jails Dockwell writes, in a file of its own,
// /etc/fail2ban/jail.d/dockwell.conf, which it writes whole from the jails
// changes kept (ADR 0052). A change is checked with fail2ban-client -t before
// fail2ban loads it, and the file goes back as it was if either refuses.
package jails

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/agent/hosttool"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/fail2ban"
)

// File is where Dockwell writes its jails. fail2ban reads jail.d's files in
// name order after jail.conf, so dockwell.conf comes after Debian's
// defaults-debian.conf, and before a person's jail.local, which can set a jail
// otherwise.
const File = "/etc/fail2ban/jail.d/dockwell.conf"

// clientWait bounds each fail2ban-client the agent runs.
const clientWait = 30 * time.Second

// client is fail2ban-client, at the paths the distributions and pip put it.
var client = hosttool.Tool{Name: "fail2ban-client", Paths: []string{"/usr/bin/fail2ban-client", "/usr/local/bin/fail2ban-client"}}

// Manager keeps Dockwell's jails. Its methods are the jail driver's backend.
type Manager struct {
	// Dir holds jails.json, the jails as last written.
	Dir string
	// Root is the server's files: "/", or the stand-ins' folder in a test.
	Root  string
	Tools *hosttool.Runner
	// Control says the control server's addresses, which no jail bans, or
	// that it can't tell now. Nil says none.
	Control func(ctx context.Context) ([]string, error)
	// Sessions says the addresses signed in to the server over SSH from outside
	// it now, for a plan to say which of them a jail could ban. Nil says none.
	Sessions func(ctx context.Context) []string
	Logger   *slog.Logger

	mu     sync.Mutex
	jails  map[string]*resourcev1.Jail
	loaded bool
}

var (
	_ fail2ban.Backend = (*Manager)(nil)
	_ fail2ban.Planner = (*Manager)(nil)
	_ resource.Readier = (*Manager)(nil)
)

func (m *Manager) logger() *slog.Logger {
	if m.Logger != nil {
		return m.Logger
	}
	return slog.Default()
}

func (m *Manager) path() string {
	root := m.Root
	if root == "" {
		root = "/"
	}
	return filepath.Join(root, File)
}

// Ready says whether the server can run Dockwell's jails: it has fail2ban, and
// its jail.d folder.
func (m *Manager) Ready(context.Context) error {
	// A server without fail2ban is a server as it is, not a fault.
	if _, ok := m.Tools.Find(client); !ok {
		return resource.NotHere(errors.New("fail2ban isn't installed on this server, so Dockwell can't keep its jails. Install fail2ban with the server's package manager"))
	}
	if info, err := os.Stat(filepath.Dir(m.path())); err != nil || !info.IsDir() {
		return resource.NotHere(fmt.Errorf("fail2ban has no %s folder here, so Dockwell has nowhere to write its jails. Reinstall fail2ban with the server's package manager", filepath.Dir(File)))
	}
	return nil
}

// Jail returns the named jail as Dockwell last wrote it, or nil.
func (m *Manager) Jail(_ context.Context, name string) (*resourcev1.Jail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadLocked(); err != nil {
		return nil, err
	}
	if j := m.jails[name]; j != nil {
		return proto.CloneOf(j), nil
	}
	return nil, nil
}

// Put writes the named jail, checks the file with fail2ban-client -t, and
// loads it, or leaves fail2ban as it was.
func (m *Manager) Put(ctx context.Context, name string, jail *resourcev1.Jail) error {
	jail = proto.CloneOf(jail)
	return m.change(ctx, func(j map[string]*resourcev1.Jail) { j[name] = jail })
}

// Delete removes the named jail, and loads what's left.
func (m *Manager) Delete(ctx context.Context, name string) error {
	return m.change(ctx, func(j map[string]*resourcev1.Jail) { delete(j, name) })
}

// change writes the file whole from the jails as fn leaves them: with nothing
// left it removes the file. fail2ban-client -t checks it first, and a reload
// loads it; if either refuses, the old file goes back. A fail2ban that isn't
// running loads it as it starts, and the step says so.
func (m *Manager) change(ctx context.Context, fn func(map[string]*resourcev1.Jail)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadLocked(); err != nil {
		return err
	}
	next := make(map[string]*resourcev1.Jail, len(m.jails))
	for name, j := range m.jails {
		next[name] = proto.CloneOf(j)
	}
	fn(next)
	// Checked again here, whatever sent them: what's written is read by fail2ban
	// as root, and a name or a path that says more than itself would set other
	// jails, or run a command.
	var kind fail2ban.Driver
	for name, j := range next {
		if err := kind.CheckName(name); err != nil {
			return fmt.Errorf("fail2ban stays as it was: %w", err)
		}
		if err := kind.Validate(j); err != nil {
			return fmt.Errorf("fail2ban stays as it was, as the %s jail isn't one Dockwell writes: %w", name, err)
		}
	}
	path := m.path()
	// Only a regular file is Dockwell's to read and replace: a link there would
	// have it write wherever the link leads.
	if info, lerr := os.Lstat(path); lerr == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("%s isn't a regular file, so it isn't Dockwell's to write, and fail2ban stays as it was. Remove it, then try again", File)
	}
	old, err := os.ReadFile(path) //nolint:gosec // fail2ban's own folder
	had := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("can't read %s, so fail2ban stays as it was: %w", File, err)
	}
	if had && !IsDockwells(string(old)) {
		// Someone else's file under Dockwell's name: it isn't Dockwell's to replace.
		return fmt.Errorf("%s is there already, and Dockwell didn't write it, so it stays as it is, and so does fail2ban. Move what it sets to jail.local or another file in jail.d, remove it, then try again", File)
	}
	control, err := m.controlLocked(ctx, string(old))
	if err != nil && len(next) > 0 {
		return err
	}
	putBack := func() {
		var err error
		if had {
			err = writeFile(path, old)
		} else {
			err = os.Remove(path)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			m.logger().ErrorContext(ctx, "can't put back fail2ban's jails as they were", slog.String("file", File), slog.String("error", err.Error()))
		}
	}
	if len(next) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("can't remove %s, so fail2ban stays as it was: %w", File, err)
		}
	} else if err := writeFile(path, []byte(Render(next, control))); err != nil {
		return fmt.Errorf("can't write %s, so fail2ban stays as it was: %w", File, err)
	}
	if res, err := m.run(ctx, "-t"); err != nil || res.Code != 0 {
		putBack()
		return fmt.Errorf("fail2ban-client -t refused the jails, so fail2ban stays as it was: %s", why(res, err))
	}
	if res, err := m.run(ctx, "ping"); err == nil && res.Code != 0 {
		// Its server isn't running: it reads the file as it starts.
		resource.Report(ctx, resource.Outcome{Note: "fail2ban isn't running, so the jail loads as it starts."})
	} else if res, err := m.run(ctx, "reload"); err != nil || res.Code != 0 {
		putBack()
		if res, err := m.run(ctx, "reload"); err != nil || res.Code != 0 {
			m.logger().ErrorContext(ctx, "can't reload fail2ban with its jails as they were", slog.String("error", why(res, err)))
		}
		return fmt.Errorf("fail2ban didn't reload with the jails, so it stays as it was: %s", why(res, err))
	}
	if err := m.saveLocked(next); err != nil {
		return err
	}
	m.jails = next
	return nil
}

func (m *Manager) run(ctx context.Context, args ...string) (hosttool.Result, error) {
	return m.Tools.Run(ctx, client, clientWait, nil, args...)
}

func why(res hosttool.Result, err error) string {
	if err != nil {
		return err.Error()
	}
	return hosttool.LastLines(res.Errs+"\n"+string(res.Out), 2)
}

// IsDockwells reports a jail file's text that Dockwell wrote, by its first line.
func IsDockwells(text string) bool { return strings.HasPrefix(text, header) }

// header is the file's first line, by which Dockwell knows the file as its own.
const header = "# Dockwell writes this file whole, from the jails its changes kept (ADR 0052).\n"

// Render is Dockwell's file of jails: a section for each, in name order, each
// ignoring the server's loopback and the control server's addresses, beside
// what it ignores itself.
func Render(jails map[string]*resourcev1.Jail, control []string) string {
	var b strings.Builder
	b.WriteString(header)
	b.WriteString("# Change these jails in Dockwell, or set them otherwise in jail.local, which fail2ban reads after it.\n")
	for _, name := range slices.Sorted(maps.Keys(jails)) {
		j := jails[name]
		fmt.Fprintf(&b, "\n[%s]\nenabled = true\nfilter = %s\n", name, j.GetFilter())
		if j.GetBackend() != "" {
			fmt.Fprintf(&b, "backend = %s\n", j.GetBackend())
		}
		for i, p := range j.GetLogPaths() {
			if i == 0 {
				fmt.Fprintf(&b, "logpath = %s\n", p)
			} else {
				fmt.Fprintf(&b, "          %s\n", p)
			}
		}
		if len(j.GetPorts()) > 0 {
			ports := make([]string, len(j.GetPorts()))
			for i, p := range j.GetPorts() {
				ports[i] = strconv.FormatUint(uint64(p), 10)
			}
			fmt.Fprintf(&b, "port = %s\n", strings.Join(ports, ","))
		}
		if j.GetMaxRetry() > 0 {
			fmt.Fprintf(&b, "maxretry = %d\n", j.GetMaxRetry())
		}
		if s := seconds(j.GetFindTime()); s > 0 {
			fmt.Fprintf(&b, "findtime = %d\n", s)
		}
		switch {
		case j.GetBanForGood():
			b.WriteString("bantime = -1\n")
		case seconds(j.GetBanTime()) > 0:
			fmt.Fprintf(&b, "bantime = %d\n", seconds(j.GetBanTime()))
		}
		fmt.Fprintf(&b, "ignoreip = %s\n", strings.Join(NeverBans(control, j.GetIgnore()), " "))
	}
	return b.String()
}

func seconds(s string) int64 {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0
	}
	return int64(d / time.Second)
}

// ControlAddresses says the addresses of the control server at url, which no
// jail bans, each as fail2ban takes it: an address alone, or what its name
// resolves to, looked up with lookup, or the resolver when that's nil. One on
// the server's own loopback is none, as every jail ignores that already, and
// one with a zone is left out, as fail2ban takes none and a % in its file is a
// value for it to fill in. It's an error only when the name doesn't resolve.
func ControlAddresses(controlURL string, lookup func(ctx context.Context, host string) ([]string, error)) func(ctx context.Context) ([]string, error) {
	if lookup == nil {
		lookup = net.DefaultResolver.LookupHost
	}
	return func(ctx context.Context) ([]string, error) {
		u, err := url.Parse(controlURL)
		if err != nil || u.Hostname() == "" {
			return nil, fmt.Errorf("the control server's address %q has no host", controlURL)
		}
		host := u.Hostname()
		found := []string{host}
		if _, err := netip.ParseAddr(host); err != nil {
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if found, err = lookup(ctx, host); err != nil {
				return nil, fmt.Errorf("can't look up the control server, %s: %w", host, err)
			}
		}
		var out []string
		zoned := 0
		for _, s := range found {
			a, err := netip.ParseAddr(s)
			switch {
			case err != nil || a.IsLoopback():
			case a.Zone() != "":
				zoned++
			default:
				out = append(out, a.Unmap().String())
			}
		}
		if len(out) == 0 && zoned > 0 {
			// Not "none": the control server is somewhere, at an address a jail can't name.
			return nil, fmt.Errorf("the control server, %s, has only addresses with a zone, such as fe80::1%%eth0, which fail2ban can't be told to ignore. Reach it by an address without one", host)
		}
		slices.Sort(out)
		return slices.Compact(out), nil
	}
}

// controlLocked is the control server's addresses for the file: as they
// resolve now, kept for the next time; or, when they don't resolve, as they
// last did. With neither, it's an error, as the jail would be written without
// them while its plan says it never bans the control server.
func (m *Manager) controlLocked(ctx context.Context, written string) ([]string, error) {
	if m.Control == nil {
		return nil, nil
	}
	kept := filepath.Join(m.Dir, "control.json")
	found, err := m.Control(ctx)
	if err == nil {
		if b, merr := json.Marshal(found); merr == nil {
			if werr := os.MkdirAll(m.Dir, 0o700); werr == nil {
				_ = writeFile(kept, b)
			}
		}
		return found, nil
	}
	var last []string
	if b, rerr := os.ReadFile(kept); rerr == nil && json.Unmarshal(b, &last) == nil { //nolint:gosec // the agent's own folder
		m.logger().WarnContext(ctx, "can't tell the control server's addresses now, so the jails ignore those it last had", slog.String("error", err.Error()))
		return last, nil
	}
	// Neither now nor kept: the file as written still says them, if it's the
	// file Dockwell wrote from the jails it holds.
	if back, ok := m.controlFromFileLocked(written); ok {
		m.logger().WarnContext(ctx, "can't tell the control server's addresses now, so the jails ignore those their file was written with", slog.String("error", err.Error()))
		if b, merr := json.Marshal(back); merr == nil {
			_ = writeFile(kept, b)
		}
		return back, nil
	}
	return nil, fmt.Errorf("fail2ban stays as it was: %w, and Dockwell has no address of it from before, so the jail would be written without the control server among what it never bans. Check the server's DNS, then try again", err)
}

// controlFromFileLocked reads the control server's addresses back from the
// file as written: what a jail's ignoreip line holds beyond loopback and that
// jail's own list. It's taken only when writing the jails Dockwell holds with
// those addresses gives the file back byte for byte, which proves both that
// the file is the one Dockwell wrote from them and what the addresses were.
func (m *Manager) controlFromFileLocked(written string) ([]string, bool) {
	if written == "" || len(m.jails) == 0 {
		return nil, false
	}
	section := ""
	for line := range strings.SplitSeq(written, "\n") {
		if name, ok := strings.CutPrefix(line, "["); ok && strings.HasSuffix(name, "]") {
			section = strings.TrimSuffix(name, "]")
			continue
		}
		list, ok := strings.CutPrefix(line, "ignoreip = ")
		j := m.jails[section]
		if !ok || j == nil {
			continue
		}
		var control []string
		for _, a := range strings.Fields(list) {
			if a != "127.0.0.1/8" && a != "::1" && !slices.Contains(j.GetIgnore(), a) {
				control = append(control, a)
			}
		}
		if Render(m.jails, control) == written {
			return control, true
		}
		return nil, false
	}
	return nil, false
}

// Stored are the jails Dockwell holds, by name, as it last wrote them.
func (m *Manager) Stored() map[string]*resourcev1.Jail {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadLocked(); err != nil {
		return nil
	}
	out := make(map[string]*resourcev1.Jail, len(m.jails))
	for name, j := range m.jails {
		out[name] = proto.CloneOf(j)
	}
	return out
}

// NeverBans is what a jail never bans: the server's loopback, the control
// server's addresses, and what it ignores itself.
func NeverBans(control, ignore []string) []string {
	out := []string{"127.0.0.1/8", "::1"}
	for _, a := range slices.Concat(control, ignore) {
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

// covers reports whether ranges between them hold every address of whole:
// one of them holds it, or both of its halves are covered. It looks no deeper
// than 16 bits down, which is further than anyone cuts everything up by hand.
func covers(ranges []netip.Prefix, whole netip.Prefix, depth int) bool {
	for _, r := range ranges {
		if r.Addr().Is4() == whole.Addr().Is4() && r.Bits() <= whole.Bits() && r.Contains(whole.Addr()) {
			return true
		}
	}
	if depth >= 16 || whole.Bits() >= whole.Addr().BitLen() {
		return false
	}
	low, err := whole.Addr().Prefix(whole.Bits() + 1)
	if err != nil {
		return false
	}
	// The other half starts at the bit just set.
	b := whole.Addr().AsSlice()
	b[whole.Bits()/8] |= 0x80 >> (whole.Bits() % 8)
	highAddr, _ := netip.AddrFromSlice(b)
	high := netip.PrefixFrom(highAddr, whole.Bits()+1)
	return covers(ranges, low, depth+1) && covers(ranges, high, depth+1)
}

// JailPlan says whom a jail, as planned, never bans, which of the addresses
// signed in over SSH now it could, and whether it ignores every address.
func (m *Manager) JailPlan(ctx context.Context, desired *resourcev1.Jail) *resource.JailPlan {
	m.mu.Lock()
	written, _ := os.ReadFile(m.path())
	_ = m.loadLocked()
	control, _ := m.controlLocked(ctx, string(written))
	m.mu.Unlock()
	out := &resource.JailPlan{NeverBans: NeverBans(control, desired.GetIgnore())}
	var ignored []netip.Prefix
	for _, s := range out.NeverBans {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			a, aerr := netip.ParseAddr(s)
			if aerr != nil {
				continue // a host name, which fail2ban looks up itself
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		ignored = append(ignored, p.Masked())
	}
	// Every address of a family, as one range or as halves that add up to it.
	out.IgnoresEverything = covers(ignored, netip.MustParsePrefix("0.0.0.0/0"), 0) || covers(ignored, netip.MustParsePrefix("::/0"), 0)
	if m.Sessions != nil {
		for _, s := range m.Sessions(ctx) {
			a, err := netip.ParseAddr(s)
			if err != nil {
				continue
			}
			if !slices.ContainsFunc(ignored, func(p netip.Prefix) bool { return p.Contains(a.Unmap()) }) {
				out.SignedInNotIgnored = append(out.SignedInNotIgnored, a.Unmap().String())
			}
		}
	}
	return out
}

// loadLocked reads jails.json, once.
func (m *Manager) loadLocked() error {
	if m.loaded {
		return nil
	}
	m.jails = map[string]*resourcev1.Jail{}
	b, err := os.ReadFile(filepath.Join(m.Dir, "jails.json"))
	if errors.Is(err, os.ErrNotExist) {
		m.loaded = true
		return nil
	}
	if err != nil {
		return err
	}
	var stored map[string]json.RawMessage
	if err := json.Unmarshal(b, &stored); err != nil {
		return fmt.Errorf("can't read %s: %w. Move it aside, and the agent starts from no jails of its own", filepath.Join(m.Dir, "jails.json"), err)
	}
	for name, raw := range stored {
		j := &resourcev1.Jail{}
		if err := protojson.Unmarshal(raw, j); err != nil {
			return fmt.Errorf("can't read jail %s in %s: %w", name, filepath.Join(m.Dir, "jails.json"), err)
		}
		m.jails[name] = j
	}
	m.loaded = true
	return nil
}

func (m *Manager) saveLocked(jails map[string]*resourcev1.Jail) error {
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return err
	}
	out := map[string]json.RawMessage{}
	for name, j := range jails {
		b, err := protojson.Marshal(j)
		if err != nil {
			return err
		}
		out[name] = b
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFile(filepath.Join(m.Dir, "jails.json"), b); err != nil {
		return fmt.Errorf("fail2ban loaded the jails, and the agent can't record them: %w", err)
	}
	return nil
}

// writeFile replaces a file whole, so a crash leaves the old one or the new.
// It writes beside it under a name nobody can have put there, as a link at a
// name it could guess would have it write wherever that leads, and refuses to
// replace what isn't a regular file.
func writeFile(path string, b []byte) error {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("%s isn't a regular file", path)
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err := f.Chmod(0o644); err != nil { // fail2ban reads it, as root
		_ = f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
