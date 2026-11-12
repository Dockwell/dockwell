// SPDX-License-Identifier: AGPL-3.0-only

package updates

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
)

// Installing a change's updates (ADR 0064): a plan from the exact command
// simulated, a download that's safe to stop, then the install from what was
// downloaded in a unit of the init system's own, which a dropped stream, a
// restart of the control plane or the agent, or a deadline doesn't stop.
const (
	// DownloadTimeout bounds fetching the packages, which changes nothing.
	DownloadTimeout = 30 * time.Minute
	// followEvery is how often the install's output is read while it runs.
	followEvery = time.Second
	// stillAfter is when following says the install still runs, and goes on.
	stillAfter = 30 * time.Minute
	// unitWait is how long following waits out systemctl failing, as while a
	// systemd upgrade re-executes it, before it gives up.
	unitWait = time.Minute
	// finishing is the suffix of the unit that finishes what dpkg left.
	finishing = "-finish"
	// unitPrefix names an install's unit after its change.
	unitPrefix = "dockwell-install-"
)

// packageName and packageVersion are what a package's name and version may hold,
// as Debian's policy says, an architecture after the name included: the agent
// installs nothing else, whatever it's sent.
var (
	packageName    = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]*(:[a-z0-9-]+)?$`) })
	packageVersion = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9.+~:-]+$`) })
)

// ErrChanged is an install whose updates aren't what its plan said any more, at
// its first start: a person never applies a list they didn't see.
var ErrChanged = errors.New("the updates waiting changed since this plan")

// ErrNotYet is a package manager Dockwell doesn't install with yet.
var ErrNotYet = errors.New("installing updates takes apt for now")

// ErrNotListed is a package asked for by name that isn't among the updates the
// server's own read lists: the agent installs nothing it didn't list.
var ErrNotListed = errors.New("not among the updates waiting")

// maxNames is the most packages one install names.
const maxNames = 1000

// Units runs a program as a unit of the init system's own, outside the agent's,
// so it outlives the agent and whatever called it: systemd's transient units.
type Units interface {
	// Start runs argv as the unit name, with env, its output appended to log.
	Start(ctx context.Context, name string, argv, env []string, log string) error
	// Active reports whether the unit runs.
	Active(ctx context.Context, name string) (bool, error)
	// Ended says how a unit that ran ended, its exit status or -1 when the init
	// system can't say, and lets it go.
	Ended(ctx context.Context, name string) (int, error)
}

// InstallPlan is what installing will do, as the package manager simulates the
// exact command: every package it installs, those it pulls in too, and what
// follows.
type InstallPlan struct {
	// Packages from the version installed to the one it gets; a package pulled
	// in has no version installed.
	Packages []*dockwellv1.PackageUpdate
	// Reboot says a package takes effect only after a reboot, which is a change of
	// its own.
	Reboot bool
	// DockerRestarts says Docker's engine restarts, and KeepsRunning that it keeps
	// containers running meanwhile (live-restore).
	DockerRestarts, KeepsRunning bool
	// Asked are the packages asked for by name, sorted, for an install of a
	// selection: every other package is one they need.
	Asked []string
}

// PlanInstall simulates installing the updates waiting, as the exact command
// would, with versions pinned and what they pull in: the packages named and what
// they need, or with none named the security fixes, or all of them. A name the
// read doesn't list is refused.
func (r *Reader) PlanInstall(ctx context.Context, all bool, names ...string) (*InstallPlan, error) {
	got, err := r.Read(ctx, false, false)
	if err != nil {
		return nil, err
	}
	if got.GetUnavailable() != "" {
		return nil, resource.Plainly(got.GetUnavailable(), errors.New(got.GetUnavailable()))
	}
	if got.GetManager() != dockwellv1.PackageManager_PACKAGE_MANAGER_APT {
		return nil, resource.Plainly("Dockwell installs updates with apt for now. Install them on this server over SSH, with the command the Updates tab shows.", ErrNotYet)
	}
	if !r.exists("run/systemd/system") {
		return nil, resource.Plainly("This server doesn't run systemd, which Dockwell installs updates under so nothing stops them halfway. Install them on it over SSH, with the command the Updates tab shows.", ErrNoUnits)
	}
	if len(names) > 0 {
		return r.planPicked(ctx, got, names)
	}
	var pick []*dockwellv1.PackageUpdate
	for _, p := range got.GetPackages() {
		if all || p.GetSecurity() {
			pick = append(pick, p)
		}
	}
	if len(pick) == 0 {
		return &InstallPlan{}, nil
	}
	return r.simulate(ctx, pick, got.GetDockerLiveRestore())
}

