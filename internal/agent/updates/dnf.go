// SPDX-License-Identifier: AGPL-3.0-only

package updates

import (
	"bufio"
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// dnf's programs.
const (
	dnfPath = "/usr/bin/dnf"
	rpmPath = "/usr/bin/rpm"
)

// dnf reads updates on AlmaLinux, Rocky Linux, RHEL and Fedora.
type dnf struct{}

func (dnf) kind() dockwellv1.PackageManager { return dockwellv1.PackageManager_PACKAGE_MANAGER_DNF }

// listsTime is when dnf last fetched a repository's metadata, or the agent last
// had it do so.
func (dnf) listsTime(r *Reader) time.Time {
	t := r.newestModTime("var/cache/dnf/*/repodata/repomd.xml")
	if r.refreshed.After(t) {
		t = r.refreshed
	}
	return t
}

func (dnf) refresh(ctx context.Context, r *Reader) error {
	args := []string{"-q", "-y", "--refresh", "makecache"}
	res, err := r.run(ctx, dnfPath, RefreshTimeout, args...)
	if err != nil {
		return err
	}
	if res.code != 0 {
		return fail(dnfPath, res, args...)
	}
	r.refreshed = r.now()
	return nil
}

func (dnf) list(ctx context.Context, r *Reader) ([]*dockwellv1.PackageUpdate, []string, bool, error) {
	args := []string{"-q", "--cacheonly", "check-update"}
	res, err := r.run(ctx, dnfPath, ReadTimeout, args...)
	if err != nil {
		return nil, nil, false, err
	}
	// 100 says there are updates, and 0 that there are none.
	if res.code != 100 && res.code != 0 {
		return nil, nil, false, fail(dnfPath, res, args...)
	}
	waiting := parseDnfCheckUpdate(res.out)
	if len(waiting) == 0 {
		return nil, nil, true, nil
	}
	marks := true
	args = []string{"-q", "--cacheonly", "updateinfo", "list", "--security"}
	res, err = r.run(ctx, dnfPath, ReadTimeout, args...)
	switch {
	case err != nil:
		return nil, nil, false, err
	case res.code != 0:
		// A repository with no advisories leaves what's a security fix unknown.
		r.logger.Warn("couldn't read which updates are security fixes", slog.Any("error", fail(dnfPath, res, args...)))
		marks = false
	default:
		advisories := parseDnfUpdateinfo(res.out)
		for _, p := range waiting {
			if a, ok := advisories[p.GetName()+"."+p.GetArchitecture()]; ok {
				p.Security, p.Advisory, p.Severity = true, a.id, a.severity
			}
		}
	}
	if err := rpmInstalled(ctx, r, waiting); err != nil {
		return nil, nil, false, err
	}
	return waiting, nil, marks, nil
}

// parseDnfCheckUpdate reads dnf check-update: a package, its new version and its
// repository, which dnf wraps onto a second line when the name is long. What it
// lists after "Obsoleting Packages" are the packages those replace.
func parseDnfCheckUpdate(out []byte) []*dockwellv1.PackageUpdate {
	var words []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), maxOutput)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "Obsoleting Packages") {
			break
		}
		if strings.HasPrefix(line, "Last metadata expiration check") || strings.HasPrefix(line, "Security:") {
			continue
		}
		words = append(words, strings.Fields(line)...)
	}
	var found []*dockwellv1.PackageUpdate
	for i := 0; i+2 < len(words); i += 3 {
		nameArch, version, repo := words[i], words[i+1], words[i+2]
		dot := strings.LastIndexByte(nameArch, '.')
		if dot <= 0 {
			continue
		}
		found = append(found, &dockwellv1.PackageUpdate{
			Name: nameArch[:dot], Architecture: nameArch[dot+1:], Available: version, Origin: repo,
		})
	}
	return found
}

// advisory is a security advisory that names a package.
type advisory struct{ id, severity string }

// parseDnfUpdateinfo reads dnf updateinfo list: an advisory, its severity and
// kind, as "Important/Sec.", and a package it fixes, by name, epoch, version,
// release and architecture. It returns the advisory of each package by its name
// and architecture, the most severe where several name it.
func parseDnfUpdateinfo(out []byte) map[string]advisory {
	found := map[string]advisory{}
	for line := range strings.Lines(string(out)) {
		f := strings.Fields(line)
		if len(f) < 3 || !strings.Contains(f[1], "Sec") {
			continue
		}
		name, arch, ok := splitNEVRA(f[2])
		if !ok {
			continue
		}
		severity, _, _ := strings.Cut(f[1], "/")
		key := name + "." + arch
		if was, ok := found[key]; ok && severityRank(was.severity) >= severityRank(severity) {
			continue
		}
		found[key] = advisory{id: f[0], severity: severity}
	}
	return found
}

