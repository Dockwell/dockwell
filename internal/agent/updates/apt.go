// SPDX-License-Identifier: AGPL-3.0-only

package updates

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// apt's programs.
const (
	aptGet    = "/usr/bin/apt-get"
	aptCache  = "/usr/bin/apt-cache"
	dpkgQuery = "/usr/bin/dpkg-query"
)

// apt reads updates on Debian, Ubuntu and what's built on them.
type apt struct{}

func (apt) kind() dockwellv1.PackageManager { return dockwellv1.PackageManager_PACKAGE_MANAGER_APT }

// listsTime is when the lists were last fetched: the newest Release file, or
// the stamp Ubuntu's daily update leaves, or the agent's own last refresh.
func (apt) listsTime(r *Reader) time.Time {
	t := r.newestModTime("var/lib/apt/lists/*Release")
	if s := r.newestModTime("var/lib/apt/periodic/update-success-stamp"); s.After(t) {
		t = s
	}
	if r.refreshed.After(t) {
		t = r.refreshed
	}
	return t
}

func (apt) refresh(ctx context.Context, r *Reader) error {
	args := []string{"-q", "-o", "DPkg::Lock::Timeout=120", "update"}
	res, err := r.run(ctx, aptGet, RefreshTimeout, args...)
	if err != nil {
		return err
	}
	if res.code != 0 {
		return fail(aptGet, res, args...)
	}
	r.refreshed = r.now()
	return nil
}

func (apt) list(ctx context.Context, r *Reader) ([]*dockwellv1.PackageUpdate, []string, bool, error) {
	args := []string{
		"-q", "-o", "Debug::NoLocking=true", "-o", "APT::Get::Show-User-Simulation-Note=false",
		"--simulate", "--with-new-pkgs", "upgrade",
	}
	res, err := r.run(ctx, aptGet, ReadTimeout, args...)
	if err != nil {
		return nil, nil, false, err
	}
	if res.code != 0 {
		return nil, nil, false, fail(aptGet, res, args...)
	}
	waiting, held := parseAptSimulation(res.out)
	if err := aptSummaries(ctx, r, waiting); err != nil {
		return nil, nil, false, err
	}
	return waiting, held, true, nil
}

// aptSummaries fills in each package's description: from dpkg for what's
// installed, and from apt's lists for what an upgrade brings in new.
func aptSummaries(ctx context.Context, r *Reader, ps []*dockwellv1.PackageUpdate) error {
	var installed, fresh []string
	for _, p := range ps {
		if p.GetInstalled() != "" {
			installed = append(installed, p.GetName())
		} else {
			fresh = append(fresh, p.GetName())
		}
	}
	summaries := map[string]string{}
	if len(installed) > 0 {
		args := append([]string{"-W", "-f=${Package}\t${Architecture}\t${binary:Summary}\n", "--"}, installed...)
		res, err := r.run(ctx, dpkgQuery, ReadTimeout, args...)
		if err != nil {
			return err
		}
		// 1 is a name it doesn't know, such as one with another architecture's
		// suffix, which is then left without a description.
		if res.code != 0 && res.code != 1 {
			return fail(dpkgQuery, res, args...)
		}
		for line := range strings.Lines(string(res.out)) {
			parts := strings.SplitN(strings.TrimRight(line, "\n"), "\t", 3)
			if len(parts) == 3 {
				summaries[parts[0]+":"+parts[1]] = parts[2]
			}
		}
	}
	if len(fresh) > 0 {
		args := append([]string{"show", "--no-all-versions", "--"}, fresh...)
		res, err := r.run(ctx, aptCache, ReadTimeout, args...)
		if err != nil {
			return err
		}
		if res.code != 0 && res.code != 100 {
			return fail(aptCache, res, args...)
		}
		for k, v := range parseAptCacheShow(res.out) {
			summaries[k] = v
		}
	}
	for _, p := range ps {
		name, _, _ := strings.Cut(p.GetName(), ":")
		if s, ok := summaries[name+":"+p.GetArchitecture()]; ok {
			p.Summary = s
		} else if s, ok := summaries[name+":all"]; ok {
			p.Summary = s
		}
	}
	return nil
}

// aptInst is one line of what apt-get --simulate would install: the package, the
// version it has in brackets when it has one, then the new version, where it's
// from and its architecture.
var aptInst = regexp.MustCompile(`^Inst (\S+)(?: \[([^\]]*)\])? \((\S+) (.+) \[([^\]\s]+)\]\)`)