// planPicked simulates installing the packages named, each one the read lists,
// and what they need. A package new to the server that none of the others named
// pulls in is refused, as apt names only upgrades beside each other.
func (r *Reader) planPicked(ctx context.Context, got *dockwellv1.ServerUpdates, names []string) (*InstallPlan, error) {
	asked := slices.Clone(names)
	slices.Sort(asked)
	asked = slices.Compact(asked)
	if len(asked) > maxNames {
		return nil, fmt.Errorf("asked to install %d packages, more than the %d one install names", len(asked), maxNames)
	}
	byName := make(map[string]*dockwellv1.PackageUpdate, len(got.GetPackages()))
	for _, p := range got.GetPackages() {
		byName[p.GetName()] = p
	}
	pick := make([]*dockwellv1.PackageUpdate, 0, len(asked))
	for _, name := range asked {
		p, ok := byName[name]
		if !ok || !packageName().MatchString(name) {
			return nil, resource.Plainly(quoteIfOdd(name)+" isn't among the updates waiting on this server, so Dockwell won't install it. Check again on the Updates tab.",
				fmt.Errorf("%q: %w", name, ErrNotListed))
		}
		pick = append(pick, p)
	}
	plan, err := r.simulate(ctx, pick, got.GetDockerLiveRestore())
	if err != nil {
		return nil, err
	}
	planned := make(map[string]bool, len(plan.Packages))
	for _, p := range plan.Packages {
		planned[p.GetName()] = true
	}
	for _, p := range pick {
		if !planned[p.GetName()] {
			return nil, resource.Plainly(p.GetName()+" is new to this server, and none of the other packages named needs it. Update it on its own, or with every update.",
				fmt.Errorf("%s isn't in the install's simulation", p.GetName()))
		}
	}
	plan.Asked = asked
	return plan, nil
}

// quoteIfOdd quotes a name that isn't a package's, so what was sent shows as sent.
func quoteIfOdd(name string) string {
	if packageName().MatchString(name) {
		return name
	}
	return fmt.Sprintf("%q", name)
}

// named are the packages a command names: the upgrades, as apt pulls in what they
// need itself, and naming a package that isn't installed would mark it as wanted
// for its own sake. Only when nothing but pulled-in packages is left are those
// named.
func named(ps []*dockwellv1.PackageUpdate) (out []*dockwellv1.PackageUpdate, onlyUpgrade bool) {
	for _, p := range ps {
		if p.GetInstalled() != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return ps, false
	}
	return out, true
}

// flags are a command's flags for what it names.
func flags(onlyUpgrade bool) []string {
	if onlyUpgrade {
		return []string{"--only-upgrade", "--no-remove"}
	}
	return []string{"--no-remove"}
}

