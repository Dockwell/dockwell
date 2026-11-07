// SPDX-License-Identifier: AGPL-3.0-only

package updates

import (
	"context"
	"strings"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// apk's program.
const apkPath = "/sbin/apk"

// apk reads updates on Alpine, which doesn't mark security fixes.
type apk struct{}

func (apk) kind() dockwellv1.PackageManager { return dockwellv1.PackageManager_PACKAGE_MANAGER_APK }

// listsTime is when apk last fetched a repository's index, or the agent last had
// it do so.
func (apk) listsTime(r *Reader) time.Time {
	t := r.newestModTime("var/cache/apk/APKINDEX.*.tar.gz")
	if r.refreshed.After(t) {
		t = r.refreshed
	}
	return t
}

func (apk) refresh(ctx context.Context, r *Reader) error {
	args := []string{"-q", "update"}
	res, err := r.run(ctx, apkPath, RefreshTimeout, args...)
	if err != nil {
		return err
	}
	if res.code != 0 {
		return fail(apkPath, res, args...)
	}
	r.refreshed = r.now()
	return nil
}

func (apk) list(ctx context.Context, r *Reader) ([]*dockwellv1.PackageUpdate, []string, bool, error) {
	args := []string{"version", "-l", "<"}
	res, err := r.run(ctx, apkPath, ReadTimeout, args...)
	if err != nil {
		return nil, nil, false, err
	}
	if res.code != 0 {
		return nil, nil, false, fail(apkPath, res, args...)
	}
	waiting := parseApkVersion(res.out)
	if len(waiting) == 0 {
		return nil, nil, false, nil
	}
	args = []string{"info", "-d", "--"}
	for _, p := range waiting {
		args = append(args, p.GetName())
	}
	res, err = r.run(ctx, apkPath, ReadTimeout, args...)
	if err != nil {
		return nil, nil, false, err
	}
	if res.code != 0 {
		return nil, nil, false, fail(apkPath, res, args...)
	}
	summaries := parseApkInfo(res.out)
	for _, p := range waiting {
		p.Summary = summaries[p.GetName()]
	}
	return waiting, nil, false, nil
}

// parseApkVersion reads apk version -l '<': each installed package, as its name
// and version, beside the newer version its repositories have.
func parseApkVersion(out []byte) []*dockwellv1.PackageUpdate {
	var found []*dockwellv1.PackageUpdate
	for line := range strings.Lines(string(out)) {
		f := strings.Fields(line)
		if len(f) != 3 || f[1] != "<" {
			continue
		}
		name, version, ok := splitApk(f[0])
		if !ok {
			continue
		}
		found = append(found, &dockwellv1.PackageUpdate{Name: name, Installed: version, Available: f[2]})
	}
	return found
}

// splitApk reads "py3-foo-1.2.3-r0" as its name and version: Alpine's versions
// hold no "-" but the one before the release.
func splitApk(nameVersion string) (name, version string, ok bool) {
	release := strings.LastIndexByte(nameVersion, '-')
	if release <= 0 {
		return "", "", false
	}
	v := strings.LastIndexByte(nameVersion[:release], '-')
	if v <= 0 {
		return "", "", false
	}
	return nameVersion[:v], nameVersion[v+1:], true
}

// parseApkInfo reads apk info -d: a line naming each package and version, as
// "busybox-1.36.1-r29 description:", then its description.
func parseApkInfo(out []byte) map[string]string {
	found := map[string]string{}
	name := ""
	for line := range strings.Lines(string(out)) {
		line = strings.TrimSpace(line)
		if pkg, ok := strings.CutSuffix(line, " description:"); ok {
			name, _, _ = splitApk(pkg)
			continue
		}
		if name != "" && line != "" {
			found[name] = line
			name = ""
		}
	}
	return found
}

// reboot can't be told from apk, so the reader goes by the kernels installed.
func (apk) reboot(context.Context, *Reader) (bool, []string, bool) { return false, nil, false }

// automatic: Alpine has no tool of its own that installs updates.
func (apk) automatic(*Reader) *dockwellv1.AutomaticUpdates { return &dockwellv1.AutomaticUpdates{} }
