// SPDX-License-Identifier: AGPL-3.0-only

package updates

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
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
	// unitPrefix names an install's unit after its change.
	unitPrefix = "dockwell-install-"
)

// ErrChanged is an install whose updates aren't what its plan said any more, at
// its first start: a person never applies a list they didn't see.
var ErrChanged = errors.New("the updates waiting changed since this plan")

// ErrNotYet is a package manager Dockwell doesn't install with yet.
var ErrNotYet = errors.New("installing updates takes apt for now")

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
}

// PlanInstall simulates installing the updates waiting, the security fixes or all
// of them, as the exact command would: with versions pinned, and what they pull in.
func (r *Reader) PlanInstall(ctx context.Context, all bool) (*InstallPlan, error) {
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
		plan.DockerRestarts = plan.DockerRestarts || p.GetEffect() == dockwellv1.PackageEffect_PACKAGE_EFFECT_DOCKER_RESTART
	}
	return plan, nil
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
	if err := os.MkdirAll(in.Dir, 0o700); err != nil {
		return fmt.Errorf("can't make a folder for the install's output: %w", err)
	}
	name := unit(change)
	if active, err := in.Units.Active(ctx, name); err != nil {
		return err
	} else if active {
		say("Following the install that's running")
		return in.follow(ctx, change, planned, say)
	}
	_, err := os.Stat(in.log(change))
	started := err == nil
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
		if err := in.finishDpkg(ctx, say); err != nil {
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

// follow says the install's output as it comes until its unit ends, then checks
// every package reached its version. A caller that leaves stops the following,
// never the install, which the next start follows again.
func (in *Installer) follow(ctx context.Context, change string, planned []*dockwellv1.PackageUpdate, say func(string)) error {
	name := unit(change)
	var offset int64
	var partial string
	read := func() {
		f, err := os.Open(in.log(change))
		if err != nil {
			return
		}
		defer f.Close()
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return
		}
		b, _ := io.ReadAll(io.LimitReader(f, maxOutput))
		offset += int64(len(b))
		text := partial + string(b)
		lines := strings.Split(text, "\n")
		partial = lines[len(lines)-1]
		for _, l := range lines[:len(lines)-1] {
			if l = strings.TrimSpace(l); l != "" {
				say(l)
			}
		}
	}
	tick := time.NewTicker(followEvery)
	defer tick.Stop()
	for {
		read()
		active, err := in.Units.Active(ctx, name)
		if err != nil {
			return err
		}
		if !active {
			read()
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("stopped following the install, which goes on in its own unit; it's followed again when the change resumes: %w", ctx.Err())
		case <-tick.C:
		}
	}
	code, _ := in.Units.Ended(ctx, name)
	left, err := in.left(ctx, planned)
	if err != nil {
		return err
	}
	if len(left) > 0 {
		tail := lastLines(string(in.tail(change)))
		in.done(ctx, change)
		return fmt.Errorf("the install ended with status %d, and %s didn't reach the planned version: %s", code, plural(len(left), "package"), cmpOr(tail, "it said nothing"))
	}
	in.done(ctx, change)
	return nil
}

// left are the planned packages not yet at their planned version.
func (in *Installer) left(ctx context.Context, planned []*dockwellv1.PackageUpdate) ([]*dockwellv1.PackageUpdate, error) {
	names := make([]string, 0, len(planned))
	for _, p := range planned {
		names = append(names, p.GetName())
	}
	args := append([]string{"-W", "-f=${Package}\t${Version}\t${db:Status-Abbrev}\n", "--"}, names...)
	res, err := in.Reader.run(ctx, "Reading the installed versions", dpkgQuery, ReadTimeout, args...)
	if err != nil {
		return nil, err
	}
	// 1 is a name dpkg doesn't know yet, as a package an install pulls in.
	if res.code != 0 && res.code != 1 {
		return nil, fail("Reading the installed versions", res)
	}
	have := map[string]string{}
	for line := range strings.Lines(string(res.out)) {
		parts := strings.SplitN(strings.TrimRight(line, "\n"), "\t", 3)
		// Only a package installed and configured ("ii") counts.
		if len(parts) == 3 && strings.HasPrefix(parts[2], "ii") {
			have[parts[0]] = parts[1]
		}
	}
	var out []*dockwellv1.PackageUpdate
	for _, p := range planned {
		if have[p.GetName()] != p.GetAvailable() {
			out = append(out, p)
		}
	}
	return out, nil
}

// finishDpkg configures what a stopped run left unpacked, when dpkg says it did.
func (in *Installer) finishDpkg(ctx context.Context, say func(string)) error {
	res, err := in.Reader.run(ctx, "Asking dpkg what it left half done", dpkg, ReadTimeout, "--audit")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(res.out)) == "" {
		return nil
	}
	say("Finishing what a stopped install left half done")
	res, err = in.Reader.run(ctx, "Finishing what a stopped install left", dpkg, InstallTimeout, "--force-confold", "--configure", "-a")
	if err != nil {
		return err
	}
	if res.code != 0 {
		return fail("Finishing what a stopped install left", res)
	}
	return nil
}

// tail is the end of the install's output, for a failure's words.
func (in *Installer) tail(change string) []byte {
	b, err := os.ReadFile(in.log(change))
	if err != nil {
		return nil
	}
	if len(b) > errTail {
		b = b[len(b)-errTail:]
	}
	return b
}

// done lets the install go: its output is deleted, as nothing of it is kept.
func (in *Installer) done(ctx context.Context, change string) {
	_ = os.Remove(in.log(change))
	_, _ = in.Units.Ended(ctx, unit(change))
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