// simulate runs apt's simulation of installing exactly these versions, and says
// everything it would install, what they pull in included.
func (r *Reader) simulate(ctx context.Context, ps []*dockwellv1.PackageUpdate, live bool) (*InstallPlan, error) {
	pick, only := named(ps)
	args := append(append([]string{
		"-q", "-o", "Debug::NoLocking=true", "-o", "APT::Get::Show-User-Simulation-Note=false", "--simulate",
	}, flags(only)...), append([]string{"install", "--"}, pinned(pick)...)...)
	res, err := r.run(ctx, "Simulating the install", aptGet, ReadTimeout, args...)
	if err != nil {
		return nil, err
	}
	if res.code != 0 {
		return nil, fail("Simulating the install", res)
	}
	waiting, _ := parseAptSimulation(res.out)
	plan := &InstallPlan{Packages: waiting, KeepsRunning: live}
	for _, p := range waiting {
		p.Effect = effectOf(dockwellv1.PackageManager_PACKAGE_MANAGER_APT, p.GetName())
		plan.Reboot = plan.Reboot || p.GetEffect() == dockwellv1.PackageEffect_PACKAGE_EFFECT_REBOOT
		if p.GetEffect() == dockwellv1.PackageEffect_PACKAGE_EFFECT_DOCKER_RESTART {
			plan.DockerRestarts = true
			// live-restore keeps containers across Docker's patch releases alone: a
			// new minor version, or containerd or runc, stops them.
			plan.KeepsRunning = plan.KeepsRunning && slices.Contains(dockerEngines, p.GetName()) && samePatchLine(p.GetInstalled(), p.GetAvailable())
		}
	}
	return plan, nil
}

// samePatchLine reports two versions of Docker's engine apart by a patch release
// at most: "5:27.4.0-1~ubuntu.24.04~noble" and "5:27.4.1-1~..." are; 27.5 isn't.
func samePatchLine(from, to string) bool {
	line := func(v string) string {
		if _, after, ok := strings.Cut(v, ":"); ok {
			v = after
		}
		parts := strings.SplitN(v, ".", 3)
		if len(parts) < 2 {
			return ""
		}
		return parts[0] + "." + parts[1]
	}
	return from != "" && line(from) != "" && line(from) == line(to)
}

// pinned is each package as name=version, for apt to install exactly that.
func pinned(ps []*dockwellv1.PackageUpdate) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.GetName()+"="+p.GetAvailable())
	}
	return out
}

// Installer installs a change's updates.
type Installer struct {
	Reader *Reader
	Units  Units
	// Dir is where an install keeps its output while it runs, under the agent's
	// data folder: deleted when it ends, so nothing of it is kept (ADR 0043).
	Dir string
	// Now is the clock. Nil is time.Now.
	Now func() time.Time
}

// unit is the change's install unit.
func unit(change string) string { return unitPrefix + change }

func (in *Installer) log(change string) string { return filepath.Join(in.Dir, change+".log") }