// parseAptSimulation reads apt-get --simulate upgrade: an Inst line for each
// package it would install, and the packages it holds back, which it lists
// indented under a line that says so.
func parseAptSimulation(out []byte) (waiting []*dockwellv1.PackageUpdate, held []string) {
	inHeld := false
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), maxOutput)
	for sc.Scan() {
		line := sc.Text()
		if inHeld {
			if strings.HasPrefix(line, " ") {
				held = append(held, strings.Fields(line)...)
				continue
			}
			inHeld = false
		}
		switch {
		case strings.HasPrefix(line, "The following packages have been kept back:"),
			strings.HasPrefix(line, "The following upgrades have been deferred due to phasing:"):
			inHeld = true
			continue
		case !strings.HasPrefix(line, "Inst "):
			continue
		}
		m := aptInst.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		p := &dockwellv1.PackageUpdate{Name: m[1], Installed: m[2], Available: m[3], Architecture: m[5]}
		p.Origin, p.Security = aptOrigin(m[4])
		waiting = append(waiting, p)
	}
	slices.Sort(held)
	return waiting, slices.Compact(held)
}

// aptOrigin reads where apt says a version comes from, "Ubuntu:24.04/noble-updates,
// Ubuntu:24.04/noble-security", into the suite to show, and whether one of them
// is a security suite. That's the one shown when there is.
func aptOrigin(from string) (suite string, security bool) {
	for _, item := range strings.Split(from, ", ") {
		label, rest, ok := strings.Cut(item, ":")
		if !ok {
			rest, label = item, ""
		}
		archive := rest
		if i := strings.LastIndexByte(rest, '/'); i >= 0 {
			archive = rest[i+1:]
		}
		sec := strings.HasSuffix(archive, "-security") || strings.Contains(label, "Security")
		if suite == "" || sec && !security {
			suite = archive
		}
		security = security || sec
	}
	return suite, security
}

// parseAptCacheShow reads apt-cache show's stanzas into each package's one-line
// description, by its name and architecture.
func parseAptCacheShow(out []byte) map[string]string {
	found := map[string]string{}
	var name, arch, summary string
	flush := func() {
		if name != "" && summary != "" {
			found[name+":"+arch] = summary
		}
		name, arch, summary = "", "", ""
	}
	for line := range strings.Lines(string(out)) {
		line = strings.TrimRight(line, "\n")
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "Package: "):
			name = strings.TrimPrefix(line, "Package: ")
		case strings.HasPrefix(line, "Architecture: "):
			arch = strings.TrimPrefix(line, "Architecture: ")
		case strings.HasPrefix(line, "Description: "), strings.HasPrefix(line, "Description-en: "):
			if summary == "" {
				_, summary, _ = strings.Cut(line, ": ")
			}
		}
	}
	flush()
	return found
}

// reboot is what Debian and Ubuntu write when an update asks for a reboot.
// Ubuntu always does, so there no file means none is needed; Debian writes it
// only with update-notifier-common.
func (apt) reboot(_ context.Context, r *Reader) (bool, []string, bool) {
	known := r.exists("usr/share/update-notifier/notify-reboot-required")
	if !r.exists("run/reboot-required") {
		return false, nil, known
	}
	b, _ := os.ReadFile(r.path("run/reboot-required.pkgs"))
	pkgs := strings.Fields(string(b))
	slices.Sort(pkgs)
	return true, slices.Compact(pkgs), true
}

// aptPeriodic is the setting that turns unattended-upgrades on.
var aptPeriodic = regexp.MustCompile(`^\s*APT::Periodic::Unattended-Upgrade\s+"([^"]*)"`)

// automatic says whether unattended-upgrades is installed and on, by apt's
// settings, the last file's word winning as apt reads them.
func (apt) automatic(r *Reader) *dockwellv1.AutomaticUpdates {
	if !r.exists("usr/bin/unattended-upgrade") {
		return &dockwellv1.AutomaticUpdates{}
	}
	files, _ := filepath.Glob(r.path("etc/apt/apt.conf.d/*"))
	slices.Sort(files)
	on := false
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // apt's own settings, by a fixed pattern under the root
		if err != nil {
			continue
		}
		for line := range strings.Lines(string(b)) {
			if m := aptPeriodic.FindStringSubmatch(line); m != nil {
				on = m[1] != "" && m[1] != "0"
			}
		}
	}
	return &dockwellv1.AutomaticUpdates{Tool: "unattended-upgrades", On: on}
}