// severityRank orders advisories' severities, so the worst is the one shown.
func severityRank(s string) int {
	switch s {
	case "Critical":
		return 4
	case "Important":
		return 3
	case "Moderate":
		return 2
	case "Low":
		return 1
	}
	return 0
}

// splitNEVRA reads "openssl-libs-1:3.0.7-28.el9_4.x86_64" as its name and
// architecture: the version and release are the last two parts with a "-".
func splitNEVRA(nevra string) (name, arch string, ok bool) {
	dot := strings.LastIndexByte(nevra, '.')
	if dot <= 0 {
		return "", "", false
	}
	nevr, arch := nevra[:dot], nevra[dot+1:]
	release := strings.LastIndexByte(nevr, '-')
	if release <= 0 {
		return "", "", false
	}
	version := strings.LastIndexByte(nevr[:release], '-')
	if version <= 0 {
		return "", "", false
	}
	return nevr[:version], arch, true
}

// rpmInstalled fills in each package's installed version and description, from
// rpm. A kernel has several installed; the newest is the one it updates from.
func rpmInstalled(ctx context.Context, r *Reader, ps []*dockwellv1.PackageUpdate) error {
	args := []string{"-q", "--qf", `%{NAME}.%{ARCH}\t%|EPOCH?{%{EPOCH}:}:{}|%{VERSION}-%{RELEASE}\t%{SUMMARY}\n`, "--"}
	for _, p := range ps {
		args = append(args, p.GetName()+"."+p.GetArchitecture())
	}
	res, err := r.run(ctx, rpmPath, ReadTimeout, args...)
	if err != nil {
		return err
	}
	// 1 is a package that isn't installed, as one an update brings in new.
	if res.code != 0 && res.code != 1 {
		return fail(rpmPath, res, args...)
	}
	type found struct{ version, summary string }
	have := map[string]found{}
	for line := range strings.Lines(string(res.out)) {
		parts := strings.SplitN(strings.TrimRight(line, "\n"), "\t", 3)
		if len(parts) != 3 {
			continue
		}
		if was, ok := have[parts[0]]; !ok || compareVersions(parts[1], was.version) > 0 {
			have[parts[0]] = found{version: parts[1], summary: parts[2]}
		}
	}
	for _, p := range ps {
		if f, ok := have[p.GetName()+"."+p.GetArchitecture()]; ok {
			p.Installed, p.Summary = f.version, f.summary
		}
	}
	return nil
}

// reboot asks dnf's needs-restarting, where dnf-plugins-core has it, which says
// whether a core package was updated since the server started, and which.
func (dnf) reboot(ctx context.Context, r *Reader) (bool, []string, bool) {
	res, err := r.run(ctx, dnfPath, ReadTimeout, "-q", "--cacheonly", "needs-restarting", "-r")
	if err != nil {
		return false, nil, false
	}
	out := string(res.out)
	switch {
	case strings.Contains(out, "Reboot is required"):
		var pkgs []string
		for line := range strings.Lines(out) {
			if name, ok := strings.CutPrefix(strings.TrimSpace(line), "* "); ok {
				pkgs = append(pkgs, name)
			}
		}
		return true, pkgs, true
	case strings.Contains(out, "Reboot should not be necessary"):
		return false, nil, true
	}
	return false, nil, false
}

// automatic says whether dnf-automatic installs updates: its install timer, or
// its plain one with apply_updates on.
func (dnf) automatic(r *Reader) *dockwellv1.AutomaticUpdates {
	if !r.exists("usr/bin/dnf-automatic") {
		return &dockwellv1.AutomaticUpdates{}
	}
	wants := "etc/systemd/system/timers.target.wants/"
	on := r.exists(wants + "dnf-automatic-install.timer")
	if !on && r.exists(wants+"dnf-automatic.timer") {
		b, _ := os.ReadFile(r.path("etc/dnf/automatic.conf"))
		for line := range strings.Lines(string(b)) {
			key, val, ok := strings.Cut(line, "=")
			if ok && strings.TrimSpace(key) == "apply_updates" {
				v := strings.ToLower(strings.TrimSpace(val))
				on = v == "yes" || v == "true" || v == "1" || v == "on"
			}
		}
	}
	return &dockwellv1.AutomaticUpdates{Tool: "dnf-automatic", On: on}
}