// Install installs the packages the change's plan fixed, saying each line the
// install prints. Its first start simulates again and refuses what moved; the
// download is safe to stop; the install runs in its own unit. Started again, as
// after a dropped stream or a restart, it follows a unit that still runs, takes
// packages at their planned version as done, finishes what dpkg left half done,
// and installs the rest while their planned versions are still the candidates.
func (in *Installer) Install(ctx context.Context, change string, planned []*dockwellv1.PackageUpdate, say func(string)) error {
	if len(planned) == 0 {
		return nil
	}
	for _, p := range planned {
		if !packageName().MatchString(p.GetName()) || !packageVersion().MatchString(p.GetAvailable()) {
			return fmt.Errorf("refused to install %q at %q: that isn't a package's name and version", p.GetName(), p.GetAvailable())
		}
	}
	if err := os.MkdirAll(in.Dir, 0o700); err != nil {
		return fmt.Errorf("can't make a folder for the install's output: %w", err)
	}
	in.sweep(ctx, change)
	name := unit(change)
	if active, err := in.active(ctx, name+finishing); err != nil {
		return err
	} else if active {
		say("Following dpkg as it finishes what a stopped install left")
		if err := in.watch(ctx, change, name+finishing, say); err != nil {
			return err
		}
	}
	if active, err := in.active(ctx, name); err != nil {
		return err
	} else if active {
		say("Following the install that's running")
		return in.follow(ctx, change, planned, say)
	}
	_, err := os.Stat(in.log(change))
	started := err == nil
	if started {
		// A run that ended while nobody followed it is let go, so its unit's name
		// is free to start the rest under.
		_, _ = in.Units.Ended(ctx, name)
	}
	left, err := in.left(ctx, planned)
	if err != nil {
		return err
	}
	if len(left) == 0 {
		in.done(ctx, change)
		return nil
	}
	if started {
		// A stopped run can leave packages unpacked and not configured.
		if err := in.finishDpkg(ctx, change, say); err != nil {
			return err
		}
		if left, err = in.left(ctx, planned); err != nil {
			return err
		}
		if len(left) == 0 {
			in.done(ctx, change)
			return nil
		}
	}
	// What's left goes ahead only as planned: at its first start, the whole plan as
	// simulated again; on a resume, each package left still a candidate at its
	// planned version.
	again, err := in.Reader.simulate(ctx, left, false)
	if err != nil {
		return err
	}
	if !same(again.Packages, left, started) {
		return resource.Plainly("The updates waiting on this server changed since this plan. Plan again.", ErrChanged)
	}
	say("Downloading the packages")
	if err := in.download(ctx, left); err != nil {
		return err
	}
	f, err := os.OpenFile(in.log(change), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("can't make a file for the install's output: %w", err)
	}
	_ = f.Close()
	pick, only := named(left)
	argv := append(append([]string{in.Reader.path(aptGet), "-q", "-y", "--no-download"}, flags(only)...), append([]string{
		"-o", "Dpkg::Options::=--force-confold", "-o", "Dpkg::Options::=--force-confdef",
		"-o", "DPkg::Lock::Timeout=120", "install", "--",
	}, pinned(pick)...)...)
	// needrestart, run after apt on Ubuntu, only lists what uses an old library:
	// nothing restarts that the plan didn't say.
	envs := append(slices.Clone(env), "NEEDRESTART_MODE=l", "NEEDRESTART_SUSPEND=")
	if err := in.Units.Start(ctx, name, argv, envs, in.log(change)); err != nil {
		return fmt.Errorf("can't start the install: %w", err)
	}
	say("Installing " + plural(len(left), "package"))
	return in.follow(ctx, change, planned, say)
}

// same reports whether a simulation installs what was planned: at a first start,
// exactly the planned list, pulled-in packages included; on a resume, at least each
// planned package at its planned version.
func same(got, want []*dockwellv1.PackageUpdate, resuming bool) bool {
	byName := map[string]string{}
	for _, p := range got {
		byName[p.GetName()] = p.GetAvailable()
	}
	for _, p := range want {
		if byName[p.GetName()] != p.GetAvailable() {
			return false
		}
	}
	return resuming || len(got) == len(want)
}

// download fetches the packages, changing nothing: a run stopped partway leaves
// part of a cache, and the next one fetches the rest.
func (in *Installer) download(ctx context.Context, ps []*dockwellv1.PackageUpdate) error {
	pick, only := named(ps)
	args := append(append([]string{"-q", "-y", "--download-only"}, flags(only)...), append([]string{"-o", "DPkg::Lock::Timeout=120", "install", "--"}, pinned(pick)...)...)
	res, err := in.Reader.run(ctx, "Downloading the packages", aptGet, DownloadTimeout, args...)
	if err != nil {
		return err
	}
	if res.code != 0 {
		return fail("Downloading the packages", res)
	}
	return nil
}

// watch says what a unit prints as it comes until it ends, then says whether it
// ended well. A caller that leaves stops the watching, never the unit, which the
// next start follows again.
func (in *Installer) watch(ctx context.Context, change, name string, say func(string)) error {
	tail := &tailer{path: in.log(change)}
	now := in.now
	began := now()
	still := false
	tick := time.NewTicker(followEvery)
	defer tick.Stop()
	for {
		tail.read(say)
		active, err := in.active(ctx, name)
		if err != nil {
			return err
		}
		if !active {
			tail.read(say)
			break
		}
		if !still && now().Sub(began) >= stillAfter {
			still = true
			say("Still installing after " + human.Duration(stillAfter) + "; it goes on")
		}
		select {
		case <-ctx.Done():
			return resource.Plainly("Dockwell stopped following the install, which goes on in its own unit, "+name+", on this server. Check it with systemctl status "+name+", then plan again once it ends.",
				fmt.Errorf("stopped following the install, which goes on in its own unit: %w", ctx.Err()))
		case <-tick.C:
		}
	}
	if strings.HasSuffix(name, finishing) {
		code, _ := in.Units.Ended(ctx, name)
		if code != 0 {
			return fmt.Errorf("dpkg ended with status %d finishing what a stopped install left: %s", code, cmpOr(lastLines(string(in.tail(change))), "it said nothing"))
		}
	}
	return nil
}

