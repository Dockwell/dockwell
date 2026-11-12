// SPDX-License-Identifier: AGPL-3.0-only

package autoupdate

import (
	"strconv"
	"strings"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
)

// versionVars are the variables official images say their version in, where it
// isn't the repository's own name with _VERSION, as NGINX_VERSION is nginx's.
var versionVars = map[string]string{
	"postgres": "PG_VERSION",
}

// VersionOf is the version an image says it is: its version label, or the
// variable its repository's official image keeps it in, such as GHOST_VERSION.
// Empty where it says none.
func VersionOf(image string, c *dockwellv1.ImageConfig) string {
	if c == nil {
		return ""
	}
	if v := strings.TrimSpace(c.GetVersion()); v != "" {
		return v
	}
	repo := docker.Repository(image)
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		repo = repo[i+1:]
	}
	name, ok := versionVars[repo]
	if !ok {
		name = strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(repo)) + "_VERSION"
	}
	for _, v := range c.GetEnv() {
		if v.GetName() == name {
			return strings.TrimSpace(v.GetValue())
		}
	}
	return ""
}

// numbers are a version's numbers, as "v5.3.1-alpine" has 5, 3 and 1, or nil for
// one that doesn't start with a number. A Debian epoch, as "1:11.4.4", goes.
func numbers(version string) []int {
	if _, after, ok := strings.Cut(version, ":"); ok {
		version = after
	}
	version = strings.TrimPrefix(strings.TrimPrefix(version, "v"), "V")
	end := strings.IndexFunc(version, func(r rune) bool { return r != '.' && (r < '0' || r > '9') })
	if end >= 0 {
		version = version[:end]
	}
	var out []int
	for _, p := range strings.Split(strings.Trim(version, "."), ".") {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil
		}
		out = append(out, n)
	}
	return out
}

// Step is how far a moving tag's new image moves from the one it replaces, as
// their versions say: "patch" for the same release rebuilt or its last place,
// "minor" for its second place, "major" for its first. A move of a version with
// two places in its second, such as 5.2 to 5.3, could be either and counts as
// the larger, minor. Empty where either image says no version, or not one of
// numbers (ADR 0080).
func Step(image string, was, now *dockwellv1.ImageConfig) string {
	a, b := numbers(VersionOf(image, was)), numbers(VersionOf(image, now))
	if len(a) == 0 || len(b) == 0 {
		return ""
	}
	at := func(v []int, i int) int {
		if i < len(v) {
			return v[i]
		}
		return 0
	}
	switch {
	case at(a, 0) != at(b, 0):
		return "major"
	case at(a, 1) != at(b, 1):
		return "minor"
	}
	return "patch"
}

// NamesARelease says an image's tag names one release whole, with three places
// or more, as 1.27.3 does: its new image is that release rebuilt. A tag that
// names less, as latest, 16, 1.27 or 16.4 can, may move to another release.
func NamesARelease(image string) bool {
	tag := tagOf(image)
	v, _, _ := strings.Cut(tag, "-")
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) < 3 {
		return false
	}
	for _, p := range parts {
		if _, err := strconv.Atoi(p); err != nil {
			return false
		}
	}
	return true
}

// tagOf is an image's tag, or "latest" where it names none.
func tagOf(image string) string {
	image, _, _ = strings.Cut(image, "@")
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		return image[i+1:]
	}
	return "latest"
}
