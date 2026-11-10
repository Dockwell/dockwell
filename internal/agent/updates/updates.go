// SPDX-License-Identifier: AGPL-3.0-only

// Package updates reads which package updates wait on a server, with the
// distribution's own package manager: apt, dnf or apk. It runs each as root from
// its fixed path, with no shell, a clean environment and a deadline, one run at a
// time, and keeps nothing between runs. Reading changes nothing on the server;
// refreshing the package lists happens only when they're more than a day old, or
// when a person asks. See docs/adr/0043-package-updates-on-a-server.md.
package updates

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/resource"
)

// ListsStale is how old a server's package lists may get before a read refreshes
// them first.
const ListsStale = 24 * time.Hour

// ForceEvery is how often a person's Check now refreshes the lists: one sooner
// reads them as they are.
const ForceEvery = time.Minute

// Why a server's updates aren't a current read, as ServerUpdates.reason says.
const (
	ReasonUnsupported   = "unsupported"
	ReasonAgentOutdated = "agent_outdated"
	ReasonServerOffline = "server_offline"
	ReasonReadFailed    = "read_failed"
)

// RetryAfter is how long a refresh that failed holds off the next one a read
// would make, as a mirror that's down is down for a while. A person's Check now
// doesn't wait for it.
const RetryAfter = time.Hour

// Refreshing is the progress a read says as it refreshes the lists first, which
// the control plane passes on.
const Refreshing = "Refreshing the package lists"