// follow says the install's output as it comes until its unit ends, then checks
// every package reached its version.
func (in *Installer) follow(ctx context.Context, change string, planned []*dockwellv1.PackageUpdate, say func(string)) error {
	name := unit(change)
	if err := in.watch(ctx, change, name, say); err != nil {
		return err
	}
	code, _ := in.Units.Ended(ctx, name)
	left, err := in.left(ctx, planned)
	if err != nil {
		return err
	}
	if len(left) > 0 {
		tail := lastLines(string(in.tail(change)))
		msg := fmt.Sprintf("the install ended with status %d, and %s didn't reach the planned version: %s", code, plural(len(left), "package"), cmpOr(tail, "it said nothing"))
		if half, _ := in.halfDone(ctx); half != "" {
			msg += ". dpkg left packages half done, which the next install on this server finishes first"
		}
		in.done(ctx, change)
		return errors.New(msg)
	}
	if code > 0 {
		say(fmt.Sprintf("apt ended with status %d, though every package reached its planned version: %s", code, cmpOr(lastLines(string(in.tail(change))), "it said nothing")))
	}
	in.done(ctx, change)
	return nil
}

// active reports whether a unit runs, waiting out systemctl failing for a while,
// as it can while a systemd upgrade re-executes it.
func (in *Installer) active(ctx context.Context, name string) (bool, error) {
	var first time.Time
	for {
		active, err := in.Units.Active(ctx, name)
		if err == nil {
			return active, nil
		}
		if first.IsZero() {
			first = in.now()
		}
		if in.now().Sub(first) >= unitWait {
			return false, err
		}
		select {
		case <-ctx.Done():
			return false, err
		case <-time.After(followEvery):
		}
	}
}

func (in *Installer) now() time.Time {
	if in.Now != nil {
		return in.Now()
	}
	return time.Now()
}

// tailer reads what's been added to a file since it last looked, a chunk at a
// time, and says each whole line.
type tailer struct {
	path    string
	offset  int64
	partial string
}