// Options configure a Reader.
type Options struct {
	// Root is where the server's files are: "/", but for tests, which put stand-ins
	// for the tools under a folder of their own.
	Root string
	// Unavailable, when set, says why no package manager runs, as in a test that
	// has no stand-ins, which must read nothing of the machine it runs on.
	Unavailable string
	// LiveRestore says whether Docker keeps containers running while it
	// restarts. Nil means there's no Docker.
	LiveRestore func(context.Context) bool
	// StateDir is where the reader keeps when it last refreshed the lists, and last
	// tried to, so a restart doesn't forget: the agent's data folder. Empty keeps
	// them in memory only.
	StateDir string
	Logger   *slog.Logger
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

// Reader reads a server's package updates.
type Reader struct {
	root        string
	unavailable string
	liveRestore func(context.Context) bool
	logger      *slog.Logger
	now         func() time.Time
	grace       time.Duration
	stateDir    string
	// turn is held by the one run on the server at a time.
	turn chan struct{}
	// forced is when a person last had the lists refreshed, refreshed when the
	// agent last did, attempted when it last tried, and failed why that attempt
	// failed, if it did, all held with turn.
	forced    time.Time
	refreshed time.Time
	attempted time.Time
	failed    string
}

// state is what the reader keeps in its data folder.
type state struct {
	Refreshed time.Time `json:"refreshed,omitzero"`
	Attempted time.Time `json:"attempted,omitzero"`
}

// stateFile is where the reader keeps its state, under the agent's data folder.
const stateFile = "package-lists.json"

// load reads what the reader kept, where it kept anything.
func (r *Reader) load() {
	if r.stateDir == "" {
		return
	}
	b, err := os.ReadFile(filepath.Join(r.stateDir, stateFile))
	if err != nil {
		return
	}
	var s state
	if json.Unmarshal(b, &s) == nil {
		r.refreshed, r.attempted = s.Refreshed, s.Attempted
	}
}

// save keeps when the lists were refreshed and last tried, for after a restart.
// It's a hint, so one that can't be written is said and passed over.
func (r *Reader) save() {
	if r.stateDir == "" {
		return
	}
	b, _ := json.Marshal(state{Refreshed: r.refreshed, Attempted: r.attempted})
	if err := os.WriteFile(filepath.Join(r.stateDir, stateFile), b, 0o600); err != nil {
		r.logger.Warn("can't keep when the package lists were refreshed; a restart refreshes them again", slog.Any("error", err))
	}
}

// New returns a Reader.
func New(o Options) *Reader {
	r := &Reader{
		root: cmp.Or(o.Root, "/"), unavailable: o.Unavailable, liveRestore: o.LiveRestore,
		logger: o.Logger, now: o.Now, grace: KillGrace, turn: make(chan struct{}, 1), stateDir: o.StateDir,
	}
	r.load()
	if r.logger == nil {
		r.logger = slog.New(slog.DiscardHandler)
	}
	if r.now == nil {
		r.now = time.Now
	}
	return r
}

// path is p under the reader's root.
func (r *Reader) path(p string) string { return filepath.Join(r.root, p) }

// manager is one package manager: how it refreshes its lists, reads what waits,
// and says what the server needs.
type manager interface {
	kind() dockwellv1.PackageManager
	// listsTime is when the package lists were last refreshed, or zero.
	listsTime(r *Reader) time.Time
	refresh(ctx context.Context, r *Reader) error
	// list reads what waits, what an upgrade holds back, and whether the
	// distribution marked which are security fixes.
	list(ctx context.Context, r *Reader) (waiting []*dockwellv1.PackageUpdate, held []string, marks bool, err error)
	// reboot says whether the server needs a reboot now, and for what, where it
	// says, and false for known when it can't tell.
	reboot(ctx context.Context, r *Reader) (needed bool, packages []string, known bool)
	automatic(r *Reader) *dockwellv1.AutomaticUpdates
}

// Read says which updates wait. With refresh, it refreshes the lists first when
// they're more than a day old; with force, whatever their age, unless a person
// had them refreshed within the last minute. A refresh that fails leaves the lists
// as they were, which it reads, and says why. It waits for a run under way.
func (r *Reader) Read(ctx context.Context, refresh, force bool) (*dockwellv1.ServerUpdates, error) {
	if r.unavailable != "" {
		return &dockwellv1.ServerUpdates{Unavailable: r.unavailable, Reason: ReasonUnsupported}, nil
	}
	m, why := r.manager()
	if m == nil {
		return &dockwellv1.ServerUpdates{Unavailable: why, Reason: ReasonUnsupported}, nil
	}
	select {
	case r.turn <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("another package manager run on this server didn't end in time: %w", ctx.Err())
	}
	defer func() { <-r.turn }()
	out := &dockwellv1.ServerUpdates{Manager: m.kind()}
	lists := m.listsTime(r)
	now := r.now()
	if force && now.Sub(r.forced) < ForceEvery {
		force = false
	}
	stale := lists.IsZero() || now.Sub(lists) > ListsStale
	// A refresh that failed holds off the next a read would make for a while, so
	// a mirror that's down isn't asked on every look.
	waiting := r.attempted.After(r.refreshed) && now.Sub(r.attempted) < RetryAfter
	switch {
	case force || refresh && stale && !waiting:
		if force {
			r.forced = now
		}
		resource.Progress(ctx, Refreshing)
		r.attempted = now
		err := m.refresh(ctx, r)
		switch {
		case err != nil && ctx.Err() != nil:
			return nil, err
		case err != nil:
			r.logger.Warn("couldn't refresh the package lists", slog.Any("error", err))
			r.failed = err.Error()
		default:
			r.refreshed, r.failed = r.now(), ""
		}
		r.save()
		lists = m.listsTime(r)
	case waiting && r.failed == "":
		r.failed = "the last refresh didn't finish"
	}
	if r.attempted.After(r.refreshed) {
		out.RefreshError = r.failed
	}
	if !lists.IsZero() {
		out.ListsTime = timestamppb.New(lists)
	}
	found, held, marks, err := m.list(ctx, r)
	if err != nil {
		r.logger.Warn("couldn't read the package updates", slog.Any("error", err))
		return nil, err
	}
	for _, p := range found {
		p.Effect = effectOf(m.kind(), p.GetName())
		if p.GetEffect() == dockwellv1.PackageEffect_PACKAGE_EFFECT_DOCKER_RESTART && r.liveRestore != nil {
			out.DockerLiveRestore = r.liveRestore(ctx)
		}
	}
	sortUpdates(found)
	out.Packages, out.Held, out.MarksSecurity = found, held, marks
	needed, packages, known := m.reboot(ctx, r)
	if !known {
		needed = r.newerKernel()
	}
	out.RebootRequired, out.RebootPackages = needed, packages
	out.Automatic = m.automatic(r)
	out.ReadTime = timestamppb.New(r.now())
	return out, nil
}

// sortUpdates puts security fixes first, then the rest, each by name.
func sortUpdates(ps []*dockwellv1.PackageUpdate) {
	slices.SortStableFunc(ps, func(a, b *dockwellv1.PackageUpdate) int {
		if a.GetSecurity() != b.GetSecurity() {
			if a.GetSecurity() {
				return -1
			}
			return 1
		}
		return cmp.Or(strings.Compare(a.GetName(), b.GetName()), strings.Compare(a.GetArchitecture(), b.GetArchitecture()))
	})
}

// manager picks the server's package manager by its os-release, and says why
// there's none when there isn't.
func (r *Reader) manager() (manager, string) {
	release := r.osRelease()
	ids := append([]string{release["ID"]}, strings.Fields(release["ID_LIKE"])...)
	var m manager
	for _, id := range ids {
		m = byID(id)
		if m != nil {
			break
		}
	}
	name := cmp.Or(release["PRETTY_NAME"], release["NAME"], release["ID"], "this system")
	if m == nil {
		return nil, fmt.Sprintf("Dockwell reads updates with apt, dnf 4 or apk, and %s uses none of them", name)
	}
	if m.kind() == dockwellv1.PackageManager_PACKAGE_MANAGER_DNF && r.dnf5() {
		return nil, fmt.Sprintf("Dockwell reads updates with dnf 4, and %s has dnf 5, which it doesn't read yet", name)
	}
	if tool := toolOf(m); !r.executable(tool) {
		return nil, fmt.Sprintf("Dockwell reads updates on %s with %s, and %s isn't there", name, filepath.Base(tool), tool)
	}
	return m, ""
}

// byID is the package manager of a distribution, by its os-release ID, or nil.
func byID(id string) manager {
	switch id {
	case "debian", "ubuntu":
		return apt{}
	case "rhel", "centos", "almalinux", "rocky":
		// Fedora has dnf 5, whose output differs, so it isn't one of these yet.
		return dnf{}
	case "alpine":
		return apk{}
	}
	return nil
}

// toolOf is the path of a manager's own program.
func toolOf(m manager) string {
	switch m.kind() {
	case dockwellv1.PackageManager_PACKAGE_MANAGER_APT:
		return aptGet
	case dockwellv1.PackageManager_PACKAGE_MANAGER_DNF:
		return dnfPath
	default:
		return apkPath
	}
}

// dnf5 says the server's dnf is dnf 5, as Fedora's is, which /usr/bin/dnf leads to.
func (r *Reader) dnf5() bool {
	to, err := filepath.EvalSymlinks(r.path(dnfPath))
	return err == nil && strings.HasPrefix(filepath.Base(to), "dnf5")
}

// osRelease reads the server's os-release.
func (r *Reader) osRelease() map[string]string {
	b, err := os.ReadFile(r.path("etc/os-release"))
	if err != nil {
		b, _ = os.ReadFile(r.path("usr/lib/os-release"))
	}
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		key, val, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		if unq, err := strconv.Unquote(val); err == nil {
			val = unq
		} else {
			val = strings.Trim(val, `"'`)
		}
		out[key] = val
	}
	return out
}

// executable says whether the file at p, under the root, is a program.
func (r *Reader) executable(p string) bool {
	fi, err := os.Stat(r.path(p))
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}

// newestModTime is when the newest of the files matching pattern, under the root,
// last changed, or zero when none does.
func (r *Reader) newestModTime(pattern string) time.Time {
	matches, _ := filepath.Glob(r.path(pattern))
	var newest time.Time
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
	}
	return newest
}

// exists says whether p, under the root, is there.
func (r *Reader) exists(p string) bool {
	_, err := os.Stat(r.path(p))
	return err == nil
}

// newerKernel says whether a kernel newer than the one running is installed, as
// /lib/modules has it, by the same flavour: "generic" on Ubuntu, "amd64" on
// Debian, "lts" or "virt" on Alpine, and the architecture on dnf's. False when
// it can't tell, as in a container, which has no kernel of its own there.
func (r *Reader) newerKernel() bool {
	running, err := os.ReadFile(r.path("proc/sys/kernel/osrelease"))
	if err != nil {
		return false
	}
	run := strings.TrimSpace(string(running))
	entries, err := os.ReadDir(r.path("lib/modules"))
	if err != nil {
		return false
	}
	flavour := kernelFlavour(run)
	for _, e := range entries {
		if !e.IsDir() && e.Type()&fs.ModeSymlink == 0 {
			continue
		}
		name := e.Name()
		if name == run || kernelFlavour(name) != flavour {
			continue
		}
		// A modules folder with no modules is what a removed kernel leaves.
		if !r.exists(filepath.Join("lib", "modules", name, "modules.dep")) {
			continue
		}
		if compareVersions(name, run) > 0 {
			return true
		}
	}
	return false
}