func (t *tailer) read(say func(string)) {
	f, err := os.Open(t.path)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return
	}
	buf := make([]byte, 64<<10)
	for range maxOutput / len(buf) {
		n, err := f.Read(buf)
		if n > 0 {
			t.offset += int64(n)
			lines := strings.Split(t.partial+string(buf[:n]), "\n")
			t.partial = lines[len(lines)-1]
			if len(t.partial) > errTail {
				// A line that long, as a progress bar's, is cut, not kept whole.
				t.partial = t.partial[len(t.partial)-errTail:]
			}
			for _, l := range lines[:len(lines)-1] {
				if l = strings.TrimSpace(l); l != "" {
					say(l)
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// sweep lets go of what installs of other changes left that no unit runs any
// more, as a change that failed while its install went on: their output is
// deleted, and their units let go.
func (in *Installer) sweep(ctx context.Context, keep string) {
	logs, _ := filepath.Glob(filepath.Join(in.Dir, "*.log"))
	for _, l := range logs {
		change := strings.TrimSuffix(filepath.Base(l), ".log")
		if change == keep {
			continue
		}
		if active, err := in.Units.Active(ctx, unit(change)); err != nil || active {
			continue
		}
		if active, err := in.Units.Active(ctx, unit(change)+finishing); err != nil || active {
			continue
		}
		in.done(ctx, change)
	}
}

// left are the planned packages not yet at their planned version.
func (in *Installer) left(ctx context.Context, planned []*dockwellv1.PackageUpdate) ([]*dockwellv1.PackageUpdate, error) {
	names := make([]string, 0, len(planned))
	for _, p := range planned {
		names = append(names, p.GetName())
	}
	args := append([]string{"-W", "-f=${Package}\t${Architecture}\t${Version}\t${db:Status-Abbrev}\n", "--"}, names...)
	res, err := in.Reader.run(ctx, "Reading the installed versions", dpkgQuery, ReadTimeout, args...)
	if err != nil {
		return nil, err
	}
	// 1 is a name dpkg doesn't know yet, as a package an install pulls in.
	if res.code != 0 && res.code != 1 {
		return nil, fail("Reading the installed versions", res)
	}
	// Each by name and architecture: dpkg names libc6:i386 libc6, as it does the
	// native one.
	have := map[string]string{}
	for line := range strings.Lines(string(res.out)) {
		parts := strings.SplitN(strings.TrimRight(line, "\n"), "\t", 4)
		// Only a package installed and configured ("ii") counts.
		if len(parts) == 4 && strings.HasPrefix(parts[3], "ii") {
			have[parts[0]+"\t"+parts[1]] = parts[2]
			if _, ok := have[parts[0]]; !ok {
				have[parts[0]] = parts[2]
			}
		}
	}
	var out []*dockwellv1.PackageUpdate
	for _, p := range planned {
		name, arch, _ := strings.Cut(p.GetName(), ":")
		arch = cmpOr(arch, p.GetArchitecture())
		got, ok := have[name+"\t"+arch]
		if !ok {
			// One of no architecture of its own ("all"), or a plan that didn't say.
			got, ok = have[name+"\tall"]
		}
		if !ok && arch == "" {
			got = have[name]
		}
		if got != p.GetAvailable() {
			out = append(out, p)
		}
	}
	return out, nil
}

// halfDone is what dpkg says it left unpacked and not configured, or "".
func (in *Installer) halfDone(ctx context.Context) (string, error) {
	res, err := in.Reader.run(ctx, "Asking dpkg what it left half done", dpkg, ReadTimeout, "--audit")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(res.out)), nil
}

// finishDpkg configures what a stopped run left unpacked, when dpkg says it did,
// in a unit of its own as the install runs: a dropped stream, a restart or a
// deadline can't stop dpkg partway through a package's scripts.
func (in *Installer) finishDpkg(ctx context.Context, change string, say func(string)) error {
	left, err := in.halfDone(ctx)
	if err != nil || left == "" {
		return err
	}
	say("Finishing what a stopped install left half done")
	name := unit(change) + finishing
	_, _ = in.Units.Ended(ctx, name)
	argv := []string{in.Reader.path(dpkg), "--force-confold", "--force-confdef", "--configure", "-a"}
	if err := in.Units.Start(ctx, name, argv, env, in.log(change)); err != nil {
		return fmt.Errorf("can't start finishing what a stopped install left: %w", err)
	}
	return in.watch(ctx, change, name, say)
}

// tail is the end of the install's output, for a failure's words.
func (in *Installer) tail(change string) []byte {
	f, err := os.Open(in.log(change))
	if err != nil {
		return nil
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > errTail {
		if _, err := f.Seek(st.Size()-errTail, io.SeekStart); err != nil {
			return nil
		}
	}
	b, _ := io.ReadAll(io.LimitReader(f, errTail))
	return b
}

// done lets the install go: its output is deleted, as nothing of it is kept.
func (in *Installer) done(ctx context.Context, change string) {
	_ = os.Remove(in.log(change))
	_, _ = in.Units.Ended(ctx, unit(change))
	_, _ = in.Units.Ended(ctx, unit(change)+finishing)
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

func cmpOr(s, or string) string {
	if s == "" {
		return or
	}
	return s
}