// kernelFlavour is what tells a kernel's kind apart from its version: what follows
// its last "-" when that's a word, as "generic" or "lts", or its architecture,
// as "x86_64" in "5.14.0-427.13.1.el9_4.x86_64".
func kernelFlavour(release string) string {
	if i := strings.LastIndexByte(release, '-'); i >= 0 {
		if f := release[i+1:]; f != "" && !strings.ContainsAny(f[:1], "0123456789") {
			return f
		}
	}
	if i := strings.LastIndexByte(release, '.'); i >= 0 {
		return release[i+1:]
	}
	return ""
}

// compareVersions orders two versions part by part: runs of digits by their
// value, and anything else as text. It's enough to tell two kernels apart, and
// isn't any package manager's own order.
func compareVersions(a, b string) int {
	for a != "" || b != "" {
		pa, ra := versionPart(a)
		pb, rb := versionPart(b)
		if c := comparePart(pa, pb); c != 0 {
			return c
		}
		a, b = ra, rb
	}
	return 0
}

// versionPart splits off the leading run of digits, or of anything else, of v.
func versionPart(v string) (part, rest string) {
	if v == "" {
		return "", ""
	}
	digit := v[0] >= '0' && v[0] <= '9'
	i := 1
	for i < len(v) && (v[i] >= '0' && v[i] <= '9') == digit {
		i++
	}
	return v[:i], v[i:]
}

func comparePart(a, b string) int {
	na, ea := strconv.ParseUint(a, 10, 64)
	nb, eb := strconv.ParseUint(b, 10, 64)
	switch {
	case ea == nil && eb == nil:
		return cmp.Compare(na, nb)
	case ea == nil:
		return 1
	case eb == nil:
		return -1
	}
	return strings.Compare(a, b)
}

// rebootFor is what takes effect only after a reboot, by package manager: a
// kernel, libc and D-Bus, the core set dnf's needs-restarting goes by, with
// systemd where it can't re-execute itself, and the CPU's microcode.
var rebootFor = map[dockwellv1.PackageManager][]string{
	// Debian and Ubuntu re-execute systemd as it's upgraded, so it isn't one.
	dockwellv1.PackageManager_PACKAGE_MANAGER_APT: {
		"libc6", "dbus", "dbus-daemon", "dbus-broker", "linux-firmware", "intel-microcode", "amd64-microcode",
	},
	dockwellv1.PackageManager_PACKAGE_MANAGER_DNF: {
		"kernel", "kernel-core", "kernel-modules", "kernel-modules-core", "kernel-modules-extra", "kernel-rt", "kernel-rt-core",
		"glibc", "linux-firmware", "systemd", "dbus", "dbus-broker", "dbus-daemon", "microcode_ctl",
	},
	dockwellv1.PackageManager_PACKAGE_MANAGER_APK: {
		"linux-lts", "linux-virt", "linux-edge", "linux-stable", "linux-rpi", "linux-rpi2", "linux-rpi4",
	},
}

// dockerEngines are the packages that hold Docker's engine, which restarts when
// one is installed, and every container with it.
var dockerEngines = []string{"docker-ce", "docker.io", "docker-engine", "moby-engine"}

// dockerRestarters restart Docker as they're installed: its engine, and the
// containerd and runc it runs containers with, which docker.service requires.
var dockerRestarters = append(slices.Clone(dockerEngines), "containerd.io", "containerd", "runc")

// effectOf is what installing a package needs beyond replacing its files.
func effectOf(m dockwellv1.PackageManager, name string) dockwellv1.PackageEffect {
	switch {
	case slices.Contains(rebootFor[m], name),
		m == dockwellv1.PackageManager_PACKAGE_MANAGER_APT && (strings.HasPrefix(name, "linux-image-") || strings.HasPrefix(name, "linux-signed-image-") || strings.HasPrefix(name, "linux-modules-")):
		return dockwellv1.PackageEffect_PACKAGE_EFFECT_REBOOT
	case slices.Contains(dockerRestarters, name):
		return dockwellv1.PackageEffect_PACKAGE_EFFECT_DOCKER_RESTART
	}
	return dockwellv1.PackageEffect_PACKAGE_EFFECT_UNSPECIFIED
}
